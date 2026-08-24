# Lark Controller Compose 灰度运行手册

## 状态和边界

本手册只定义根目录唯一 `docker-compose.yml` 的 Lark 灰度顺序。New API fork、Controller 与 correction CLI 已推送到 GHCR，并记录了带 provenance/SBOM 的 multi-arch registry index digest；三个个人包已切换为 `public`，并已按实际标签完成匿名 manifest 拉取和 digest 校验。真实 Lark tenant、生产恢复/reconciliation 演练和服务器验收也尚未完成，因此当前不能据此宣称生产可上线。

2026-08-24 registry receipt：

| Image | Immutable reference | Visibility |
| --- | --- | --- |
| New API fork | `ghcr.io/vivym/new-api-lark-fork:dbfcf0c7@sha256:47c8bce2491f10d27f8f1a75b68aecffa3e543ff4f3af47f94850d59a1f1edf4` | `public` |
| Controller | `ghcr.io/vivym/lark-quota-controller:f55103e@sha256:fd111b27e4f4668c76f6006360c246b073dbc71a62e72f9209f6e5b95d62c225` | `public` |
| correction CLI | `ghcr.io/vivym/lark-correction:f55103e@sha256:43bc7bb62d8d422e02642144a6fc0828b418250d16c355de9fd2069be0273ff5` | `public` |

每个 registry index 均包含 `linux/amd64`、`linux/arm64` 以及每个平台对应的 provenance/SBOM
attestation。本次切换为 `public` 后，已在无凭证环境重新验证这三个 immutable reference 的匿名拉取。

基础 New API 迁移和 Lark rollout 是两个独立变更。先按 `migrate-to-new-api-deploy.md` 完成并验收基础迁移；不要在同一个维护窗口首次启用 Lark profile。

未经服务器操作授权，不得在远端执行本手册命令。

## 启动门禁

启用前必须同时满足：

1. New API fork、Controller 与 correction CLI 镜像已切换为公开 GHCR package，并已通过匿名拉取验证；`.env` 使用 reviewed `tag@sha256:digest`。
2. `EDGE_SUBNET`、`NEW_API_DATA_SUBNET`、`SUB2API_DATA_SUBNET` 和 `LARK_INTEGRATION_SUBNET` 与主机现有 Docker networks 不重叠。
3. `lark-runtime/policies/` 包含完整历史 `*.policy.json` 和 `approval-bindings.json`，且 active version 与 `LARK_ACTIVE_POLICY_VERSION` 一致。
4. `lark-runtime/secrets/{shared,controller,new-api}/` 三个 consumer 子目录及其中所有 secret 的 owner 固定为 Controller runtime UID/GID `10001:10001`，子目录为 `0700`、文件为 `0600`；`scripts/verify-lark-secret-permissions.sh` 已通过，且没有 secret 进入 `.env`、Git、镜像或日志。顶层 `secrets/` 只负责组织这些 bind source，不作为容器内权限边界。
5. New API Custom OAuth provider、Lark app 可用范围、审批定义和事件订阅已有独立变更记录，但 shadow 第一步尚不开放 OAuth。
6. Controller SQLite 的受控备份/恢复方案、恢复后 reconciliation 清单和回滚负责人已确认；至少一份生产形态测试包已在隔离环境完成全量恢复演练。

`backup-deployment.sh`、两个 restore runner 和 correction runner 现在共用 host-only `maintenance.session` 互斥目录与带 `backup/restore/correction/readonly` mode 的容器启动锁，并以停写方式把 Controller volume、两个 Postgres、两个 Redis、runtime 和 policy 绑定到 v2 manifest。enabled 配置和 Controller volume 必须同时存在；disabled 包带显式 absent marker。该实现已本地测试但尚未完成生产恢复演练，因此启动门禁第 6 项仍未通过。

初始 callback 合约固定为 `https://ai.x2r.store/oauth/lark`，Controller callback 固定为 `https://ai.x2r.store/integrations/lark/oauth/callback`。启用前 `NEW_API_HOST` 必须为 `ai.x2r.store`；更换域名需要先修改并重新验证两个代码库的 callback contract，不能只改 Compose。

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
LARK_CONTROLLER_INTEGRATION_SECRET_FILE=/run/secrets/lark-controller/shared/lark_integration_secret
```

轮换是独立审批变更，按以下顺序执行，不能直接覆盖 current 后同时重启两端：

1. 生成新的 printable token，用 `sudo install -o 10001 -g 10001 -m 0600` 写入 `shared/lark_integration_secret_next`，执行 `sudo scripts/verify-lark-secret-permissions.sh --include-next`。该检查按 runtime 的 LF/CRLF 规则比较 effective token，并拒绝与 current 相同的 next。
2. 设置 `NEW_API_LARK_INTEGRATION_SECRET_NEXT_FILE=/run/secrets/lark-controller/shared/lark_integration_secret_next`，只重建 New API。此时 New API 同时接受 current/next，Controller 仍发送 current；执行 deployment verify。
3. 设置 `LARK_CONTROLLER_INTEGRATION_SECRET_FILE=/run/secrets/lark-controller/shared/lark_integration_secret_next`，只重建 Controller；确认 Controller 使用 next 的写入/reconciliation shadow receipt 成功。
4. 用 `sudo install -o 10001 -g 10001 -m 0600 lark-runtime/secrets/shared/lark_integration_secret_next lark-runtime/secrets/shared/lark_integration_secret` 原子安装新 current，清空 `NEW_API_LARK_INTEGRATION_SECRET_NEXT_FILE`，只重建 New API。运行中的旧 New API 在重建前仍持有内存中的 old current/new next，不依赖被替换的文件内容。
5. 把 `LARK_CONTROLLER_INTEGRATION_SECRET_FILE` 恢复为 current path，只重建 Controller；确认 verify 和 shadow receipts 后删除 next file，再执行默认 permission check。任何一步失败都停在仍有一个双方共同接受的 token 的状态，不得同时删除 old current 和 next。

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

渲染结果必须满足：Controller 没有 `ports`，基础 New API `3001` 没有 host publish 或 Traefik service，Controller 不在 `new-api-data`，且 `LARK_OAUTH_PUBLIC_ENABLED=false`。`new-api-correction-endpoint`、`lark-correction` 和无网络/无 secret/SQLite 只读的 `lark-correction-readonly` 只出现在 `lark-ops` profile；写路径两服务均无 Traefik label/host port，常驻 Controller 镜像 target 不包含 correction CLI。基础 New API 和 Controller 在 `lark-runtime/ops/maintenance.lock` 任意 mode 存在时拒绝启动，临时 write endpoint/CLI 只接受 `mode=correction`；只读 pending runner 使用 `mode=readonly` 和固定名 `new-api-lark-correction-readonly-ops`，清理并精确验空后才解锁。`maintenance.session` 只在 host 上仲裁 runner，不挂入容器。两者只能由 backup/restore/correction host runner 管理，临时 container 清理无法确认时必须同时保留。

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

runner 会先取得 `lark-runtime/ops/maintenance.session`，再取得 `maintenance.lock/mode=backup`，停止原本正在运行的 Traefik、Controller、New API、Sub2API 和两个 Redis，再生成 v2 同包归档。成功后只重启原本运行的服务，最后释放 session。检查输出包权限为 `0600`，在隔离位置解包并确认存在：

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
