# ChatGPT Web worker protocol v1

This opt-in image provider leaves all existing channels unchanged. The gateway owns API keys, account scheduling, refresh tokens, credential updates, final artifacts, and usage. The worker owns only web protocol execution and its durable submission fence. No Chrome, FlareSolverr, account pool, RT refresh, account switching, or generation retries.

## Configuration

Gateway: `CHATGPT_WEB_ENABLED` (default false), `CHATGPT_WEB_WORKER_URL` (private HTTP URL or trusted HTTPS), `CHATGPT_WEB_WORKER_SECRET`, `CHATGPT_WEB_TIMEOUT_SECONDS` (default 180, bounded 10..600). Worker: the same secret, `CHATGPT_WEB_STATE_DIR` (private durable volume). No raw upstream endpoint override in request payloads. The worker authenticates every route except a secret-free liveness check. Its default listener is loopback; Compose exposes it only to the gateway network.

API Key channel: `chatgpt_web`. Only Images and image job entrypoints are supported; text, WebSocket, compact and live entrypoints fail closed. Models for the first implementation: exactly `gpt-image-2`. Existing `auto` and `codex` routing remain unchanged. Web enablement is tracked separately from native Codex quota/model cooldowns. Active, unexpired Web keys that allow images and explicitly bind positive group IDs initialize eligible, previously unconfigured OAuth members on Web state/model/check/dispatch reads. Current group/plan/account-key authorization applies; unrestricted or ungrouped-only keys do not opt in the whole pool. Existing state, especially explicit disable, is never overwritten. New state is enabled/unverified, not ready; no upstream check or generation is triggered. Unbinding does not undo persisted enablement, but dispatch still enforces current authorization and observation freshness.

## Worker requests

All authenticated requests carry `Authorization: Bearer <worker secret>`. Account secrets exist only in request memory, not in worker state or diagnostics.

`POST /v1/accounts/check` accepts:

```json
{"account_id":123,"access_token":"...","chatgpt_account_id":"","expected_email":"owner@example.com","credential_revision":"sha256-of-current-AT","environment_revision":1,"proxy_url":"socks5h://...","environment":{"device_id":"uuid","session_id":"uuid"},"timeout_seconds":30}
```

`chatgpt_account_id` may be omitted or empty **for check only**. The worker bootstraps, authenticates `GET /backend-api/me`, then fetches `GET /backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-480`; neither authenticated discovery call sends a `ChatGPT-Account-ID` header. Without an explicit target, only `accounts.default.account.account_id` is eligible; there is no arbitrary team fallback or JWT workspace inference. With an explicit target, the exact ID must occur in upstream account membership, with `is_deactivated: false`; missing/deactivated membership fails with `workspace_unavailable`, never fallback. Both paths require an upstream-confirmed nondeactivated workspace. Workspace IDs are 1..160 ASCII letters, digits, underscores or hyphens and cannot start with `user-` (case-insensitive). Native credential `account_id` values are never rewritten or used as discovered workspaces.

`expected_email` is optional for check and execute; omit it when unknown (an empty value is invalid). When supplied, authenticated `/me.email` must match case-insensitively, otherwise `identity_mismatch` fails closed before workspace/quota checking or execution preparation. Tokens are authenticated upstream, not trusted merely because JWT claims can be decoded. Credential hash, environment revision/device/session and fixed egress validation remain required.

The quota probe is `POST /backend-api/conversation/init`, scoped with `ChatGPT-Account-ID` set to the resolved workspace. Response fields: `status` (`ready`, `unverified`, `auth_required`, `challenge`, `quota_exhausted`), `capabilities` (verified `generate` and/or `edit` only), `image_quota_remaining` (integer or null), `quota_reset_at` (RFC3339 or null), `error_code` (fixed safe enum), `workspace_id` (resolved ID, otherwise empty), `stage`, and `upstream_status` (actual HTTP status for the current stage, or 0 if no response). Successful observations, including known zero quota, end at `complete`; failures retain `bootstrap`, `identity`, `workspace` or `quota` as appropriate. A quota-stage failure can retain the independently verified workspace, but identity/workspace failures cannot echo an unverified requested ID.

Never infer Free capability merely from plan type. Explicit positive image quota is required to return `ready`; missing, malformed, negative, boolean or noninteger quota is `unverified` / `quota_unknown` at stage `quota`, not ready or exhausted. Checks never generate images or alter account memory settings. A successful example:

```json
{"status":"ready","capabilities":["generate"],"image_quota_remaining":3,"quota_reset_at":null,"error_code":null,"workspace_id":"verified-workspace-id","stage":"complete","upstream_status":200}
```

Check protocol/transport failures use safe fixed codes, never upstream bodies, URLs, email values or tokens: `workspace_unavailable`, `identity_mismatch`, `quota_unknown`, `worker_timeout`, `worker_unavailable`, `worker_protocol_error`, or the existing `auth_required`, `environment_challenge`, `quota_exhausted`, `upstream_error`. HTTP 401 remains `auth_required`; it does not claim to distinguish an expired token from other authentication failure. Malformed JSON/response structure is `worker_protocol_error`; transport failures have status 0 unless response headers were received. Invalid check payloads are non-2xx safe error objects with `code`, `message`, `stage: admission`, `http_status`, and `upstream_status: 0`; valid check payloads return HTTP 200 observations even when upstream checking fails.

`POST /v1/images/execute` accepts the account fields above plus:

```json
{"execution_id":"opaque-unique-id","operation":"generate","model":"gpt-image-2","prompt":"...","images":[{"b64_json":"...","mime_type":"image/png"}],"n":1,"size":"auto","quality":"auto","output_format":"png","timeout_seconds":180}
```

Unlike check, execute requires a concrete, nonempty, non-`user-` workspace in `chatgpt_account_id`; the gateway must pass the checked `workspace_id`, not the native credentials' user ID. Execute never discovers or substitutes a workspace. If `expected_email` is provided, it guards preparation and upstream resume/download work on that same account/exit; completed cached replay performs no new upstream requests. The email guard is not a new immutable binding component: existing credential, workspace, environment, egress and payload hashes, submission fencing and resume/idempotency rules remain unchanged.

One worker execution submits at most one generation. The gateway owns batch decomposition and partial-success reporting. Images are bounded, validated binary input transported as base64; the worker must not fetch caller supplied image URLs. Unsupported options are rejected before submission. Size/quality mapping must be explicit; exact output dimensions are not promised. Model and output format are checked, never arbitrary upstream parameters.

Execute response is `application/x-ndjson`, with one event per line:

```json
{"type":"stage","stage":"prepared"}
{"type":"stage","stage":"submitting"}
{"type":"stage","stage":"submitted","conversation_id":"...","task_id":"..."}
{"type":"stage","stage":"generated","asset_ids":["..."]}
{"type":"stage","stage":"downloading"}
{"type":"image","image":{"b64_json":"...","mime_type":"image/png","width":1024,"height":1024,"asset_id":"..."}}
{"type":"completed","usage_known":false}
```

Asset references are not HTTP URLs. Both gateway validation and persistence accept
a bounded bare ID (`[A-Za-z0-9_-]{1,160}`) or `sediment://` followed by that ID.
The prefix is preserved: it selects the authenticated attachment resolver and is
not interchangeable with a bare file ID. Signed URLs, paths, traversal, query or
fragment suffixes are rejected. Replayed asset references are deduplicated.
Invalid events must not mutate the persisted execution snapshot; classified
failure records and known conversation IDs remain available for diagnosis.

Only an exact HTTPS `chatgpt.com:443/backend-api/estuary/content` download GET may
receive the account bearer token. External pre-signed storage URLs and uploads
receive no account Authorization/Cookie. The transport enforces this again and
does not follow redirects. A native `curl_slist` pins CONNECT_TO through proxies
while preserving TLS verification of the original hostname; its lifetime ends
only after the isolated request session closes.

Terminal error event:

```json
{"type":"error","code":"submission_unknown","message":"Safe fixed explanation","stage":"submitting","http_status":502,"retryable":false,"result_unknown":true}
```

Useful safe error codes include `invalid_request`, `auth_required`, `environment_challenge`, `workspace_unavailable`, `identity_mismatch`, `quota_unknown`, `worker_timeout`, `worker_unavailable`, `worker_protocol_error`, `quota_exhausted`, `unsupported_model`, `unsupported_parameter`, `submission_unknown`, `image_stream_interrupted`, `image_poll_timeout`, `image_download_failed`, `content_policy_violation`, `upstream_text_reply`, `upstream_error`, `execution_conflict`.

`GET /v1/executions/{execution_id}` returns secret-free current stage, binding IDs/revisions, conversation/task/asset IDs, safe error code and whether a result is available. It does not generate. `POST /v1/images/execute` with the SAME ID may replay cached results or resume GET/poll/download for the SAME credential/environment/egress and request fingerprint, but MUST NOT re-submit after a durable `submitting` marker. Different bindings or payload reuse return conflict. An active owner returns in-progress/conflict, not a second execution. Restarted ambiguous submissions return unknown. Only a known never-submitted execution may be retried; the gateway initially chooses the conservative no-automatic-retry policy.

## State and privacy

Before any upstream generation POST, atomically persist a submission fence. Record no tokens, cookies, proxy passwords, prompts, base64 inputs or signed URLs in durable ledger/logs. Payload/credential/egress fingerprints are sufficient to reject mismatched replay. Output files are private, bounded, and subject to documented retention; never accept arbitrary file paths from requests. Session state is account/credential/environment/egress scoped. Follow-up queries and downloads stay on the original account and exit. Downloads use trusted upstream host validation, pinned public destinations, TLS verification, and no bearer credentials on signed CDN requests; validate redirects or disable them.

## Gateway administrative contract

- `GET /api/admin/chatgpt-web`: deployment status (`enabled`, `configured`, `protocol_version`, `supported_models`); no secret value.
- `GET /api/admin/chatgpt-web/accounts`: array of account web states, after insert-only group-default initialization; includes future group members without resaving keys.
- `PUT /api/admin/chatgpt-web/accounts/:id`: `{enabled: boolean}` only; changing enable/environment invalidates readiness until check. Explicit disable, including for an absent row and through the batch endpoint, persists and wins over default initialization.
- `POST /api/admin/chatgpt-web/accounts/:id/check`: explicit environment/capability check, no generation.
- `GET /api/admin/chatgpt-web/executions?account_id=...`: bounded recent execution summaries; no prompt, token, raw URL, or image data.

Gateway exported methods used by the admin package:
`ChatGPTWebDeploymentStatus() map[string]any` and `(*Handler).CheckChatGPTWebAccount(ctx context.Context, accountID int64) (*database.ChatGPTWebAccountState, error)`.

Database shared types/methods are implemented in `database/chatgpt_web.go`; native Codex account credentials remain single-owned. The backend ledger must bind execution ID to API Key, request hash, account, credential revision, environment revision and proxy fingerprint, enforce CAS transitions/lease ownership, and survive restart. Failed/unknown output is never billed as a successful image. Provider token usage is unknown unless actually reported.
