# Codex2API · saiveen1 fork

**English** | [中文](README.zh-CN.md)

A personal fork of Codex2API, an OpenAI / Anthropic-compatible gateway. This README focuses on the features and fixes maintained in **[saiveen1/codex2api](https://github.com/saiveen1/codex2api)** rather than repeating the upstream feature catalog.

## Fork features

### Integrated OAuth gateway management

- Import `identifier----password----TOTP_SECRET` or JSON material in the existing admin UI, authorize through an explicitly selected external gateway, and insert results directly into the local account pool. No separate bridge service is required.
- Supports per-account OAuth proxies, execution-machine proxy checks, separate runtime proxies, durable task tracking and safe archives. Unknown outcomes never trigger a new authorization or a blind import retry.
- Reuses administrator authentication and the application database; configure `CODEX_CRED_ENCRYPTION_KEY` before enabling. Ordinary imports never overwrite accounts. Explicitly target an existing account for repair and opt into encrypted material retention for identity-, workspace- and credential-version-guarded 401 recovery.
- The page loads capabilities, quota and groups automatically, shows full account identifiers and explains disabled actions. Retention is off by default and can be explicitly revoked. Encrypted task state upgrades to v3; check compatibility before downgrading.

See [Setup, API and limits](docs/oauth-gateway.md).

### Privacy-conscious request diagnostics

Implemented in `a6179fb` and `4939c24`.

- Correlates the gateway request ID, upstream attempts, upstream request IDs and WebSocket parent requests. HTTP status and business outcome are recorded separately.
- Distinguishes inbound request structure from the structure actually submitted upstream, with bounded metadata, fingerprints and HTTP / SSE / WebSocket error evidence.
- Adds a **Diagnostic details** dialog to Usage and Operations Errors, safe JSON copying and diagnostic context in error exports. Evidence is stored with usage records and in rotating JSONL files; `LOG_DISABLED=true` disables file output without disabling existing usage-database recording.
- Does not store raw prompts, conversation text, tool arguments/results or credentials in the new diagnostic context. Missing, redacted, truncated and unavailable evidence are identified explicitly; generic upstream errors are not presented as a known root cause.

See [Error diagnostics](docs/error-diagnostics.md).

### Explicit HTTP 400 retry control

Implemented in `a80d951`.

- Adds a dedicated **Retry HTTP 400 / Bad Request** switch under continuous upstream-error retry settings, using the existing status-code selector and preserving other selections.
- Does not enable 400 retries automatically. Existing cancellation and duration limits still apply; repeatedly submitting an invalid request can add latency and cost. Diagnostics alone never trigger a retry.

### Optional ChatGPT Web image channel — experimental

Implemented in `cc6fabf`.

- Adds an explicitly scoped `chatgpt_web` API Key channel, disabled by default and separate from native Codex. Supports `gpt-image-2` generation, JSON / multipart edits, Image Studio and queued image jobs.
- Keeps account management, credential refresh, authorization, usage and the gallery in the gateway. An optional private Python worker executes the website protocol without Chrome or a second account pool.
- Uses separate web capability/quota observations, fixed account and egress bindings, persistent submission fences and stable job/output IDs. Ambiguous submissions are not regenerated; known tasks may only resume querying/downloading.
- Does not silently fall back to Codex, change accounts or replay generation after download failure. Mock tests do **not** establish live account availability or production readiness.

See [Integration and limits](docs/chatgpt-web-images.md), [Worker protocol](docs/chatgpt-web-worker-protocol.md) and [Worker setup](services/chatgpt-web-worker/README.md). The worker has a separate license; see below.

## Fork fixes and compatibility

### Codex HTTP first-send encoding

Implemented in `e9a8610`.

- When request compression is enabled, only compresses a final body of at most **128 KiB** if it produces a complete single-block zstd frame that is smaller than the original. Larger or unsuitable bodies are sent uncompressed on the **first** dispatch, without modifying the context or adding a recovery replay.
- Keeps encoding/length headers consistent with the actual body, removes conflicting account headers and leaves `/responses/compact` uncompressed. The shared policy covers native Codex HTTP requests and the native image pipeline, not WebSocket frames.
- Preserves upstream request-ID correlation, including `x-oai-request-id` and fallback handling. This is a conservative compatibility policy, **not** an upstream body-size limit or a fix for every HTTP 400.

See [Configuration](docs/CONFIGURATION.md) for `CODEX_REQUEST_COMPRESSION` and runtime settings.

### Diagnostic evidence correctness

Included in `4939c24`.

- A normal `error:null` stream event no longer hides a later failure; an empty attempt error no longer masks final event evidence in the details dialog.
- Old-stream evidence stays attached to its own attempt, compressed request bytes are decoded before structural inspection, and historical records are not given fabricated request context.

## Documentation and source

Use this fork's source when building these changes; upstream prebuilt images are not a guarantee that they contain fork-specific features.

- [Documentation index](docs/README.md) · [API](docs/API.md) · [Deployment](docs/DEPLOYMENT.md)
- [Configuration](docs/CONFIGURATION.md) · [Error diagnostics](docs/error-diagnostics.md)
- [ChatGPT Web images](docs/chatgpt-web-images.md) · [Optional Compose overlay](docker-compose.chatgpt-web.yml)
- [Inherited Kiro integration](docs/KIRO.md)

## Upstream

Based on **[james-6-23/codex2api](https://github.com/james-6-23/codex2api)**; currently integrated through **`af5f94e` (including v3.1.6 and subsequent Kiro/client compatibility updates)**. The original gateway and inherited features/fixes belong to upstream and its contributors, not to this fork's additions. See the [inherited changelog](CHANGELOG.md).

- **Inherited features:** the dedicated Kiro channel, credential import/export and sign-in, Messages / Responses / Chat adapters, and scoped account usage/reset permissions in the Key portal. Kiro management is separate from this fork's OAuth gateway.
- **Inherited fixes and optimizations:** OpenMausBot client-fingerprint normalization with protected code/schema literals and a fast no-op path; Antigravity `-xhigh` pricing aliases; portal titles, shared account cards and permission-save fixes.

## License and notices

- The gateway retains the upstream **MIT License** declaration. Existing copyright and third-party notices are retained; this README does not relicense them.
- The optional `services/chatgpt-web-worker/` subtree is **AGPL-3.0-only**, with protocol adaptations from `yukkcat/chatgpt2api`. Preserve its [LICENSE](services/chatgpt-web-worker/LICENSE), [NOTICE](services/chatgpt-web-worker/NOTICE) and [PROVENANCE](services/chatgpt-web-worker/PROVENANCE.md). Follow that subtree's release and corresponding-source requirements before combined distribution; a separate process is not assumed to exempt it.
- ModelTrace attribution and MIT terms remain in [NOTICE](third_party/modeltrace/NOTICE) and [LICENSE](third_party/modeltrace/LICENSE).
- The inherited Kiro protocol is ported from `ZyphrZero/kiro.rs`; its attribution and MIT terms remain in [NOTICE](third_party/kiro.rs/NOTICE) and [LICENSE](third_party/kiro.rs/LICENSE).

Provided without warranty. Verify compatibility, data handling and the applicable terms before production use.
