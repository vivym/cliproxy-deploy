# New API + CLIProxyAPI API Site Design

## Goal

Build a public API site on top of the existing CLIProxyAPI deployment while keeping the first commercial version deliberately small.

The v1 site should provide:

- A single public API and user portal domain.
- Invite-code registration.
- Redeem-code balance top-up.
- USD-denominated user balances.
- New API-managed API keys, model permissions, quota, routing, and real-time deduction.
- CLIProxyAPI as an internal upstream adapter for Codex, Claude Code, Gemini CLI, and related OAuth/CLI account pools.
- Usage audit through CPA Usage Keeper or a small collector reading CLIProxyAPI's Redis-compatible usage queue.

The v1 site should not provide:

- Online payment integration.
- Invoices.
- Customer support ticketing.
- Automatic refunds.
- Balance transfer, withdrawal, or resale between users.
- Enterprise billing.
- Unlimited plans.
- A custom billing system.
- A permanent public CLIProxyAPI management UI.

## Architecture

Use one VPS and one Docker Compose project.

Public traffic enters through one domain:

```text
ai.x2r.store
  -> Cloudflare
  -> Traefik
  -> New API
```

New API is the only public user and SDK entry point. It serves the user portal, administrator login, OpenAI-compatible API endpoints, token management, invitation flow, redeem-code flow, model permissions, channel routing, and real-time balance deduction.

CLIProxyAPI stays internal:

```text
New API
  -> http://cliproxyapi:8317
  -> CLIProxyAPI
  -> Codex / Claude Code / Gemini CLI / other upstream accounts
```

The existing public `cliproxy.x2r.store` route should be removed from the production API-site path or retained only as a temporary migration/testing endpoint. It must not become a user-facing commercial API endpoint.

Implementation must remove CLIProxyAPI from Traefik public routing before launch. The current repository routes `cliproxyapi` publicly through Traefik labels; the API-site migration must remove those labels or set `traefik.enable=false` on the `cliproxyapi` service. Only the `new-api` service may have a public Traefik router for `ai.x2r.store`.

There is no v1 user migration from the existing CLIProxyAPI public API key setup. Existing CLIProxyAPI client keys are treated as operator/testing keys, not API-site customer keys. If `cliproxy.x2r.store` is kept temporarily, it is a private migration/test endpoint with explicit operator-only access and a dated decommission plan.

Any temporary public CLIProxyAPI route must live in a separate Compose override file, not in the production base Compose file. The production base Compose file must keep CLIProxyAPI private.

Compose services:

```text
traefik
new-api
postgres
redis
cliproxyapi
cpa-usage-keeper or usage-collector
```

Postgres stores New API's durable business data. Redis supports New API runtime/cache behavior. CLIProxyAPI keeps its existing `config.yaml`, `auths/`, and `logs/` mounts. CPA Usage Keeper or the collector stores audit data separately from the New API billing ledger.

The v1 default audit service is CPA Usage Keeper. A custom collector is only a fallback if CPA Usage Keeper cannot consume the required CLIProxyAPI usage queue fields or cannot be deployed cleanly in the Compose environment.

Network layout:

```text
proxy network:
  traefik
  new-api

backend network:
  new-api
  postgres
  redis
  cliproxyapi
  cpa-usage-keeper
```

Traefik must not join the backend network. Postgres, Redis, CLIProxyAPI, and CPA Usage Keeper must not publish host ports.

## Version Strategy

New API should use the latest stable release available at implementation time.

Current checked baseline during design:

```text
New API v0.13.2
```

Rules:

- Do not use alpha, beta, nightly, or development builds for production.
- Define "latest stable" as the highest non-prerelease semver tag, excluding `alpha`, `beta`, `rc`, nightly, and development tags. For example, `v1.0.0-rc.2` is not stable.
- Do not trust GitHub's "Latest" release marker or `/releases/latest` blindly, because it can point at a release candidate. Inspect tags and choose the highest non-prerelease semver tag.
- Do not leave the production Compose file on an unconstrained `latest` tag.
- Re-check the current stable release tag immediately before implementation.
- Pin the image to a specific version tag after validation.
- Validate `/v1/responses`, redeem codes, quota deduction, channel routing, and fallback behavior before production upgrade.
- Audit New API top-up/payment settings after every version change and keep online payment providers disabled for v1.

## User And Credit Model

Registration uses invitation codes. An invitation code only allows account creation; it does not grant balance.

Initial account state:

```text
registered user
balance: $0
API access: disabled by insufficient balance
```

Top-up uses redeem codes only. The system does not process online payment in v1.

Recommended redeem-code denominations:

```text
$10
$50
$100
$500
```

New API remains the real-time billing ledger. Users see USD balances, while New API internally uses its quota and model/group rate system. Balance deduction happens before or during New API request handling. If a user has insufficient balance, New API must reject the request before sending traffic to CLIProxyAPI.

CPA Usage Keeper or the collector must not directly modify user balances in v1. It exists for audit, reporting, and manual investigation.

Launch conversion policy:

```text
Display unit: USD
Internal unit: New API quota
Baseline conversion: follow New API's default $1 = 500,000 quota units
Redeem code $10: 5,000,000 quota units
Redeem code $50: 25,000,000 quota units
Redeem code $100: 50,000,000 quota units
Redeem code $500: 250,000,000 quota units
```

If the selected New API release changes this default conversion, implementation must preserve the visible USD-to-quota mapping above through configuration or explicitly revise the spec before launch.

Initial group policy:

| Group | Purpose | Initial balance | Model access | Official fallback |
| --- | --- | ---: | --- | --- |
| `unactivated` | Registered users before redeeming a code | `$0` | None | No |
| `standard` | Redeemed users | Redeemed USD amount | Validated CLIProxyAPI-backed models only | No by default |
| `trusted` | Manually promoted users | Redeemed USD amount | Validated CLIProxyAPI-backed models plus selected high-cost models | Only for explicitly enabled models after validation |
| `admin-test` | Operator validation | Manual | All channels under test | Yes, for validation only |

Initial model rates must be conservative. Any model backed by official API fallback must be priced at or above the official provider cost plus an operational margin. CLIProxyAPI-backed models may be priced lower, but each model must have a documented rate before it is visible to `standard` users.

Initial model/rate matrix:

| Model alias shown in New API | Primary channel | Upstream model ID | Enabled groups | Endpoint requirement | Initial billing rule |
| --- | --- | --- | --- | --- | --- |
| `codex-cli` | CLIProxyAPI | `gpt-5.2-codex` or the CLIProxyAPI Codex model name validated at implementation | `standard`, `trusted`, `admin-test` | `/v1/responses`, streaming, tools | CLIProxyAPI-backed rate, no official fallback for `standard` |
| `gpt-general-cli` | CLIProxyAPI | `gpt-5.2` or the CLIProxyAPI GPT-compatible model name validated at implementation | `trusted`, `admin-test` at launch; promote to `standard` only after cost review | `/v1/responses` and `/v1/chat/completions` | CLIProxyAPI-backed rate, no official fallback for `standard` |
| `openai-codex-official-test` | OpenAI official API | `gpt-5.2-codex` | `admin-test` only | `/v1/responses`, streaming, tools | Official provider cost plus margin; validation only |
| `openai-general-official-test` | OpenAI official API | `gpt-5.2` | `admin-test` only | `/v1/responses`, streaming, tools | Official provider cost plus margin; validation only |

Before any model is visible to `standard` users, implementation must record the exact New API model ratio, completion/output ratio if supported, group ratio, cache-token handling if supported, and whether failed upstream attempts are charged. If New API cannot express input/output/cache/reasoning rates separately for a model, the launch plan must document the effective total-token billing rule and the margin used to cover that limitation.

Billing acceptance tests must capture user balance before and after each representative request and compare the observed quota delta to the expected New API formula. The launch plan must include at least these cases:

| Case | Endpoint | Required usage shape | Expected result |
| --- | --- | --- | --- |
| Responses non-stream | `/v1/responses` | input tokens, output tokens, request id | Successful deduction equals configured model/group formula within documented rounding tolerance |
| Responses stream | `/v1/responses` streaming | final usage event present | Same deduction as equivalent non-stream usage within tolerance |
| Tool/function call | `/v1/responses` | tool call plus final text | Tool-call tokens included in deduction or explicitly documented if provider usage excludes them |
| Long context | `/v1/responses` | large input near configured limit | Success with correct deduction or rejection before upstream call |
| Upstream retry | CLIProxyAPI-backed model | retry or credential switch | User is charged only for final billable provider usage, or retry charging is documented and priced |
| Upstream failure | CLIProxyAPI-backed model | no successful completion | No balance deduction unless New API/provider explicitly reports billable usage |
| Official fallback test | admin-test only | official provider request id | Deduction and provider bill reconcile before any trusted-user exposure |

## Upstream Strategy

Use a hybrid upstream strategy.

Primary channels:

- CLIProxyAPI-backed Codex/GPT-compatible channels.
- CLIProxyAPI-backed Claude-compatible channels.
- CLIProxyAPI-backed Gemini-compatible channels.

Fallback channels:

- A small number of official API channels.
- Enabled by default only for the `admin-test` group.
- May be enabled for the `trusted` group only after the specific model, Responses API behavior, quota deduction, and provider cost have passed validation.
- Disabled by default for the `standard` group.

Do not configure unconditional fallback from all low-priced CLIProxyAPI channels to high-cost official API channels. That would create a loss path during upstream incidents.

The v1 launch default is conservative: paid `standard` users use CLIProxyAPI-backed channels only. Official fallback starts as an operator validation path, not as a general paid-user reliability guarantee.

Initial official fallback allowlist:

| Channel alias | Provider | Upstream model ID | Endpoint requirement | Enabled group | Launch state |
| --- | --- | --- | --- | --- | --- |
| `openai-codex-responses-test` | OpenAI official API | `gpt-5.2-codex` | `/v1/responses`, streaming, tools | `admin-test` only | Validation only |
| `openai-general-responses-test` | OpenAI official API | `gpt-5.2` | `/v1/responses`, streaming, tools | `admin-test` only | Validation only |

No official fallback channel is enabled for `standard` users at v1 launch. Enabling official fallback for `trusted` users requires a configuration change after validation and must name the specific model, group, rate, and maximum exposure.

Official fallback cost controls:

- Use a dedicated official provider project/key for the API-site fallback path.
- Configure provider-side budget alerts and hard limits where available.
- Keep `admin-test` group balance and API-key rate limits low enough that a leaked key or loop cannot create material spend.
- Keep official fallback disabled for all non-test groups until a manual change names the model, group, rate, per-key limit, and maximum expected exposure.

New API owns:

- User group permissions.
- Model visibility.
- Model and group rates.
- Channel priority.
- Channel availability and fallback policy.
- Balance, quota, and rate-limit rejection.

CLIProxyAPI owns:

- OAuth/CLI account pool access.
- Internal account rotation.
- Session affinity.
- Retry behavior across credentials.
- Redis-compatible usage queue events.

## Codex And `/v1/responses`

Codex support is a P0 requirement.

The production path must support:

```text
Codex CLI
  -> https://ai.x2r.store/v1/responses
  -> New API
  -> CLIProxyAPI or official fallback
```

Only models and channels that pass real `/v1/responses` validation may be enabled for Codex users.

Required validation:

- Record the exact Codex CLI version used for validation.
- Codex CLI API-key mode works against `https://ai.x2r.store` with the configured base URL pointing at the New API site.
- A real Codex CLI agent run succeeds in a disposable test repository, not just a hand-written `curl`.
- `/v1/responses` non-streaming request succeeds.
- `/v1/responses` streaming request succeeds and Codex CLI can parse the SSE/event stream without protocol errors.
- Tool/function call behavior works for supported models.
- Long-context requests fail predictably or succeed within configured limits.
- `store: false` or equivalent response-storage disabling behavior is accepted where relevant.
- New API deducts quota correctly for Responses API traffic.
- CLIProxyAPI usage queue emits corresponding events for CLIProxyAPI-backed requests.
- Request ids or another correlation key allow the New API deduction, CLIProxyAPI usage event, and official provider bill to be reconciled for representative requests.
- Official fallback channels used for Codex also support Responses API, not only `/v1/chat/completions`.

If `/v1/responses` fails validation, Codex traffic must not be opened to users.

## CLIProxyAPI Operations

CLIProxyAPI should not expose a permanent public management UI or management API.

Day-to-day management happens through SSH to the VPS:

```bash
cd /path/to/cliproxy-deploy
docker compose exec cliproxyapi /CLIProxyAPI/CLIProxyAPI -no-browser --codex-login
```

This operations path is used for:

- Adding Codex credentials.
- Adding Claude Code or Gemini CLI credentials.
- Inspecting logs.
- Editing `config.yaml`.
- Checking persisted credential files under `auths/`.
- Running internal health checks against `http://cliproxyapi:8317`.

If OAuth login requires a browser callback, use a temporary SSH tunnel and close it after login. Do not permanently expose OAuth callback ports.

For exceptional troubleshooting, a temporary protected CLIProxyAPI management route may be created with IP allowlisting or Cloudflare Access, then removed after maintenance.

## Security Boundary

Publicly exposed:

- Traefik ports `80` and `443`.
- `ai.x2r.store` routed to New API.

Not publicly exposed:

- `cliproxyapi:8317`.
- CLIProxyAPI management UI.
- CLIProxyAPI management API.
- Postgres.
- Redis.
- CPA Usage Keeper or collector database.
- CLIProxyAPI OAuth callback ports.

Security requirements:

- New API administrator password must be strong and unique.
- New API default administrator credentials must be rotated before public launch.
- New API administrator login must be hardened with login rate limiting and captcha or 2FA where supported.
- If New API supports admin-path restriction, apply it. Otherwise add Cloudflare WAF/rate-limit rules for admin login paths and consider Cloudflare Access before opening real users.
- Disable unused New API authentication methods.
- Disable all online payment providers, payment routes/menus where configurable, registration bonus/free-credit settings, invoice features, refund features, and support affordances for v1.
- Public registration must require invitation codes.
- New users receive no free balance.
- Low-balance or untrusted users cannot access high-cost models.
- User and API-key rate limits must be configured.
- Official fallback must be limited by model/group policy.
- Secrets must stay in `.env` or local runtime config files and must not be committed.
- CLIProxyAPI `request-log` must be `false` for production launch except during short troubleshooting windows.
- CLIProxyAPI management may remain enabled for internal container access, but it must not be publicly routed. The management key is a local secret and is required by the usage collector.

Cloudflare requirements for `ai.x2r.store`:

- SSL/TLS mode must be Full strict.
- Cache must be bypassed for API and application paths.
- WebSocket/SSE streaming must remain compatible with Codex and other streaming clients.
- WAF/rate-limit rules must protect login, registration, redeem-code, and API paths without breaking streaming.

## Persistence And Backup

Back up:

- Postgres: New API users, tokens, balances, redeem codes, channel configuration, rates, logs, and other durable data.
- CLIProxyAPI `auths/`: upstream OAuth/CLI credentials.
- CLIProxyAPI `config.yaml`: management secret, internal API keys, routing settings, and usage queue settings.
- CPA Usage Keeper or collector database: audit history and operational reporting.

Redis should be treated according to the selected New API deployment mode. If it only contains cache/runtime data, backup is less critical. If deployment settings make Redis durable for required behavior, persistence and backup must be configured explicitly.

Minimum backup and recovery requirements:

- Run scheduled Postgres backups, such as daily `pg_dump`, with at least 7 daily and 4 weekly retained copies.
- Store backups off-host and encrypted.
- Back up CLIProxyAPI `auths/` and `config.yaml` after any credential or configuration change.
- Back up CPA Usage Keeper data if used for operational reporting.
- Take a database backup or volume snapshot before New API upgrades or migrations.
- Keep the previous pinned Compose image tags available for rollback.
- Perform at least one restore drill before selling meaningful volume.

## Monitoring And Audit

New API is the primary billing ledger. It decides user balance and quota.

CPA Usage Keeper or the collector reads CLIProxyAPI usage queue events:

```text
CLIProxyAPI usage queue
  -> CPA Usage Keeper / collector
  -> audit database / dashboard
```

Audit checks should compare:

- New API user/API-key consumption.
- CLIProxyAPI usage events by API key, model, provider, latency, token count, status, and request id.
- Official API provider bills for fallback traffic.

The collector should alert or surface anomalies but should not auto-adjust balances in v1.

Important anomaly classes:

- Missing CLIProxyAPI usage events.
- Unexpected zero-token usage.
- Model mapping mismatch.
- High fallback rate to official API.
- High retry/failure rate for a CLI/OAuth account.
- Sudden cost spike by user, key, model, or channel.
- Usage queue retention too short for the collector polling interval.

CPA Usage Keeper deployment requirements:

- Run only on the backend Docker network by default.
- Do not attach a Traefik public router by default.
- Use a pinned image/tag, not an unconstrained `latest`, once validated.
- Persist its data directory or database volume.
- Pass the CLIProxyAPI management key through local secrets or `.env`, never through git.
- If the dashboard is ever exposed, enable its authentication and add Cloudflare Access or equivalent protection.

CLIProxyAPI usage queue settings:

```yaml
usage-statistics-enabled: true
redis-usage-queue-retention-seconds: 3600
```

The collector should poll frequently enough to drain the queue well inside retention; the launch target is every 1 to 5 seconds and drain-until-empty behavior per poll cycle. Usage queue audit is best-effort until events are persisted by CPA Usage Keeper or the collector. CLIProxyAPI restarts or collector downtime longer than retention can lose audit events, so New API remains the billing source of truth.

## Test And Launch Gates

Functional gates:

- `ai.x2r.store` routes to New API over HTTPS.
- Invitation-code registration works.
- Registration without invitation fails.
- New registered users have `$0` balance.
- Users with `$0` balance cannot call models.
- Redeem code increases visible USD balance.
- Redeemed codes cannot be reused.
- Users can create and revoke API keys.
- New API deducts quota after successful calls.
- Balance exhaustion blocks further calls.
- Online payment providers and registration bonuses are disabled.
- No public top-up route succeeds except redeem-code redemption.
- No registration bonus or free-credit path grants balance to a newly created user.

Codex and API gates:

- Codex CLI works through `https://ai.x2r.store/v1/responses`.
- `/v1/responses` non-streaming works.
- `/v1/responses` streaming works.
- `/v1/chat/completions` non-streaming works for intended clients.
- `/v1/chat/completions` streaming works for intended clients.
- Tool/function call behavior is validated for supported models.
- Long-context behavior is documented and enforced.
- Unsupported model access returns a clear API error.

Routing and cost gates:

- CLIProxyAPI primary channel works.
- Official fallback works only for intended groups/models.
- Official fallback does not catch all low-price traffic unconditionally.
- High-cost models require appropriate group/rate configuration.
- Channel failures produce expected user-facing errors.

Audit gates:

- CLIProxyAPI usage queue is enabled.
- CPA Usage Keeper or collector can read usage events.
- Usage events are persisted.
- New API consumption and CLIProxyAPI events can be manually compared for representative requests.
- Collector downtime shorter than usage queue retention does not lose events during normal operation.

Security gates:

- CLIProxyAPI is not reachable from the public internet.
- CLIProxyAPI has no public Traefik router.
- Postgres is not reachable from the public internet.
- Redis is not reachable from the public internet.
- CLIProxyAPI management UI and API are not permanently public.
- CLIProxyAPI production `request-log` is disabled.
- New API default admin credentials are changed.
- Admin login is rate-limited and hardened with captcha/2FA where supported.
- Secrets are absent from git.
- Backups exist for Postgres, `auths/`, and `config.yaml`.
- A restore drill has been completed before meaningful paid usage.

## Open Risks

Upstream terms risk remains material. Wrapping OAuth/CLI or subscription account capabilities into a paid public API service may conflict with upstream provider terms. The design reduces operational blast radius but does not remove this risk.

Regulatory risk remains material for a public paid AI API service. The v1 design avoids online payments and in-site money handling beyond redeem codes, but the service can still be interpreted as a commercial AI API offering depending on jurisdiction, users, marketing, and payment workflow.

Billing accuracy must be proven empirically. Responses API, streaming, tool calls, retries, cache tokens, and provider-specific usage fields can create accounting mismatches. The system must pass the launch gates before selling meaningful volume.

Official fallback can create unexpected cost exposure. It must remain controlled by group, model, and rate policy.

## References

- New API repository: <https://github.com/QuantumNous/new-api>
- New API releases: <https://github.com/QuantumNous/new-api/releases>
- OpenAI Responses API reference: <https://developers.openai.com/api/reference/resources/responses/methods/create>
- OpenAI Codex agent loop article: <https://openai.com/index/unrolling-the-codex-agent-loop/>
- CLIProxyAPI repository: <https://github.com/router-for-me/CLIProxyAPI>
- CLIProxyAPI Redis Usage Queue: <https://help.router-for.me/management/redis-usage-queue.html>
- CPA Usage Keeper repository: <https://github.com/Willxup/cpa-usage-keeper>
