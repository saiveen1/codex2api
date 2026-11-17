# ChatGPT Web images (opt-in)

This integration adds a **ChatGPT website protocol** image channel to Codex2API.
It does not replace Codex, run Chrome, import a second account pool, or promise
that every Free account can generate images. The Python executor follows the
pinned `yukkcat/chatgpt2api` protocol baseline documented in its `PROVENANCE.md`.
Implementation and mock verification are distinct from live upstream acceptance.

## Ownership and deployment boundary

Codex2API remains the only public API, admin UI, account scheduler, credential
refresher, usage owner and gallery. `services/chatgpt-web-worker` is an optional
private Python 3.13/curl-cffi executor. It receives only the chosen account AT,
fixed egress and environment for one execution. It cannot refresh RTs, choose
another account, switch proxy, launch a browser or fall back to Codex.

The worker includes AGPL-3.0-only licensing and pinned source attribution. Retain
its LICENSE/NOTICE/PROVENANCE and offer corresponding modified source as required
by its license. These files do not assert that process separation settles every
combined-distribution question. Review release obligations before public
publication; do not relabel upstream-derived code as MIT.

The default channel selection and all existing Keys remain unchanged; the gateway
image now bundles the optional worker but does not enable it automatically. Building/running
the modified gateway applies additive local schema migrations; review and obtain
approval before running it against an existing production database. Do not start
a test gateway using production DATABASE_URL or copied production secrets.

## Configuration

### Built-in service control (recommended)

The project Dockerfile bundles the existing locked Python worker. In Settings →
Codex → Image settings → ChatGPT Web → Service control, select **Built-in managed worker**, adjust the
10–600 second timeout and click **Save and enable**. No worker URL, shared secret,
server environment edit or gateway restart is needed. The default remains off.
The manager creates a stable private secret and a loopback-only listener, verifies
readiness and reports starting/running/draining/error separately from desired
settings. Refresh the page status after a phase change; saving is not proof of
successful image generation.

Disable immediately gates new gateway work and gracefully drains the owned
worker. It does not delete image artifacts, submission fences or account state.
Switch modes only after disabling and waiting for the owned process to stop.
Only Linux execution is supported; bare binaries need the same bundled worker
layout or can use external mode. Container/service shutdown deadlines still
apply during a full deployment restart, so drain before restarting an instance
with active image requests.

Configuration is private local state under `chatgpt-web` in the gateway's existing
data root (`PROXY_SUBSCRIPTION_DATA_DIR`, default `/data`), alongside—not inside—the
proxy subscription state. Preserve this volume, especially `worker-state`, across
upgrades. No database schema changes are required. Never expose this directory
through static routes. A single gateway instance owns its manager and directory.

### External worker compatibility

Select **External worker** to save an existing private origin and secret in the
same admin UI. A blank secret preserves its saved value and responses never echo
it. This mode controls gateway admission only, not the external process. Saved
configuration takes precedence over environment variables. The variables below
are imported only when no saved configuration exists, preserving older setups;
subsequent changes belong in the admin UI.


| Gateway setting | Default | Meaning |
|---|---|---|
| `CHATGPT_WEB_ENABLED` | false | Allow the explicit image-only web channel |
| `CHATGPT_WEB_WORKER_URL` | unset | Private worker origin, no path/query/userinfo |
| `CHATGPT_WEB_WORKER_SECRET` | unset | Shared secret, at least 16 characters |
| `CHATGPT_WEB_TIMEOUT_SECONDS` | 180 | Overall request budget, 10–600 seconds |

Gateway-to-worker requests ignore environment proxies, require private/loopback
DNS destinations, do not follow redirects, and retain TLS verification.
The worker's `CHATGPT_WEB_STATE_DIR` must be a durable private local volume.
Its default standalone bind is `127.0.0.1:8091`.

`docker-compose.chatgpt-web.yml` is an **optional** overlay: it builds this
checkout, adds a worker with no published ports and a private state volume, and
shares a dedicated bridge network with the gateway. The bridge permits outbound
traffic, but only the gateway/worker join it. Ensure the worker can route to the
account's selected proxy; it never retries a failed proxy by going direct.
Do not attach arbitrary untrusted containers to this network.

After deployment is separately approved, the general Compose invocation is:

```sh
docker compose -f docker-compose.yml -f docker-compose.chatgpt-web.yml config
# Review the rendered configuration locally; it contains secrets, do not paste it.
docker compose -f docker-compose.yml -f docker-compose.chatgpt-web.yml up -d --build
```

Keep the shared secret in your existing private secret configuration, never in
Git or shell history. The example overlay alone is not a production deployment
or a claim that Docker images have been built or run.

## 中文快速使用

Web 生图是默认关闭的独立通道，不等同于 Codex 原生生图。在设置 → Codex → 生图设置 → ChatGPT Web 生图（实验性）→ 服务控制中保存并启用服务。创建通道为 `chatgpt_web` 的专用 API Key 并绑定分组后，即可在生图工作台选择该 Key 发起生成；没有就绪账号时会在授权范围内做有界按需检查，不必先逐个手动检查。API Key 配置及工作台保留设置直达链接，便于手动诊断认证、额度或代理问题。Web 失败不会自动回退原生通道。

支持七个调用名称：`gpt-image-2`、`gpt-image-2.5`、`gpt-image-2.5-flare`、`gpt-image-2.5-sunburst`、`gpt-image-2.6`、`gpt-image-2.6-flare`、`gpt-image-2.6-sunburst`。这些是受控兼容名称，按所选名称记账和绑定执行；不保证能锁定上游图片引擎版本。仅支持自动质量、比例尺寸、PNG/JPEG/WebP和最多两张参考图；不新增2k/4k后缀、GIF、蒙版等未经适配的参数。外部Worker需同步更新到支持这些名称的版本。

`/v1/models` 返回该通道支持且被 Key 模型权限允许的目录，不因账号暂时未检查、额度耗尽、检查过期或服务尚未运行而清空；读取目录不初始化账号、不触发上游请求。目录可见不代表当下生成必定成功。现有Key若只允许`gpt-image-2`，仍只返回这一项，不自动扩大白名单。

普通新生成先使用现成就绪账号；找不到时，每个HTTP请求（含n张批量）最多检查3个账号，上游检查预算20秒且不延长原请求的上游工作期限；结束后诊断写入和租约释放共享最多3秒独立收尾预算，进程内最多7个并发自动检查，并继续使用数据库账号租约防重复检查。候选仅限当前Key的分组、套餐、账号白名单、亲和/预算范围；近期失败退避2分钟，已确认额度未重置或缺少编辑能力时不盲查。检查通过后仍再次校验身份、workspace、额度、出口和容量，才提交图片。恢复旧执行、结果未知的提交不触发自动准备或换号重发。

自动准备只为实际选中的少量候选初始化缺失的Web状态，并仅限Key明确绑定的正数分组；不限分组或仅未分组的Key不会全库启用。已有手动停用状态不覆盖。管理页读取Web状态和显式手动检查仍保留分组默认初始化；手动检查不受自动失败退避影响。账号列表/额度弹窗可查看记录或主动检查，失败保留旧额度快照并显示最近错误。Web检查与Codex原生额度独立。

设置页的“批量控制 Web 账号”用于手动覆盖：按勾选账号、现有 Codex 分组或 Web API Key 授权范围启用/停用，单次最多 1000 个账号，逐项返回结果。对未配置账号停用也会保存显式停用记录，防止被自动启用；它不检查额度、刷新 token 或生成图片。

Web 检查使用有效的显式 workspace，或通过同一凭据/出口认证后发现上游默认 workspace；`user-` 登录身份和缺失 JWT workspace 不会被误当作账号不可用。显式空间不存在或不属于该账号时不会切换到默认空间。导入/凭据更新也在公共流程中规范化用户与空间身份，缺失身份按受控预算补全；无法确认的身份保留待验证状态，不伪造 workspace。

升级会为 Web 状态添加独立的 workspace、请求空间绑定和最近检查诊断字段。旧 ready 记录缺少新身份绑定时降为待检查，不删除账号、图片或执行围栏。检查、调度和执行恢复共同校验 token、空间、环境与出口；不要通过手工替换 `user-` 前缀或删除校验来绕过失败。

## Enabling a test scope

1. Configure the private worker and leave existing Key channels unchanged.
2. In Settings → ChatGPT Web, explicitly enable one eligible OAuth account.
3. Run the environment check. It observes identity/capability/quota metadata; it
   does not submit an image. Only positive evidence produces readiness. Missing
   facts remain unverified; no plan-name inference is used.
4. Create a dedicated API Key whose channel is `chatgpt_web`, restricted to the
   intended account group. Do not repurpose an existing production Key.
5. Separately authorize one synthetic generation and one edit before enabling
   ordinary traffic. Environment readiness alone is not an end-to-end success.

Web state is separate from Codex limits. A changed AT, environment revision,
proxy fingerprint or stale observation requires a new check. Codex2API alone
owns token refresh; worker authentication errors do not rotate tokens or disable
native Codex usage automatically. An unavailable web provider does not silently
route to a Plus/Codex account.

## API and supported parameters

Use existing `/v1/images/generations`, `/v1/images/edits`, image job APIs and the
image studio with the explicitly web-scoped Key. Text, compact, realtime and
Responses WebSocket endpoints reject that Key's channel. `/v1/models` exposes
only the verified web image capability, not the native Codex catalog.

```json
{"model":"gpt-image-2","prompt":"A red square on white","size":"auto","quality":"auto","output_format":"png","n":1,"response_format":"b64_json"}
```

- Initial model: exactly `gpt-image-2`; existing aliases do not change channel.
- Size: `auto` or ratio `1:1`, `2:3`, `3:2`, `3:4`, `4:3`, `9:16`, `16:9`.
  Ratios are instructions to the model, not pixel guarantees. Actual dimensions
  are decoded and returned. Exact pixel requests are rejected, not silently resized.
- Quality: `auto` only. Output: PNG/JPEG/WebP; format conversion never resizes.
- Direct batch count: 1–4, decomposed into distinct single-image executions;
  failures preserve successful outputs. The studio retains its own batch limit.
- Edits: JSON `images[].image_url` (data URL or safely fetched public URL), or
  multipart `image` / `image[]`. The current worker accepts at most two references.
  Each file is bounded to 20 MiB and 40 megapixels. No arbitrary worker-side fetch.
- Mask, explicit background, input fidelity, moderation overrides, output
  compression and partial-image knobs are not implemented on this channel and
  are rejected when supplied. Standard native channels retain their own support.
- URL responses use existing gateway storage/data-URL handling. Never expose the
  worker's internal file paths or upstream signed links as public artifacts.

Streamed responses carry stage events, then standard image completion events
only after validated image bytes and a worker completion event. HTTP 200,
heartbeats and a tool-call signal do not count as image success. Provider token
usage remains unknown (`usage_known:false`); image counts/dimensions are real.

## Idempotency, uncertainty and recovery

An optional `Idempotency-Key` scopes a stable execution to the authenticated Key.
Studio/queued outputs use stable server-owned job/output identity. A caller
cannot choose an upstream account or overwrite execution ownership.

Before contacting the worker, the gateway persists a submission fence; before
sending the generation POST upstream, the worker persists its own fence. Both
survive restarts. A worker restart must use its existing state volume.

- Reusing a completed execution returns conflict rather than generating again.
  Retrieve saved job artifacts through the existing authenticated result API.
- An active lease returns conflict. A known conversation/task/asset can resume
  only under its original account, credential/environment/egress binding after
  the previous lease expired; only querying/downloading is permitted.
- An ambiguous submission with no recovery ID returns `result_unknown`, never a
  new generation. Do not remove the Idempotency-Key to work around that error.
- Download failures do not trigger re-generation. Web job PNG→JPEG fallback is
  disabled. The worker performs no generation retry or account switch.
- This is at-most-once submission fencing, not a claim of upstream exactly-once
  execution or a guarantee that a disconnected upstream task was cancelled.

The admin execution list exposes stages and safe IDs. Secrets, Cookie bundles,
raw prompt/image inputs and signed upstream URLs are excluded from the execution
ledger and ordinary diagnostics. Main job prompt retention follows existing
Codex2API storage policy; the integration does not silently change that policy.

## Verification and rollback

Offline checks use temporary SQLite databases, mock worker/upstream transports,
and synthetic images. See `proxy/chatgpt_web_images_test.go`,
`database/chatgpt_web_test.go`, `auth/chatgpt_web_test.go`,
`admin/chatgpt_web_test.go` and worker `tests/`. They do not prove current real
Free/Plus webpage access, production PostgreSQL behavior or live image quality.

Disable the explicit web channel to stop new work; preserve pending execution
IDs, worker state volume and existing image artifacts. The native Codex routes
remain independent. Do not restore old database dumps or delete submission
fences as a rollback shortcut. See the worker README for bounded output retention
and storage-capacity behavior.
