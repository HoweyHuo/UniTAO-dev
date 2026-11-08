# Feature Plan：Channel 改为队列模型（单 publisher、多 subscriber）

> 状态：草案 v1。决定已与需求方确认（见第 8 节）。替换 `feature-distributed-ds` 中的 snapshot + segment 通道协议。

## 1. 背景

现在的通道协议（manifest + `snapshots/` + `segments/`）有两个问题：

1. **仓库里反复存全量数据。** publisher 每隔 `snapshotIntervalSec` 就发一份全量 snapshot，不管期间有没有变更。旧文件虽然从工作目录删掉，但都留在 git 历史里，仓库体积持续增长。
2. **publisher 不知道有谁在读。** 它无法判断哪些增量已经没人需要，只能靠 snapshot 覆盖旧 segment。

我们实际需要的是一个**基于 git 的、单生产者、低频更新的 Kafka**：

- 一个 vDS 相当于一个 topic，只有一个 publisher（shared vDS），可以有多个 subscriber（mirrored vDS）。
- 每个 subscriber 记录自己的消费进度；增量 batch 在所有 subscriber 都处理过之后删除。
- 新 subscriber 加入时，由 publisher 单独为它打包一份当前数据（history batch）。
- subscriber 太久没有进度，视为死亡；它重新上线后从头订阅，整个替换本地数据。
- 删除文件后压缩 git 历史，让仓库只保留尚未被消费的内容。

## 2. 地址：repo + branch + folder

一个 vDS 在通道上的位置由三部分决定：

| 部分 | 配置 | 说明 |
|---|---|---|
| repo | `channel.repo` | git 远端（dir 通道为 `channel.path`，即根目录） |
| branch | `channel.branch` | 默认 `main` |
| folder | `channel.folder` | 仓库内的目录，可以多级，如 `dc/catalog`；默认等于 vDS 名字 |

同类或同一应用的 vDS 放在同一个 branch 上，一次 fetch 就能一起拉下来。一个仓库可以同时容纳多个 branch、每个 branch 上多个 vDS。

> 用 `folder` 而不是 `path`，因为 dir 通道的 `path` 已经表示通道根目录。

## 3. 目录结构

```
<folder>/
  manifest.json                 publisher 写，ed25519 签名
  manifest.sig
  queue/<from>-<to>.jsonl.gz.enc        增量 batch，seq 连续
  history/<subscriberId>-<seq>.jsonl.gz.enc   给某个 subscriber 的全量 batch
  subscribers/<subscriberId>.json       subscriber 自己写的进度
```

### manifest.json（version 2）

```json
{
  "version": 2,
  "source": "catalog",
  "publicKey": "<ed25519 公钥 base64>",
  "lastSeq": 135,
  "queue": [
    { "from": 121, "to": 135, "file": "queue/...", "sha256": "...", "published": "2026-11-08T05:30:09Z" }
  ],
  "histories": [
    { "subscriber": "<id>", "request": "<请求 id>", "seq": 120, "file": "history/...", "sha256": "...", "published": "..." }
  ],
  "updated": "..."
}
```

- `lastSeq` 单独记录：queue 可能被清空，不能再靠 batch 推算最大 seq。
- `published` 是 publisher 自己的时钟，死亡判定只用它，不受各机器时钟偏差影响。
- manifest 签名覆盖所有 queue / history 文件的 sha256，subscriber 校验签名和摘要后才应用。`subscribers/` 不在 manifest 里。

### subscribers/\<id\>.json

```json
{ "id": "<id>", "request": "<请求 id>", "state": "subscribe", "cursor": 0, "updated": "..." }
{ "id": "<id>", "request": "<请求 id>", "state": "active",    "cursor": 135, "updated": "..." }
```

## 4. 身份与加密

- **加密是必须的。** shared vDS 必须配置 `channel.encrypt.recipientsFile`，mirrored vDS 必须配置 `channel.decrypt.identityFile`，否则启动时配置校验失败。
- **subscriber 的 id = 它加密公钥（X25519）的 sha256 十六进制。** `recipientsFile` 就是 subscriber 白名单：publisher 只认 id 在白名单里的 subscriber 文件，其它文件记日志后忽略。这样能写仓库的人也无法冒充 subscriber 拿到数据。
- queue batch 对白名单里所有公钥加密；history batch 只对请求它的那个 subscriber 加密。
- 每个 mirrored vDS 实例要有自己的 identity。两个实例共用一把 identity 会写同一个 subscriber 文件，互相干扰。
- 进度文件第一版**不签名**。伪造进度最多让 batch 被提前删除；受影响的 subscriber 会发现缺口并自动重新订阅，不会拿到错误的数据。

## 5. 流程

### 5.1 发布（不变）

写入 → Journal → outbox（seq 连续）→ publisher 按连续 seq 打包成 `queue/` batch，追加到 manifest，推进 `lastSeq`。

### 5.2 订阅

1. subscriber 生成新的请求 id，写 `subscribers/<id>.json`：`state=subscribe, cursor=0`。
2. publisher 发现某个 subscriber 处于 `subscribe`，且 manifest 里没有这个请求 id 对应的 history：在发布锁内扫描数据（与现在的 snapshot 相同），生成 `history/<id>-<S>`，S = 当前 `lastSeq`。
3. subscriber 看到属于自己请求的 history：**清空本地同步数据**，加载 history，本地 cursor = S；然后写 `state=active, cursor=S`。
4. 之后从 queue 的 S+1 开始按顺序读。

### 5.3 消费

subscriber 每次拉取后，按顺序应用 seq > cursor 的 batch，更新本地 cursor（`state.json`），再把 cursor 写进自己的进度文件推送。**cursor 没有变化时不写**，空闲时不产生提交。

本地 `state.json` 是真实进度，远端进度文件只是汇报。远端比本地大（本地状态丢失）时，subscriber 重新订阅。

### 5.4 删除

publisher 每轮发布后：

- 存活 subscriber 中最小的进度 = min（`active` 取 cursor；`subscribe` 且已有 history 的取 history 的 S）。
- 删除 `to <= min` 的 queue batch；没有存活 subscriber 时 queue 全部删除。
- `active` 且 cursor >= S 的 subscriber，删除它的 history。

### 5.5 死亡判定

shared vDS 配置 `subscriberTimeoutHours`（默认 720，即 30 天）。

- `active` 的 subscriber：它**第一个还没处理的 batch** 发布超过 timeout，判定死亡。cursor 已经追平 `lastSeq` 的永远不会被判死，空闲不需要心跳。
- `subscribe` 的 subscriber：history 发布超过 timeout 仍没加载，判定死亡。
- 处理：publisher 删除它的进度文件和 history，它不再参与 min 计算。

subscriber 重新上线后，发现下面任一情况就**重新订阅**（5.2），加载 history 时整个替换本地数据：

- 本地记录为已订阅，但远端进度文件不见了；
- cursor + 1 已经不在 queue 里（中间的 batch 已删除）；
- 远端进度文件的请求 id 与本地不一致。

重新订阅期间，镜像继续提供旧数据，直到新 history 加载完成。

### 5.6 压缩 git 历史

publisher 删除过文件、且距上次压缩超过 `squashIntervalHours`（默认 24）时：

1. 用当前文件树建一个**没有父提交**的新提交；
2. `git push --force-with-lease=refs/heads/<branch>:<fetch 到的提交>`：只有远端在这次 fetch 之后没有变化才能成功，不会冲掉别人刚推上去的提交；失败就等下一轮。
3. 本地执行 `reflog expire` + `gc --prune=now` 回收旧对象。

服务端什么时候回收旧对象由 git 服务决定（Gitee 等），我们控制不了。分支必须允许强推。dir 通道没有历史，不做这一步。

## 6. git 写入方式

现在 publisher 是「fetch + rebase」。引入 subscriber 写入和强推之后，同一个 branch 上有多个写入方，有人改写历史后本地 rebase 会出问题。改为：

1. `fetch` 后 `reset --hard` 到远端；
2. 只写自己负责的文件（publisher：自己 folder 下的 manifest、queue、history，以及删除死亡 subscriber 的文件；subscriber：自己的进度文件）；
3. `commit` 后普通 `push`；被拒就回到第 1 步重做，最多重试 N 次。

每个写入方只碰自己的文件，所以重做不会冲掉别人的修改。写操作实现成「给定当前文件树，算出要写的内容」的函数，便于重做。

subscriber 现在也要 push，所以**镜像方需要可写的凭据**（之前只读即可）。

## 7. 配置变化

```json
"shared": [{
  "name": "catalog",
  "signingKeyFile": "...",
  "publishIntervalSec": 60,
  "subscriberTimeoutHours": 720,
  "squashIntervalHours": 24,
  "channel": { "type": "git", "repo": "...", "branch": "infra", "folder": "dc/catalog", "workDir": "...",
               "encrypt": { "recipientsFile": "..." } }
}],
"mirrored": [{
  "name": "catalog",
  "trustedFingerprint": "...",
  "pullIntervalSec": 60,
  "channel": { "type": "git", "repo": "...", "branch": "infra", "folder": "dc/catalog", "workDir": "...",
               "decrypt": { "identityFile": "..." } }
}]
```

- 删除 `snapshotIntervalSec`。
- 新增 `subscriberTimeoutHours`、`squashIntervalHours`、`channel.folder`。
- `channel.encrypt` / `channel.decrypt` 改为必填。
- `state.json` 中 mirrored 的状态增加请求 id 和订阅状态。
- `GET /{vDS}/config`：shared 显示 subscriber 列表（id、状态、cursor、是否判死）和 queue 范围；mirrored 显示订阅状态、请求 id、cursor。

## 8. 决定

| # | 决定 |
|---|---|
| Q1 | **直接替换**现有 snapshot + segment 协议，dir 和 git 两种通道都换。不兼容旧格式：publisher 发现 version 1 的 manifest 时清空该 folder 重新开始；镜像方旧状态作废，重新订阅。 |
| Q2 | 死亡按**延迟**判定（5.5），不用心跳；超时小时数由 shared vDS 配置 `subscriberTimeoutHours`。 |
| Q3 | subscriber 身份和加密绑定，**加密必须开启**；进度文件第一版不签名。 |
| Q4 | 压缩历史设最小间隔 `squashIntervalHours`，默认 24 小时。 |

## 9. 实现步骤

每一步都用 `test/src/VdsTest` 覆盖，并用 `docker-compose/shared-mirror*` 例子做端到端验证。

1. **协议与订阅**
   - Channel：manifest v2、`queue/`、`history/`、`subscribers/`；`folder` 配置；加密必填与 subscriber id。
   - Publisher：发布 queue batch；处理订阅请求、生成 history；按最小进度删除 queue 和 history。
   - Mirror：订阅流程、加载 history 时整体替换、按 queue 消费、写进度。
   - 配置校验、`/config` 输出。
   - 测试：新 subscriber 拿到 history 后接着消费增量；多个 subscriber 各自进度；全部消费后 batch 被删除；不在白名单的 subscriber 被忽略。
2. **git 写入、死亡判定、压缩**
   - git 改为 fetch + reset + 重做（第 6 节）；subscriber 推送进度。
   - 死亡判定与重新订阅（时钟可注入，便于测试）。
   - 压缩历史 + `--force-with-lease` + 本地 gc。
   - 测试：判死后重新订阅并整体替换；两个写入方并发时压缩不丢提交；压缩后其它写入方能继续工作。
3. **同 branch 共用克隆、例子和文档**
   - 同一进程里 repo + branch + workDir 相同的 vDS 共用一个工作目录和一次 fetch。
   - 两个 shared-mirror 例子：生成加密密钥；SiteB 对通道改为可写。
   - README、CLAUDE.md、`docker-compose/shared-mirror-git/README.md`。
