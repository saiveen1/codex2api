# ChatGPT Web image worker (private, experimental)

Standalone Python **3.13** / curl-cffi executor for protocol v1 in
`../../docs/chatgpt-web-worker-protocol.md`. The gateway alone owns users, API
keys, scheduling, credentials/refresh, account state, billing and final artifacts.
This worker has no UI, account pool, RT refresh, browser, FlareSolverr, fallback,
account mutation, migration, automatic generation retry or history-by-prompt scan.

**Implemented and mock-tested is not live-verified.** No real account or image
generation was used in implementation. The pinned private webpage protocol may
have changed, and the narrow asset allowlist may reject legitimate new hosts.
Do not interpret health, HTTP 200, tool signals or a Plus/Free plan as generation
success. Only validated downloaded bytes produce an `image` event. Token usage
is never estimated or fabricated (`usage_known: false`).

## Local setup

```sh
uv sync --locked --python 3.13
# Supply CHATGPT_WEB_WORKER_SECRET through your private secret mechanism.
# Supply CHATGPT_WEB_STATE_DIR as a private durable directory.
uv run --locked uvicorn app:create_app --factory --host 127.0.0.1 --port 8091 --no-access-log
uv run --locked pytest -q
```

`.venv` is isolated and gitignored. The server factory refuses to start without a
nonempty secret. Every route except `GET /health` compares the complete Bearer
header in constant time; health contains no config, account, credential or state.
Do not expose the service publicly. Do not enable body/header/proxy/HTTP debug
logging, reverse-proxy request dumps or access logs. Windows operators must apply
an owner-only ACL to the state volume; POSIX creation uses private modes/umask.
SQLite and file locks require a local filesystem, **not NFS/shared object storage**.
The same state volume must survive restarts. There is intentionally no admin
endpoint to clear fences. No credentials need to be put in this source tree.

The Dockerfile is optional build material, not a deployment authorization. It
installs only locked Python runtime dependencies and starts an unprivileged
loopback listener. Put the gateway in the same network namespace, or explicitly
configure a private interface when deployment is separately authorized. Do not
publish the port. Mount a durable writable private state volume owned by UID
10001. There is no Chrome, Node, upscale step or frontend build.

## Exact accepted parameters

- Required binding: positive integer `account_id`, nonempty `access_token` and
  `chatgpt_account_id`, lowercase hex SHA256(UTF-8 access_token) as
  `credential_revision`, positive integer `environment_revision`, and exactly
  `environment.device_id` / `environment.session_id` (1..160 safe identifier chars).
- `proxy_url`: empty/omitted means explicitly direct, otherwise a fixed
  `http`, `https`, `socks5` or `socks5h` URL with host and explicit port. The
  gateway must authorize that proxy; this is not an arbitrary public proxy API.
  Environment proxy variables are ignored, and proxy failure never falls back
  to direct. A proxy is a trusted egress boundary and must honor pinned CONNECT
  targets. The same proxy/account/environment is used throughout each execution.
- `execution_id`: 1..160 ASCII letters, digits, `_` or `-`; immutable and unique.
- `operation`: `generate` (zero inputs) or `edit` (one or two inputs).
- `model`: exactly `gpt-image-2`, mapped to pinned webpage `gpt-5-3` plus
  `picture_v2`; one image is requested (`n` absent or integer `1`).
- `prompt`: nonblank, at most 32768 UTF-8 bytes.
- `size`: `auto` (default), `1:1`, `2:3`, `3:2`, `3:4`, `4:3`, `9:16`, `16:9`.
  Non-auto values append an explicit aspect-ratio instruction to the prompt.
  This is **not** an exact pixel-size guarantee; no resizing or upscaling occurs.
- `quality`: only `auto`; other quality levels are rejected, not ignored.
- `output_format`: `png` (default), `jpeg`, `webp`. Actual downloaded bytes are
  checked and, if necessary, locally converted with Pillow without resizing.
  JPEG uses RGB conversion; transparency is not promised.
- `timeout_seconds`: integer 10..600, default 180; one overall execution deadline,
  not an extra deadline per request. Changing only timeout does not change binding.
- Each edit input is exactly `{b64_json, mime_type}`: strict base64 (not a data URL,
  filename or remote URL), PNG/JPEG/WebP matching decoded MIME, static single-frame,
  at most 20 MiB and 40 million pixels. HTTP request body is at most 58 MiB;
  decoded output is at most 32 MiB per image / 40 million pixels. At most four
  unexpected upstream output assets are preserved; excess fails closed.
- All unknown fields (including URL, headers, masks, background, pixel-size,
  arbitrary upstream parameters) are rejected before submission.

Account check makes only homepage/bootstrap, authenticated `/me` GET and
`conversation/init` metadata POST requests; it never prepares/submits generation
or changes settings. `ready` requires a positive authenticated ID and an explicit
integer positive `limits_progress[feature_name=image_gen].remaining` fact.
Missing/unknown generation quota is `unverified`, zero is `quota_exhausted`.
A ready account may attempt attachment-based edits when no separate `image_edit`
entry is reported. If present, that entry must have a positive integer balance.
Eligibility is not proof of a successful upload or edit. RFC3339 reset strings are returned only when parseable
with timezone; relative/ambiguous values are left null.

## Execution and recovery

`POST /v1/images/execute` emits NDJSON stage/image/completed/error events. Invalid
requests fail before streaming with fixed safe JSON and HTTP 4xx. Streaming
errors carry their own `http_status`; HTTP 200 is not a terminal result.

SQLite `BEGIN IMMEDIATE` / `synchronous=FULL` commits `submitting` **before** the
single upstream generation POST. A cross-process nonblocking OS file lock owns
the execution for its lifetime. Binding fingerprints cover account, actual AT
hash, chatgpt account, environment revision/content, exact proxy URL, prompt,
inputs and options. Mismatches or an active owner return `execution_conflict`.
The ledger never stores AT, cookies, proxy URL/password, prompts, input base64,
requirements/conduit tokens or signed URLs. It contains only IDs, revisions,
hashes, safe state/error enums and private output cache references.

- Identical completed IDs replay private cached results without upstream traffic.
- `submitting`/unknown after crash or cancellation without conversation/task/asset
  IDs returns `submission_unknown` / `result_unknown`; it cannot submit again.
- Known conversation/task IDs permit **GET recovery only**; known assets permit
  resolution/download only. Original user inputs are excluded from output IDs.
- Each execute does one bounded original-conversation GET and, if necessary, a
  filtered original-task GET recovery probe. No progress returns
  `image_poll_timeout`; the gateway may explicitly resume the SAME ID later.
  There is no blind sleep/poll loop or generation resubmission.
- Each successful image is durably cached before emission. Later download errors
  preserve earlier output and do not regenerate. A partial error is not completed
  success. A same-ID read/download resume replays already cached images first.
- GET `/v1/executions/{id}` reads sanitized state only, never creates upstream work.

Output cache retention is **24 hours**, opportunistically pruned at execution
and writes; idle expiration is therefore removed on next activity (not a strict
wall-clock deletion service). The aggregate private cache admission bound is
**2 GiB**. Submission fences/IDs are retained indefinitely to prevent execution-ID
reuse from causing duplicate generations, with at most **100000 records** before
new IDs fail `capacity_exceeded`; never clear old fences to make space without an
explicit gateway-level archival/idempotency policy. Locks are hash-named and
bounded by admitted IDs. Keep the private volume quota independently enforced.
Crashes during an atomic cache write can leave orphan files; their actual bytes
count against admission and they are pruned after retention on next activity.
Operators may remove unreferenced outputs only with the worker stopped; never
remove ledger/fences.
Do not use returned hashes as credentials or expose the private state endpoint
to end users.

## Upstream security and compatibility

All upstream API origins/paths and browser headers are source-defined. TLS
verification is always enabled. curl-cffi impersonates **chrome110**, while the
reference UA/client hints describe Edge/Chromium **143**; the deliberate pinned
reference mismatch is retained, not represented as a live-tested fingerprint.
Client version is `prod-a194cd50d4416d3c0b47c740f206b12ce60f5887`, build `6708908`.

Homepage script URLs are parsed as PoW metadata only; downloaded scripts are
never executed. Requirements prepare/finalize and bounded SHA3 PoW are supported
(maximum 500000 candidates / 3 seconds); Arkose and Turnstile, including the
reference's executable Turnstile VM, fail closed with `environment_challenge`.
There is no browser launch or third-party solver.

Signed upload/download URLs are accepted only after trusted API asset resolution,
HTTPS-only, no userinfo/fragment/nonstandard port, exact source-defined allowlist
(`files.oaiusercontent.com`, `oaiusercontent.com`,
`oaidalleapiprodscus.blob.core.windows.net`,
`prod-files-secure.s3.us-west-2.amazonaws.com`). All DNS answers must be public;
chosen public IP is pinned using curl CONNECT_TO/RESOLVE while retaining original
TLS SNI/certificate verification. Redirects are disabled. Asset sessions are
separate from authenticated web sessions and receive no bearer or cookies.
Other tenants' Azure buckets and wildcard S3/CDN hosts are not trusted by default.
Host/protocol changes require reviewed source changes and mock tests, not a
caller-supplied bypass. Proxy administrators remain a trust boundary. IPv4/IPv6,
actual proxy CONNECT/SOCKS behavior, sentinel acceptance, current host inventory,
real edit readiness, mapping, time-to-image and quota semantics require separately
authorized live verification; this implementation makes no success claim for them.

## Tests and licensing

The suite blocks real sockets/DNS and scripts the HTTP transport. It covers
generate/edit/check, NDJSON/auth, patches, input/output trust, original-ID recovery,
unknown submissions, restarts, concurrent duplicate ownership, immutable binding,
cancellation, partial errors, quotas/challenges, MIME/format, retention/admission,
SSRF, DNS, pinned TLS/egress and credential-free CDN requests. No credentials,
real generation, production access or database migrations are needed.

This subtree is **AGPL-3.0-only**; see the full `LICENSE`, `NOTICE` and
`PROVENANCE.md`. Adaptations are marked in source. No upstream application UI,
account service, database service, browser solver or token-usage estimator was
bulk-copied. Before any public release or combined distribution, review the
AGPL obligations, corresponding source/network source-offer obligations,
notices, build/distribution materials, dependency licenses and compatibility
with the surrounding gateway. **Subprocess separation is not a conclusion that
these duties do not apply.** No public push/deployment is authorized here.
