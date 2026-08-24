# New API + Lark 员工额度集成设计

## 文档状态

- 状态：设计已确定；WP2 本地 fork 已实现，WP3 已实现 shadow/active grant runtime、实时 `contact.user.deleted_v3` 停用、opaque OAuth credential store、Lark token/userinfo adapter、公开 authorize/callback、内部 token/userinfo handlers、幂等基础订阅投递、active principal 每日在职对账、运行期 stale claim 恢复、事件驱动 approval reversal 栅栏、周期性 approval reconciliation 和管理员一次性纠正流程；WP4 已完成本地 Compose/profile/network/volume/least-privilege secret/verify 基础接入、correction credential 隔离、offline quiesce backup/restore 同包合同和 GHCR multi-arch 发布收据，但三个个人 GHCR package 当前仍为 `private`，匿名拉取、全相邻网络探测、真实租户配置和生产恢复演练仍未完成；WP5 未实施，未部署或端到端验收
- 日期：2026-08-19
- 部署入口：`https://ai.x2r.store`
- New API 上游基线：`v0.13.2`（peeled commit `bee339d279ccecbf8c8a89e14ddbbd902f78bd5d`）
- 当前额度换算：`$1 = 500,000 quota`
- 实施形态：独立 `lark-quota-controller` + 最小化 New API fork

本文定义 Lark 登录、员工基础订阅、一次性钱包加额、永久订阅等级升级、审批撤销和员工离职的完整语义与实施契约。

本文中的 `quota` 都指 New API 内部额度单位。美元只用于管理员理解政策，不作为 Controller 内部计算的权威输入。

## 目标

系统需要实现以下能力：

1. 员工可在 New API 登录页使用 Lark 登录。
2. 新员工首次完成 Lark 登录后，自动获得基础订阅等级。
3. 员工额度不够时，可通过 Lark 审批申请一次性钱包额度。
4. 员工可通过另一条 Lark 审批申请永久提高订阅等级。
5. 审批重复推送、网络重试和进程重启不能造成重复加额或重复订阅。
6. 员工离职后，系统自动停用其 New API 账户和托管订阅。
7. 所有自动变更可审计、可对账，异常撤销可人工处理。

## 非目标

第一版不实现：

- 为 Lark 权益新增在线支付、退款、发票或个人购买套餐；New API 既有支付能力仍需执行本文的托管账户策略门禁。
- 让 Lark 直接操作 New API 数据库。
- 让 Traefik 承担 OAuth、审批或额度业务逻辑。
- 用邮件地址作为员工的主身份。
- 一次性额度按月自动恢复或到期清零。
- 审批通过后自动降低订阅等级。
- 员工重新入职后自动恢复旧账户和旧 API key。
- 多 Controller 副本的主动主动部署。
- 自定义 Lark 审批定义的自动创建。审批定义由管理员在 Lark 后台创建并维护。

## 已确定的业务语义

### 一次性额度是钱包余额

一次性审批通过后，系统增加 New API 用户的 wallet quota：

```text
wallet_after = wallet_before + approved_quota_delta
```

它具有以下性质：

- 只增加一次。
- 不进入订阅额度。
- 不随订阅月度重置而恢复。
- 默认永久结转，直到被请求消耗。
- 不改变员工当前订阅等级。

如果未来需要“本月底失效的临时额度”，不能继续复用 wallet quota。New API 当前钱包模型不能可靠区分不同批次余额的来源和有效期，届时应增加独立的 expiring credit ledger。

### 订阅等级是月度权益

员工的托管订阅等级为：

```text
basic < plus < pro < power
```

每个等级映射到一个由管理员维护、用户不可购买的 New API subscription plan。订阅额度每月重置。

“永久提高订阅等级”表示：

- 在员工仍然在职期间，目标等级持续有效。
- 它不是在旧订阅上叠加另一份月额度。
- 每个员工最多只有一份有效的 Lark 托管订阅。
- 后续更高等级审批会替换当前等级。
- 普通升级审批不能降级。
- 离职、显式降级或管理员纠正可以终止该等级。

### 消耗顺序

所有托管员工强制使用：

```text
monthly subscription quota -> wallet quota
```

对应 New API 配置为：

```text
billing_preference = subscription_first
allow_wallet_overflow = true
allow_balance_pay = false
```

当前 fork（上游基线 `v0.13.2`）的 `subscription_first` 是请求级、全额预扣语义，而不是把同一个请求拆分到两个资金源：

1. 如果某份有效订阅可以覆盖本次请求的全部预扣额度，整个请求由订阅支付。
2. 如果订阅不能覆盖本次请求的全部预扣额度，并且允许 wallet overflow，整个请求改由 wallet 支付。
3. 因此订阅中可能留下小于单次请求预扣额的尾量。第一版接受并明确展示该语义，不实现一个请求内的 subscription + wallet 混合结算。

托管账户的 `billing_preference` 是服务端策略锁定字段。New API fork 必须在读取和更新计费偏好的接口中识别 active managed principal：

- 读取时固定返回 `subscription_first` 和 `managed_policy_locked=true`。
- 员工尝试修改为其他模式时返回 `409 managed_policy_locked`。
- integration transaction 和 reconciliation 都复核并写回 `subscription_first`。
- 不能只依赖 UI 隐藏控件，也不能只在权益变更时临时写回。

## 额度目录

Controller 和审批表单只接受稳定的业务代码，不接受可编辑的 `plan_id`、美元金额或原始 quota。

### 订阅等级目录

以下数值是首版建议值，不是已批准的生产政策。上线前必须由业务负责人确认后写入版本化配置。

| `level_code` | 建议月度价值 | 建议 monthly quota | 用途 | 可由普通升级审批达到 |
| --- | ---: | ---: | --- | --- |
| `basic` | `$10` | `5,000,000` | 所有在职员工基础权益 | 自动分配 |
| `plus` | `$25` | `12,500,000` | 经常使用 AI 的员工 | 是 |
| `pro` | `$50` | `25,000,000` | 研发及高频使用者 | 是 |
| `power` | `$100` | `50,000,000` | 经预算负责人批准的重度使用者 | 是，需额外审批人 |

共享策略配置必须包含稳定代码、等级 rank 和 quota，但不能包含环境相关的 New API 数据库 ID：

```yaml
policy_version: employee-entitlements-2026-09-v1
levels:
  basic:
    rank: 10
    monthly_quota: 5000000
  plus:
    rank: 20
    monthly_quota: 12500000
  pro:
    rank: 30
    monthly_quota: 25000000
  power:
    rank: 40
    monthly_quota: 50000000
```

`policy_version + level_code -> plan_id` 的环境绑定只存在于 New API 的版本化 `managed_subscription_levels` 表中。Controller 和 Lark 都不知道 `plan_id`。

### 一次性钱包包目录

以下也是首版建议值，需在上线前确认：

| `package_code` | 建议显示值 | wallet quota delta | 建议审批链 |
| --- | ---: | ---: | --- |
| `topup_5` | `$5` | `2,500,000` | 直属主管 |
| `topup_10` | `$10` | `5,000,000` | 直属主管 |
| `topup_25` | `$25` | `12,500,000` | 直属主管 + 成本负责人 |
| `topup_50` | `$50` | `25,000,000` | 直属主管 + 成本负责人 |

金额换算只在配置发布时完成。运行时根据 `package_code` 查表，不能读取审批人填写的数字再乘 `quota_per_unit`。

同一份版本化策略文件以 read-only 方式提供给 Controller 和 New API fork：

- Controller 用它验证审批代码并生成权益命令。
- New API 用它复核 `package_code/quota_delta` 和 `level_code/monthly_quota`，防止错误或被篡改的 Controller 请求绕过目录。
- 两端 payload 都携带 `policy_version`。New API 必须保留仍有 active assignment、在途审批或历史 grant replay 的只读历史版本；对首次出现且未注册的版本拒绝写入，不猜测兼容关系。

### 策略版本

每次额度语义变化都发布一个不可变、单调演进的 `policy_version`，例如：

```text
employee-entitlements-2026-09-v1
```

New API 维护一个显式 `active_policy_version`，但 active 只表示新登录和新审批默认使用的版本，不会改变历史 grant。基础订阅的幂等键包含版本：

```text
lark:base:<tenant_key>:<open_id>:<policy_version>
```

策略版本规则：

- 已发布的 package、level、quota、rank 和 plan binding 不可原地修改。
- `level_code` 的业务含义和 rank 顺序跨版本稳定；若等级语义或顺序改变，必须使用新的 level code。跨版本乱序保护用 assignment 历史 binding 与目标 binding 的 rank 比较。
- 修改目录数值不会自动补发历史钱包额度，也不会自动重算正在生效的月度订阅。
- active assignment 保存其实际 `policy_version`；reconciliation 按 assignment 的版本校验投影，不能拿当前 active policy 强制迁移旧 assignment。
- 每个审批定义版本通过 `approval_code + schema_fingerprint + locale` 绑定到唯一 `policy_version`。政策改变时创建新的审批定义和不可变 schema 绑定，旧定义保留到所有在途实例结束。
- 同一 `external_id` 的 replay 先查询已存在的 grant 并比较历史 payload hash；只有首次应用时才校验该政策版本是否允许新写入。升级 active policy 后，旧 grant 的正确重放仍必须返回 `replayed`。
- 任何存量迁移都使用独立、可审计的 migration external ID，不能依赖员工下一次登录或每日 reconciliation 偶然触发。

发布新版本的固定顺序：

1. 生成不可变 policy bundle、definition manifest 和 hash，创建新的 `managed_only` plan，不修改历史 plan。
2. 在 New API 一个事务中插入 policy、level、wallet package 和新 approval binding，初始不可接受业务写入。
3. Controller 与 New API 分别加载同一 bundle，校验文件 hash、数据库 catalog hash、plan snapshot 和 schema fingerprint 完全一致。
4. 短暂停止新的 OAuth 完成/base job 派发并 drain 已创建的 base job；关闭旧审批定义的新发起入口并记录 `accept_instance_started_before`，再让旧 policy 进入 draining、新 policy 与新审批定义同时激活，随后恢复 OAuth。未曾应用的旧 base job 只能在确认 principal 仍无 assignment 后，以新 active version 和新 external ID 重新生成。
5. 旧定义所有在途实例终结且超过追溯窗口后，policy 才进入 retired；Controller bundle 用显式 RFC3339 `retire_after` 记录该窗口终点，并在切换时拒绝未来时间或仍有本地未终结 job 的版本；历史 grant replay 永久不依赖 active 状态。

任一步校验失败都不得激活新版本。已产生 grant 后不能回写旧目录来“回滚”，只能停止新写入并发布 correction 或后续版本。

## 领域模型

### Principal

一个 Lark 员工在本系统中的稳定身份：

```text
provider_slug = lark
subject = <tenant_key>:<open_id>
```

`open_id` 是同一 Lark 应用内的稳定用户标识，`tenant_key` 防止不同租户发生碰撞。New API fork 必须把该身份保存到独立的 `integration_principals` 表；`user_oauth_bindings` 只负责登录方式，不能作为权益和离职停用的唯一映射。

禁止用以下字段作为主身份：

- `email`
- `enterprise_email`
- 姓名
- 手机号
- 可由管理员修改的工号

New API 用户名由 subject 的稳定哈希生成，并满足现有 20 字符上限：

```text
lark_<lower(base32-no-padding(sha256(subject)))[0:15]>
```

15 个 base32 字符提供 75 bit 哈希空间。用户名冲突必须返回稳定错误并告警，不能退回顺序 ID 后静默改变命名语义。姓名和头像只作为可更新的展示信息；写入 New API 前按 Unicode code point 截断到 20 字符，不能按 UTF-8 byte 截断。

托管 principal 约束：

- `(provider_slug, subject)` 全局唯一并永久映射到一个 New API user。
- 同一用户最多绑定一个 active Lark managed principal。
- 只有 enabled common user 可以建立 Lark managed principal；root、admin 和 break-glass 账户禁止绑定。
- 员工不能自助解除 active managed Lark binding。解除、换绑和主体纠正只能走审计化管理员流程。
- 权益发放、离职停用和 reconciliation 一律解析 `integration_principals`，不能因 OAuth binding 被删除而创建第二个账户或漏停旧账户。

### Wallet Credit

一次性、可累积、无自动重置的钱包加额。权威变更量为正整数 `quota_delta`。

### Managed Subscription Assignment

员工当前的托管订阅等级。它是长期业务状态，New API 的 `user_subscriptions` 记录是该状态的运行时投影。

### Entitlement Grant

来自一个外部业务事实的幂等权益命令。每条命令有全局唯一 `external_id` 和规范化 payload hash。

### Event Inbox

Controller 对 Lark webhook 的持久化收件箱。它先落盘再应答 Lark，后台 worker 再执行查询和加额。

## 总体架构

```text
Browser
  |
  | 1. Lark login
  v
New API ------------------------------+
  |                                   |
  | Custom OAuth                      | narrow internal entitlement interface
  v                                   v
lark-quota-controller ----------> New API fork
  |          ^                        |
  |          |                        +--> New API Postgres / Redis
  |          |
  |          +--- Lark webhooks
  v
Lark OAuth / Approval / Contact APIs
```

模块职责：

| 模块 | 负责 | 不负责 |
| --- | --- | --- |
| New API | 用户、OAuth 绑定、钱包余额、订阅实例、实时扣费、会话和 API key 校验 | Lark 表单解析、审批策略、Lark webhook 重试 |
| `lark-quota-controller` | Lark OAuth 适配、事件收件箱、审批回查、目录映射、重试和对账 | 直接改 New API 表、模型调用计费 |
| Lark | 员工身份、审批流程、离职事件 | New API 余额账本 |
| Traefik | TLS 和路径路由 | 身份绑定、审批、额度业务规则 |

Controller 对 New API 只依赖五个窄接口：

```http
POST /api/integrations/v1/entitlement-grants
POST /api/integrations/v1/principals/disable
GET  /api/integrations/v1/principals?provider_slug=lark&status=active&cursor=...
POST /api/integrations/v1/entitlement-corrections/preview
POST /api/integrations/v1/entitlement-corrections
```

前三个是常驻 Controller 使用的两个幂等写接口和一个离职补偿只读接口；后两个只供 one-shot 管理员纠正工具使用。principal 枚举只返回分页 active principal subject、principal version 和 opaque cursor，不返回邮箱、手机号、wallet、API key 或内部用户表内容。correction preview 只返回操作者作出 CAS 决策所需的 wallet、usage、last-login 和 managed-subscription 投影，不返回 API key、token 或个人资料。它们共同构成 Controller 与 New API 的集成边界；New API 内部的用户表、订阅表、缓存和事务细节不暴露给 Controller。

## Lark 登录设计

### 为什么需要 OAuth bridge

当前 New API Generic OAuth 不能直接、安全地接 Lark OAuth v3：

1. New API token exchange 使用 `application/x-www-form-urlencoded`，Lark v3 要求 JSON。
2. New API 对空 scope 会补成 `openid profile email`，Lark 未开通这些 scope 时可能返回 `20027`。
3. New API debug 日志会记录 Generic OAuth 响应 body，直接连接可能泄露真实 `user_access_token`。

因此由 Controller 适配协议，并只向 New API 返回短期 opaque handle。

### Controller OAuth 接口

公开接口：

```http
GET /integrations/lark/oauth/authorize
GET /integrations/lark/oauth/callback
```

仅 New API 可访问的内部接口：

```http
POST /internal/oauth/token
GET /internal/oauth/userinfo
```

New API Custom OAuth provider 配置：

```text
slug: lark
authorization_endpoint: https://ai.x2r.store/integrations/lark/oauth/authorize
token_endpoint: http://lark-quota-controller:8080/internal/oauth/token
userinfo_endpoint: http://lark-quota-controller:8080/internal/oauth/userinfo
auth_style: 1
user_id_field: sub
username_field: username
display_name_field: name
email_field: unset
```

New API 中配置的 OAuth `client_id/client_secret` 是 Controller bridge 自己的随机客户端凭证，不是 Lark App ID/App Secret。Lark App Secret 只能由 Controller 持有。

Controller 允许的 New API 回调初始只有：

```text
https://ai.x2r.store/oauth/lark
```

内部 token endpoint 只接受 exact `POST`、不超过 16 KiB 的
`application/x-www-form-urlencoded` body，且 body 必须恰好各含一个非空
`grant_type=authorization_code`、`code`、固定 `redirect_uri`、`client_id` 和
`client_secret`；不接受 Basic Auth、query 参数、未知字段或重复字段。client credential 使用
constant-time comparison，并在任何 login code 消费前校验。成功响应只包含：

```json
{
  "access_token": "<60-second-single-use-opaque-handle>",
  "token_type": "Bearer",
  "expires_in": 60
}
```

内部 userinfo endpoint 只接受 exact `GET`、一个 `Authorization: Bearer <handle>` header，
且不接受 query 参数。成功时原子消费 handle，并只返回：

```json
{
  "sub": "<tenant_key>:<open_id>",
  "username": "<deterministic-lark-username>",
  "name": "<display-name-up-to-20-code-points>"
}
```

两个内部 endpoint 的 `HEAD` 和其他错误方法都在限流、数据库访问和 credential 消费前返回
`405`。token 的稳定错误为 `invalid_request`、`invalid_client`、`unsupported_grant_type`、
`invalid_grant`、`rate_limited` 和 `temporarily_unavailable`；userinfo 使用
`invalid_request`、`invalid_token`、`rate_limited` 和 `temporarily_unavailable`，其中
`invalid_token` 返回 `WWW-Authenticate: Bearer error="invalid_token"`。所有响应均为
`no-store`，错误不包含 secret、handle、identity 或底层数据库文本。

### OAuth 流程

```text
1. New API redirects browser to Controller /oauth/authorize.
2. Controller validates client_id and exact redirect_uri allowlist.
3. Controller creates single-use state and redirects to Lark authorize page.
4. Lark redirects to Controller /oauth/callback with code and state.
5. Controller validates and consumes state.
6. Controller sends JSON to https://accounts.feishu.cn/oauth/v3/token.
7. Controller calls GET /open-apis/authen/v1/user_info.
8. Controller normalizes identity and stores a 60-second single-use login code.
9. Controller redirects browser to New API callback with opaque code and original state.
10. New API exchanges opaque code at Controller /internal/oauth/token.
11. Controller returns a second short-lived opaque access handle.
12. New API calls Controller /internal/oauth/userinfo with that handle.
13. New API atomically creates or binds the local user、OAuth binding 和 immutable integration principal。
14. Controller asynchronously ensures the base managed subscription.
```

Controller 不持久化 Lark `user_access_token` 或 `refresh_token`。登录只需要立即读取一次 user info，初始授权不申请 `offline_access`。

初始 Lark authorize 请求不转发 New API 传入的 `openid profile email`。Controller 使用固定的 `LARK_LOGIN_SCOPES`，初始为空。Lark 官方 user info 接口返回 `name`、`open_id`、`union_id` 和 `tenant_key` 不要求额外 API scope。

### Opaque handle 约束

- 至少 256 bit 随机值。
- 数据库只存 SHA-256 hash，不存明文 handle。
- `state`、login code 和 access handle 都是单次使用。
- 默认 TTL 60 秒，OAuth state 默认 TTL 5 分钟。
- 消费使用带 expiry 条件的原子 `DELETE ... RETURNING`。
- 不写 access log query string。
- 所有 OAuth 错误只返回稳定错误码，不回显 Lark token response。

### 账户创建与绑定

新员工：

- Lark 首次登录可创建 New API 普通用户，并在同一数据库事务建立 OAuth binding 和 `integration_principals` 映射。
- 创建事务先取得 `(provider_slug, subject)` 的数据库 identity lock 并检查离职 tombstone；存在 tombstone 时返回 `principal_disabled`，不能创建 user。disable 流程使用同一把锁，关闭“离职事件先到、OAuth 后提交”的竞态。
- Lark provider 的新用户 wallet 初始值强制为 0，不继承全局 `QuotaForNewUser`。
- Lark OAuth state 不接受或传播 affiliate code，不发放 `QuotaForInvitee`，避免绕过审批账本产生钱包余额。
- 创建后 Controller 重试基础订阅任务，直到 integration principal 可解析到 New API user ID。

已有 New API 用户：

- 先用原登录方式进入自己的 enabled common user 账户。
- 在账户设置中执行一次 Lark binding；binding 事务同时创建 immutable integration principal。
- 绑定完成前，不按 email 自动合并。
- root、admin、disabled user 和 break-glass 账户拒绝绑定，返回稳定错误码。
- active managed Lark binding 不显示自助解绑操作，直接调用现有 DELETE binding API 也返回 `409 managed_identity_locked`。

禁止按 email 自动合并，因为 Lark 官方也明确说明 email 不是实时验证的登录凭证。错误合并会把一个员工绑定到另一个人的 API key 和钱包余额。

相关用户/admin 路径使用稳定错误码：active managed identity 的自助或普通 admin 解绑返回 `409 managed_identity_locked`；仍有 pending subscription order 的账户绑定返回 `409 pending_subscription_order_conflict`；root/admin/break-glass/disabled user 绑定返回 `409 managed_identity_forbidden`。这些错误由 New API 产生并展示给登录用户，不作为 Controller entitlement job 的重试错误。

迁移期建议：

- 关闭密码注册。
- 暂时保留密码登录，供现有用户完成绑定和管理员救援。
- 完成迁移和管理员 break-glass 验证后，再决定是否关闭普通密码登录。

### 基础订阅时序

Controller 从唯一 active policy 解析 `basic`。userinfo 只有在同一个 SQLite 事务中单次消费 access handle、创建或复用基础订阅账本、保存密封 grant job 并写入独立审计后才返回成功。该 job 投递：

```json
{
  "external_id": "lark:base:<tenant_key>:<open_id>:<policy_version>",
  "source": "base_login",
  "policy_version": "employee-entitlements-2026-09-v1",
  "identity": {
    "provider_slug": "lark",
    "subject": "<tenant_key>:<open_id>"
  },
  "grant": {
    "type": "subscription_level",
    "level_code": "basic",
    "minimum_rank_only": true
  }
}
```

这里不伪造 Lark webhook event。基础订阅使用独立的 `base_subscription_grants` 和 `base_subscription_audit`，因为它没有 webhook `event_key`；grant job 仍复用审批 job 的 release、claim、retry、dead-letter 和 executor runtime。shadow mode 保持 `held_shadow`，active runtime 在 New API 完成 OAuth principal 事务后释放并执行；若 job 先被执行，下面的 `principal_not_ready` 重试覆盖该并发窗口。

同一员工在同一 policy version 重复登录时，只有 request hash、subject hash、policy version、catalog hash、level 和 monthly quota 全部一致才记为 `shadow_replayed`。重放保留首条 job 的 key ID、nonce 和 ciphertext，不重新密封替换。任何 planner、sealer、校验、metadata mismatch 或 SQLite 错误都会回滚 handle 消费，使 New API 能以同一 handle 重试；`subject_sha256` 必须等于实际 OAuth subject 的 SHA-256。

release gate 会放行所有历史审批 job，但基础订阅 job 只允许当前 active policy version。policy snapshot sync 和 active runtime 的 startup/release gate 都会拒绝任何非 active version 的 held/pending/processing/retry base job，因此已经 release 或重启恢复的旧 job 也不能在切换后被发送，旧 policy 有未终结审批 grant 时也不能进入 retired。按上述固定发布流程，这类 base job 必须先 drain；未应用遗留项的 principal/assignment 核验与新版本重建仍属于 policy migration 操作，不能靠共享 worker 猜测或错误释放。

如果 New API 尚未完成 integration principal 事务，权益接口返回可重试的 `principal_not_ready`。Controller 使用指数退避重试，不在 OAuth callback 中阻塞等待用户创建事务。

如果 principal 已存在但用户为 disabled，权益接口返回终态 `principal_disabled`，Controller 不重试，也不能创建或恢复 subscription。重新入职只能由管理员显式启用账户并提交独立的恢复命令。

## Lark 审批设计

### 审批定义一：`AI 一次性额度追加`

表单字段：

| 字段 | 类型 | 约束 |
| --- | --- | --- |
| 额度包 | 单选 | 固定唯一显示文本，由版本化 definition manifest 精确映射为 `package_code` |
| 申请原因 | 多行文本 | 必填 |
| 项目或成本中心 | 单选或文本 | 必填 |
| 预计用途 | 多行文本 | 可选 |

审批表单不能包含自由填写的 quota、美元金额或 New API user ID。

推荐审批链：

- `topup_5`、`topup_10`：直属主管。
- `topup_25`、`topup_50`：直属主管 + 成本负责人。

通过后的命令：

```json
{
  "external_id": "lark:wallet-topup:<instance_code>",
  "source": "lark_approval",
  "policy_version": "employee-entitlements-2026-09-v1",
  "identity": {
    "provider_slug": "lark",
    "subject": "<tenant_key>:<open_id>"
  },
  "grant": {
    "type": "wallet_quota",
    "package_code": "topup_10",
    "quota_delta": 5000000
  },
  "evidence": {
    "approval_code": "<approval_code>",
    "instance_code": "<instance_code>",
    "instance_started_at": "2026-08-19T01:02:03Z",
    "schema_fingerprint": "sha256:<hex>",
    "locale": "zh-CN"
  }
}
```

### 审批定义二：`AI 订阅等级升级`

表单字段：

| 字段 | 类型 | 约束 |
| --- | --- | --- |
| 目标等级 | 单选 | 固定唯一显示文本，由版本化 definition manifest 精确映射为 `plus`、`pro`、`power` |
| 申请原因 | 多行文本 | 必填 |
| 项目或成本中心 | 单选或文本 | 必填 |
| 预计每月使用场景 | 多行文本 | 必填 |

表单不出现 New API `plan_id` 和 raw quota。

推荐审批链：

- `plus`：直属主管。
- `pro`：直属主管 + 成本负责人。
- `power`：直属主管 + 成本负责人 + AI 平台管理员。

通过后的命令：

```json
{
  "external_id": "lark:subscription-level:<instance_code>",
  "source": "lark_approval",
  "policy_version": "employee-entitlements-2026-09-v1",
  "identity": {
    "provider_slug": "lark",
    "subject": "<tenant_key>:<open_id>"
  },
  "grant": {
    "type": "subscription_level",
    "level_code": "pro",
    "minimum_rank_only": true
  },
  "evidence": {
    "approval_code": "<approval_code>",
    "instance_code": "<instance_code>",
    "instance_started_at": "2026-08-19T01:02:03Z",
    "schema_fingerprint": "sha256:<hex>",
    "locale": "zh-CN"
  }
}
```

### 订阅和回查

每个政策版本使用一组不可变审批定义。应用对该版本的两个审批定义各调用一次：

```http
POST /open-apis/approval/v4/approvals/:approval_code/subscribe
```

该接口使用 `tenant_access_token`。同一应用只需对每个 `approval_code` 成功调用一次。政策升级时创建新的 approval code；旧 approval code 保持订阅，直到所有在途实例进入终态并超过追溯窗口。

Controller 接收旧版审批实例事件 `approval_instance`。该事件的顶层 `uuid` 是事件去重键，事件体包含 `tenant_key`、`approval_code`、`instance_code` 和 `status`。

事件只是触发器，不是发放额度的权威载荷。收到事件后必须回查：

```http
GET /open-apis/approval/v4/instances/:instance_code
```

只有同时满足以下条件才可执行权益命令：

- `approval_code` 已登记在不可变 `approval_policy_bindings` 中。
- 实例回查成功。
- `status == APPROVED`。
- `reverted == false`。
- 发起人 `open_id` 存在。
- 表单结构与该 approval code 登记的 schema fingerprint 匹配。
- `approval_code + schema_fingerprint + locale` 能唯一解析出发起时的 `policy_version`。
- `package_code` 或 `level_code` 在该历史 policy version 的目录中。
- 事件和实例的租户与配置租户一致。

不要相信 webhook 中的状态后直接加额，也不要从显示文本解析金额或做模糊匹配。Lark 原生固定 `radioV2` 在实例回查中返回选项显示文本，而不是自定义机器 code，因此表单解析使用以下受控流程：

1. 使用开发者模式配置并验证稳定 `custom_id`。
2. Controller 以固定 locale（首版 `zh-CN`）回查实例。
3. 对单选值只允许与该 approval code 的不可变 definition manifest 做 exact match；manifest 显式保存 `display_text -> package_code/level_code`。
4. 显示文本必须在同一控件内唯一，禁止 trim 后重复、子串匹配、数值提取或根据美元金额计算 quota。
5. schema、locale 或选项不匹配时 fail closed 并 dead-letter，不退回按控件名称或顺序解析。

`schema_fingerprint` 是不可变 definition manifest 的 SHA-256，不包含申请人填写的值。manifest 使用确定性 JSON 编码，至少包含 approval kind、locale，以及按 `custom_id` 排序的控件类型、required 标志和有序 option display-text/code mapping；发布工具、Controller 和 New API 使用同一 canonicalization 测试向量。Controller 逐项验证实例结构后才把绑定 fingerprint 写入 evidence，New API 再用 `approval_code + fingerprint + locale` 复核 policy version 和 approval kind。

首版权益选择控件固定为 `wallet_package` 和 `target_level`。成本中心等辅助 `radioV2` 同样按 manifest exact match 校验，但只有对应审批 kind 的权益选择控件能产生 `package_code` 或 `level_code`。

如果后续使用 Lark external option，则可以让外部 option ID 直接等于业务 code；切换前仍需发布新 approval code 和 schema binding，不能改变历史实例解析规则。

政策切换时先停止旧审批定义接受新发起，再记录其 `accept_instance_started_before` 截止时间；旧 definition 继续订阅以处理截止时间前已创建的实例。实例回查的 `start_time` 必须写入 evidence，Controller 和 New API 都拒绝截止时间后的旧定义实例。manifest 内容不可变，生命周期窗口只能单向关闭并保留审计。

### Webhook 收件箱

公开接口：

```http
POST /integrations/lark/events
```

处理顺序：

```text
verify signature / decrypt
  -> handle URL verification challenge
  -> normalize event key
  -> INSERT event inbox with unique key
  -> return HTTP 200 within 3 seconds
  -> background fetch authoritative instance
  -> validate policy
  -> submit idempotent entitlement grant
```

审批 v1 事件去重键：

```text
lark:v1:<top-level uuid>
```

通讯录 v2 事件去重键：

```text
lark:v2:<header.event_id>
```

重复事件返回 HTTP 200，不重复投递任务。

### 事件乱序

Lark 事件可能重复或乱序。处理规则：

- `PENDING`、`REJECTED`、`CANCELED`、`DELETED`、`OVERTIME_CLOSE` 不发放权益。
- `APPROVED` 总是回查实例后再尝试发放。
- `OVERTIME_RECOVER` 只触发回查；除非权威实例随后明确为 `APPROVED` 且 `reverted=false`，否则不发放。
- `REVERTED` 进入撤销处理，不得作为新的 approval grant。若当前 Lark event/API 版本提供 `reverted_instance_code`，必须先验证它；否则只能用该 reversal 事件携带且已登记的原 `instance_code` 精确定位原 grant。禁止按申请人、时间或金额猜测；无法唯一定位时进入 `reversal_pending`。
- 任何未知状态默认 fail closed，持久化原始状态并告警，不按 `APPROVED` 处理。
- 较低等级的延迟审批不能降低已经生效的较高等级。
- 同一等级的重复审批记录为成功 no-op，不创建新订阅。
- 显式降级必须使用独立管理员流程，不能借普通升级审批实现。

## New API fork 接口

### 认证

New API fork 为 integration router 增加独立内部 listener，例如 `0.0.0.0:3001`。公开 New API 继续监听 `:3000`，内部接口只注册到 `:3001`，且 `:3001` 不 publish host port、不配置 Traefik router。

Controller 通过 `lark-integration` Docker network 访问 `http://new-api:3001`。不能只依赖“不给内部 path 单独创建 router”，因为现有 New API 的 `Host(ai.x2r.store)` catch-all 会把同一 public listener 上的所有 path 都暴露出去。

Docker 端口不能按 network 单独附着：监听 `0.0.0.0:3001` 后，同一 New API 容器连接的其他容器网络也能到达该端口。因此安全边界是“未发布、未路由 + 专用 bearer auth”，而不是声称只有 `lark-integration` 可达。生产验收必须从 `edge` / `new-api-data` 同网容器验证未授权请求返回 `401`。如果未来要求严格的网络级单一可达性，应改为共享 Unix socket、独立 sidecar 或明确的容器防火墙规则。

Controller 使用专用窄权限凭证：

```http
Authorization: Bearer <LARK_INTEGRATION_SECRET>
```

该凭证：

- 不是 New API 管理员 PAT。
- 不能调用任何现有 admin endpoint。
- 至少 32 随机字节。
- 只通过 secret file 或受保护的环境变量注入。
- 支持 current/next 双密钥轮换。
- 服务端用 constant-time compare 校验。

correction 使用另一份默认不挂载到常驻 Controller 的短期运维凭证：

```http
Authorization: Bearer <LARK_CORRECTION_SECRET>
```

只有设置 `LARK_CORRECTION_SECRET_FILE` 或 `LARK_CORRECTION_SECRET` 时才注册两个 correction endpoint。该值必须是至少 32 bytes 的单个 printable ASCII token，并且不得等于 current 或 next integration secret。它不能调用 grant、disable 或 principal-list endpoint；常驻 integration credential 也不能调用 correction endpoint。首版 correction credential 没有 current/next 双密钥窗口，变更单完成后应撤销或轮换，并从 one-shot 操作环境移除。correction command 还必须携带 `operator`、`reason`、`change_ticket`、原 grant external ID 和 expected state。

### `POST /api/integrations/v1/entitlement-grants`

请求：

```json
{
  "external_id": "lark:wallet-topup:81D31358-...",
  "source": "lark_approval",
  "policy_version": "employee-entitlements-2026-09-v1",
  "identity": {
    "provider_slug": "lark",
    "subject": "tenant-key:ou_xxx"
  },
  "grant": {
    "type": "wallet_quota",
    "package_code": "topup_10",
    "quota_delta": 5000000
  },
  "evidence": {
    "approval_code": "7C468A54-...",
    "instance_code": "81D31358-...",
    "instance_started_at": "2026-08-19T01:02:03Z",
    "schema_fingerprint": "sha256:<hex>",
    "locale": "zh-CN"
  }
}
```

响应：

```json
{
  "status": "applied",
  "external_id": "lark:wallet-topup:81D31358-...",
  "user_id": 42,
  "result": {
    "grant_type": "wallet_quota",
    "quota_delta": 5000000
  }
}
```

订阅等级 grant 的 `result` 固定返回投影和 assignment 收据：

```json
{
  "status": "applied",
  "external_id": "lark:subscription-level:81D31358-...",
  "user_id": 42,
  "result": {
    "grant_type": "subscription_level",
    "level_code": "pro",
    "subscription_id": 701,
    "assignment_version": 3,
    "transition": "updated"
  }
}
```

`subscription_id` 和 `assignment_version` 必须为正整数，`level_code` 必须与请求一致。
`transition` 只能为 `created`、`updated`、`noop` 或 `ignored_stale`，并与外层
`status` 保持以下关系：`applied -> created|updated`、`noop -> noop`、
`ignored_stale -> ignored_stale`。`replayed` 返回历史不可变 result，因此可保留上述任一
合法 `transition`。

`status` 可能为：

- `applied`：本次事务首次生效。
- `replayed`：相同 external ID 和相同 payload 已经生效，返回原结果。
- `noop`：命令有效，但目标状态已经满足。
- `ignored_stale`：低等级或旧版本命令不会覆盖较新状态。

错误：

```json
{
  "error": {
    "code": "principal_not_ready",
    "message": "principal_not_ready"
  }
}
```

所有非 2xx 响应使用该嵌套 envelope。Controller 只按 HTTP status 和登记过的 `code`
分类；`message` 不作为控制流输入，也不写入持久化错误记录。

| HTTP | code | 语义 | Controller 动作 |
| ---: | --- | --- | --- |
| `400` | `invalid_request` | 结构错误 | dead-letter |
| `401` | `integration_unauthorized` | 凭证错误 | 告警，不重试 |
| `404` | `principal_not_ready` | integration principal 事务尚未可见 | 退避重试 |
| `409` | `principal_disabled` | principal 或用户已停用 | 终态，不重试，不创建订阅 |
| `409` | `external_id_payload_mismatch` | 同一 ID 对应了不同 payload | 高优先级告警，人工处理 |
| `409` | `unmanaged_subscription_conflict` | 已有非托管有效订阅 | 暂停自动变更，人工迁移 |
| `409` | `policy_version_mismatch` | 首次 grant 的版本或目录 hash 未登记 | 停止新写入，修复发布；不影响历史 replay |
| `409` | `approval_binding_mismatch` | approval evidence 与版本、kind 或实例窗口不匹配 | dead-letter + 告警 |
| `422` | `unknown_package` | New API 未配置该历史钱包包 | dead-letter + 告警 |
| `422` | `unknown_level` | New API 未配置该稳定等级 | dead-letter + 告警 |
| `422` | `quota_out_of_range` | delta 非正数或 checked addition 溢出 | dead-letter + 高优先级告警 |
| `503` | `temporarily_unavailable` | 数据库或缓存暂不可用 | 退避重试 |

### 幂等事务

服务端执行以下单事务：

```text
BEGIN
  select/insert and lock integration_grants by external_id
  if existing: compare canonical payload hash and return stored result
  validate registered historical policy and source-specific provenance for first application
  resolve immutable integration_principal to New API user
  lock user, managed assignment and current subscription
  reject disabled/non-common principals and unmanaged subscription conflicts
  apply checked wallet delta or in-place subscription transition
  store immutable result and audit evidence
COMMIT
run durable post-commit cache/auth publish side effects
```

要求：

- `external_id` 有数据库唯一约束。
- canonical payload 采用确定性 JSON 编码后 SHA-256。
- 同 ID、同 hash 在任何当前 policy 检查之前返回已存结果。
- 同 ID、不同 hash 返回 `409`，绝不能尝试“修正”历史记录。
- `source=base_login` 只允许 active policy 的 `basic + minimum_rank_only=true`；`source=lark_approval` 必须校验 approval code、kind、schema fingerprint、locale 和 instance start window。migration/correction 使用独立受限凭证、external ID、operator 和 reason，不能伪装成上述来源。
- 钱包使用 `bigint/int64`，通过 checked addition 更新；钱包增量与幂等记录在同一数据库事务提交。
- 不能先调用现有 `/api/user/manage` 再记录幂等结果。
- cache invalidation、Redis auth-version publish 等跨系统副作用不能伪装成数据库事务的一部分。需要可重试 post-commit outbox；限制性操作在提交前先设置 fail-closed auth-version fence。

当前 New API `add_quota` 只是执行 `quota = quota + value`，没有外部幂等键。Controller 若直接调用它，在“数据库已加额但 HTTP 响应丢失”的故障窗口中会重复入账，所以不可用于本集成。

### 托管订阅转换

New API fork 维护每个用户唯一的 `managed_subscription_assignment`。

等级变更是绝对目标：

```text
set managed level to pro
```

而不是：

```text
add one pro subscription
```

转换事务：

1. 按固定顺序锁定用户、integration principal、managed assignment 和当前 managed subscription。
2. 校验用户为 enabled common user，且不存在 active non-managed subscription；有冲突则不自动取消任何订阅，返回 `unmanaged_subscription_conflict`。
3. 从 `policy_version + level_code` 的 managed level binding 解析受控 plan。
4. 对当前 subscription 在锁内执行 lazy reset；如果 `next_reset_time <= now`，先推进周期并把过期周期用量清零，再处理升级。
5. 若目标 rank 低于当前 rank 且 `minimum_rank_only=true`，记录 `ignored_stale`。
6. 若现有 assignment 的 rank 已等于目标 rank，记录 `noop`，即使命令来自不同 policy version 也不隐式迁移；基础 ensure 命令遇到任何不低于 basic 的 active assignment 同样 no-op。policy migration 必须使用独立 migration external ID。
7. 首次 basic assignment 创建一条 managed subscription；已有 managed subscription 升级时原地更新同一行的 `plan_id`、`amount_total` 和受控 plan snapshot 字段，保留 subscription ID。
8. 保留当前 `amount_used`、`last_reset_time`、`next_reset_time`、`start_time` 和当前周期边界。所有 managed level 必须使用相同 reset period/anchor 语义，否则政策发布失败。
9. 更新 assignment 的 `level_code`、`policy_version`、`version` 和 `source_external_id`；已有 projection 的 `subscription_id` 不变。
10. 强制用户计费偏好为 `subscription_first`，并失效以 subscription ID 为键的 plan info cache。

升级不能通过现有的“先 invalidate、再 admin create”两个 HTTP 调用完成，因为两步之间存在无订阅窗口，也无法与 grant 幂等记录原子提交。也不能在单事务中“取消旧行 + 新建行”：New API 的在途 `SubscriptionPreConsumeRecord` 绑定旧 subscription ID，后续 settle/refund 会继续更新旧行，导致新行用量漂移。原行升级使预扣、结算和退款始终落到同一 subscription。

### 月内升级的用量继承

升级不应重置当月已使用额度：

```text
old total = 10
old used = 9
new total = 30
new used = 9
new remaining = 21
```

同一条 managed subscription 原地升级并保留：

- `amount_used`
- `last_reset_time`
- `next_reset_time`
- 当前周期边界

如果由显式管理员降级导致 `old used > new total`：

```text
new used = new total
new remaining = 0
```

普通 Lark 升级审批永远不走该降级分支。

### 托管计划配置

每个 plan：

- `enabled = true`
- `managed_only = true`，由 fork 新增并在所有用户购买/公开列表接口中过滤
- 月度 quota reset
- 长有效期，例如 10 年
- `allow_wallet_overflow = true`
- `allow_balance_pay = false`
- `upgrade_group` 和 `downgrade_group` 均为空，不让托管订阅隐式改变 New API 用户组
- 不出现在用户可购买目录中
- 不设置可由用户触发的支付产品 ID

“10 年”只是 New API subscription snapshot 的技术有效期，不是业务承诺。业务权威状态是 managed assignment，New API fork 的内部 reconciliation job 应确保 assignment 的运行时投影存在且正确。

现有用户如果已有 active non-managed subscription，首次绑定 Lark 后进入人工迁移队列。系统保留其 wallet 和原订阅，不自动叠加 basic，也不擅自取消原订阅。管理员完成迁移后再重放基础等级命令。

### 托管订阅写入门禁

`managed_only` 只解决“普通用户不能购买托管 plan”，不能解决“托管员工另购普通 plan”。只要用户存在 `status=active` 的 managed principal，该用户的任何 subscription 创建都只能来自 integration transaction；该规则不限制普通 wallet top-up，但禁止通过其他路径获得第二份订阅。

New API fork 必须把门禁放在共享 model/transaction 层，并由所有入口复用：

- 余额购买在锁定 user 和 principal 后、扣 wallet 前拒绝。
- Stripe、Creem、Epay、Waffo 等每个 subscription checkout/order 创建入口在创建 pending order 前拒绝。
- `CompleteSubscriptionOrder` 在锁定 pending order、user 和 principal 后再次拒绝，覆盖“先创建订单、后绑定 Lark、再收到支付成功回调”的竞态。
- 支付已经成功但回调被门禁阻止时，事务把订单置为 `blocked_managed_policy`，保存支付凭证并投递人工退款 outbox；对支付网关幂等确认，不能创建 subscription，也不能无限重试回调。
- 普通 `AdminBindSubscription`、admin create-user-subscription 和同类直接绑定入口同样拒绝。确需处理存量的管理员只能使用独立的审计迁移命令，提供 operator、reason、external ID 和 expected assignment version，并在一个事务内证明最终仍只有一份 active subscription。
- 非托管用户也不能从任何公开、支付或普通 admin 路径获得 `managed_only=true` 的 plan。

仅隐藏 plan、仅在 HTTP controller 检查，或仅在创建订单时检查都不够。Lark binding 事务还应检测 pending subscription order 并返回 `pending_subscription_order_conflict`，先完成或取消订单再绑定；支付完成函数的二次门禁仍是最终保障。

active managed user 修改 billing preference、创建 subscription order、余额购买或普通 admin bind 均在交互 API 返回 `409 managed_policy_locked`。支付 webhook 的内部 completion 记录同一策略原因，但在原子写入 `blocked_managed_policy` 和退款 outbox 后按支付网关协议返回成功确认，避免重复扣款通知。该错误不应被 Controller 误当作 entitlement endpoint 的可重试结果。

### `POST /api/integrations/v1/principals/disable`

请求：

```json
{
  "external_id": "lark:disable:<event_id>",
  "source": "contact_event",
  "identity": {
    "provider_slug": "lark",
    "subject": "<tenant_key>:<open_id>"
  },
  "reason": "contact.user.deleted_v3"
}
```

处理顺序：

1. 先取得 `(provider_slug, subject)` 的 PostgreSQL transaction-scoped identity lock，再解析并锁定 `integration_principals`；不能查询或依赖可删除的 OAuth binding。OAuth principal 创建使用同一锁顺序。
2. principal 不存在时，原子写入 `integration_principal_tombstones` 和幂等账本并返回成功 `noop`；tombstone 会阻止稍后的 OAuth 开户。principal 存在时，即使 OAuth binding 已被管理员误删，也继续停用其永久映射的 user。
3. 对已解析 user 先安装短 TTL、fail-closed 的 auth deny fence。Redis/fence 不可用时返回 `503`，不开始停用事务，避免数据库已经 disabled 而旧缓存仍放行。
4. 在同一个 PostgreSQL 事务中锁定 principal、user、managed assignment 和 managed subscription；将 principal 和 user 设为 disabled，推进 `auth_version`，撤销所有 active `user_sessions`，终止 managed subscription，将 assignment 设为 inactive，并写入幂等结果、审计和 durable outbox。
5. 提交后由 outbox 幂等发布新的 auth version，并失效 relay token、API key、user、quota、subscription 等缓存。只有这些副作用确认完成后才移除 deny fence；outbox 失败持续重试并告警。

所有 dashboard session 和 API token 校验都必须识别 deny fence，并最终以 user status/auth version 为准。相同 `external_id` 重放先返回原结果；已停用 principal 再次停用返回 `noop`。后续基础 grant 对 disabled principal 返回终态 `principal_disabled`，不得重建 subscription。

deny fence 建立前已经持久化的 `SubscriptionPreConsumeRecord` 仍允许对同一已终止 subscription 行完成 settle/refund，以结清真实在途请求；该操作不能恢复 subscription、assignment、principal 或用户状态。fence 建立后不得创建新的预扣。

### `GET /api/integrations/v1/principals`

该接口仅用于 Controller 的离职补偿巡检，要求 integration credential、固定 `provider_slug=lark`、`status=active`、有上限的 `limit` 和 opaque keyset cursor。首次响应固定本轮 `snapshot_max_id`，后续页沿用同一签名 cursor，避免分页过程中新增 principal 造成重复或漏页；状态并发变化允许下一轮收敛。

响应只包含 `provider_slug`、`subject`、`principal_version`、`updated_at`、`next_cursor` 和 `scan_complete`。不返回 New API user ID、姓名、邮箱、余额、token 或 subscription 细节。Controller 只有在 `scan_complete=true` 且权限健康探针正常时，才能把本轮 not-found 计入连续确认。

### `POST /api/integrations/v1/entitlement-corrections/preview`

该接口只接受 correction credential，用于在人工决定前读取目标 principal 的当前 CAS 状态：

```json
{
  "identity": {
    "provider_slug": "lark",
    "subject": "tenant-key:ou_xxx"
  }
}
```

响应不包含 New API user ID 或身份资料：

```json
{
  "wallet_quota": 3000000,
  "used_quota": 7000000,
  "last_login_at": 1788192100,
  "managed_subscription": {
    "policy_version": "employee-entitlements-2026-09-v1",
    "level_code": "pro",
    "assignment_version": 3,
    "source_external_id": "lark:subscription-level:...",
    "subscription_id": 701,
    "amount_total": 15000000,
    "amount_used": 9000000,
    "start_time": 1785542400,
    "end_time": 2100902400,
    "last_reset_time": 1785542400,
    "next_reset_time": 1788220800
  }
}
```

没有 active managed assignment 时省略 `managed_subscription`。principal 不存在、已停用或投影不一致时 fail closed，不返回部分数据。

### `POST /api/integrations/v1/entitlement-corrections`

correction 是新的审计命令，不会修改原 grant。钱包纠正使用 signed delta，包含可审计的 `0` no-op：

```json
{
  "external_id": "lark:correction:CHG-2026-0060:wallet",
  "source": "correction",
  "policy_version": "employee-entitlements-2026-09-v1",
  "identity": {
    "provider_slug": "lark",
    "subject": "tenant-key:ou_xxx"
  },
  "correction": {
    "type": "wallet_quota",
    "quota_delta": -1000000,
    "expected_wallet_quota": 3000000
  },
  "evidence": {
    "operator": "ops@example.com",
    "reason": "reverted approval after partial use",
    "change_ticket": "CHG-2026-0060",
    "original_external_id": "lark:wallet-topup:..."
  }
}
```

订阅纠正使用 absolute target 和 assignment version CAS：

```json
{
  "external_id": "lark:correction:CHG-2026-0061:subscription",
  "source": "correction",
  "policy_version": "employee-entitlements-2026-09-v1",
  "identity": {
    "provider_slug": "lark",
    "subject": "tenant-key:ou_xxx"
  },
  "correction": {
    "type": "subscription_level",
    "level_code": "basic",
    "expected_assignment_version": 3
  },
  "evidence": {
    "operator": "ops@example.com",
    "reason": "approved downgrade after reversal",
    "change_ticket": "CHG-2026-0061",
    "original_external_id": "lark:subscription-level:..."
  }
}
```

New API 在 `integration_grants` 中复用 external-ID ledger 和 outbox。`lark:correction:*` external ID 的历史同-payload replay 必须在 current expected-state CAS 和 policy lifecycle 检查之前返回，因此“数据库已提交、HTTP response 丢失”后可以安全重放。首次写入允许已发布的 `active`、`draining` 或 `retired` policy，拒绝 `staged`。钱包结果返回 final `wallet_quota`；负余额、业务上限和 checked addition 溢出均原子拒绝。订阅纠正在同一 subscription ID 上原地设置目标等级，保留 start/end/reset timestamps；降级时若当前 `amount_used` 超过目标总额，则将 used 截到目标总额，remaining 为零。相同 level code 即使命令指向另一已发布 policy 也只推进审计 grant 并返回 `noop`，不推进 assignment version，不迁移 assignment policy/source，不更新用户 setting、subscription 或 cache outbox；policy migration 必须走独立流程。

correction endpoint 的稳定错误矩阵：

| HTTP | code | 语义 | 操作动作 |
| ---: | --- | --- | --- |
| `400` | `invalid_request` | 结构、namespace、source 或审计证据错误 | 修正命令，禁止重用已提交 external ID |
| `401` | `integration_unauthorized` | correction credential 缺失或错误 | 停止并修复凭证，不重试写入 |
| `404` | `principal_not_ready` | principal 不存在 | 核实 subject 和登录/绑定状态 |
| `409` | `principal_disabled` | principal 或用户已停用 | 终态，不自动恢复 |
| `409` | `external_id_payload_mismatch` | external ID 已绑定其他 payload | 高优先级告警，禁止覆盖 |
| `409` | `correction_state_mismatch` | wallet quota 或 assignment version 已变化 | 重新 preview 和审批，使用新的 external ID |
| `409` | `correction_original_grant_mismatch` | New API 已存在的原 grant 与 correction 的 provider、subject、type、approval source 或终态不一致 | 停止并调查主体/原 grant 选择，不重试 |
| `409` | `correction_already_applied` | 该 original grant 已由另一 correction external ID 取得唯一栅栏 | 禁止第二次纠正；恢复并复用首条 receipt |
| `409` | `policy_version_mismatch` | policy 未发布或仍 staged | 修复 policy/target，不猜测替代版本 |
| `409` | `unmanaged_subscription_conflict` | managed projection 不一致或存在冲突 | 人工调查，不自动改订阅 |
| `409` | `managed_plan_mismatch` | 目录绑定的 plan 缺失或 reset/quota contract 漂移 | 修复目录/plan 漂移并重新审批，不重试原命令 |
| `422` | `unknown_level` | target level 不在指定历史目录 | 修正目标并重新审批 |
| `422` | `quota_out_of_range` | wallet 结果越界或算术溢出 | 修正 delta 并重新审批 |
| `503` | `temporarily_unavailable` | 数据库或内部依赖暂不可用 | 用完全相同 external ID/payload 重试 |

## 数据模型

### New API Postgres

上游 `v0.13.2` 的 `users.quota` 和 `users.used_quota` 原为 PostgreSQL `int`；WP2 fork 已把两列、Go model、批量聚合器、Redis cache、日志、API DTO 和 quota 算术迁移到 `bigint/int64`，使用 checked addition，并把默认业务上限限制在 `2^53-1` 以内以保持 JSON number 兼容。部署 migration 前仍必须 preflight 现有值和完整读写链，不能只变更 `integration_grants.quota_delta`。

建议新增 `integration_principals`，作为 OAuth binding 之外的永久身份映射：

| 字段 | 类型 | 约束或用途 |
| --- | --- | --- |
| `id` | bigint | PK |
| `provider_slug` | varchar(64) | NOT NULL |
| `subject` | varchar(255) | NOT NULL |
| `user_id` | bigint | FK users, NOT NULL |
| `status` | varchar(32) | active/disabled |
| `disabled_reason` | varchar(128) | nullable audit |
| `created_at` | timestamptz | audit |
| `updated_at` | timestamptz | audit |
| `disabled_at` | timestamptz | nullable audit |

增加 `UNIQUE(provider_slug, subject)`，以及 active 状态上的部分唯一约束 `UNIQUE(provider_slug, user_id) WHERE status = 'active'`。映射不得物理删除或改指另一个 user；主体纠正通过有审计的状态转换和 migration command 完成，历史映射继续可审计。

建议新增 `integration_principal_tombstones`：

| 字段 | 类型 | 约束或用途 |
| --- | --- | --- |
| `provider_slug` | varchar(64) | composite PK |
| `subject` | varchar(255) | composite PK |
| `source_external_id` | varchar(255) | UNIQUE disable evidence |
| `reason` | varchar(128) | contact event/reconciliation |
| `evidence_hash` | char(64) | no raw personal data |
| `created_at` | timestamptz | audit |
| `superseded_at` | timestamptz | nullable, only explicit rehire workflow |

OAuth create/bind 和 disable 都先取得由 provider+subject 稳定派生的 PostgreSQL advisory transaction lock，再检查 principal/tombstone，避免“检查后插入”竞态。锁 hash 碰撞最多造成额外串行，不得改变身份结果。tombstone 不自动过期；重新入职只能由审计恢复命令标记 superseded。

建议新增 `entitlement_policy_versions`：

| 字段 | 类型 | 约束或用途 |
| --- | --- | --- |
| `policy_version` | varchar(128) | PK |
| `catalog_hash` | char(64) | UNIQUE, normalized policy hash |
| `state` | varchar(32) | active/draining/retired |
| `activated_at` | timestamptz | audit |
| `retired_at` | timestamptz | nullable audit |
| `created_at` | timestamptz | audit |

`active` 接受新登录和新审批，`draining` 只接受已绑定旧 approval definition 的在途实例，`retired` 只允许历史 replay 和显式 migration/correction。retired 不会删除目录，也不会强制迁移仍引用该版本的 active assignment；reconciliation 继续按其历史 binding 校验投影。目录子表一经发布不可更新；state 转换不改变目录内容。数据库只允许一个 active version。

建议新增版本化 `managed_subscription_levels`：

| 字段 | 类型 | 约束或用途 |
| --- | --- | --- |
| `policy_version` | varchar(128) | FK policy versions, composite PK |
| `level_code` | varchar(64) | composite PK |
| `rank` | int | 单调等级顺序 |
| `monthly_quota` | bigint | policy authority |
| `plan_id` | bigint | FK subscription plans |
| `reset_contract_hash` | char(64) | reset period/anchor contract |
| `created_at` | timestamptz | audit |

增加 `UNIQUE(policy_version, plan_id)`。已被版本绑定的 plan 的 quota、reset、wallet overflow 和 group snapshot 字段必须锁定不可编辑；变更通过新 policy version 和新 plan 完成。

建议新增版本化 `managed_wallet_packages`：

| 字段 | 类型 | 约束或用途 |
| --- | --- | --- |
| `policy_version` | varchar(128) | FK policy versions, composite PK |
| `package_code` | varchar(64) | composite PK |
| `quota_delta` | bigint | positive, within business limit |
| `created_at` | timestamptz | audit |

建议新增 `approval_policy_bindings`：

| 字段 | 类型 | 约束或用途 |
| --- | --- | --- |
| `approval_code` | varchar(255) | composite PK |
| `schema_fingerprint` | varchar(80) | composite PK |
| `locale` | varchar(16) | composite PK, first release `zh-CN` |
| `policy_version` | varchar(128) | FK policy versions |
| `approval_kind` | varchar(32) | wallet_topup/subscription_level |
| `definition_manifest_hash` | char(64) | immutable manifest hash |
| `accept_instance_started_before` | timestamptz | nullable one-way close window |
| `created_at` | timestamptz | audit |

该表只保存 New API 防御性校验所需的 binding。完整 `custom_id + exact display text -> business code` manifest 保存在 Controller 的只读 policy bundle 和 SQLite 快照中；两端启动时比较 hash。

建议新增 `integration_grants`，作为 grant、disable 和 correction 写接口共享的幂等命令账本：

| 字段 | 类型 | 约束或用途 |
| --- | --- | --- |
| `id` | bigint | PK |
| `external_id` | varchar(255) | UNIQUE, NOT NULL |
| `payload_hash` | char(64) | NOT NULL |
| `command_type` | varchar(64) | wallet_quota/subscription_level/principal_disable |
| `command_source` | varchar(64) | base_login/lark_approval/contact_event/employment_reconciliation/migration/correction |
| `policy_version` | varchar(128) | grant 使用的版本；disable nullable |
| `principal_id` | bigint | FK integration principals, nullable only for missing-principal noop |
| `provider_slug` | varchar(64) | immutable request audit |
| `subject` | varchar(255) | immutable request audit |
| `user_id` | bigint | FK users, nullable only for missing-principal noop |
| `package_code` | varchar(64) | wallet grant only |
| `quota_delta` | bigint | wallet grant only |
| `level_code` | varchar(64) | subscription grant only |
| `subscription_id` | bigint | projection ID; upgrade before/after remain identical |
| `prior_level_code` | varchar(64) | nullable audit/reversal |
| `prior_policy_version` | varchar(128) | nullable audit/reversal |
| `assignment_version` | bigint | nullable audit/reversal ordering |
| `status` | varchar(32) | applied/noop/ignored_stale/reversal_pending |
| `evidence_json` | jsonb | approval identifiers/fingerprint/locale, no secrets |
| `result_json` | jsonb | immutable response |
| `created_at` | timestamptz | audit |
| `applied_at` | timestamptz | audit |

不存在 `created_subscription_id`：首次 basic 和原地升级都统一记录最终 `subscription_id`，`result_json` 说明是 created 还是 updated。replay 以该不可变结果为准。

建议新增 `managed_subscription_assignments`：

| 字段 | 类型 | 约束或用途 |
| --- | --- | --- |
| `id` | bigint | PK |
| `user_id` | bigint | UNIQUE, FK users |
| `principal_id` | bigint | UNIQUE, FK integration principals |
| `policy_version` | varchar(128) | composite FK to managed level |
| `level_code` | varchar(64) | composite FK, current absolute target |
| `subscription_id` | bigint | UNIQUE current projection |
| `version` | bigint | CAS and reversal ordering |
| `source_external_id` | varchar(255) | last applied source |
| `active` | boolean | employment state |
| `created_at` | timestamptz | audit |
| `updated_at` | timestamptz | audit |

New API 原有 `user_subscriptions` 允许多个有效订阅，因此必须增加部分唯一约束或事务级等价约束，保证每个 active assignment 恰好一条 active managed subscription，并禁止 active managed user 拥有其他 active subscription。

建议新增 `integration_outbox`，至少保存 `event_id`、`aggregate_type/id`、`event_type`、`payload_json`、`attempts`、`next_attempt_at`、`delivered_at` 和 `last_error`。grant、disable、auth version 推进与 outbox INSERT 同一事务提交；worker 幂等执行 cache invalidation 和 auth-version publish，失败可重试和告警。

New API fork 同时给 `subscription_plans` 增加 `managed_only boolean NOT NULL DEFAULT false`。所有 plan 列表、余额购买、支付下单、支付完成和普通 admin bind 都执行前述双向门禁；只有 integration transaction 可以创建或原地更新 managed subscription snapshot。

### Controller SQLite

首版使用单副本 SQLite WAL 和独立持久卷。建议表：

| 表 | 用途 |
| --- | --- |
| `oauth_states` | 原始 New API state/redirect 关联，单次消费 |
| `oauth_login_codes` | Controller 回给 New API 的短期 code hash |
| `oauth_access_handles` | userinfo 短期 handle hash |
| `principals` | subject、展示信息、最近登录和在职回查状态；不是 New API 身份映射权威 |
| `policy_versions` | 已加载 policy bundle 的版本、hash 和状态快照 |
| `approval_policy_bindings` | approval code、locale、schema fingerprint 和 definition manifest |
| `lark_event_inbox` | webhook 去重或确定性 reconciliation event、显式 origin、规范化 event、处理状态 |
| `approval_instances` | 回查结果摘要、schema hash、处理决策 |
| `approval_reversals` | 脱敏撤销证据、原 external ID、原 grant 摘要、bounded manual-review result/reason，以及组级 correction receipt 引用 |
| `approval_reversal_correction_intents` | New API 写入前持久化的 correction attempt ledger；每条 attempt 不可变绑定 correction external ID、type、subject/request hash 和审计字段，并以 `active/abandoned/remote_conflict/resolved` 记录生命周期；只有非 `abandoned` attempt 按 original external ID 唯一占用恢复栅栏 |
| `approval_reversal_resolution_receipts` | 每个 original external ID 唯一的不可变 correction receipt；correction external ID 为主键，并绑定 subject hash、request hash、operator/ticket/result/resolved_at |
| `approval_reconciliation_cursors` | 每个 approval code 已完整扫描到的 monotonic UTC 时间 |
| `approval_reconciliation_audit` | 成功或失败窗口、bounded result 和实例数；不保存上游错误正文 |
| `entitlement_command_shadows` | 脱敏 grant receipt、external ID/request hash 重放账本 |
| `base_subscription_grants` | userinfo 基础订阅的确定性 external ID、hash、policy/catalog/level/quota 重放账本 |
| `base_subscription_audit` | 无 webhook event key 的基础订阅 plan/replay/result/retry/dead-letter 审计 |
| `entitlement_grant_jobs` | AES-256-GCM 密封的 canonical request；shadow 写入 `held_shadow`，active gate 后转为可执行状态 |
| `principal_disable_jobs` | AES-256-GCM 密封的 disable command；实时事件可选关联 inbox，reconciliation 不伪造 event |
| `principal_disable_audit` | disable shadow/retry/result/dead-letter 脱敏审计，不依赖 webhook event key |
| `employment_reconciliation_runs` | 每个 UTC evidence date 的最新完整或失败扫描状态 |
| `employment_checks` | 每个 UTC evidence date 最新一次尝试的 subject hash、查询时间、稳定结果、Lark result code、权限健康状态和 evidence hash |
| `employment_reconciliation_audit` | 同日失败与成功均保留的 append-only bounded result 审计 |
| `employment_missing_evidence` | subject hash 的连续 not-found 次数及首次、最近一次健康完整证据时间 |
| `processing_recovery_audit` | stale claim 恢复的 bounded queue/count 审计，不保存 subject 或 payload |
| `jobs` | retry schedule、attempt、last_error、dead-letter |
| `controller_audit` | Controller 决策轨迹 |

SQLite 中不保存 New API 管理员凭证、New API Postgres 凭证或长期 Lark user token。

如果未来需要两个以上 Controller 副本，应先迁移到 Postgres，再开启多副本。不能把 SQLite volume 同时挂载给多个写实例。

## 撤销处理

### 钱包加额审批被撤销

不要自动扣回完整 `quota_delta`。

原因：

- 员工可能已经消耗部分或全部额度。
- New API wallet 是混合余额，不能判断剩余余额来自哪一次 grant。
- 直接减余额可能扣掉充值、兑换码或其他审批带来的合法额度。

处理方式：

```text
REVERTED
  -> mark original grant reversal_pending
  -> notify operator
  -> show original delta, current wallet and usage timestamps
  -> operator decides correction
  -> preview, apply a new correction command, store resolution
  -> move inbox and fenced grant job to reversal_resolved
```

人工纠正必须产生新的独立 `external_id`，不能修改原 grant 记录；但同一个 original grant 最多只能有一条成功 correction receipt。one-shot `cmd/lark-correction` 默认只 preview；`--apply` 才提交 New API 写入。`--list-pending` 使用 SQLite `mode=ro + query_only` 的 Store 连接枚举待处理项，不运行 schema migration 或 processing recovery，不需要 correction credential，也不要求运维人员执行 raw SQLite。输出包含可选的当前非 abandoned attempt bounded lifecycle 摘要，但不输出 subject hash；历史 abandoned attempt 仅保留在 SQLite audit ledger。CLI 在读取 credential 或访问 New API 前将 `--subject` 做 SHA-256，并与原 `entitlement_command_shadows.subject_sha256` 精确比较，同时查询 original-level receipt 和非 abandoned attempt；晚到 reversal 只能 preview/attach 完全相同的历史 receipt，不再次访问 New API。

`--apply` 在读取 credential 和调用 New API 前，先按 original external ID 原子写入 attempt。每条 attempt 的 external ID、request hash、correction type 与审计字段不可变；其 lifecycle 为 `active`、`abandoned`、`remote_conflict` 或 `resolved`。credential/client/preview 的 fresh-attempt 失败、fresh preview expected-state 不匹配，以及 New API 明确返回的无提交业务拒绝会把 attempt 标为 `abandoned`，释放 original external ID，允许经过新审批的 external ID 继续；旧 external ID 不能绑定不同 payload。timeout、transport failure、不可验证的成功响应和未知远端错误保留 `active`，因为远端可能已经提交，只允许完全相同命令恢复。`correction_already_applied` 和 `external_id_payload_mismatch` 标为 `remote_conflict`，继续阻止替代命令，直到人工对账。成功 resolution 在同一事务把 attempt 标为 `resolved`。若 New API 已提交但 response 丢失，active attempt 保留原 external ID/request hash，默认 preview 返回 `existing_intent`，完全相同的 `--apply` 可跨过 stale expected-state 恢复远端 replay 和本地 resolution。

Store 在 resolution 事务内再次校验或补写同一 attempt 和原 grant 证据。CLI 和 Store 还校验 correction type 与原 grant type 相同；若 New API 已存在原 grant ledger，New API 再验证 provider、subject、type、`lark_approval` source 和 terminal status。New API 在 correction ledger 创建时用 nullable `correction_original_external_id` 唯一索引抢占 original grant，顺序早于任何 wallet/subscription side effect；并发不同 correction 中最多一个成功，其他稳定返回 `correction_already_applied`。Controller 的 attempt 和组级 receipt 都以 correction external ID 为主键；receipt 的 original external ID 永久唯一，attempt 只对 `active/remote_conflict/resolved` 建立 original external ID partial unique index。`OpenCorrection` 在允许运维写入前验证两表主键、receipt 唯一索引和 attempt partial unique predicate。resolution 一旦提交，只允许同 original subject hash、external ID、request hash、operator、reason、ticket、response status 和 result 的精确重放，任何差异返回冲突。

### 等级升级审批被撤销

仅当以下条件全部成立时可自动恢复 prior level：

- 原 grant 保存了 `prior_level_code`。
- 当前 assignment 的 `source_external_id` 仍等于被撤销 grant。
- 当前 assignment version 未被后续命令推进。
- 恢复目标仍在目录中。

否则标记 `reversal_pending`，由管理员处理。

自动恢复也提交一个新的幂等 subscription level 命令，并携带 expected assignment version。它不能删除或改写原 grant。

## 离职处理

应用订阅：

```text
contact.user.deleted_v3
```

该事件使用 v2 envelope，唯一键为 `header.event_id`，身份来自：

```text
header.tenant_key + ":" + event.object.open_id
```

Controller 持久化事件后调用 principal disable 接口。

最小事件权限可从以下权限中选择满足订阅的一项，首选：

```text
contact:contact.base:readonly
```

只为停用账户不需要申请邮箱、手机号、工号或完整通讯录字段权限。

Lark userinfo 返回 `20021 User resigned` 只能阻止该员工再次登录，不能终止既有 New API 浏览器会话和 API key，所以不能替代离职事件。

### 离职事件丢失补偿

Webhook 不是永久可靠的离职事实来源。Controller 使用同一 `contact:contact.base:readonly` 权限，每日对 New API 中所有 active Lark principal 分批查询 Lark 用户状态，并受 Lark rate limit 控制；初始实现串行查询，包含前后 health probe 在内的请求起点至少间隔 100ms（最多 10 QPS），`429` 后遵守更长但不超过正常 reconciliation interval 的 `Retry-After`。该任务只读取最小身份和在职状态，不读取邮箱、手机号或工号。

自动停用必须满足严格证据门槛：

- Lark 明确返回 resigned/exited/deleted 等权威终态；或
- 同一 subject 在至少两次、相隔至少 24 小时的完整巡检中稳定 not-found，同时 tenant、应用可用范围和权限健康探针均正常。

超时、`429`、`5xx`、token/scope/permission 错误、租户不匹配、应用可用范围收缩或不完整分页都只记录巡检失败并告警，不能累计 not-found 确认次数，更不能停用用户。一次成功查询为 active 会清零此前的缺失计数。

达到门槛后 Controller 生成独立幂等命令：

```text
lark:disable-reconcile:<tenant_key>:<open_id>:<evidence_date>
```

命令携带 `source=employment_reconciliation` 并调用同一个 principal disable 接口。`employment_checks` 保存查询时间、Lark result code、权限健康状态和证据 hash；不保存额外个人信息。该补偿流程与实时离职事件并行，任一路径先成功后，另一路径只会得到已停用 `noop`。

重新入职时：

- 不自动 enable 旧用户。
- 不自动恢复旧 subscription。
- 不自动恢复旧 API key。
- 管理员核实身份后显式启用，并要求轮换 API key。

## Lark 应用配置

建议使用一个企业自建应用承载登录、审批和离职事件。

### Redirect URL

```text
https://ai.x2r.store/integrations/lark/oauth/callback
```

必须在 Lark 应用安全设置中精确登记。

### 权限

最小建议集：

| 能力 | 权限 | 说明 |
| --- | --- | --- |
| 登录 user info | 无额外 API scope | 不读取 email/mobile |
| 审批订阅和实例回查 | `approval:approval` | 单一权限覆盖订阅与读取，但权限本身较宽，应限制应用管理员 |
| 离职事件 | `contact:contact.base:readonly` | 足够取得基本事件和 open_id |

Lark 的审批订阅接口目前要求 `approval:approval` 或 `approval:definition` 之一，均比纯读取权限宽。选 `approval:approval` 可以同时满足实例回查。此权限风险必须在应用管理员清单和变更审计中体现。

### 事件

- 审批实例状态变更：`approval_instance`
- 员工离职：`contact.user.deleted_v3`

同时配置：

- Webhook Request URL
- Verification Token
- Encrypt Key
- 每个已登记 approval code 的一次性 subscribe 调用，包含仍在 draining 的历史定义

应用可用范围只开放给允许使用 AI Gateway 的员工或组织单元。Controller 仍以 `tenant_key` allowlist 做第二层校验。

## 当前本地部署拓扑（尚未生产验收）

当前根 Compose 已在 `lark` profile 中本地接入 `lark-quota-controller`、`:3001` integration listener、`lark-integration` network、policy mount、Controller volume 和按 consumer 隔离的 file-backed secrets，并提供 current/next integration credential window。基础 New API 与常驻 Controller 均不挂 correction credential；`lark-ops` profile 使用独立 correction image target 和临时、无 edge/Traefik/host-port 的 New API endpoint。`--list-pending` 走无网络、无 secret、Controller SQLite 只读挂载的独立 service，不启动 New API endpoint。backup、restore 和 correction runner 共用 host-only `maintenance.session` mutex 与带 `backup/restore/correction/readonly` mode 的容器启动 lock；任意 lock mode 都阻止常驻 New API/Controller 启动，临时写服务只接受 `correction`。readonly runner 使用固定名 one-off container，强删并精确验空后才释放边界。restore 在受控启动前释放容器 lock，但直到 readiness 结束始终持有 session，因而其他 host runner 不能抢占；handoff 内任何非零退出都会重建 restore lock 并停掉部分启动的 writer。无法确认临时 snapshot/restore/correction container 已清理时同时保留 session/lock 并失败。该接入已完成本地渲染、镜像构建、GHCR 发布收据和测试；三个个人 package 当前仍为 private，尚未完成匿名拉取、真实 tenant 配置、生产网络和恢复演练。New API 继续作为完整部署的主入口，Controller 和 Sub2API 都是其内部实现。

网络：

```text
edge (new-api-edge):
  traefik
  new-api
  lark-quota-controller

new-api-data:
  new-api
  new-api-postgres
  new-api-redis

lark-integration (new-api-lark-integration):
  new-api
  lark-quota-controller
```

约束：

- Controller 不加入 `new-api-data`。
- Controller 不获得 New API Postgres 或 Redis 密码。
- `lark-integration` 不发布 host port。
- Controller 通过 `edge` 获得对 Lark API 的出站网络。
- Traefik 只把三个 Lark public path 路由到 Controller。
- `/internal/*` 和 New API `/api/integrations/v1/*` 都不配置公网 router。
- New API `:3001` 不 `publish` host port，也不加入 Traefik service labels；由于进程监听 `0.0.0.0:3001`，它仍可被该容器所在的所有 Docker network 访问，专用 bearer auth 是必需边界。
- 部署验收从 `edge` 和 `new-api-data` 上的非 Controller 容器主动请求 `:3001`，确认无凭证为 `401`、公网没有 route；如需网络级隔离，再增加容器防火墙、Unix socket 或独立 sidecar。

Traefik 路由需要比 New API catch-all 更高优先级：

```text
Host(ai.x2r.store) && PathPrefix(/integrations/lark/)
```

公开路径只包含：

```text
/integrations/lark/oauth/authorize
/integrations/lark/oauth/callback
/integrations/lark/events
```

### 配置和 secret

Controller 至少需要：

```text
LARK_CONTROLLER_MODE
LARK_APP_ID
LARK_APP_SECRET_FILE
LARK_VERIFICATION_TOKEN_FILE
LARK_ENCRYPT_KEY_FILE
LARK_TENANT_KEY_ALLOWLIST
LARK_ACTIVE_POLICY_VERSION
LARK_POLICY_BUNDLE_DIR
LARK_APPROVAL_BINDINGS_FILE
LARK_GRANT_PAYLOAD_KEYRING_FILE
LARK_INTEGRATION_SECRET_FILE
NEW_API_INTERNAL_BASE_URL
LARK_RECONCILIATION_HEALTH_OPEN_ID
LARK_RECONCILIATION_INTERVAL
LARK_APPROVAL_RECONCILIATION_INTERVAL
LARK_APPROVAL_RECONCILIATION_LOOKBACK
NEW_API_BRIDGE_CLIENT_ID
NEW_API_BRIDGE_CLIENT_SECRET_FILE
NEW_API_OAUTH_CALLBACK_ALLOWLIST
LARK_OAUTH_PUBLIC_ENABLED
LARK_OAUTH_RATE_LIMIT_PER_MINUTE
LARK_OAUTH_TRUSTED_PROXY_CIDRS
LARK_PROCESSING_LEASE_TIMEOUT
LARK_PROCESSING_RECOVERY_INTERVAL
CONTROLLER_DATABASE_PATH
```

`LARK_POLICY_BUNDLE_DIR` 和 `LARK_APPROVAL_BINDINGS_FILE` 必须能同时保留 active、draining 和 replay 所需的历史版本，不能只配置两个“当前 approval code”。`LARK_GRANT_PAYLOAD_KEYRING_FILE` 每行保存一个 64 字符小写 hex key，整个文件统一使用 LF 或 CRLF，拒绝混合换行和裸 CR；第一行是新 job 的 primary key，后续行只用于解密轮换前的非终态 job。轮换必须先以“新 key + 全部旧 key”原子替换文件，重启 Controller 并通过 startup gate 后才恢复服务；只有旧 key 不再关联任何非终态 job 后，才能原子安装删去该 key 的文件并再次重启通过同一门禁。`LARK_INTEGRATION_SECRET_FILE` 保存一个不少于 32 字节的 printable、无空白 ASCII bearer token，可带一个 LF 或 CRLF 结尾；仅 active mode 读取。`LARK_RECONCILIATION_HEALTH_OPEN_ID` 同样仅 active mode 必需，必须是当前 tenant 和应用可用范围内稳定且已知 active 的 open_id；扫描前后任一探针不为 present 时，本次扫描只记失败，不提交证据。`LARK_RECONCILIATION_INTERVAL` 仅 active mode 生效，默认 `24h`，只允许 `24h..168h`；失败后至少间隔 15 分钟重试，若 Lark 给出更长 `Retry-After` 则遵守该值，但不超过正常 reconciliation interval。`LARK_PROCESSING_LEASE_TIMEOUT` 和 `LARK_PROCESSING_RECOVERY_INTERVAL` 在 shadow/active 均生效，默认分别为 `5m` 和 `1m`；lease 只允许 `1m..1h`，扫描周期至少 `10s` 且不能超过 lease，两者之和必须小于 readiness queue age。`NEW_API_BRIDGE_CLIENT_SECRET_FILE` 保存一个 32 至 4096 字节的 printable、无空白 ASCII client secret，可带一个 LF 或 CRLF 结尾；shadow/active 均在 startup gate 读取，明文不能放入环境变量、日志或 SQLite。`NEW_API_INTERNAL_BASE_URL` 初始值为 `http://new-api:3001`，`NEW_API_OAUTH_CALLBACK_ALLOWLIST` 初始只允许 `https://ai.x2r.store/oauth/lark`。`LARK_OAUTH_PUBLIC_ENABLED` 只接受 `true/false`，默认 `false`；关闭时 Controller 不注册公开 authorize/callback handler，Traefik exact path 稳定返回 `404`，内部 token/userinfo handler 不受影响。OAuth authorize、callback、token 和 userinfo 使用四个独立的 per-client 一分钟固定窗口，`LARK_OAUTH_RATE_LIMIT_PER_MINUTE` 默认 30；authorize 另有每个 resolved client 每 5 分钟最多签发 20 个 state、全局每分钟最多签发 500 个 state 的固定硬限制。IPv4 client key 使用单个地址，IPv6 client key 统一按 masked `/64` 归组。被后续门禁拒绝或 state 持久化失败时必须回滚已预留的签发计数和空 map entry；`429 Retry-After` 必须反映实际拒绝请求的一分钟或五分钟窗口。只有直接对端位于显式 `LARK_OAUTH_TRUSTED_PROXY_CIDRS` 时才解析 `X-Forwarded-For`，该配置必须只包含实际 Controller 前置代理网段。

approval reconciliation 在 shadow/active 都运行。`LARK_APPROVAL_RECONCILIATION_INTERVAL` 默认 `15m`、只允许 `1m..24h`；`LARK_APPROVAL_RECONCILIATION_LOOKBACK` 默认 `72h`、只允许 `1h..720h`。失败后至少等待 5 分钟，较长的 `Retry-After` 最多遵守 24 小时。它与 active-only employment reconciliation 共用 100ms process-wide Lark request pacer。

日志必须对以下字段脱敏：

- `client_secret`
- `user_access_token`
- opaque OAuth handle
- approval form 中的自由文本
- webhook 原始加密 payload

New API production debug 必须保持关闭。

## 备份与恢复

现有 New API Postgres 备份会包含 fork 新增的 principal、policy、grant、assignment 和 outbox 表，但 Controller SQLite 是另一个事务域。把两个文件放进同一压缩包不等于一致截点。当前本地实现选择明确停机的 offline quiesce barrier，不声称无停机或 SQLite online backup：

1. backup runner 先原子取得 host-only `maintenance.session`，再取得 `maintenance.lock/mode=backup`。session 排斥其他 host runner；lock 存在时常驻 New API/Controller entrypoint 一律拒绝启动，correction 写服务因 mode 不匹配也拒绝启动。
2. runner 记录原运行集合，按 Traefik、Controller、New API、Sub2API 顺序停止入口和全部应用 writer。已经停止的服务保持停止；Controller webhook 连接失败由 Lark 重试。此后没有 Controller -> New API 在途命令、Postgres producer 或 Redis writer。
3. Lark enabled 配置必须与 `new-api-lark-controller-data` 同时存在，并要求完整 `approval-bindings.json` 与至少一个 versioned policy；disabled 配置必须没有 Controller volume。前者在 writer 停止后把整个 volume（`controller.sqlite` 以及存在时的 WAL/SHM）归档，后者写显式 absent marker。
4. barrier 保持期间，再依次生成两个 Postgres custom dump、两个 Redis 持久化副本和 deployment runtime/policy 归档。任一步失败都不发布最终包。
5. `backup-manifest.json` 保存 format version、package ID、barrier ID、UTC 时间、原先停止的服务集合、Lark enabled/absent receipt，以及所有 payload 的 size/SHA-256；`SHA256SUMS` 必须精确覆盖 manifest 和全部 payload。只有确认命名 snapshot container 不存在后才发布 `.tgz`、解除锁并逆序恢复原运行集合。长期 Lark secret 不进入包。

恢复先在临时目录验证外层和 Controller 内层 archive path/type、根目录唯一 regular `controller.sqlite`、checksum 完整覆盖、manifest inventory/digest、listener/Controller/policy 配对和 Compose render；这些步骤不改变部署。随后取得 session 与 `mode=restore` lock 并执行 `compose down`。enabled 包删除旧 Controller volume、恢复同包 volume 与 policy，强制 `LARK_CONTROLLER_MODE=shadow`、`LARK_OAUTH_PUBLIC_ENABLED=false`；absent 或兼容的 pre-v2 disabled 包删除旧 Controller volume 和旧 policy，保留 host secrets/ops。Postgres/Redis 和 runtime 全部恢复完成并确认 restore container 不存在后，释放容器 lock、在 session 保护下启动服务；readiness 失败则重建 restore lock、再次停止并核实 writer。`restore-new-api.sh` 同时拒绝 enabled/Lark-artifact 源包和 listener、Controller/correction、Controller volume 任一存在的目标，不能把不同 package 的 Postgres 与 SQLite 混合恢复。破坏性阶段失败会保留 session、restore lock 和停机状态；确认恢复前不得人工删锁。

恢复后必须执行 reconciliation：

- 检查所有 `processing` inbox/job。
- 校验 backup barrier receipt、policy/approval manifest hash 和两个数据库的 schema version。
- 重放未确认任务。
- 对比 applied grants 与 New API result。
- drain `integration_outbox` 并确认 deny fence 没有遗留。
- 运行 New API fork 的 managed projection reconciliation，检查每个 active assignment 是否恰有一份 active managed subscription。
- 抽样验证 disabled principal 的 browser session/API key 被拒绝，并验证历史 external ID replay 返回原结果。

由于权益接口幂等，安全重放应返回 `replayed` 或补全未提交事务，不会重复加额。

## 重试与故障处理

Controller job 状态：

```text
held_shadow --explicit active release--> pending
pending -> processing -> succeeded
                    -> retry_wait -> processing
                    -> dead_letter
                    -> reversal_pending
reversal_pending --one-shot correction resolution--> reversal_resolved
```

`held_shadow` 的保存时间不计入 `principal_not_ready` 的 24 小时窗口；窗口从显式 release
写入的 `activated_at` 开始。仅 active-mode startup/runtime gate 可以 release，shadow event
worker 和普通 claimant 都不能领取 held job。

建议重试：

- 指数退避：`5s, 15s, 1m, 5m, 15m, 1h`
- `principal_not_ready` 最长重试 24 小时
- Lark `429` 或 `5xx` 遵守 `Retry-After`，否则指数退避加 jitter
- New API `503` 可重试
- `400/409/422` 不自动重试并告警

OAuth 登录接口与 webhook 不应共用一个全局 rate-limit bucket。尤其不能让 Lark webhook 突发或单个员工反复登录把所有 New API 登录请求打成 `429`。

## 对账与可观测性

### 日志关联键

每条日志至少带一个：

- `oauth_flow_id`
- `event_key`
- `instance_code`
- `external_id`
- `subject_hash`
- `new_api_user_id`

日志不写完整 subject、token 或审批自由文本。

### 指标

```text
lark_webhook_received_total{event_type}
lark_webhook_duplicate_total{event_type}
lark_event_processing_seconds{event_type}
lark_approval_fetch_total{result}
lark_controller_inbox_events{state}
lark_controller_jobs{state}
lark_controller_dead_letter_total{reason}
lark_controller_oldest_active_job_age_seconds
lark_controller_oldest_ready_job_age_seconds
lark_controller_ready
lark_policy_validation_failure_total
entitlement_grant_total{type,status}
entitlement_grant_retry_total{reason}
entitlement_dead_letter_total{reason}
managed_subscription_drift_total{kind}
managed_policy_block_total{path,reason}
integration_outbox_backlog
integration_outbox_delivery_total{event_type,result}
oauth_login_total{result}
oauth_handle_reuse_total
principal_disable_total{result}
principal_disable_retry_total{reason}
principal_disable_dead_letter_total{reason}
lark_principal_disable_jobs{state}
employment_reconciliation_total{result}
approval_reconciliation_total{result}
approval_reconciliation_cursor_initialized{approval_code}
approval_reconciliation_cursor_lag_seconds{approval_code}
lark_controller_processing_recovered_total{queue}
auth_deny_fence_age_seconds
```

### 告警

- dead-letter 数量大于 0。
- `external_id_payload_mismatch` 任意一次。
- wallet grant 在 10 分钟内仍未 applied。
- approval 回查连续失败超过 15 分钟。
- active assignment 没有 subscription 或出现多个 subscription。
- 离职事件停用失败超过 5 分钟。
- 每日在职巡检未完整成功，或连续两天没有成功完成全量 active principal 扫描。
- approval reconciliation 任一时间窗失败，或 active/draining binding 的成功游标长期不前进。
- `integration_outbox` 超过 5 分钟未投递，或 auth deny fence 超过配置 TTL 仍未确认清除。
- 支付成功订单进入 `blocked_managed_policy`，需要人工退款。
- OAuth 失败率 5 分钟窗口显著上升。

### 每日 reconciliation

对账分属两个模块，避免为巡检扩大 Controller 接口：

- Controller 检查 Lark inbox、jobs、approval decision 与已发送 external ID，重试未完成的业务命令，并按“离职事件丢失补偿”分批核验所有 active principal 的在职状态。
- New API fork 在本地数据库内检查 principal、policy binding、assignment、subscription projection、billing preference、purchase gate 和 outbox。

New API 检查：

- assignment 的 `policy_version + level_code` 与历史 managed plan binding 对应。
- 每个 active principal 恰好一个 active assignment 和一个 active managed subscription，且 subscription ID 与 assignment 一致。
- active managed user 没有其他 active subscription，也没有绕过策略完成的 subscription order。
- 托管用户 billing preference 为 `subscription_first`。
- disabled principal 没有 active managed subscription。
- 没有用户通过公开购买路径获得 `managed_only` plan。
- outbox 没有长期积压，deny fence 与已提交 auth version 一致。

自动修复必须写入 New API integration audit，并使用新的、可审计的 reconciliation ID；不能静默改表。

## 安全要求

- OAuth `state` 必须校验并单次消费。
- `redirect_uri` 使用精确 allowlist，不允许前缀匹配或任意回跳。
- Lark webhook 必须验签、校验 Verification Token，并在配置加密时先解密再解析。
- 所有 Lark event 必须校验 `app_id` 和 `tenant_key`。
- 表单只接受固定 `custom_id`、locale、schema fingerprint 和 exact display-text mapping；未知文本与未知状态一律 fail closed。
- Controller 不能持有 New API 管理员 PAT 和数据库密码。
- New API integration credential 只能调用两个幂等写接口和一个最小只读 principal 枚举接口；独立 correction credential 只能调用 preview/apply 两个纠正接口。
- `:3001` 在 New API 所有容器网络可达，不能被当作认证边界；必须不 publish、不路由并强制 bearer auth。
- 数据库事务和唯一约束是幂等性的最终保障，不能只依赖进程内锁或 Redis 锁。
- active managed principal 的 identity、billing preference 和 subscription creation policy 均为服务端锁，UI 隐藏不是安全控制。
- wallet 用 checked `int64` 算术和业务上限，任何溢出或超上限请求在写入前拒绝。
- 审批自由文本属于敏感业务数据，默认不进入长期日志。
- OAuth 和 webhook endpoint 需要独立限流，不得复用密码登录的失败计数器。
- 管理员 break-glass 登录与 Lark 应用故障演练必须在正式切换前完成。

## 测试计划

### Controller 单元测试

- New API 默认 `openid profile email` 不会被转发到 Lark。
- OAuth state 过期、重复、篡改和 redirect mismatch 被拒绝。
- Lark token JSON request 符合 v3 协议。
- opaque code 和 access handle 单次消费。
- userinfo 不包含 email，subject 使用 tenant + open_id。
- 审批表单按 `custom_id + locale + schema fingerprint` 定位 manifest，并把 `radioV2` 显示文本 exact match 到 code。
- 未知/重复显示文本、locale 漂移、schema 漂移、字段顺序变化和模糊匹配全部 fail closed；不会从文本计算金额。
- 新 active policy 发布后，截止时间前创建的旧 approval 实例仍按旧 policy version 生成命令；截止时间后的旧实例拒绝。
- policy bundle、数据库 catalog、plan snapshot 或 canonicalization 测试向量任一 hash 不一致时，新版本不能激活。
- 未知 package/level 和当前定义不允许的 code 拒绝。
- webhook 验签、解密和 URL challenge。
- v1 `uuid` 与 v2 `event_id` 去重。
- APPROVED 事件总是回查实例。
- `PENDING/REJECTED/CANCELED/DELETED/OVERTIME_CLOSE/OVERTIME_RECOVER/REVERTED` 和未知状态按状态矩阵处理；使用仓库内按真实 Lark 协议结构脱敏固化的 v1/v2 event 与 instance fixture。
- REVERTED 使用当前 event/API 可用的显式关联码或已登记原 `instance_code` 精确找到原 grant，歧义时进入 `reversal_pending`。
- correction CLI 默认 preview、脱敏输出 reversal/current state，只有 `--apply` 写入；subject hash 或类型错配在读取凭据或调用 New API 前拒绝；`--list-pending` 不迁移或恢复运行期队列状态。
- correction `--apply` 在远端写入前持久化 original-level attempt；fresh 确定无提交失败转 `abandoned` 并允许新 external ID，response loss 保持 `active` 并使用相同 external ID/payload 重放，远端 ledger 冲突转 `remote_conflict` 并阻止替代请求；New API 成功而 Controller resolution 失败时可补写，Controller 已保存 resolution 时不再次调用 New API。
- Store 只允许 correction type 与 original grant type 一致，并将 inbox、普通 job 和 fenced grant job 原子推进到 `reversal_resolved`。
- 429、5xx 和 `Retry-After` 重试。
- 每日在职巡检只有权威终态或两次健康全量扫描的 not-found 才停用；权限、scope、分页、`429`、`5xx` 和应用范围错误只告警。

### New API fork 单元与数据库测试

- 同 external ID、同 payload 并发 100 次只增加一次 wallet。
- 同 external ID、不同 payload 返回 `409`。
- 模拟提交成功但响应丢失，重试不重复加额。
- correction 在原 grant ledger 存在时拒绝 subject/type/source 不一致；plan drift 返回稳定不可重试的 `409`。
- subscription correction 即使已到普通 lazy-reset 时间也保留 amount-used 和 reset timestamps；same-level no-op 不产生 setting/subscription/assignment/outbox 隐藏写入，也不因 policy version 不同而隐式迁移 assignment。
- 新 principal 尚未绑定返回 `principal_not_ready`。
- Lark 新用户在全局 `QuotaForNewUser/QuotaForInvitee` 非零且带 affiliate code 时，wallet 仍为 0，邀请双方都不发奖励。
- user、OAuth binding 和 integration principal 任一步失败时整笔创建回滚；相同 subject 并发登录只映射到一个 user。
- 离职 disable 先于或并发于首次 OAuth 创建时写入 tombstone；两个提交顺序下最终都不能留下 active principal/user。
- active managed principal 的自助解绑和普通 admin 解绑都拒绝；root/admin/break-glass/disabled user 绑定拒绝。
- OAuth binding 缺失但 integration principal 存在时，登录不创建第二个 user，离职仍停用原 user。
- 旧 external ID replay 在当前 policy 校验之前返回旧 result；同 ID 不同 hash 仍为 `409`。
- `source=base_login` 不能申请 wallet 或高等级，`source=lark_approval` 缺少/伪造 approval evidence 会被拒绝，Controller credential 不能伪装 migration/correction。
- active principal 枚举的 cursor 篡改、越权 filter 和超大 limit 被拒绝；多页扫描固定 snapshot boundary，只有完整扫描才产生 not-found 证据。
- wallet 在业务上限边界成功，超过上限、负数和 `int64` 溢出均原子拒绝；数据库和 Redis 值一致。
- basic assignment 重放不创建第二份 subscription。
- 已有 active non-managed subscription 时不自动叠加 basic，也不取消原订阅。
- `basic -> pro` 原地更新同一 `user_subscriptions.id`，不创建或取消第二行。
- subscription 预扣后升级，再 settle 或 refund，仍更新同一 subscription ID 且用量正确。
- `next_reset_time <= now` 时先在锁内 lazy reset，再原地升级；周期边界只推进一次。
- `pro -> power` 保留当月 used、start 和 reset time。
- 延迟 `plus` 不把 `power` 降级。
- 同等级审批为 no-op。
- `remaining=1, request=2` 时整次请求走 wallet、subscription remaining 仍为 1，settle/refund 回到原资金源。
- active managed user 修改 billing preference、余额购买、各支付 provider 下单、普通 admin bind 均返回策略锁错误。
- 绑定前已创建的 pending subscription order 在支付回调时不创建 subscription，并进入 `blocked_managed_policy` 退款流程。
- 非托管用户也不能从公开、支付或普通 admin 路径取得 `managed_only` plan。
- 事务失败不会留下 grant applied 但 quota 未加，或 quota 已加但 grant 缺失。
- disable 在一个 PostgreSQL 事务更新 principal/user/subscription/assignment/auth version/session/outbox；任一步失败整体回滚。
- disabled user 的会话和 API key 在 Redis 旧缓存仍存在时也被 deny fence 立即拒绝。
- disable 前已存在的 subscription preconsume 可结算/退款但不会重新激活订阅；fence 后不能创建新预扣。
- outbox/cache publish 失败可观察、可重试且 reconciliation 可修复；成功前不提前移除 deny fence。

### 端到端测试

1. 新员工 Lark 登录，创建用户并自动获得 basic。
2. 老用户先登录再绑定 Lark，不创建重复账户。
3. 未绑定老用户不会因为相同 email 被合并。
4. 一次性审批通过后 wallet 增加，subscription remaining 不变。
5. 同一审批事件重放十次只加一次。
6. basic 可完整覆盖请求时整次从 subscription 扣；不能完整覆盖时整次从 wallet 扣，两个资金源不拆分。
7. pro 审批通过后同一 subscription ID 的月度总额提升，wallet 不变。
8. 升级前已使用额度、预扣结算/退款和周期边界在升级后正确保持。
9. 新政策发布后处理旧 approval 实例，并重放旧 grant，均使用旧版本且不重复发放。
10. 低等级乱序事件不降级。
11. wallet 审批撤销进入 manual reversal，不自动全额扣回。
12. level 审批撤销仅在无后续变更时恢复 prior level。
13. 托管员工不能改 billing preference，也不能通过余额、支付或普通 admin 获得其他 subscription。
14. 离职事件后浏览器会话和既有 API key 都被拒绝，基础 grant 重放不会恢复账户。
15. 人工丢弃离职 webhook 后，每日在职巡检达到证据门槛并通过同一 disable 接口停用；权限故障不会误停。
16. Controller 重启后未完成 inbox/job 会继续处理。
17. 带 barrier receipt 的联合备份恢复后，outbox 可 drain，事件/grant 重放不重复发放。

### 生产试点验收

- 5 名测试员工完成 Lark 登录。
- 至少覆盖 1 个已有用户绑定和 1 个新用户创建。
- 每种审批定义完成 approve、reject、revert 各一次演练。
- 人工重复投递同一 webhook，余额不重复变化。
- 模拟 New API 响应丢失，重试后仍只入账一次。
- 执行 1 次离职测试账号停用演练。
- 执行 1 次丢失离职事件后的 reconciliation 停用演练，以及 1 次 scope 故障不误停演练。
- 从其他容器网络和公网验证 `:3001` 的未授权/不可路由边界。
- 完成 1 次 Controller + New API 联合备份恢复演练。
- 审计能从 Lark instance code 追到 New API user、grant 和最终结果。

## 实施工作包

### WP1：政策和 Lark 配置

- 确认四个订阅等级的月额度。
- 确认四个一次性包及审批链。
- 发布不可变 policy bundle，包含 versioned level、wallet package、rank、quota、locale 和 catalog hash。
- 为首个 policy version 创建两个 Lark 审批定义，固定控件 `custom_id` 和唯一显示文本；后续版本创建新定义，不覆盖旧定义。
- 建立企业自建应用、可用范围、redirect URL、权限和事件。
- 记录每个 `approval_code + schema_fingerprint + locale -> policy_version` binding 和 exact definition manifest，并定义旧审批 draining/retirement 流程。

### WP2：New API fork

- 将 `users.quota/used_quota` 及完整读写链迁移到 checked `bigint/int64`。
- 增加 integration principal/tombstone、policy version、wallet package、approval binding、grant、assignment 和 outbox migration。
- 增加复合键 managed level binding、不可变 plan 约束和双向 subscription purchase gate。
- 增加独立 `:3001` listener，确保不 publish、不路由；明确它仍可被 New API 所在的其他容器网络访问。
- 增加专用 integration auth middleware。
- 实现四个 POST 接口和一个最小、分页、只读的 active principal 枚举接口；两个 correction 接口使用独立可选凭证。
- 实现原子 wallet grant。
- 实现 managed subscription 原地升级、lazy reset、用量继承和 preconsume settle/refund 兼容。
- 锁定 managed identity 和 billing preference；在余额购买、支付下单/回调及普通 admin bind 的共享事务层执行门禁。
- 实现 disable 的 deny fence、单事务状态/auth/session 更新和 post-commit outbox。
- 补齐并发、事务和故障窗口测试。

当前本地实现收据：fork 从上游 `v0.13.2` 的 peeled commit
`bee339d279ccecbf8c8a89e14ddbbd902f78bd5d` 开始，bigint、wallet grant、managed
subscription、managed OAuth、principal disable 和 operator correction 六个 tracer slice 均已实现，常驻 wire contract
固定于 `f2ef0d95`，correction boundary 固定于 `f1be0d7a`，确定性 Bun dependency source
构建修复固定于 `dbfcf0c7`。`go test ./service ./router ./middleware ./model ./controller`
已通过；`controller/token_test.go` 和 `model/integration_migration_test.go` 的迁移门禁也已在
SQLite、MySQL 8.0 和 PostgreSQL 16 上通过。`dbfcf0c7` 源码已完成本地完整镜像构建，
`linux/arm64` local image ID 为
`sha256:68f25465624a0e736a2bfacf58adad608b0e2777f4f89be0272a174bf952e973`。
仓库全量 `go test ./...` 仍重现 4 个非 Lark 基线失败：Claude file-content 转换 3 个，
timing-sensitive stream-scanner 失败 1 个；后者在不同运行中可能表现为 assertion failure
或 ticker panic。对应 `relay/channel/claude` 和 `relay/helper` 路径相对上述 peeled upstream
commit 无 diff。这些是本地验证收据，不是 multi-arch registry manifest digest，也不代表
镜像已发布或生产已验收。

### WP3：Controller

- 实现 Lark OAuth bridge 和 opaque handle store。
- 实现 v1/v2 webhook 验证、解密和 inbox。
- 实现 versioned approval fetch、固定 locale/schema parser、exact radio text mapping 和历史目录映射。
- 实现 job retry、dead-letter、reversal pending。
- 实现 New API adapter、metrics、health 和 audit。
- 实现 Controller 侧每日 inbox/job/approval reconciliation 和 active principal 在职状态核验，权限故障 fail open 并告警。

当前本地实现边界（shadow/active grant，尚未部署）：OAuth state、login code 和 access handle
已使用三个 SQLite 表持久化；Controller 生成 256-bit 随机 credential，数据库只保存 SHA-256
digest，state 默认五分钟、code/handle 默认 60 秒，并通过带 expiry/consumed 条件的原子更新实现
单次消费。消费使用 `DELETE ... RETURNING` 原子取出并删除记录；过期清理最多每分钟执行一次，
且只使用三个 `expires_at` 索引。state、login code 和 access handle 的硬上限分别为
10,000、5,000 和 5,000 行，state 洪泛不能挤占下游 code/handle 容量；这些记录不是审计账本，
不能无限留存。login code 到 access
handle 的交换在同一事务完成；subject 固定为
`tenant_key:open_id`，username 按 75-bit base32 规则确定性生成。v1/v2 webhook 验证与 durable inbox、
authoritative Approval v4 fetch、versioned policy/manifest 解析、固定 locale 与 exact
display-text mapping、有限重试/dead-letter/reversal pending、重启恢复、SQLite audit
snapshot，以及 `/healthz`、`/readyz`、`/metrics` 已实现。Controller 现在还会生成精确的
New API grant canonical request，并在同一 SQLite 事务保存 sanitized shadow receipt 与
AES-256-GCM 密封的 canonical request；密封记录使用 `held_shadow` 状态，现有 claimant
不会领取，也不计入 ready-queue age。同 payload 的 external ID 重放复用首条密封记录并记为
`shadow_replayed`，不同 payload 进入 `external_id_payload_mismatch` dead-letter。密钥通过
`LARK_GRANT_PAYLOAD_KEYRING_FILE` 读取严格的多行 keyring；第一行只负责新 job 密封，后续行
解密轮换前记录，loader 会清零原始文件 buffer 和临时 decoded key。启动时会在 worker 前验证
每个非终态 grant job 的 key ID 都存在，缺失时 fail closed；succeeded/dead-letter 历史记录
不阻止旧 key 退役。active startup 会在配置、credential/client、SQLite、webhook server 和
listen socket 均准备好后释放历史 held job；active grant runtime 也会在每轮 claim 前释放新产生的
held job。审批 job 不受 active policy 切换影响；base job 只有其 policy version 等于当前 active version
才会被释放；任何历史非终态 base job 都会让 policy snapshot/startup gate fail closed，等待 drain 或
显式 migration 处理。旧 policy 的非终态审批 grant 同样会阻止 retirement。
已接入的 `GrantExecutor` 可解封请求、调用既有 adapter、
保存 sanitized result、处理 response-loss replay，并按登记 code retry/dead-letter；
`principal_not_ready` 的 24 小时从 `activated_at` 计算。active result/retry/dead-letter metrics
及从 `activated_at` 开始的 released-job queue age 已实现。shadow mode 不读取 New API URL 或
credential、不构造 executor，也不 release。
Controller compatibility receipt
`166bbeb` 与 New API Gin router receipt `f2ef0d95` 已共同固定 nested error envelope、subscription
result 和分页 active Lark principal wire contract；active mode 现在读取专用 integration
credential、构造 client/executor 并执行幂等 entitlement write，shadow mode 保持零 New API
调用。principals contract 不返回 New API user ID、wallet、token 或 subscription 明细。
Lark OAuth v3 adapter 已按 JSON contract 交换 authorization code，并立即以 user bearer token
读取 userinfo；返回身份仅包含 `tenant_key:open_id` subject、确定性 username 和按 Unicode code
point 截断至 20 字符的 display name。token/userinfo 响应均限制为 64 KiB，错误只暴露固定 reason，
不返回、不持久化也不记录 access/refresh token 或上游错误描述。adapter 只接受已登记的固定
Controller callback 和 HTTPS upstream origin（测试可使用 loopback HTTP），并拒绝所有 redirect，
避免 App Secret、authorization code 或 bearer token 被重放到其他 endpoint。OAuth
公开 authorize/callback handlers 已接入启动路径：只接受固定 bridge client ID 和精确的
`https://ai.x2r.store/oauth/lark` callback，不转发 scope、affiliate code 或未知参数；state 在
成功、拒绝和失败路径上均先单次消费，成功时只返回 60 秒 opaque login code。两个 endpoint
只允许 exact `GET`，`HEAD` 在任何限流或持久化副作用前返回 `405`。Lark authorize error 中
`access_denied` 精确映射为用户拒绝，`server_error/temporarily_unavailable` 映射为脱敏的可重试
错误，未知值 fail closed 为 `server_error`；Lark API `408/429/5xx`（包括 oversized/malformed
body）、transport 和 timeout 同样映射为 `temporarily_unavailable`，其他上游终态失败映射为
`server_error`。两个 public endpoint 使用独立 per-client 限流；IPv6 client 按 masked `/64`
归组，authorize state 签发另受每 client 每 5 分钟 20 个和全局每分钟 500 个硬限制。callback
使用 10 秒总 context，并只为自身扩展 response write deadline，不改变 webhook 的 3 秒 ACK 预算。
内部 token handler 已按 New API Generic OAuth 的 params auth contract 校验固定 client、grant type 和
redirect URI，再原子地把 60 秒 login code 换成第二个 60 秒 opaque Bearer handle；userinfo handler
从唯一 active policy 解析 `basic`，并在一个 SQLite 事务中消费该 handle、创建或复用
`lark:base:<tenant_key>:<open_id>:<policy_version>` 账本、保存 AES-256-GCM 密封的 `held_shadow`
job、写入独立 base audit，提交后只返回 `sub/username/name`，不返回 email。事务失败保留 handle；稳定
replay 比较 request/subject/policy/catalog/level/quota 元数据并复用首条密封 payload，漂移 fail closed。
base job 与审批 job 共用 active release/executor/retry/dead-letter runtime，结果和失败审计汇总到同一组
bounded metrics，但不伪造 webhook event。两个内部 endpoint exact method、独立
per-client 限流、稳定错误和 `no-store` 响应均已实现，bridge client secret 只从必需的 secret file
读取。`contact.user.deleted_v3` 已按 v2 `event_id` 去重；普通 inbox payload 只保存 subject hash，
完整 `contact_event` disable request 使用同一 AES-256-GCM keyring 做带 domain separation 的密封，
并在 ACK 前与独立 `principal_disable_jobs`/audit ledger 原子提交为 `held_shadow`。重复事件保留首条
key ID、nonce 和 ciphertext。shadow mode 不 release；active startup/runtime 校验所有非终态 grant
和 disable job key 后显式 release，使用同一 external ID 处理 response-loss replay、有限重试、
dead-letter、重启恢复及脱敏结果审计。disable ledger 的 `event_key` 是可选关联，使后续
`employment_reconciliation` 能复用执行器而无需伪造 webhook。active mode 现在会立即启动一次
每日在职 reconciliation，并按 cursor 分页枚举 New API 中所有 active Lark principal；扫描前后
均验证已知 active open_id。`is_resigned/is_exited` 在健康完整扫描提交时生成 eventless
`held_shadow` disable job；`41012` 只有在两次相隔至少 24 小时的健康完整扫描后才生成 job，present
会清除旧 missing evidence。间隔使用每个 Lark 查询实际完成时间，不使用整轮扫描开始时间，避免长扫描
或跨日重启把短间隔误算为 24 小时。所有 Lark 查询串行，request start 至少相隔 100ms。
`41050`、`429`、`5xx`、token/scope/permission、单用户查询失败、分页
不完整、重复 cursor/subject 或超过页数上限都只追加 bounded failure audit，partial checks 统一标记
`permission_healthy=false`，不会推进 missing evidence；其中已确认的 present 仍会清除旧 missing evidence，
防止下一次 not-found 被误判为连续缺失。失败后至少 15 分钟重试，并遵守更长但不超过正常周期的
Lark `Retry-After`；同一 UTC evidence date
成功后幂等跳过，之前的失败 audit 仍保留。Controller 现在也在 shadow/active 两种模式运行
processing lease recovery：默认每分钟扫描一次，将持续 `processing` 超过 5 分钟的 approval、grant
和 disable claim 原子地重新排队并保留 attempts；approval completion/retry 使用 attempts fencing，
迟到 worker 的整个事务回滚，grant/disable 则沿用既有 attempts fencing 和幂等 external ID。
recovery 不释放 `held_shadow`，不提前处理 future `retry_wait`，也不改终态；审计只记录 bounded
queue/count。配置门禁要求 lease 加扫描周期小于 readiness queue age，避免恢复窗口晚于默认告警。
事件驱动 approval reversal 也已实现：inbox 持久化 `reverted_instance_code`；旧库启动迁移会从
normalized `payload_json` 事务性回填非空关联码并重算 durable payload hash，保证待处理旧事件和合法
重投仍使用相同显式 target。处理时优先使用该
显式关联码，否则只回退到 event 自身的精确 `instance_code`；Controller 回查目标实例并要求
`approval_code`、target instance code 完全匹配且权威 `reverted=true`。原 grant 只能由同 tenant、
同 approval/instance 的 `shadow_authority_verified` decision 与 command shadow 联表解析，legacy
unresolved evidence 不合格；多个 source event 指向同一个 distinct external ID 可接受，零个或多于
一个 external ID 均进入人工待处理。`approval_reversals` 仅保存 normalized code、权威状态、原
external ID、grant type/quota/policy/business 摘要和 bounded result/reason，不保存 open_id 或表单
原文。若原 grant job 仍是 `held_shadow/pending/processing/retry_wait`，同一 SQLite 事务把它改为
`reversal_pending` 并保留 attempts，晚到 worker completion 因 status/attempt fence 失败；无论原
grant 是否可能已由 New API 应用，都不会自动扣钱包或降订阅。终态或耗尽的 reversal fetch 失败
同样进入 manual pending，不混入普通 approval dead-letter。Prometheus 通过
`lark_approval_reversal_total{result}` 暴露 bounded 结果，并通过
`lark_approval_reversal_pending{reason}` 暴露当前待人工处理数；仓库内
`lark-controller/monitoring/lark-controller-alerts.yml` 提供 `LarkApprovalReversalPending` 告警规则。
生产环境仍必须显式加载该 rule 并配置 Alertmanager operator route，当前没有部署这部分监控配置。
周期性 approval reconciliation 也已实现：worker 从 policy snapshot 派生所有非 retired binding，
active 扫到本轮 UTC 时间，draining 只扫到 `accept_instance_started_before`；每个 `approval_code`
持久化独立 monotonic cursor，首次按配置 lookback 回看，续扫回退 10 分钟 overlap，并把 Lark 的
实例创建时间范围拆成不超过 10 小时的窗口。每页 instance code 都权威回查，仅对 `APPROVED` 或
`reverted=true` 生成带 `origin=approval_reconciliation` 的确定性内部 inbox event；它不计入
`lark_webhook_*`。已有 validated grant 在 policy 未 retired 时持续作为精确 recheck target，因此
webhook 丢失后的晚撤销仍进入同一 reversal deny fence。分页 token/code 重复、authority identity
不匹配、响应缺字段、`429/5xx` 或任一窗口失败都不推进该窗口 cursor，只追加 bounded audit；
对象级失败不会阻塞后续 binding 或 grant recheck，tenant-wide rate limit 则结束本轮以遵守
`Retry-After`。`approval_reconciliation_total{result}`、per-binding cursor target lag 以及
`LarkApprovalReconciliationFailed`/`LarkApprovalReconciliationCursorStalled` 暴露失败和长期停滞。approval 与
employment reconciliation 共用 100ms process-wide pacer，避免两类定时任务叠加超过 10 QPS。
管理员 correction command 也已在本地实现：独立 `CorrectionClient` 和 one-shot
`cmd/lark-correction` 默认 preview，`--list-pending` 通过不迁移、不恢复 processing 的只读 Store 枚举待处理 reversal，只有显式
`--apply` 且 original-level receipt 不存在时才调用 New API。远端调用前，SQLite original-level attempt 已不可变绑定 correction external ID、canonical request hash、type、subject hash 和审计字段；fresh 确定无提交失败转 `abandoned` 后释放 original，timeout/response loss 保持 `active` 供 exact replay，远端 ledger 冲突转 `remote_conflict` 并继续 fail closed。异请求无法跨过非 abandoned attempt 的本地 fence。SQLite 组级 receipt 保存 correction external ID、canonical request hash、
operator/reason/change ticket、response status、sanitized result 和 resolved timestamp，并把 inbox、
同 original 的全部 pending reversal、普通 job 与 fenced grant job 原子推进到 `reversal_resolved` 并核验预期行数。晚到 reversal 复用并 attach 同一 receipt；同 resolution 可重放，任何 payload
漂移冲突；CLI 和 Store 均要求 correction type 与 original grant type 一致，并把输入 subject 的 SHA-256 与原 command shadow 绑定；New API 在原 grant ledger 存在时再次验证主体和类型。New API response loss
使用相同 external ID/payload 重放；New API 的 original-grant unique fence 阻止并发不同 correction 二次生效。Controller 已保存 resolution 时 CLI 直接返回或 attach 本地收据，不再次调用 New API。实现和运行说明见 `docs/runbooks/lark-entitlement-correction.md`。根 Compose 的 `lark-ops` profile 将 correction CLI 与短期 credential 隔离到独立 image target；常驻 New API/Controller 不持有该 credential。生产验证仍未实现。

当前 Approval fetch 对 HTTP `408/429/5xx`、Lark business code `99991400`、timeout
和 transport failure 使用 `5s, 15s, 1m, 5m, 15m, 1h` 加 deterministic jitter 的
有限退避；合法 `Retry-After` 优先但上限为 24 小时。第七次失败进入 dead-letter。
其他 `4xx`、token rejection、invalid response 和 unclassified error fail closed，持久化
内容只使用固定 reason，不保存 Lark 原始错误文本。readiness 只用已经到期可执行的最老
job 年龄，未来的 `retry_wait` 不会被误判为卡死。

### WP4：部署和运维

- [x] 修改根目录唯一的 `docker-compose.yml`，不引入 overlay 或第二套部署入口。
- [x] 增加显式命名为 `new-api-lark-integration` 的 `lark-integration` network、分离的 events/OAuth Traefik exact-path router 和 Controller volume。
- [x] 发布并固定 New API fork、Controller 和 correction CLI image digest；三个 image 已推送 GHCR，registry index 含 `linux/amd64`、`linux/arm64` 和对应 provenance/SBOM attestation。
- [ ] 将三个个人 GHCR package 从 `private` 切换为 `public`，并从无凭证环境完成匿名拉取验证。GitHub 当前没有个人 package visibility 的 REST/GraphQL 接口，只能在每个 package 的 `Package settings → Change visibility → Public` 中操作；公开后不可逆。
- [x] 扩展 `.env.example` 和按 `shared/controller/new-api` consumer 隔离的 file-backed secret mount，但不提交任何 secret；Controller/correction image target 已分离，integration current/next rotation window 与 correction 三方独立性检查已接入。
- [x] 扩展带 offline quiesce barrier、v2 manifest/receipt、精确 checksum coverage 和 Lark enabled/absent 同包校验的 backup/restore 脚本；restore 强制 shadow/OAuth-off，New API-only restore 拒绝拆分 enabled 包。生产恢复与 reconciliation 演练仍待执行。
- [x] 本地 verify 已检查公网 integration path 为 `404`、New API 容器内无凭证访问 `:3001` 为 `401`、Controller `readyz` 和 OAuth 灰度门禁。
- [ ] 在获准的生产维护窗口从 `edge`、`new-api-data` 和 `lark-integration` 各自独立 probe，完成公网与所有相邻网络验收。
- [x] 增加 Compose 灰度、secret/policy/rotation 配置、host-side correction maintenance lock 和回滚 runbook；Lark 后台实租户配置、dead-letter 和 reversal 演练仍待执行。

当前本地 `linux/arm64` 构建收据如下；它们都是 local image ID，不能替代 registry
manifest digest：

- New API fork `dbfcf0c7`：`sha256:68f25465624a0e736a2bfacf58adad608b0e2777f4f89be0272a174bf952e973`
- Controller `f55103e`：`sha256:9f5a2401242ed2cd90daadcc5249037323250d9f6f90384bdbd93570469d538e`
- correction CLI `f55103e`：`sha256:840befffed905094ab3e99d33aaab90c57023f9251800bc41c53c90b5613c8aa`

2026-08-24 又使用 Buildx 以 `linux/amd64,linux/arm64`、`--provenance=false`、
`--sbom=false` 和 OCI exporter 完成本地双架构构建。archive 内 index blob 的 SHA-256
已独立复算，且每个 index 只包含预期的两个 Linux platform descriptor：

- New API fork `dbfcf0c7` local OCI index：`sha256:bedab26b032ee2b4b8654495c6f1d52bcd987fd047e2157b91d9d96edc41d24a`
- Controller `f55103e` local OCI index：`sha256:a16a36248214119b5f13e373637d52488d561f8a7bb5e566e86c66016e161867`
- correction CLI `f55103e` local OCI index：`sha256:19d9154c941528afeb7676bb8f15302cb128ca561c2a32acf827beec35d17316`

两架构 config 还分别验证了 New API entrypoint、Controller/correction entrypoint、Controller
healthcheck 和 runtime UID/GID `10001:10001`。验证后的临时 OCI archive 已删除。这三个
local OCI index digest 不是 registry digest；启用 attestations、改变 exporter 或由 registry
转换 media type 都可能产生不同 digest。2026-08-24 的 GHCR registry receipt 如下：

| Image | Immutable reference | Registry OCI index digest | Visibility |
| --- | --- | --- | --- |
| New API fork | `ghcr.io/vivym/new-api-lark-fork:dbfcf0c7@sha256:47c8bce2491f10d27f8f1a75b68aecffa3e543ff4f3af47f94850d59a1f1edf4` | `sha256:47c8bce2491f10d27f8f1a75b68aecffa3e543ff4f3af47f94850d59a1f1edf4` | `private` |
| Controller | `ghcr.io/vivym/lark-quota-controller:f55103e@sha256:fd111b27e4f4668c76f6006360c246b073dbc71a62e72f9209f6e5b95d62c225` | `sha256:fd111b27e4f4668c76f6006360c246b073dbc71a62e72f9209f6e5b95d62c225` | `private` |
| correction CLI | `ghcr.io/vivym/lark-correction:f55103e@sha256:43bc7bb62d8d422e02642144a6fc0828b418250d16c355de9fd2069be0273ff5` | `sha256:43bc7bb62d8d422e02642144a6fc0828b418250d16c355de9fd2069be0273ff5` | `private` |

Registry inspection confirmed two platform manifests (`linux/amd64`, `linux/arm64`) and two
attestation manifests per image. The source branches are `feature/lark-wp2-bigint-quota`
(`dbfcf0c7`) and `feature/lark-controller-shadow` (`f55103e`); neither remote `main` was
changed. The images were published, but not deployed or server-tested.

### WP5：灰度上线

1. 部署 fork 和 Controller，但不开放 OAuth provider。
2. 只启用 webhook shadow mode，验证事件和实例回查，不发权益。
3. 为测试员工开放 Lark 登录，保留密码登录。
4. 开启 basic 自动分配。
5. 只开放最小 wallet 包，完成幂等与撤销演练。
6. 开启全部 wallet 包。
7. 开启 subscription level 审批。
8. 先以 shadow mode 运行每日在职 reconciliation，验证 scope/分页/权限健康探针。
9. 开启离职事件和 reconciliation 自动停用。
10. 连续观察一周后再评估关闭普通密码登录。

## 回滚

### Controller 或 Lark 故障

- 从 New API 禁用 Lark Custom OAuth provider。
- 保留管理员 break-glass 登录。
- Traefik 可临时关闭 webhook router，但要记录停机窗口，恢复后通过审批实例列表补采。
- 不自动撤销已经成功发放的 wallet 或 subscription 权益。

### New API fork 故障

- 立即关闭 Controller worker 的 grant/disable 写入，只保留 inbox 收件。
- 保留事件，修复后重放。
- 若回退 New API 镜像，必须确认数据库 migration 的向后兼容性。
- 不删除 `integration_grants`，它是避免恢复后重复发放的必要账本。

### 政策配置错误

- 停止对应审批定义的 worker。
- 保留原 grant，不原地修改历史 payload。
- 通过新的 correction external ID 执行人工纠正。
- 发布新的 `policy_version`。

## 上线前必须确认的业务参数

以下内容不会阻塞模块开发，但会阻塞生产启用：

1. `basic`、`plus`、`pro`、`power` 的最终 monthly quota。
2. 一次性 wallet 包的最终数量和额度。
3. wallet 是否确认为永久结转。本文默认是。
4. 每个包和等级的正式审批链。
5. 哪些部门或员工在 Lark 应用可用范围内。
6. 现有用户绑定迁移期限和最终密码登录策略。
7. wallet grant 撤销的人工处理负责人和响应时限。
8. wallet 的 `int64` 业务上限，以及 API 保持 JSON number 还是改为 decimal string。
9. 离职 reconciliation 的查询频率、连续 not-found 门槛和告警负责人；本文默认每日、两次且至少间隔 24 小时。

## 实施完成判定

只有同时满足以下条件，才算 Lark 集成完成：

- Lark 登录、已有账户绑定和新账户创建均通过端到端测试。
- OAuth binding 与永久 integration principal 的职责分离，托管身份不能自助解绑或绑定特权账户。
- 基础订阅可幂等分配，Lark 新用户不获得 welcome/affiliate wallet quota。
- 一次性审批只增加 wallet，并且重复事件只增加一次。
- policy、level、wallet package 和 approval binding 均可按历史版本解析；旧审批和旧 grant 在 active version 切换后仍正确处理。
- 等级审批只原地更新同一 managed subscription，并保留当月使用量、周期边界及在途预扣结算/退款正确性。
- 请求级 fallback、wallet `int64` 边界和资金源退款语义通过测试。
- 低等级乱序事件不会降级。
- managed identity、billing preference、余额购买、支付下单/回调和普通 admin bind 的服务端策略锁通过绕过测试。
- 撤销进入规定的自动或人工路径。
- 离职事件和每日补偿巡检都会通过永久 principal 停用账户；离职先于开户时 tombstone 阻止后续创建，已有用户的浏览器会话和 API key 在旧缓存下也立即失效，权限故障不误停。
- Controller 不持有 New API 数据库凭证或管理员 PAT。
- `:3001` 未 publish/未路由且所有访问强制认证，容器网络与公网验收通过。
- durable outbox、deny fence、带 barrier receipt 的 backup/restore 和事件重放演练通过。
- 审计链可从 Lark instance code 完整追踪到 New API 结果。

## 参考依据

Lark 官方文档：

- OAuth authorize：`https://open.feishu.cn/document/common-capabilities/sso/api/obtain-oauth-code`
- OAuth v3 token：`https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/authentication-management/access-token/get-user-access-token-v3`
- User info：`https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/authen-v1/user_info/get`
- 订阅审批事件：`https://open.feishu.cn/document/server-docs/approval-v4/event/event-interface/subscribe`
- 审批资源与控件 `custom_id`：`https://open.feishu.cn/document/server-docs/approval-v4/approval/overview-of-approval-resources.md`
- 获取审批实例：`https://open.feishu.cn/document/server-docs/approval-v4/instance/get`
- 审批实例事件：`https://open.feishu.cn/document/server-docs/approval-v4/event/common-event/approval-instance-event`
- 员工离职事件：`https://open.feishu.cn/document/server-docs/contact-v3/user/events/deleted`

上游 New API `v0.13.2`（peeled commit `bee339d279ccecbf8c8a89e14ddbbd902f78bd5d`）及当前 fork 检查位置：

- `oauth/generic.go`：form token exchange、debug body logging、userinfo mapping
- `controller/oauth.go`、`controller/custom_oauth.go`：OAuth 创建/绑定流程和现有自助/admin 解绑入口
- `controller/subscription.go`：现有 billing preference、余额购买和普通 admin bind 入口
- `controller/user.go`、`model/user.go`：上游新用户/邀请额度、原有 `int` wallet 字段，以及 fork 的 checked `int64` 迁移
- `model/subscription.go`：支付订单完成、subscription snapshot，以及绑定旧 subscription ID 的 preconsume/settle/refund
- `service/billing_session.go`：`subscription_first` 和 wallet overflow

这些依据说明了为什么本设计采用 OAuth bridge、独立 Controller 和 New API 内部事务接口，而不是直接拼接现有管理员接口。
