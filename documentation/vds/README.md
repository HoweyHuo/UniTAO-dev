# shared / mirrored vDS 接入流程

本文按操作顺序说明如何把一个网络的数据通过 Communication Channel（git 仓库）共享给另一个网络：

- **发布方**：在自己的 Data Service 上开一个 `shared/{name}` vDS，写进去的数据会推到 git 仓库。
- **订阅方**：在另一个网络的 Data Service 上开一个 `mirrored/{name}` vDS，从 git 仓库拉取，得到一份只读副本。

两边**不需要网络互通**，只需要都能访问同一个 git 仓库。设计细节（queue、history、epoch、死亡判定、压缩）见根目录 `README.md` 的 *Virtual Data Services* 一节。

示例中的路径按容器部署来写（镜像 `ghcr.io/howeyhuo/unitao-dev`，容器以宿主机用户的 UID 运行），命令里的 `$IMG` 指这个镜像。用 `docker-compose/script/unitao_vds.sh` 部署时，下面的 config 由脚本写好，见 `docker-compose/README.md`。

## 0. 先弄清楚安全靠什么

通道**不加密、不签名**：仓库里是普通的 gzip JSONL，能读仓库的人就能读到数据，能写仓库的人就能改。保密和权限完全交给 git 平台：

- 仓库设成**私有**，只给发布方和订阅方的 Data Service 访问权限。
- 两边唯一的密钥是各自 DS 的 **git SSH 密钥**（第 2 节），由 DS 自己生成，公钥加到 git 服务器上。
- 发布方和订阅方之间**不交换任何东西**：配同一个通道地址（仓库 + 分支 + folder）就连上了。这个地址也是来源的身份（`SourceId`），Inventory 按它把 shared 和它的各个 mirror 合并成一条记录。

## 1. 准备 git 仓库

1. 建一个空的**私有**仓库（如 gitee / GitHub / 内网 GitLab）。一个仓库可以放多个 vDS，每个 vDS 占一个目录（`folder`）。
2. 记下默认分支名。channel 的 `branch` 缺省是 `main`；gitee 新建仓库的默认分支是 `master`，要在配置里写明 `"branch": "master"`（`unitao_vds.sh` 会自己读远端的默认分支）。
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

### 3.1 配置

在 `config.json` 加 `vds` 段。**有 `vds` 段时 `local` 要显式写上**，否则只启用列出的 vDS：

```json
"vds": {
    "local": {},
    "shared": [{
        "name": "UniTao-Modeling",
        "channel": {
            "type": "git",
            "repo": "git@gitee.com:howeyhuo/unitao-channel-howey.git",
            "branch": "master",
            "folder": "WireguardNetwork/UniTao-Modeling",
            "workDir": "/opt/UniTAO/data/unitao-channel-howey"
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
| `subscriberTimeoutHours` | 订阅方超过多久不处理就判定死亡，默认 24 |
| `compactIntervalHours` | 压缩 git 历史的间隔，默认 24 |
| `historyReuseHours` | history 发布后多久之内，新订阅方直接分到它、不重新打包，默认 24 |

旧版本的 `signingKeyFile`、`channel.encrypt` 不再使用，留着也能启动（日志和 `/config` 的 `ignoredConfig` 会列出来），可以删掉。

### 3.2 启动并检查

```bash
docker compose up -d
curl -s localhost:8001/                                  # 应包含 "shared/UniTao-Modeling"
curl -s localhost:8001/shared/UniTao-Modeling/config   # 看 instanceId（来源身份）、lastPublishedSeq、subscribers、lastError
curl -s localhost:8004/inventory                         # 应包含 "UniTao-Modeling"
```

发布器启动后的第一轮会在仓库的 `folder` 下写出 `manifest.json`（`lastSeq` 为 0，`instance` 是本 DS 的实例 id，`epoch` 随机），这时还没有数据也会写。git 不保存空目录，`queue/`、`history/`、`subscribers/` 要等有文件时才出现。

### 3.3 写入数据

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

一个 `publishIntervalSec` 之内，`/config` 里的 `outboxSeq` 和 `lastPublishedSeq` 增加，仓库里出现 `queue/<from>-<to>.jsonl.gz`。还没有订阅方时 queue 会被清空，这是正常的：新订阅方总是从 history 开始。

## 4. 订阅方：开 mirrored vDS

### 4.1 配置

通道地址（repo、branch、folder）与发布方**完全相同**，不需要向发布方要任何密钥或指纹：

```json
"vds": {
    "local": {},
    "mirrored": [{
        "name": "UniTao-Modeling",
        "channel": {
            "type": "git",
            "repo": "git@gitee.com:howeyhuo/unitao-channel-howey.git",
            "branch": "master",
            "folder": "WireguardNetwork/UniTao-Modeling",
            "workDir": "/opt/UniTAO/data/unitao-channel-howey"
        }
    }]
}
```

| 字段 | 说明 |
|---|---|
| `name` | 必须等于发布方 shared vDS 的 `name` |
| `alias` | 本网使用的名字（路径 `/mirrored/{alias}` 和 Inventory dsId），缺省等于 `name`。同一来源的所有副本要用同一个 alias |
| `pullIntervalSec` | 拉取周期，默认 60 |
| `channel` | repo / branch / folder 与发布方相同。仓库地址写法可以不同（`git@host:a/b.git` 与 `ssh://git@host/a/b`、`https://host/a/b.git` 算同一个），来源身份照样一致 |

旧版本的 `trustedFingerprint`、`channel.decrypt` 不再使用，可以删掉。

订阅方的 DS 也要按第 2 节生成 SSH 身份，并把公钥加到 git 服务器（需要推送权限）。

### 4.2 启动并观察订阅过程

```bash
curl -s localhost:<port>/mirrored/UniTao-Modeling/config   # 看 subscriberId、subscription、loaded、cursor、epoch、status、reason
```

`subscriberId` 是订阅方第一次启动时生成的随机 id，保存在 `state.json`，也是它在仓库里进度文件的名字。

`subscription` 依次变化（发布周期和拉取周期都是 60 秒时，整个过程大约 2～3 分钟）：

| 订阅方 `subscription` | 发布方 `/config` 的 `subscribers[].state` | 发生了什么 |
|---|---|---|
| `none` | — | 还没订阅 |
| `waiting` | `subscribe` | 订阅方推了 `subscribers/<id>.json`（state=subscribe），等发布方打包 history |
| `waiting` | `subscribe` | 发布方推了 `history/<hid>/<n>.jsonl.gz` 并在 manifest 里把它分给这个请求；复用期内已有 history 时只在 manifest 里加一条 |
| `active` | `active` | 订阅方用 history 整体替换本地表，之后从 queue 增量消费，cursor 跟上 `lastPublishedSeq` |

之后数据可读，写入返回 405：

```bash
curl -s localhost:<port>/mirrored/UniTao-Modeling/wireguard_site/site-01
```

## 5. 日常操作

- **新增订阅方**：给它的 DS 仓库权限，配上同样的通道地址启动即可，发布方不用改任何东西。
- **移除订阅方**：在 git 平台上收回它的权限。它的进度文件在 `subscriberTimeoutHours` 后被发布方删除（用 `unitao_vds.sh remove` 删 mirrored 时立即删掉）。
- **订阅方长期离线**：超过 `subscriberTimeoutHours` 未处理会被判定死亡，进度文件被删除；它回来后自动重新订阅，等新 history 期间继续提供旧数据。
- **删除 mirrored**：从 config 删掉并重启，会**删除它的表**并从 Inventory 注销。
- **换 git 仓库或分支**：两边一起改 channel。来源身份随之改变，Inventory 里会出现新的记录；发布方在新通道上从当前 seq 开始，订阅方重新订阅。
- **备份与重建**：通道不要求备份任何东西。发布方整个丢失后，用同样的 shared 配置重建（新数据库、新 `state.json`）：它看到 manifest 是别的实例写的，日志 `take it over`，用自己的数据在新的 epoch 里重新开始这个 folder；订阅方配置不变，发现 epoch 变了就重新订阅，用重建后的数据整体替换本地副本。数据库从较旧的备份恢复时（通道上的 seq 超过数据库已提交的），也是这样重新开始。重建的 DS 有新的 SSH 密钥，要加到 git 服务器上。

## 6. 排错

| 现象 | 看哪里 / 原因 |
|---|---|
| 发布方 `/config` 有 `lastError` | 最近一轮失败的原因，下一轮自动重试 |
| `DeployKey only supports pull/fetch operations` | gitee 部署公钥只读，改用账号 SSH 公钥 |
| `Permission denied (publickey)` | DS 的 SSH 公钥还没加到 git 服务器，或加错了账号 |
| `No user exists for uid` | 容器里 `/etc/passwd` 缺少运行 UID：镜像太旧（入口脚本自动补记录之前的版本），或者以只读方式挂了不含这个 UID 的 `/etc/passwd` |
| `git ... timed out after 10m0s` / `Connection reset by peer` | 网络问题；每条 git 命令最多 10 分钟，ssh 连接卡住约 1 分钟断开，下一轮重试 |
| 发布方日志 `WARNING: channel folder was written by DS instance [...]; take it over` | 第一次出现：重建后的实例接管了 folder，正常。**每轮都出现**：两个正在运行的 DS 配了同一个 folder，在互相接管，给其中一个换 folder |
| 发布方日志 `beyond the local outbox seq ... (local state lost or restored from a backup)` | 数据库比通道旧（从旧备份恢复、或换了数据库），发布方在新 epoch 里重新开始，订阅方会重新订阅 |
| 发布方 `subscribers[].state` 为 `gap` | 该订阅方的进度落在已删除的 batch 之前，或属于旧 epoch；它下一轮会重新订阅 |
| 订阅方 `status` 为 `paused` | 最近一轮同步出错，下一轮自动重试，原因在 `reason`。网络 / git 错误会自己恢复 |
| 订阅方一直 `waiting` | 发布方没有处理它的请求：检查发布方是否在跑、两边的通道地址（repo、branch、folder）是否一致、发布方日志 |
| `/config` 有 `ignoredConfig` | config 里还留着旧版本的密钥字段（`signingKeyFile`、`trustedFingerprint`、`channel.encrypt`、`channel.decrypt`），不影响运行，可以删掉 |
