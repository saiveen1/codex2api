# managedproxy integration contract

Standard-library-only, single-owner manager. Construct **one Manager per data directory**.
`New(dataDir)` uses `dataDir/proxy-subscriptions` (e.g. `/data/proxy-subscriptions`),
not the application's DB. Do not mount this private directory through a web/static route.

Exported API:

```go
func New(dataDir string) (*Manager, error)
func (*Manager) View() View
func (*Manager) Configure(context.Context, EngineConfig) error
func (*Manager) SaveSubscription(context.Context, Subscription) (Subscription, error)
func (*Manager) DeleteSubscription(context.Context, string) error
func (*Manager) Refresh(context.Context, string) error
func (*Manager) Ensure(context.Context) error
func (*Manager) Close() error
func (*Manager) SetChangeHook(func())
```

- `Subscription` has the requested fields plus additive `URLSet bool` (`url_set`).
  Both `View` and `SaveSubscription` return `url:""` and truthful `url_set`.
  Blank URL on an existing ID preserves the private URL. Client-provided telemetry
  is ignored; new IDs are random and edits must reference an existing ID.
- `Node.URL` is intentionally the usable **authenticated local** URL for DB
  reconciliation. It contains local credentials, never the subscription URL.
  Only return this view to authorized administrators; a truly public endpoint
  should additionally omit/mask `nodes[].url` in its own DTO.
- Save and Configure only persist desired settings; call Refresh to apply edits.
  Refresh with an empty ID fetches all enabled subscriptions atomically. A targeted
  refresh also reapplies current saved filters/enabled flags to cached providers.
  It does not fetch other subscriptions. A never-refreshed enabled subscription
  stays inactive until its own Refresh (or Refresh-all), without blocking others.
- Ensure is the startup path: it reads cached feeds without remote subscription
  fetches, starts no core when no subscriptions are enabled, and starts the
  periodic refresh scheduler. Enabled subscriptions with no cached feeds at all
  return an actionable error. `refresh_minutes:0` is manual; other values must be >=5.
- Register the change hook before Ensure. The hook runs **outside the mutex** on
  refresh/ensure/delete outcomes, Close, and unexpected owned-core exit. It may
  call View; keep it quick and do not recursively call Refresh/Close. Callbacks
  from different callers can overlap, so the parent should serialize DB reconcile.
- Reconcile only owned rows by stable Node.Key. Import local Node.URL once; set
  their enabled state from Node.Active. Nodes are inactive without a healthy owned
  process, including immediately after New and on an engine error at startup.
  Never modify unrelated user/account proxies. Close the manager on app shutdown.

## Engine behavior and safety

Linux execution only. Configured binary path takes precedence, followed by PATH
`mihomo`, owned `bin/mihomo`, then opt-in automatic installation. A supplied but
invalid binary path is an error rather than silently choosing another binary.
The default new state enables auto-install, controller port 19090, first node
port 20000. Installation uses the official GitHub latest-release metadata and
requires its SHA256 digest or matching official checksum file before extraction;
Linux amd64 explicitly requires the compatible artifact. Unsupported artifacts
or OSes require an actionable manual setup; Windows spawning is rejected instead
of bypassing the WMI requirement.

All feed fetching uses direct transports, inspected/pinned public DNS answers,
redirect revalidation, HTTPS downgrade protection, finite timeouts, and an 8 MiB
post-decompression limit. Loopback/private/special-use addresses are rejected,
including mixed public/private DNS answers. Feeds are MIME-agnostic and passed
unchanged to native file providers: format compatibility is the installed core's
responsibility. No feed URLs, bodies, or controller secrets are logged. Native
stdout/stderr are discarded deliberately; safe errors identify phase/subscription
ID but cannot include a native parser's potentially credential-bearing details.

Providers use stable `mp_<subscription ID>__` prefixes. Regexes see original names.
Dedupe capture tuples define logical groups, with the full match used when no
captures exist; unmatched names stay independent. PreferRegex wins, then lexical
name order. There is no IP-based dedupe. Missing native prefix support fails closed.

Refresh keeps the old main running throughout fetch, listener-free native staging,
provider inspection and native `-t` validation. It then performs a **full owned
stop/restart**, not a forced controller reload. Successful refreshes therefore
briefly interrupt owned proxy connections. Failed staging leaves the main alone;
failed activation/persistence attempts restore the previous config, otherwise
View fails closed. Deletion rebuilds from cached feeds; when already stopped it
can remove records without requiring an available binary. Removed nodes remain
in history with inactive stable URLs. Port allocations are never recycled.

Controller and authenticated mixed listeners bind only to IPv4 loopback. Each
listener targets its exact native namespaced node; UDP, host DNS/TUN changes,
geodata updating and global inbound ports are disabled. The default rule rejects.
Ports are checked before launch; a bind race is inherently possible between a
check and native bind. Activation verifies provider selection and that all expected
listener TCP ports accept connections; this is not an outbound connectivity test.

Private directories are 0700, state/feed/config files atomically replaced at 0600,
and the owned executable is 0700. Failed candidate feeds and superseded committed
feeds are removed. An unclean application termination can leave unused private
runtime/feed artifacts; normal Close/replacement reaps tracked children, but this
package intentionally never discovers/kills processes left by a prior app instance.
A lingering process causes an explicit occupied-port error, not adoption.

## Validation handoff

No local Go compilation/build/test execution is authorized. Remote Linux command:

```sh
go test -count=1 -timeout=90s ./managedproxy
```

Tests cover filtering/preferences, multiple namespaces, stable identity/ports,
private modes/redaction, regex/schedule/URL validation, DNS/redirect SSRF defenses,
response limits, checksum selection, config JSON, callbacks, staged failure
preservation, activation rollback, deletion, startup restoration and port conflicts.
Process tests use a synthetic controller inside the test binary and make **no
external subscription/download requests**. They do not prove real mihomo adapter
routing, authentication, native feed format support, or official network installs.
Before enabling production rows, run a separate remote smoke check with the actual
installed mihomo and synthetic upstreams, including wrong/missing local credentials,
exact-node routing, deleted listener closure, and no geodata download attempts.
