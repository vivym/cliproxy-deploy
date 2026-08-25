# Lark Controller Compose 灰度运行手册

## 状态和边界

本手册只定义根目录唯一 `docker-compose.yml` 的 Lark 灰度顺序。2026-08-25 的 GHCR
收据对应 New API `4e451088` 和 deployment `54955ec`，早于周额度实现。当前周额度候选为
New API `a624396db4ef01db607cd100c24ecc6f26e77430` 和 deployment
`9a6d825db04ef28aa81bea14671c0ddb778eac39`；四个 image 已在本地完成双架构 OCI 和
`linux/amd64` 入口验证，但尚未发布到 GHCR。因此旧 registry digest 不得用于当前周额度
候选，镜像发布门禁已重新打开。真实 Lark tenant、生产恢复/reconciliation 演练和服务器
验收也仍未完成，当前不能据此宣称生产可上线或修改生产 `.env`。

### 2026-08-25 历史 registry 收据（周额度实现之前）

| Image | Immutable reference | Visibility | Anonymous manifest |
| --- | --- | --- | --- |
| New API fork | `ghcr.io/vivym/new-api-lark-fork:4e451088@sha256:1adfe184f18357dd13a1872d6d2318ebc14c48b9d4968e42292ff3d080187f46` | `public` | `200`, digest matched |
| Controller | `ghcr.io/vivym/lark-quota-controller:54955ec@sha256:4b7ddaac83f75b79b7e025a978bf143fab286cc2603cdc60f2a4315713ed7341` | `public` | `200`, digest matched |
| correction CLI | `ghcr.io/vivym/lark-correction:54955ec@sha256:4cc3330ae17c1744ef47e1234fa21f9fe137cb1a0774dbf38c3d2f9ec76825bb` | `public` | `200`, digest matched |
| config CLI | `ghcr.io/vivym/lark-config:54955ec@sha256:56f9857f775f282dcc1db0f4bb9396b13705f6e991b00d4d81de2169f463de52` | `public` | `200`, digest matched |

Registry index descriptors：

| Image | `linux/amd64` | `linux/arm64` | amd64 attestation | arm64 attestation |
| --- | --- | --- | --- | --- |
| New API fork | `sha256:53b0c9123387916a2c5e48ea250b7cc4d844eca29de09a33b0f56a6de040baa5` | `sha256:572fef92184d972c76e390053323aa15f556687e0d1ff18edcff329eb9c55e5a` | `sha256:58bab464ad98a33c9ef7f511a90bd9f60cd9e4e634dec9b02f9ccd015efdabd0` | `sha256:24b6967aec23590c446252976ffb1d78a774a2dbeda2003c323c9a3d2aeef41c` |
| Controller | `sha256:71e1eafcca673acc221c60667c8d37b8a812e0f160782fd8eb48f52304b49702` | `sha256:09a1f4a5915d269a03a88f72181d6391a8693d26df89df0a2a46088c4a77a953` | `sha256:a0cb3013e8cb3bdaf4f1226c378a104ee7b7836d5dbde9fe50ca5fe919b30d8d` | `sha256:a75d1632f33b9a46b7c7c2a76ad13ec45c647d9b0d0b7a812256df33a41d192a` |
| correction CLI | `sha256:c7ffcf7c46247f1679aef52d69a3933f281617962d3e023e8245d3b9132d0078` | `sha256:2c4a5f97908abf8dd9d95b2e070be2a40f1fbdb3bd9ec57618c2d68c532187f2` | `sha256:f7853aee176c473a914923b7d1c8df966ed4d4d1b0b1087ab1c1b5ba517cd791` | `sha256:1df2471d5074d20d1c2d0e9c569a43a2b2e7e082b6ceb06568475d43709035f6` |
| config CLI | `sha256:359027622a88fb84d8b93e4804611f3b8311e1935662bab8b17760f3b6de516b` | `sha256:e4a8b1ce62bea6fbc28acdc866d98a7405e53080475c4f30fb68b5307fbb5f0d` | `sha256:f9b7ddb9dae74c0f22bbf6e0659f053cb378d8097d617f10636fd7618e1558c0` | `sha256:81f80a77ceb6f9124789e008052f952cfd03aa22f886fe8e4fc20184f6288c55` |

四个 index 都由 Buildx 以 `--platform linux/amd64,linux/arm64`、
`--provenance=mode=max` 和 `--sbom=true` 发布；逐平台查询均能读取 provenance 和 SBOM。
两架构 config 都带精确 source/revision label。Controller、correction 和 config 的 runtime
user 为 `10001:10001`，entrypoint 分别为 `/usr/local/bin/lark-controller`、
`/usr/local/bin/lark-correction` 和 `/usr/local/bin/lark-config`；Controller image healthcheck
也已从 Docker runtime config 复核。

在 arm64 Docker host 上强制 `--platform linux/amd64` 运行上述 immutable reference：New API、
correction 和 config 的 `--help` 均返回 `0`，`lark-cli --version` 返回 `1.0.80`；Controller
成功执行并按预期因缺少 callback 配置 fail closed。所有路径均未出现 `exec format error`。
这只验证镜像架构和入口，不替代真实 tenant、Compose 或服务器验收。

`lark-config` 已在 GitHub `Package settings -> Change visibility -> Public` 完成不可逆
切换；随后从无凭证环境取得 `200` 和相同 `Docker-Content-Digest`。GitHub 的个人 package
REST/GraphQL API 没有 visibility mutation，因此本收据保留了切换后的独立匿名请求结果，
没有把 authenticated registry inspect 误记为匿名验证。

### 当前周额度候选的本地 multi-arch 收据

2026-08-25 从 New API `a624396db4ef01db607cd100c24ecc6f26e77430` 和 deployment
`9a6d825db04ef28aa81bea14671c0ddb778eac39` 使用 Buildx 构建，参数固定为
`--platform linux/amd64,linux/arm64 --provenance=false --sbom=false --output type=oci`。
该收据只证明本地 artifact，不是 registry、attestation 或匿名拉取收据：

| Image | Local OCI index | `linux/amd64` manifest | `linux/arm64` manifest |
| --- | --- | --- | --- |
| New API fork | `sha256:5126190d9169cf39e2dfc549252b95651d7a3ebf0defafe139fe620a0c63bf3e` | `sha256:2283daf219c8863a4eaff903f585a8908b98d2264e8e6500b2a5c40e76cf855d` | `sha256:6d29e22221928149a1c12a1fc784a2323a12edd3e423888385efcae5b0407fa8` |
| Controller | `sha256:08669d6698cbb9b8e0f3eebb20199b557d5bc28829153031c376fca6d56b2ea7` | `sha256:7fa7eae91f7f9d8835e9cbe6112dcb6ca3266d549b917dba90a0bb831e6bc838` | `sha256:f3168173448049296f0c342a648ac235c91771e1179990a06c1b9f9405a42ac4` |
| correction CLI | `sha256:48a18eb0a6f25e570afe526f978307493de6111f02730b60004a6ba37c37c25d` | `sha256:b57ffb598adcacaca423f777c1b506d3d78c878175b856bbbb1769b56fe03ea1` | `sha256:85629d8d818c066dda1f055e8a4e8340e109861f1079eb19f68b13206d112280` |
| config CLI | `sha256:26d8c998ed677e35e58584935f2c7f5f84d592939037101268e27d90baa6e52b` | `sha256:91b6951a09d7b73d0b894a4fdc3ea0fca74ba3fae22bee02f1f83308141a6d35` | `sha256:12e400895687422188b08e3690d29bf09196ff6079fe67db75bb9c8a566565c2` |

OCI index、platform manifest、image config 和所有 layer blob 的 digest/size 均已独立重算，
每个 index 只包含 `linux/amd64` 和 `linux/arm64`。两架构 source/revision label、entrypoint、
runtime user 与 Controller healthcheck 均匹配源码合同。在 arm64 host 强制运行本地
`linux/amd64` image 时，New API、correction 和 config 的 `--help` 返回 `0`，`lark-cli
--version` 返回 `1.0.80`；Controller 可执行，并因缺少
`NEW_API_OAUTH_CALLBACK_ALLOWLIST` 按预期 fail closed，所有路径均无 `exec format error`。
验证后的临时 tag 和 OCI archive 已精确删除，BuildKit cache 未清理。

### 2026-08-24 historical registry receipt

| Image | Immutable reference | Visibility |
| --- | --- | --- |
| New API fork | `ghcr.io/vivym/new-api-lark-fork:dbfcf0c7@sha256:47c8bce2491f10d27f8f1a75b68aecffa3e543ff4f3af47f94850d59a1f1edf4` | `public` |
| Controller | `ghcr.io/vivym/lark-quota-controller:f55103e@sha256:fd111b27e4f4668c76f6006360c246b073dbc71a62e72f9209f6e5b95d62c225` | `public` |
| correction CLI | `ghcr.io/vivym/lark-correction:db63869@sha256:f463c8c715ea0f355df8a192540330569bcd05e16ff0c24a1bbfadd72c80d7c1` | `public` |

每个 registry index 均包含 `linux/amd64`、`linux/arm64` 以及每个平台对应的 provenance/SBOM
attestation。本次切换为 `public` 后，已在无凭证环境重新验证这三个 immutable reference 的匿名拉取。

### 历史本地部署候选收据

2026-08-24 的本地部署候选固定为上表三个 immutable reference。不要把 tag 单独写入
生产 `.env`；repository 与 `tag@sha256:digest` 必须成对使用。

本地候选验证结果：

- 在 arm64 Docker host 上强制 `--platform linux/amd64` 拉取并运行三个 image。New API 和
  correction CLI 的 `--help` 均返回 `0`；Controller amd64 binary 成功执行，并按预期因缺少
  真实 callback 配置 fail closed。三个路径均未出现 `exec format error`。
- `feature/lark-controller-shadow` 的 `db63869` 通过 `go test ./...`、
  `go test -race ./...` 和 `go vet ./...`。Controller runtime image 仍固定为未受本次
  correction-only 修改影响的 `f55103e` digest。
- 使用三个 immutable reference 同时渲染 `lark` 和 `lark-ops` profile，
  `docker compose config --quiet` 通过。JSON 结构化检查确认 New API、临时 correction
  endpoint、Controller、correction 和 readonly service 均使用预期 digest；Controller 保持
  `shadow`、OAuth 保持关闭，相关服务没有 host port，Controller 不加入 `new-api-data`，
  readonly service 保持 `network_mode: none`。
- 本次验证没有生成 `.env`、没有写入真实 secret 或 policy、没有启动项目 Compose 服务、
  没有访问服务器。真实 Lark tenant、恢复演练和生产网络探测仍是独立门禁。

### `lark-config` 发布前本地 multi-arch 收据

2026-08-25 从当前候选源码以 `--platform linux/amd64,linux/arm64 --target config
--provenance=false --sbom=false --output type=oci` 构建并独立解析 OCI。该收据只证明本地
build artifact，不是 registry 或匿名拉取收据：

| Item | Digest / result |
| --- | --- |
| OCI index | `sha256:05ae38e6a16ffb65a764384930ef235a62e18b5e4fb7e8518d28750ca004a21c` |
| `linux/amd64` manifest | `sha256:85db50b40ef1ba1e3bf52b4df2996fa37c25ee0d3f54937563c735402bdf5c46` |
| `linux/arm64` manifest | `sha256:ed12be4fafe5575237bc2c828dd1ff199856ae52e6d2f2b1b7abda0d1d49972c` |
| Runtime contract | both platforms: `User=10001:10001`, `Entrypoint=/usr/local/bin/lark-config` |
| amd64 execution | `lark-config --help` exit `0`; `lark-cli --version` = `1.0.80` |

Index、platform manifest 和 image config blob 都按声明 digest 重新计算一致。后续已从
提交 `54955ec6dc585d89474591a0e7e03b2d51bbfb5f` 重建并取得本节前部的 registry digest；
attestation 和 exporter 改变了 index，因此没有复用上述 local digest。

基础 New API 迁移和 Lark rollout 是两个独立变更。先按 `migrate-to-new-api-deploy.md` 完成并验收基础迁移；不要在同一个维护窗口首次启用 Lark profile。

未经服务器操作授权，不得在远端执行本手册命令。

## 启动门禁

启用前必须同时满足：

1. 从当前已提交 revisions 重新构建并发布 New API fork、Controller、correction CLI 和 `lark-config` 四个 multi-arch image；四个公开 GHCR package 均有新的 registry index/attestation 收据和匿名拉取验证，`.env` 使用 reviewed `tag@sha256:digest`。当前周额度 revisions 只有本地 OCI 收据，尚未发布，因此该镜像发布门禁未通过。
2. `EDGE_SUBNET`、`NEW_API_DATA_SUBNET`、`SUB2API_DATA_SUBNET` 和 `LARK_INTEGRATION_SUBNET` 与主机现有 Docker networks 不重叠。
3. 已按 `lark-tenant-configuration.md` 生成并审查配置收据；`lark-runtime/policies/` 包含完整历史 `*.policy.json` 和 `approval-bindings.json`，`lark-runtime/runtime/controller.env` 中的 active version 与 catalog 一致。
4. `lark-runtime/secrets/{shared,controller,new-api}/` 三个 consumer 子目录及其中所有 secret 的 owner 固定为 Controller runtime UID/GID `10001:10001`，子目录为 `0700`、文件为 `0600`；`scripts/verify-lark-secret-permissions.sh` 已通过，且没有 secret 进入 `.env`、Git、镜像或日志。顶层 `secrets/` 只负责组织这些 bind source，不作为容器内权限边界。
5. New API Custom OAuth provider、Lark app 可用范围、审批定义和事件订阅已有独立变更记录，但 shadow 第一步尚不开放 OAuth。
6. Controller SQLite 的受控备份/恢复方案、恢复后 reconciliation 清单和回滚负责人已确认；至少一份生产形态测试包已在隔离环境完成全量恢复演练。

`backup-deployment.sh`、两个 restore runner、correction runner 和 `lark-config apply` 现在共用 host-only `maintenance.session` 互斥目录与带 `backup/restore/correction/readonly/config` mode 的锁。配置 CLI 在每次操作前复核 `mode=config`，New API 配置端点在每次 mutation 前独立复核同一 mode；backup 还会停止原本运行的配置端点，并拒绝会生成不可恢复归档的 runtime symlink/hardlink。备份以停写方式把 Controller volume、两个 Postgres、两个 Redis、reviewed config source、compiled runtime/policy 和 receipts 绑定到 v2 manifest；历史 maintenance owner 不进入归档。enabled 配置和 Controller volume 必须同时存在；disabled 包带显式 absent marker。该实现已本地测试但尚未完成生产恢复演练，因此启动门禁第 6 项仍未通过。

两个 callback 由同一个 reviewed `public_origin` 编译：New API callback 为 `<public_origin>/oauth/lark`，Controller callback 为 `<public_origin>/integrations/lark/oauth/callback`。Controller 启动时会拒绝两者 origin 不一致；更换域名必须重新生成、plan、apply 并复验，不能只改 Compose 或 `.env`。

## Runtime 文件

`lark-runtime/secrets/` 按 consumer 分目录，不能恢复成整目录共享挂载：

```text
shared/
  lark_integration_secret
  lark_integration_secret_next  # only present during a rotation window
controller/
  lark_app_secret
  lark_verification_token
  lark_encrypt_key
  lark_grant_payload_keyring
  new_api_bridge_client_secret
new-api/
  lark_correction_secret  # only present during an approved correction window
```

`lark_grant_payload_keyring` 每行是 64 字符小写 hex，第一行是 primary key。integration current、可选 next 和独立的 `lark_correction_secret` 都是至少 32 字节的 printable ASCII token，三者不得相同。`new_api_bridge_client_secret` 是 32 至 4096 字节的 printable ASCII token。各文件只能有一个可选的末尾 LF/CRLF。

常驻 Controller 固定以 UID/GID `10001:10001` 运行。host bind mount 保留数值 owner，因此不能只由当前 UID 生成 `0600` 文件。先用受控 privilege 设置 owner，再执行实际检查：

```bash
sudo install -d -o 10001 -g 10001 -m 0700 \
  lark-runtime/secrets/shared \
  lark-runtime/secrets/controller \
  lark-runtime/secrets/new-api

umask 077
grant_keyring_tmp="$(mktemp)"
integration_tmp="$(mktemp)"
bridge_tmp="$(mktemp)"
trap 'rm -f "$grant_keyring_tmp" "$integration_tmp" "$bridge_tmp"' EXIT
openssl rand -hex 32 > "$grant_keyring_tmp"
openssl rand -base64 48 | tr -d '\n' > "$integration_tmp"
openssl rand -base64 48 | tr -d '\n' > "$bridge_tmp"
sudo install -o 10001 -g 10001 -m 0600 "$grant_keyring_tmp" lark-runtime/secrets/controller/lark_grant_payload_keyring
sudo install -o 10001 -g 10001 -m 0600 "$integration_tmp" lark-runtime/secrets/shared/lark_integration_secret
sudo install -o 10001 -g 10001 -m 0600 "$bridge_tmp" lark-runtime/secrets/controller/new_api_bridge_client_secret
sudo scripts/verify-lark-secret-permissions.sh
```

Lark 提供的 app secret、verification token 和 encrypt key 先保存到权限受控的 staging file，再用 `sudo install -o 10001 -g 10001 -m 0600` 写入 `controller/` 对应文件；不要用上述随机值代替。每次轮换后都重新执行权限检查。`shared/` 只挂到 New API 与 Controller；`controller/` 只挂到常驻 Controller。`new-api/lark_correction_secret` 按 correction runbook 在变更窗口临时生成，只挂到 `lark-ops` endpoint 和 one-shot CLI，绝不挂到常驻 Controller 或基础 New API。

## Integration credential 轮换

平时保持：

```text
NEW_API_LARK_INTEGRATION_SECRET_NEXT_FILE=
production.binding.json secret_refs.integration_secret=lark_integration_secret
```

轮换是独立审批变更，按以下顺序执行，不能直接覆盖 current 后同时重启两端：

1. 生成新的 printable token，用 `sudo install -o 10001 -g 10001 -m 0600` 写入 `shared/lark_integration_secret_next`，执行 `sudo scripts/verify-lark-secret-permissions.sh --include-next`。该检查按 runtime 的 LF/CRLF 规则比较 effective token，并拒绝与 current 相同的 next。
2. 设置 `NEW_API_LARK_INTEGRATION_SECRET_NEXT_FILE=/run/secrets/lark-controller/shared/lark_integration_secret_next`，只重建 New API。此时 New API 同时接受 current/next，Controller 仍发送 current；执行 deployment verify。
3. 把当前受控 binding 中 `secret_refs.integration_secret` 改为 `lark_integration_secret_next`，重新执行 `lark-config check` 和完整远端 preflight，复核 plan digest。紧邻 apply 前停止 Controller，apply 生成新的 `runtime/controller.env` 后只启动 Controller，并通过 startup gate；确认使用 next 的写入/reconciliation shadow receipt 成功。不要通过额外环境变量覆盖 compiled runtime。
4. 用 `sudo install -o 10001 -g 10001 -m 0600 lark-runtime/secrets/shared/lark_integration_secret_next lark-runtime/secrets/shared/lark_integration_secret` 原子安装新 current，清空 `NEW_API_LARK_INTEGRATION_SECRET_NEXT_FILE`，只重建 New API。运行中的旧 New API 在重建前仍持有内存中的 old current/new next，不依赖被替换的文件内容。
5. 把 binding 的 `secret_refs.integration_secret` 恢复为 `lark_integration_secret`，再次 check、preflight、复核 plan digest；停止 Controller 后 apply，重新启动并通过 startup gate。确认 verify 和 shadow receipts 后删除 next file，再执行默认 permission check。任何一步失败都停在仍有一个双方共同接受的 token 的状态，不得同时删除 old current 和 next。

correction 窗口若遇到尚未清理的 next file，`--include-correction` 会同时验证 correction 与 current、next 均不同。

## 本地预检

保持以下初始配置：

```text
LARK_CONTROLLER_MODE=shadow
LARK_OAUTH_PUBLIC_ENABLED=false
NEW_API_INTEGRATION_LISTEN_ADDR=0.0.0.0:3001
```

然后只做渲染和构建验证：

```bash
docker compose --profile lark config --quiet
docker compose --profile lark config --services
docker compose --profile lark build lark-quota-controller
docker compose --profile lark-ops build lark-correction
sudo scripts/verify-lark-secret-permissions.sh
```

渲染结果必须满足：Controller 没有 `ports`，基础 New API `3001` 没有 host publish 或 Traefik service，Controller 不在 `new-api-data`，且 `LARK_OAUTH_PUBLIC_ENABLED=false`。`new-api-correction-endpoint`、`lark-correction` 和无网络/无 secret/SQLite 只读的 `lark-correction-readonly` 只出现在 `lark-ops` profile；写路径两服务均无 Traefik label/host port，常驻 Controller 镜像 target 不包含 correction CLI。基础 New API 和 Controller 在 `lark-runtime/ops/maintenance.lock` 任意 mode 存在时拒绝启动，correction endpoint/CLI 只接受 `mode=correction`，配置 endpoint mutation 和 CLI 只接受 `mode=config`；只读 pending runner 使用 `mode=readonly` 和固定名 `new-api-lark-correction-readonly-ops`，清理并精确验空后才解锁。`maintenance.session` 只在 host 与 one-shot config CLI 中仲裁 writer，不挂到常驻 Controller。临时 container 清理无法确认时必须同时保留 lock/session。

## Webhook-only shadow

生产执行前需要单独授权。顺序是：

```bash
docker compose up -d new-api
docker compose --profile lark up -d lark-quota-controller
scripts/verify-deployment.sh
```

此阶段只有 `/integrations/lark/events` 路由到 Controller。OAuth authorize/callback 应从公网得到 `404`。Controller 只记录和解析事件，不 release entitlement job，也不向 New API 写权益。

检查 Controller 日志、`/metrics`、approval reconciliation cursor、dead-letter 和 queue age。任何 policy hash、tenant、approval schema 或 Lark permission 不一致都停止 rollout，不得临时放宽校验。

## OAuth pilot

shadow 稳定后，为少量测试员工配置 New API Custom OAuth provider，再把：

```text
LARK_OAUTH_PUBLIC_ENABLED=true
```

作为独立变更写入 `.env`，重建 Controller labels 并复验：

```bash
docker compose --profile lark up -d lark-quota-controller
scripts/verify-deployment.sh
```

空参数访问 authorize 应为 `400`，证明请求已命中 Controller；New API `:3001` 无凭证仍必须为 `401`。保留密码和管理员 break-glass 登录。

## Active mode

只有 OAuth、基础订阅、wallet grant、subscription level、reversal 和离职流程的 shadow 收据全部通过后，才设置：

```text
LARK_CONTROLLER_MODE=active
LARK_RECONCILIATION_HEALTH_OPEN_ID=ou_known_active_employee
```

active 切换是另一项审批变更。启动后先限制到测试员工和最小 wallet package，逐项按架构文档 WP5 放量。

## 联合备份与恢复

这是停机备份，不是在线快照。另行取得生产操作授权和维护窗口后，才在部署根目录执行：

```bash
BACKUP_DIR=/var/backups/new-api scripts/backup-deployment.sh
```

runner 会先取得 `lark-runtime/ops/maintenance.session`，再取得 `maintenance.lock/mode=backup`，停止原本正在运行的 Traefik、Controller、New API、New API config endpoint、Sub2API 和两个 Redis，再生成 v2 同包归档。打包前会拒绝 runtime tree 中任何 symlink 或 hard-linked regular file，保证生成物符合 restore entry contract。成功后只重启原本运行的服务，最后释放 session。检查输出包权限为 `0600`，在隔离位置解包并确认存在：

```text
backup-manifest.json
SHA256SUMS
deployment-runtime.tgz
sub2api-postgres.dump
new-api-postgres.dump
sub2api-redis-data/
new-api-redis-data/
lark-controller-data.tgz       # enabled package
lark-controller-data.absent    # disabled package, exactly one of the two
```

enabled 包的 runtime 还包含 `lark-runtime/policies/`，但不包含 `lark-runtime/secrets/`。长期 Lark secret 必须由独立 secret backup/rotation 流程负责，不能为了方便塞进普通部署包。backup 失败时，如果命名 snapshot container 的清理无法确认，runner 会保留 maintenance lock 和 session 且不重启 writer；先确认 `new-api-lark-backup-snapshot` 不存在，再按错误信息删除 `mode`、空 lock 目录和空 session 目录并恢复原服务。

全量恢复会替换两个 Postgres、两个 Redis、Sub2API runtime，并恢复或删除 Controller volume，必须使用隔离环境演练过的完整包：

```bash
scripts/restore-deployment.sh /path/to/backup-package.tgz
```

archive/checksum/manifest/listener/policy/Compose 校验，以及 Controller archive 根目录中唯一 regular `controller.sqlite` 的校验，都在部署变更前完成。进入 `compose down` 后任何失败都会保留 `maintenance.session` 和 `mode=restore`，禁止 writer 自动启动；Compose readiness 窗口先释放容器门禁但始终持有 host session，启动失败会重建 restore lock、再次 `compose down` 并核实全部服务停止。优先修复原因，不要混入其他包的 dump 或手工删除 lock/session。enabled 恢复会保留现有 host secret 文件，恢复同包 policy/Controller state，并把 `.env` 强制改为：

```text
LARK_CONTROLLER_MODE=shadow
LARK_OAUTH_PUBLIC_ENABLED=false
```

restore 只在 Compose `--wait` 确认服务 readiness 后报告成功；keyring/secret 与恢复状态不兼容会使命令失败而不是产生假成功。absent/legacy-absent 恢复会删除旧 Controller volume 和旧 policy，但保留 `lark-runtime/secrets/` 与 `ops/`。恢复成功后先保持这两个值，完成架构文档“恢复后必须执行 reconciliation”清单：核对 barrier/package ID、policy manifest、`processing`/retry/reversal 队列、applied grant replay、New API outbox/deny fence、managed subscription projection 和 disabled principal。再把 OAuth 或 active mode 作为两个独立审批变更打开。`restore-new-api.sh` 只允许源包和目标部署均无 Lark state：缺 manifest 却含 Lark marker/archive、enabled manifest、目标 listener、运行中的 Controller/correction 或现存 Controller volume都会要求 full restore；其破坏性失败同样保留 session/restore lock。

## 网络验收

`scripts/verify-deployment.sh` 当前检查公网 integration path、Controller readiness，以及 New API 容器内访问 `127.0.0.1:3001` 的无凭证 `401`。生产验收还必须使用独立临时 probe 分别加入 `new-api-edge`、`new-api-data` 和 `new-api-lark-integration`，确认访问 `http://new-api:3001/api/integrations/v1/principals` 都返回 `401`；同时从公网确认该 path 为 `404`。probe 镜像必须固定 digest，测试后删除。

## 回滚

1. 先把 `LARK_OAUTH_PUBLIC_ENABLED=false` 写入 `.env` 并重建 Controller，使 authorize/callback 回到 `404`。
2. 在 New API 管理端禁用 Lark Custom OAuth provider，保留 break-glass 登录。
3. 停止 Controller，但保留 SQLite volume、policy 和 secret rotation history：

```bash
docker compose stop lark-quota-controller
```

4. 如需关闭 New API integration listener，清空 `NEW_API_INTEGRATION_LISTEN_ADDR` 后只重建 New API。
5. 不执行 `docker compose down --volumes`，不删除 `new-api-lark-controller-data`，不混用不同 backup package 的 SQLite/Postgres。

回滚不等于撤销已经生效的 wallet 或订阅变更。已有权益按 correction/reversal runbook 逐笔处理并保留审计收据。
