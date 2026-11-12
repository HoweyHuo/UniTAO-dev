# docker-compose

[English](README.md) | 中文

用 Docker Compose 部署 UniTAO。模板不在原地运行：脚本把模板复制到你指定的目录（如 `~/infra/<name>`），填好配置后在那里启动。每个部署出来的目录是一个 compose 项目，在该目录里用 `docker compose` 管理。

```
script/
  docker-deploy.sh     在 Ubuntu 上安装 Docker Engine（sudo）
  mongo_deploy.sh      部署 template/mongodb：MongoDB + mongo-express
  unitao_deploy.sh     部署 template/dataservice 或 template/inventory：一个 UniTAO 实例
  unitao_vds.sh        在已部署的 Data Service 上增删 shared / mirrored vDS
template/
  mongodb/             给 Data Service 用的 MongoDB（见其 README）
  dataservice/         带默认 local vDS 的 Data Service
  inventory/           Inventory Service
```

脚本在宿主机上运行，工具（`DataServiceAdmin`、`git`）用 UniTAO 镜像里的，不需要另装别的；只有 `unitao_deploy.sh` 和 `unitao_vds.sh` 需要 `python3`（Ubuntu 自带）。在 CI 发布带有当前功能的镜像之前，先在本地构建：`docker build -f ./docker/unitao/dockerfile -t ghcr.io/howeyhuo/unitao-dev:latest .`

## 部署

一个 UniTAO 系统放在一个目录里：它的 Inventory Service 和各个 Data Service 是这个目录的子目录，各自是一个 compose 项目。MongoDB 单独部署（每台主机一个，多个系统共用）。新系统按这个顺序装：

```
S=docker-compose/script
sudo $S/docker-deploy.sh                       # 1. 每台主机一次：Docker Engine，把你的用户加进 docker 组
$S/mongo_deploy.sh ~/infra/mongodb             # 2. MongoDB，建网络 mongo_net（已有 MongoDB 就跳过）
$S/unitao_deploy.sh inventory   ~/infra/unitao-howey   # 3. Inventory Service -> ~/infra/unitao-howey/inv
$S/unitao_deploy.sh dataservice ~/infra/unitao-howey   # 4. Data Service -> ~/infra/unitao-howey/ds（再运行一次：ds-2，……）
$S/unitao_vds.sh ~/infra/unitao-howey/ds               # 5. 可选：shared / mirrored vDS
```

```
~/infra/unitao-howey/        UniTAO 目录：一个系统
  inv/                       Inventory Service unitao-howey-inv
  ds/                        Data Service unitao-howey-ds
  ds-2/                      Data Service unitao-howey-ds-2
```

先装 Inventory：之后部署的 Data Service 会以它的 URL 为默认值，启动后立刻注册，马上就能通过 Inventory 查询。先装 Data Service 也能用：它每 10 秒重试一次注册，直到 Inventory 起来（之后部署 Inventory 时，用 Data Service 里填的名字、同一个网络）；但在那之前，引用其它类型的写入（`contentMediaType: "inventory/…"`）会失败。

直接运行 `$S/unitao_deploy.sh` 会一步步引导。它先问 UniTAO 目录，再找其子目录里已部署的实例（含 `inventory` 或 `dataservice` 服务的 `docker-compose.yml`）：

 - 一个都没有：新系统。建议先部署 Inventory Service（推荐），装完再接着部署一个 Data Service。
 - 有实例但没有 Inventory：建议先部署 Inventory。
 - 否则询问要部署哪一种；Data Service 以目录里那个 Inventory 的 URL 和网络为默认值。一个系统里再部署第二个 Inventory 要确认，否则拒绝。

`docker-deploy.sh` 自己选择 Docker 官方源或国内镜像（比如在中国时）。`mongo_deploy.sh` 询问 MongoDB 的 root 密码（回车自动生成），并打印连接方法；见 [`template/mongodb/README.md`](template/mongodb/README.md)。

`unitao_deploy.sh [dataservice|inventory] [<UniTAO 目录>]` 询问它需要的一切；每个问题都有默认值（回车接受），只有 MongoDB 密码必须输入。`<base>` 是 UniTAO 目录的名字（`unitao-howey`）：

| 问题 | 默认值 | |
|---|---|---|
| UniTAO 目录 | `~/infra/unitao` | 系统所在目录，不是实例目录 |
| 先部署 Inventory？ | 是 | 只在目录里还没有 Inventory 时询问 |
| 实例名 | `<base>-inv` / `<base>-ds`、`<base>-ds-2`、… | 容器名、compose 项目名，也是 Data Service 的 id |
| 实例目录 | UniTAO 目录下的 `inv` / `ds`、`ds-2`、… | 必须是新目录或空目录 |
| 本机端口 | 从 8000 / 8001 起第一个空闲的 | 跳过运行中容器的端口，以及目录里各实例的端口 |
| 镜像 | `ghcr.io/howeyhuo/unitao-dev:latest` | |
| Docker 网络 | 目录里各实例的网络，否则 `<base>_net` | 一个系统的 Data Service 和 Inventory Service 共用，不存在就建；它们用容器名互访 |
| Inventory Service URL | `http://<目录里的 Inventory>`，否则 `http://<该网络里运行中的 Inventory>`，否则 `http://<base>-inv` | 仅 Data Service。Inventory 在别的主机上时：`http://<host>:<port>` |
| 从别的主机访问的 URL | `http://<hostname>:<port>` | 仅 Data Service。和 `http://<name>` 一起注册到 Inventory；`localhost` 只在本机有效 |
| MongoDB 网络、地址、数据库、表名、用户、密码 | `mongo_net`、`mongodb://mongodb:27017`、`<name>`、`<name>`、`root` | 仅 Data Service |
| 同步间隔 | `300` | 仅 Inventory：与各 Data Service 同步 schema 的间隔秒数 |

然后它写出 `.env` 和 `config/config.json`（权限都是 600，config 里有 MongoDB 密码），建 `data/`、`logs/`（Data Service 还有 `channel/`），并启动实例。Data Service 启动时只有默认的 `local` vDS；这里不问任何通道相关的问题，那是 `unitao_vds.sh` 的事。

```
~/infra/unitao-howey/ds/
  docker-compose.yml   来自 template/dataservice
  .env                 回答的内容：COMPOSE_PROJECT_NAME、UNITAO_NAME、UNITAO_PORT、UNITAO_UID/GID、UNITAO_IMAGE、网络、UNITAO_CHANNEL_DIR
  config/              config.json、state.json，以及 config/vds/（shared / mirrored vDS 的密钥，见下文）
  data/                git 通道的克隆
  channel/             通道目录，直到 unitao_vds.sh 指定了别的目录
  logs/
```

容器以你的用户身份运行（`UNITAO_UID` / `UNITAO_GID`），所以它写出的文件都归你；镜像的入口脚本会把你的 UID 加进它的 `/etc/passwd`（ssh 需要）。Data Service 为 git 服务器生成的 ssh 密钥存在命名卷 `ssh` 里（`ssh.dir` = `/opt/UniTAO/ssh`），不在目录里。

以后要改某个回答，编辑 `.env` 或 `config/config.json`，再运行 `docker compose up -d`（改了容器名或端口会重建容器）。删除实例：在它的目录里运行 `docker compose down -v`（`-v` 连同 ssh 密钥卷一起删），再删掉目录；Data Service 的表留在 MongoDB 里，直到你删掉它的数据库。

## Shared 与 mirrored vDS

Data Service 可以经 Communication Channel（通常是双方都能访问的 git 仓库）把一个 vDS 共享给其它网络（`shared/<name>`，发布方），或保存别人共享的 vDS 的只读副本（`mirrored/<name>`，订阅方）。`unitao_vds.sh` 在已部署的 Data Service 上维护它们：

```
unitao_vds.sh <目录>                         # 交互菜单
unitao_vds.sh <目录> list
unitao_vds.sh <目录> add shared <name> --repo <url> [--folder <f>] [--branch <b>] [--publish-interval <s>] …
unitao_vds.sh <目录> add mirrored <name> --fingerprint <SHA256:…> --repo <url> [--folder <f>] [--alias <a>] [--pull-interval <s>] …
unitao_vds.sh <目录> show shared|mirrored <name>
unitao_vds.sh <目录> remove shared|mirrored <name>
unitao_vds.sh <目录> recipient list <name>
unitao_vds.sh <目录> recipient add <name> <recipient> [--note <说明>]
unitao_vds.sh <目录> recipient remove <name> <recipient|subscriber id>
```

命令行没给的值（名字、通道、指纹）会询问；`-h` 列出所有选项。用 `--dir <path>` 代替 `--repo` 就是 `dir` 通道。它修改 `config/config.json` 的 `vds` 段，把每个 vDS 的文件放在 `config/vds/<kind>/<name>/`，Data Service 在运行时重启它（`--no-restart` 不重启；配置在启动时读取）：

| 类型 | 文件 | |
|---|---|---|
| shared | `signing.pem`、`fingerprint`、`recipients` | 签名私钥（不离开本实例）；它的指纹，每个 mirror 都需要；订阅方白名单（由 Data Service 创建，每行一个 recipient） |
| mirrored | `identity`、`recipient` | 解密私钥（不离开本实例）；它的公钥，交给发布方加进白名单 |

git 通道的分支默认是仓库的默认分支（Gitee 上常是 `master`），用 Data Service 的 ssh 密钥和凭据从远端读取；空仓库用 `main`。读不到远端时（比如 Data Service 的 ssh 公钥还没加到 git 服务器上），脚本打印原因和 `config/state.json` 里的公钥；交互时等你加好公钥后重新读取（也可以直接输入分支），非交互时用 `main`。分支总会写进配置；两边必须用同一个分支，用 `--branch` 指定。

以 Data Service 运行的用户（`UNITAO_UID`，在 docker 组里）运行脚本，不要用 sudo：`add` 拒绝以 root 运行，因为 root 建出的文件 Data Service 用不了。`remove` 可以用 sudo，比如清理这种文件。

git 通道克隆在 `data/vds/<kind>/<name>/channel`。`file:///opt/UniTAO/channel/<repo>.git` 的仓库不存在时，会在通道目录里建一个空的裸仓库。

本机上的通道（`file:///opt/UniTAO/channel/<repo>.git` 或 `--dir /opt/UniTAO/channel…`）放在挂到 `/opt/UniTAO/channel` 的宿主机目录里，默认是实例目录里的 `channel/`。加第一个这样的 vDS 时会询问这个目录（`--channel-dir <宿主机路径>`）；同一台主机上两个 site 用这种方式交换数据时，两边给同一个目录。它写进 `.env` 的 `UNITAO_CHANNEL_DIR`，并重建容器；一旦有 vDS 在用，就不能再改。用 git 服务器时这些都不需要。

`remove` 删除这一项、它的文件和克隆（其它 vDS 还在用的除外）；Data Service 再次启动时删除该 vDS 的表，并从 Inventory 注销。该 vDS 留在通道上的内容不动。

**两边之间的交接**由你来做：

 - 发布方的 `fingerprint` 交给每个 mirror：`add mirrored … --fingerprint "$(cat …/config/vds/shared/<name>/fingerprint)"`。它是密钥的指纹（`SHA256:` 加 64 位十六进制），不是 `signing.pem`。
 - 每个 mirror 的 `recipient`（它 `add` / `show` 时打印）加进发布方的白名单：在发布方运行 `recipient add <name> <recipient>`。在那之前 mirror 一直是 `untrusted`。

`recipient` 修改 shared vDS 的白名单 `config/vds/shared/<name>/recipients`，并重启 Data Service（白名单在启动时读取）。`add` 检查给的值是不是 recipient，已有的跳过；`--note` 在它上面加一行 `# note:`，`list` 把它显示在 subscriber id 旁边。`remove` 接受 recipient 或它的 subscriber id（`list` 和通道上 `subscribers/<id>.json` 里的 id），连同它的说明一起删。被删的 mirror 无法解密之后发布的 batch，它的进度文件在 `subscriberTimeoutHours` 后被删除。

### 示例：同一台主机上的两个 site

site-shared 发布 `shared/catalog`，site-mirror 镜像它。每个 site 是一个 UniTAO 目录，有自己的 Inventory、Data Service 和 Docker 网络（`site-shared_net`、`site-mirror_net`），所以彼此不能直接访问；它们之间唯一的通路是一个裸 git 仓库，放在两个 Data Service 都挂作通道目录的目录里。两边共用同一个 MongoDB（不同的数据库）；真实部署中每个 site 有自己的 MongoDB，仓库在 git 服务器上。

```
S=docker-compose/script
mkdir -p ~/demo
$S/mongo_deploy.sh ~/infra/mongodb                  # 如果还没运行

# site-shared：~/demo/site-shared/{inv,ds}，端口 8034 / 8031（回答时输入），同步间隔 30
$S/unitao_deploy.sh inventory   ~/demo/site-shared
$S/unitao_deploy.sh dataservice ~/demo/site-shared
# site-mirror：~/demo/site-mirror/{inv,ds}，端口 8044 / 8041
$S/unitao_deploy.sh inventory   ~/demo/site-mirror
$S/unitao_deploy.sh dataservice ~/demo/site-mirror

REPO=file:///opt/UniTAO/channel/exchange.git
# 两个 Data Service 都把 ~/demo/channel 挂到 /opt/UniTAO/channel，仓库就在那里
$S/unitao_vds.sh ~/demo/site-shared/ds add shared catalog --repo $REPO --channel-dir ~/demo/channel --folder infra/catalog --publish-interval 5
$S/unitao_vds.sh ~/demo/site-mirror/ds add mirrored catalog --repo $REPO --channel-dir ~/demo/channel --folder infra/catalog --pull-interval 10 \
    --fingerprint "$(cat ~/demo/site-shared/ds/config/vds/shared/catalog/fingerprint)"
$S/unitao_vds.sh ~/demo/site-shared/ds recipient add catalog "$(cat ~/demo/site-mirror/ds/config/vds/mirrored/catalog/recipient)" --note site-mirror
```

试一下：

```
# 写进 site-shared 的 shared vDS
curl -X POST localhost:8031/shared/catalog -d '{"__type":"schema","__id":"rack","__ver":"0.0.1","data":{"name":"rack","version":"0.0.1","description":"rack","properties":{"name":{"type":"string"}}}}'
curl -X POST localhost:8031/shared/catalog -d '{"__type":"rack","__id":"r1","__ver":"0.0.1","data":{"name":"rack one"}}'

# site-shared 推送了什么
git -C ~/demo/channel/exchange.git ls-tree -r --name-only main   # infra/catalog/manifest.json、manifest.sig、subscribers/<site-mirror id>.json

# 约 15 秒内（每 5 秒发布、每 10 秒拉取）出现在 site-mirror 上，只读
curl localhost:8041/mirrored/catalog/rack/r1
curl localhost:8041/mirrored/catalog/config      # 订阅状态（waiting | active）、cursor、同步状态
curl localhost:8031/shared/catalog/config        # 发布方视角：各订阅方的状态和 cursor
curl localhost:8044/rack                         # site-mirror 的 Inventory 里的 ["catalog/r1"]（在它下次同步之后）
```

通道上发生了什么：

 1. site-mirror 推送 `subscribers/<它的 id>.json`，`state: subscribe`（发布方不必已经存在）。
 2. site-shared 把写入发布为 `queue/…-….jsonl.gz.enc`，看到订阅请求后，把它的全部记录写成 `history/<id>/0001.jsonl.gz.aes`；manifest 把它授权给 site-mirror（内容密钥用 site-mirror 的公钥包装）。
 3. site-mirror 加载 history（替换它的表），从那里继续消费 queue，并汇报 `state: active` 和 cursor。
 4. site-shared 删除该 history 以及 site-mirror 已经处理过的所有 batch。

如果 site-mirror 离开超过 `subscriberTimeoutHours`（默认 24，`--subscriber-timeout`），site-shared 删掉它的进度文件，不再为它保留 batch；site-mirror 回来后会重新订阅并重新加载全部数据。

清理：在四个实例目录里运行 `docker compose down -v`，`rm -rf ~/demo`，再在 MongoDB 里删掉数据库 `site-shared-ds` 和 `site-mirror-ds`。

### git 通道如何工作

两边在分支 `main` 上使用同一个 folder（`infra/catalog`）；其它 vDS 可以在别的 folder 下共用同一个仓库和分支（另见主 README 的 "How changes travel"）：

```
infra/catalog/
  manifest.json, manifest.sig             # 发布方签名：lastSeq、queue batch 和 history 分片及其 sha256、history 授权
  queue/<from>-<to>.jsonl.gz.enc          # 增量 batch，对所有接收方加密
  history/<id>/<n>.jsonl.gz.aes           # 全量 history 的分片；内容密钥在 manifest 里为每个被授权方各包装一份
  subscribers/<id>.json                   # 每个 mirror 自己写：request、state、cursor
```

 - **每次写入（两边都一样）**：`fetch --depth 1`，把克隆 reset 到远端分支，只写自己的文件，commit，push。push 被拒（别人先推了）就从 fetch 重做，不 rebase。远端是空仓库也可以，第一次 push 会建分支。
 - **发布方**推送 batch 和 history，并清理所有 mirror 都处理过的内容；最多每 `compactIntervalHours` 把分支压成一个提交（`push --force-with-lease`，并发的 push 不会丢）并对克隆运行 `gc`。
 - **mirror** 只在进度变化时推送，所以空闲的通道不会有新提交。它需要仓库的写权限。
 - git 以非交互方式运行（`GIT_TERMINAL_PROMPT=0`）：凭据必须在 Data Service 启动前就准备好。

### 使用 git 服务器

两边都把服务器的 URL 作为 `--repo`。mirror 也需要 push 权限（它写 `subscribers/<id>.json`）；发布方还需要 force-push 权限来压缩 history。服务器禁止 force-push 时，压缩会失败并记日志，其余功能照常。

 - **SSH**（`ssh://git@git.internal/unitao/exchange.git` 或 `git@git.internal:unitao/exchange.git`）：Data Service 自己生成密钥。第一次启动时在命名卷 `ssh` 里生成 ed25519 密钥 `id_ed25519`（已有就沿用），把公钥写进 `config/state.json`（`ssh.publicKey`、`ssh.fingerprint`）、日志和 `GET /{vDS}/config`（`instance.ssh`）。把这个公钥加到 git 服务器上（有写权限的 deploy key，或某个 bot 用户的账号）；私钥不离开 Data Service。git 使用它时带 `StrictHostKeyChecking=accept-new`：第一次连接时把服务器的 host key 记进同一个卷里的 `known_hosts`，之后校验。密钥在 `docker compose down` 和重建容器后都还在，只有删卷（`docker compose down -v`）才会丢。容器环境里设置了 `GIT_SSH_COMMAND` 时以它为准，可用来使用你自己的密钥：
   ```yaml
   environment:
     GIT_SSH_COMMAND: "ssh -i /opt/UniTAO/gitkey/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=/opt/UniTAO/gitkey/known_hosts"
   volumes:
     - ./gitkey:/opt/UniTAO/gitkey:ro     # id_ed25519（权限 600，属于你的用户）+ known_hosts（ssh-keyscan git.internal）
   ```
 - **HTTPS**（`https://git.internal/unitao/exchange.git`）：在 `$HOME`（容器里是 `/tmp`）配置 credential helper：
   ```yaml
   volumes:
     - ./git/.gitconfig:/tmp/.gitconfig:ro              # [credential] helper = store
     - ./git/.git-credentials:/tmp/.git-credentials:ro  # https://unitao-bot:<token>@git.internal
   ```
   不要把 token 写进仓库 URL：它也会存进克隆的 `.git/config`。Data Service 会在 `GET /{vDS}/config` 和日志里的 git 错误中隐去 URL 里的凭据，但配置文件本身仍然保存着它。
