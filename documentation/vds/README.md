# shared / mirrored vDS 接入流程

本文按操作顺序说明如何把一个网络的数据通过 Communication Channel（git 仓库）共享给另一个网络：

- **发布方**：在自己的 Data Service 上开一个 `shared/{name}` vDS，写进去的数据会被签名、加密后推到 git 仓库。
- **订阅方**：在另一个网络的 Data Service 上开一个 `mirrored/{name}` vDS，从 git 仓库拉取并解密，得到一份只读副本。

两边**不需要网络互通**，只需要都能访问同一个 git 仓库。设计细节（queue、history、死亡判定、压缩）见根目录 `README.md` 的 *Virtual Data Services* 一节。

示例中的路径按容器部署来写（镜像 `ghcr.io/howeyhuo/unitao-dev`，容器以宿主机用户的 UID 运行），命令里的 `$IMG` 指这个镜像。

## 0. 先弄清楚有哪些密钥

| 密钥 | 谁生成 | 私钥放在 | 公开的部分交给谁 |
|---|---|---|---|
| git SSH 密钥 | 每个 DS 启动时自己生成 | `ssh.dir`（建议 Docker 命名卷） | 公钥加到 git 服务器 |
| 签名密钥（ed25519） | 发布方 `DataServiceAdmin keygen` | 发布方，`signingKeyFile` | 指纹交给每个订阅方，填 `trustedFingerprint` |
| 加密身份（X25519） | 订阅方 `DataServiceAdmin enckey` | 订阅方，`decrypt.identityFile` | 公钥（recipient）交给发布方，加进 `recipientsFile` |

规则：**私钥不离开生成它的一方**，两边只交换公钥 / 指纹。指纹和 recipient 要通过可信的途径交换（当面、内部 IM 等），不要只靠 git 仓库本身——仓库里的东西任何有写权限的人都能改。

## 1. 准备 git 仓库

1. 建一个空仓库（如 gitee / GitHub / 内网 GitLab）。一个仓库可以放多个 vDS，每个 vDS 占一个目录（`folder`）。
2. 记下默认分支名。channel 的 `branch` 缺省是 `main`；gitee 新建仓库的默认分支是 `master`，要在配置里写明 `"branch": "master"`。
3. 两边都需要**推送权限**：发布方推数据，订阅方也要推自己的进度文件 `subscribers/<id>.json`。
   - gitee 的「部署公钥」只能拉取（`DeployKey only supports pull/fetch operations`），不能用；要用账号 SSH 公钥（个人账号，或只加入这个仓库的机器人账号）。GitHub / GitLab 的 deploy key 勾上写权限即可。
4. 发布方还需要**强制推送**：每 `compactIntervalHours`（默认 24）把分支压缩成一个提交。分支保护禁止强推时只是压缩失败并记日志，其它照常。

## 2. 两边共同：DS 的 SSH 身份

每个 DS 第一次启动时在 `ssh.dir` 下生成 `id_ed25519`，已有则沿用。建议放在 Docker 命名卷里，私钥不出现在宿主机的 bind-mount 目录中：

```json
"ssh": { "dir": "/opt/UniTAO/ssh" }
```

```yaml
services:
  ds:
    volumes:
      - ds-ssh:/opt/UniTAO/ssh
volumes:
  ds-ssh:
    name: unitao-ds01-ssh
    external: true     # docker compose down -v 不会删它
```

```bash
# 创建卷：镜像里 /opt/UniTAO/ssh 是 1777，新卷第一次挂载时继承这个权限，非 root UID 也能写
docker run --rm -v unitao-ds01-ssh:/opt/UniTAO/ssh --entrypoint true $IMG
```

启动后公钥在三处可以看到，加到 git 服务器上：

```bash
grep -A4 '"ssh"' <config 目录>/state.json
curl -s localhost:8001/local/config        # instance.ssh
grep 'ssh public key' <日志目录>/<DS id>.log
```

验证（在容器里，用 DS 相同的 ssh 参数）：

```bash
docker exec <ds 容器> sh -c "ssh -i /opt/UniTAO/ssh/id_ed25519 -o IdentitiesOnly=yes \
  -o UserKnownHostsFile=/opt/UniTAO/ssh/known_hosts -o StrictHostKeyChecking=accept-new \
  -o BatchMode=yes -T git@gitee.com"
# 期望：Hi <账号>! You've successfully authenticated ...
```

注意：ssh 要求运行它的 UID 在 `/etc/passwd` 里有记录，否则报 `No user exists for uid`。镜像的入口脚本会在容器启动时给非 root UID 自动补上，不用另外处理；只有自己以只读方式挂了 `/etc/passwd` 时，才需要保证里面有这个 UID。

## 3. 发布方：开 shared vDS

### 3.1 生成签名密钥

签名私钥同样放进命名卷（以只读方式挂给 DS）。新建的普通命名卷属于 root，先交给运行 DS 的 UID：

```bash
docker volume create unitao-ds01-keys
docker run --rm -v unitao-ds01-keys:/k --entrypoint sh $IMG -c 'chown 1000:1000 /k && chmod 700 /k'
docker run --rm --user 1000:1000 -v unitao-ds01-keys:/keys $IMG \
  DataServiceAdmin keygen -out /keys/<name>.pem
# 输出 fingerprint: SHA256:...   ← 交给订阅方
```

丢了签名私钥，所有订阅方都要换 `trustedFingerprint` 并重新订阅，卷要保管好。

### 3.2 准备订阅方白名单

`recipientsFile` 是订阅方白名单：一行一个 recipient（base64），可以用 `#` 写注释。它只含公钥，可以放在 config 目录。可以是空的；文件不存在时 DS 启动时会在配置的路径上建一个只有注释的空文件（日志 `created empty recipients file [...]`，`/config` 的 `channel.recipientsFile` 显示路径），路径写错时能看到这个多出来的文件。所在目录必须已经存在：

```
# <订阅方名字> subscriber id dcf15914...
DN2e4U7yVl5OaTpc4+83AeFdDtHFNXZBmLa241bd9zA=
```

**还没有订阅方时，白名单留空即可**：DS 照常启动、写出 manifest，`/config` 显示 `"recipients": 0`。这期间的变更只推进 manifest 的 `lastSeq`，不写 queue batch（没有人能解密）。订阅方的 recipient 加进来并重启 DS 后，它订阅时拿到的 history 就包含之前的全部数据，之后的变更正常走 queue。

### 3.3 配置

在 `config.json` 加 `vds` 段。**有 `vds` 段时 `local` 要显式写上**，否则只启用列出的 vDS：

```json
"vds": {
    "local": {},
    "shared": [{
        "name": "UniTao-Modeling",
        "signingKeyFile": "/opt/UniTAO/keys/UniTao-Modeling.pem",
        "channel": {
            "type": "git",
            "repo": "git@gitee.com:howeyhuo/unitao-channel-howey.git",
            "branch": "master",
            "folder": "WireguardNetwork/UniTao-Modeling",
            "workDir": "/opt/UniTAO/data/unitao-channel-howey",
            "encrypt": { "recipientsFile": "/opt/UniTAO/config/recipients" }
        }
    }]
}
```

| 字段 | 说明 |
|---|---|
| `name` | vDS 名字，只能 `[A-Za-z0-9_-]`。出现在路径 `/shared/{name}`、Inventory 的 dsId、表名 `{table.data}_shared_{name}` 里，订阅方的 `name` 必须与它相同。定下来后不要改 |
| `folder` | 在仓库里的目录，可多级，缺省等于 `name` |
| `workDir` | 本地克隆的位置。同一 DS 内 repo + branch 相同的 vDS 可以共用一个 workDir |
| `publishIntervalSec` | 发布周期，默认 60 |
| `subscriberTimeoutHours` | 订阅方超过多久不处理就判定死亡，默认 720 |
| `compactIntervalHours` | 压缩 git 历史的间隔，默认 24 |

compose 里把签名密钥卷只读挂进去：

```yaml
    volumes:
      - ds-keys:/opt/UniTAO/keys:ro
volumes:
  ds-keys:
    name: unitao-ds01-keys
    external: true
```

### 3.4 启动并检查

```bash
docker compose up -d
curl -s localhost:8001/                                  # 应包含 "shared/UniTao-Modeling"
curl -s localhost:8001/shared/UniTao-Modeling/config   # 看 fingerprint、recipients、lastPublishedSeq、subscribers、lastError
curl -s localhost:8004/inventory                         # 应包含 "UniTao-Modeling"
```

发布器启动后的第一轮会在仓库的 `folder` 下写出签名的 `manifest.json` 和 `manifest.sig`（`lastSeq` 为 0），这时还没有数据也会写。git 不保存空目录，`queue/`、`history/`、`subscribers/` 要等有文件时才出现。

### 3.5 写入数据

shared vDS 有自己独立的 schema，先注册 schema 再写数据（只能引用同一个 shared vDS 内的记录）：

```bash
curl -s -X POST localhost:8001/shared/UniTao-Modeling -H 'Content-Type: application/json' -d '{
  "__type": "schema", "__id": "wireguard_site", "__ver": "0.0.1",
  "data": {
    "name": "wireguard_site", "version": "0.0.1", "description": "a wireguard site",
    "properties": { "location": { "type": "string" } }
  }
}'
curl -s -X POST localhost:8001/shared/UniTao-Modeling -H 'Content-Type: application/json' -d '{
  "__type": "wireguard_site", "__id": "site-01", "__ver": "0.0.1",
  "data": { "location": "beijing" }
}'
```

一个 `publishIntervalSec` 之内，`/config` 里的 `outboxSeq` 和 `lastPublishedSeq` 增加，仓库里出现 `queue/<from>-<to>.jsonl.gz.enc`。白名单为空时只有 manifest 的 `lastSeq` 前进、不会出现 queue 文件；有接收方但没有订阅方时 queue 会被清空。两者都是正常的：新订阅方总是从 history 开始。

## 4. 订阅方：开 mirrored vDS

### 4.1 生成加密身份

在**订阅方**生成，私钥留在订阅方：

```bash
docker volume create unitao-ds02-keys
docker run --rm -v unitao-ds02-keys:/k --entrypoint sh $IMG -c 'chown 1000:1000 /k && chmod 700 /k'
docker run --rm --user 1000:1000 -v unitao-ds02-keys:/keys $IMG \
  DataServiceAdmin enckey -identity /keys/identity -recipient /keys/recipient
# 输出 subscriber id: ...
docker run --rm -v unitao-ds02-keys:/keys --entrypoint cat $IMG /keys/recipient   # ← 交给发布方
```

### 4.2 发布方把它加进白名单

发布方把这一行 recipient 追加到 `recipientsFile`，然后**重启发布方 DS**（白名单只在启动时读取）。

### 4.3 配置

向发布方要到**签名指纹**（3.1 的输出，或 `/shared/{name}/config` 的 `fingerprint`）：

```json
"vds": {
    "local": {},
    "mirrored": [{
        "name": "UniTao-Modeling",
        "trustedFingerprint": "SHA256:4c8d18d7abd497fd176b247b428fb1a4086038944654a04b0f892a4ba846f44d",
        "channel": {
            "type": "git",
            "repo": "git@gitee.com:howeyhuo/unitao-channel-howey.git",
            "branch": "master",
            "folder": "WireguardNetwork/UniTao-Modeling",
            "workDir": "/opt/UniTAO/data/unitao-channel-howey",
            "decrypt": { "identityFile": "/opt/UniTAO/keys/identity" }
        }
    }]
}
```

| 字段 | 说明 |
|---|---|
| `name` | 必须等于发布方 shared vDS 的 `name` |
| `alias` | 本网使用的名字（路径 `/mirrored/{alias}` 和 Inventory dsId），缺省等于 `name`。同一来源的所有副本要用同一个 alias |
| `trustedFingerprint` | 发布方签名公钥的指纹。不一致时镜像暂停，不应用任何数据 |
| `pullIntervalSec` | 拉取周期，默认 60 |
| `channel` | repo / branch / folder 与发布方相同；`decrypt.identityFile` 指向 4.1 的私钥 |

订阅方的 DS 也要按第 2 节生成 SSH 身份，并把公钥加到 git 服务器（需要推送权限）。

### 4.4 启动并观察订阅过程

```bash
curl -s localhost:<port>/mirrored/UniTao-Modeling/config   # 看 subscriberId、subscription、loaded、cursor、status、reason
```

`subscription` 依次变化（发布周期和拉取周期都是 60 秒时，整个过程大约 2～3 分钟）：

| 订阅方 `subscription` | 发布方 `/config` 的 `subscribers[].state` | 发生了什么 |
|---|---|---|
| `none` | — | 还没订阅 |
| `waiting` | `subscribe` | 订阅方推了 `subscribers/<id>.json`（state=subscribe），等发布方打包 history |
| `waiting` | `subscribe` | 发布方推了 `history/<id>-<seq>-<request>.jsonl.gz.enc` |
| `active` | `active` | 订阅方用 history 整体替换本地表，之后从 queue 增量消费，cursor 跟上 `lastPublishedSeq` |

之后数据可读，写入返回 405：

```bash
curl -s localhost:<port>/mirrored/UniTao-Modeling/wireguard_site/site-01
```

## 5. 日常操作

- **新增订阅方**：订阅方按 4.1 生成身份 → 发布方追加 recipient 并重启 → 订阅方配置 mirrored 并启动。
- **移除订阅方**：发布方从 `recipientsFile` 删掉它那一行并重启。它的进度文件会被忽略（`untrusted`），之后的 batch 不再为它加密，也不为它保留。
- **订阅方长期离线**：超过 `subscriberTimeoutHours` 未处理会被判定死亡，进度文件被删除；它回来后自动重新订阅，等新 history 期间继续提供旧数据。
- **删除 mirrored**：从 config 删掉并重启，会**删除它的表**并从 Inventory 注销。
- **换 git 仓库或分支**：两边一起改 channel；发布方发现通道落后会从当前 seq 重建，订阅方发现自己的进度不在新通道上会重新订阅。

## 6. 排错

| 现象 | 看哪里 / 原因 |
|---|---|
| 发布方 `/config` 有 `lastError` | 最近一轮失败的原因，下一轮自动重试 |
| `DeployKey only supports pull/fetch operations` | gitee 部署公钥只读，改用账号 SSH 公钥 |
| `Permission denied (publickey)` | DS 的 SSH 公钥还没加到 git 服务器，或加错了账号 |
| `No user exists for uid` | 容器里 `/etc/passwd` 缺少运行 UID：镜像太旧（入口脚本自动补记录之前的版本），或者以只读方式挂了不含这个 UID 的 `/etc/passwd` |
| `git ... timed out after 10m0s` / `Connection reset by peer` | 网络问题；每条 git 命令最多 10 分钟，ssh 连接卡住约 1 分钟断开，下一轮重试 |
| 发布方 `subscribers[].state` 为 `untrusted` | 该订阅方的 recipient 不在 `recipientsFile` 里，或加了之后没有重启发布方 |
| 订阅方 `status` 为 `paused` | 最近一轮同步出错，下一轮自动重试，原因在 `reason`。网络 / git 错误会自己恢复；`does not match trusted fingerprint` 是 `trustedFingerprint` 与发布方不一致；解密失败是身份与白名单里的 recipient 不配对 |
| 订阅方一直 `waiting` | 发布方没有处理它的请求：检查发布方是否在跑、白名单、发布方日志 |
| 发布方启动失败 `requires [channel.encrypt.recipientsFile]` / `failed to create recipients file` | 没有配置白名单，或白名单所在目录不存在 / 不可写（路径写错、只读挂载） |
| 发布方日志 `no recipients: advanced to seq N without a queue batch` | 白名单为空，变更没有写进 queue；加入订阅方的 recipient 并重启后，订阅方从 history 拿到全部数据 |
