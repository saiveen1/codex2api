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

The default deployment and all existing Keys remain unchanged. Building/running
the modified gateway applies additive local schema migrations; review and obtain
approval before running it against an existing production database. Do not start
a test gateway using production DATABASE_URL or copied production secrets.

## Configuration

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

Web 生图是默认关闭的实验性独立通道，不等同于 Codex 原生生图。先配置私有服务，在设置中启用并检查现有 OAuth 账号，再创建通道为 `chatgpt_web` 的专用 API Key，最后在生图工作台选择 Web 来源和该 Key。检查环境只确认观察到的环境与额度信息，不能代替真实生图验收；Web 失败不会自动回退原生通道。

目前仅支持 `gpt-image-2`、自动质量和比例尺寸（不保证像素尺寸），最多两张参考图；不支持蒙版和指定背景等参数。门户通道无法确认时请重试，不要当作原生通道提交。

在账号列表或卡片中点击 Web 状态/图片额度标识，即可查看该账号的网页生图剩余额度、重置时间和观察时间；设置页的账号卡片也提供同一入口。打开弹窗只读取最近记录，手动点击“刷新额度（不生成图片）”才运行网页环境/额度检查。额度未知不等于 0 或无限；刷新失败保留旧记录。未配置服务或未启用账号时不会自动启用，也不会提交生图。

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
