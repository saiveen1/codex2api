package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func waitImportProbeSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("import probe did not reach its checkpoint")
	}
}

// A single worker makes this fence wait for every earlier import task.
func drainImportProbeQueue(t *testing.T, h *Handler) {
	t.Helper()
	done := make(chan struct{})
	h.runImportProbeTask(func(context.Context) { close(done) })
	waitImportProbeSignal(t, done)
}

func newImportUsageProbeFixture(t *testing.T) (*Handler, *auth.Account) {
	t.Helper()
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	store.SetUsageProbeConcurrency(1)
	// Claude exercises the shared queue without unrelated Codex identity/catalog work.
	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamClaude, AccessToken: "synthetic-at", Status: auth.StatusReady}
	store.AddAccount(account)
	return &Handler{db: db, store: store}, account
}

func TestImportUsageProbeDefaultAndDisabled(t *testing.T) {
	h, account := newImportUsageProbeFixture(t)
	var calls atomic.Int32
	h.probeUsage = func(context.Context, *auth.Account) error { calls.Add(1); return nil }
	if !h.store.ImportUsageProbeEnabled() {
		t.Fatal("import sampling must default to enabled")
	}
	h.triggerImportedAccountUsageProbe(account.ID(), "import")
	drainImportProbeQueue(t, h)
	if calls.Load() != 1 {
		t.Fatalf("enabled probe calls = %d, want 1", calls.Load())
	}
	h.store.SetImportUsageProbeEnabled(false)
	h.triggerImportedAccountUsageProbe(account.ID(), "import")
	h.probeImportedAccountUsage(t.Context(), account.ID(), "import_refresh")
	drainImportProbeQueue(t, h)
	if calls.Load() != 1 {
		t.Fatalf("disabled import still probed: %d", calls.Load())
	}
	// Explicit refresh remains independent of the import-only setting.
	if err := h.usageProbeFunc()(t.Context(), account); err != nil || calls.Load() != 2 {
		t.Fatalf("manual probe changed: calls=%d err=%v", calls.Load(), err)
	}
}

func TestImportUsageProbeQueuedDisableAndNonBlocking(t *testing.T) {
	h, account := newImportUsageProbeFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h.probeUsage = func(context.Context, *auth.Account) error { calls.Add(1); return nil }
	h.runImportProbeTask(func(ctx context.Context) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	waitImportProbeSignal(t, started)
	h.triggerImportedAccountUsageProbe(account.ID(), "import")
	if calls.Load() != 0 {
		t.Fatal("sampling bypassed the bounded queue")
	}
	h.store.SetImportUsageProbeEnabled(false)
	close(release)
	drainImportProbeQueue(t, h)
	if calls.Load() != 0 {
		t.Fatal("queued sampling ignored a settings change")
	}
}

func TestImportUsageProbeInvalidatesSnapshotAndReleasesFlight(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			h, account := newImportUsageProbeFixture(t)
			h.accountListCache = map[string]*accountListSnapshot{"codex": {ExpiresAt: time.Now().Add(time.Hour)}}
			h.probeUsage = func(context.Context, *auth.Account) error {
				if fail {
					return errors.New("synthetic provider failure")
				}
				account.SetUsagePercent7d(1)
				return nil
			}
			h.triggerImportedAccountUsageProbe(account.ID(), "import")
			drainImportProbeQueue(t, h)
			h.accountListCacheMu.RLock()
			remaining := len(h.accountListCache)
			h.accountListCacheMu.RUnlock()
			if remaining != 0 {
				t.Fatal("import probe left stale account filters")
			}
			if !account.TryBeginUsageProbe() {
				t.Fatal("import probe leaked the in-flight flag")
			}
			account.FinishUsageProbe()
			if account.RuntimeStatus() != "active" {
				t.Fatalf("probe failure changed import state: %s", account.RuntimeStatus())
			}
		})
	}
}

func TestImportUsageProbeSkipsInFlightAndUnsupportedRelay(t *testing.T) {
	h, account := newImportUsageProbeFixture(t)
	var calls atomic.Int32
	h.probeUsage = func(context.Context, *auth.Account) error { calls.Add(1); return nil }
	if !account.TryBeginUsageProbe() {
		t.Fatal("cannot set up existing probe")
	}
	h.probeImportedAccountUsage(t.Context(), account.ID(), "import")
	account.FinishUsageProbe()
	relay := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "synthetic-api-key", Status: auth.StatusReady}
	h.store.AddAccount(relay)
	h.probeImportedAccountUsage(t.Context(), relay.ID(), "import")
	if calls.Load() != 0 {
		t.Fatal("duplicate or unsupported probe was dispatched")
	}
}

func TestImportUsageProbeSettingsRoundTrip(t *testing.T) {
	previousRuntime := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previousRuntime) })
	h, db := newTestAdminHandlerFixture(t)
	check := func(method string, body map[string]any, want bool) {
		t.Helper()
		w := invokeAdminSettingsEndpoint(t, h, method, body)
		if w.Code != http.StatusOK {
			t.Fatalf("settings status=%d body=%s", w.Code, w.Body.String())
		}
		var response map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response["import_usage_probe_enabled"] != want {
			t.Fatalf("import_usage_probe_enabled=%v, want %v", response["import_usage_probe_enabled"], want)
		}
		if h.store.ImportUsageProbeEnabled() != want {
			t.Fatal("runtime setting not applied")
		}
	}
	check(http.MethodGet, nil, true)
	check(http.MethodPut, map[string]any{"import_usage_probe_enabled": false}, false)
	check(http.MethodPut, map[string]any{"site_name": "Import probe fixture"}, false)
	stored, err := db.GetSystemSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reloaded := auth.NewStore(db, nil, stored)
	t.Cleanup(reloaded.Stop)
	if reloaded.ImportUsageProbeEnabled() {
		t.Fatal("disabled setting was lost after runtime reload")
	}
	check(http.MethodPut, map[string]any{"import_usage_probe_enabled": true}, true)
}

func TestOAuthGatewayImportUsageProbeHonorsSettingAndDuplicates(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			h, _, _ := newOAuthGatewayAdminFixture(t)
			h.store.SetImportUsageProbeEnabled(enabled)
			h.store.SetUsageProbeConcurrency(1)
			var calls atomic.Int32
			// The proxy is local and deliberately refuses traffic, so the independent
			// legacy Daybreak follow-up cannot reach a real upstream in this fixture.
			proxyURL := "http://127.0.0.1:1"
			if _, err := h.db.InsertProxy(t.Context(), proxyURL, "synthetic-egress"); err != nil {
				t.Fatal(err)
			}
			h.probeUsage = func(_ context.Context, account *auth.Account) error {
				calls.Add(1)
				account.SetUsagePercent7d(1)
				return nil
			}
			raw := json.RawMessage(`{"platform":"openai","type":"oauth","name":"fixture","credentials":{"email":"import-probe@example.invalid","access_token":"synthetic-at","refresh_token":"synthetic-rt","chatgpt_user_id":"user-fixture","chatgpt_account_id":"workspace-fixture","plan_type":"plus"}}`)
			result, err := (oauthGatewayTarget{h}).Import(t.Context(), raw, nil, proxyURL)
			if err != nil || result.Success != 1 || len(result.CreatedIDs) != 1 {
				t.Fatalf("import: %+v %v", result, err)
			}
			drainImportProbeQueue(t, h)
			want := int32(0)
			if enabled {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("probe calls=%d, want %d", calls.Load(), want)
			}
			duplicate, duplicateErr := (oauthGatewayTarget{h}).Import(t.Context(), raw, nil, proxyURL)
			if duplicateErr == nil || duplicate.Failed != 1 {
				t.Fatalf("duplicate: %+v %v", duplicate, duplicateErr)
			}
			drainImportProbeQueue(t, h)
			if calls.Load() != want {
				t.Fatal("duplicate import sampled again")
			}
			row, err := h.db.GetAccountByID(t.Context(), result.CreatedIDs[0])
			if err != nil || row.Status != "active" {
				t.Fatalf("persisted import: %+v %v", row, err)
			}
			item := h.buildAccountListSnapshotItem(row, nil, nil, nil, nil)
			if accountListNormal(item) != enabled {
				t.Fatalf("normal filter=%v, want %v", accountListNormal(item), enabled)
			}
		})
	}
}

func TestImportUsageProbeWarmupKeepsCredentialInitialization(t *testing.T) {
	h, account := newImportUsageProbeFixture(t)
	h.store.SetImportUsageProbeEnabled(false)
	account.Mu().Lock()
	account.AccessToken = ""
	account.RefreshToken = "synthetic-rt"
	account.Mu().Unlock()
	var refreshes, probes atomic.Int32
	h.refreshAccount = func(context.Context, int64) error {
		refreshes.Add(1)
		account.Mu().Lock()
		account.AccessToken = "synthetic-at"
		account.Mu().Unlock()
		return nil
	}
	h.probeUsage = func(context.Context, *auth.Account) error { probes.Add(1); return nil }
	h.scheduleImportedAccountWarmup(account, account.ID(), "manual_add")
	drainImportProbeQueue(t, h)
	if probes.Load() != 0 || refreshes.Load() != 1 {
		t.Fatalf("initialization=%d probes=%d, want 1/0", refreshes.Load(), probes.Load())
	}
}
