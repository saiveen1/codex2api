package auth

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/codex2api/database"
	ic "github.com/codex2api/internal/intelligencecheck"
)

func autoWebDB(t *testing.T) (*database.DB, int64) {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "web-auto.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	group, err := db.CreateAccountGroup(t.Context(), "auto-web", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	return db, group
}

func autoWebAccount(t *testing.T, db *database.DB, group int64, extra map[string]any) int64 {
	t.Helper()
	creds := map[string]any{"access_token": "auto-test-token", "account_id": "auto-workspace", "plan_type": "free"}
	for k, v := range extra {
		creds[k] = v
	}
	id, err := db.InsertAccountWithCredentials(t.Context(), "auto-web", creds, "")
	if err != nil {
		t.Fatal(err)
	}
	if group > 0 {
		if err := db.SetAccountGroups(t.Context(), id, []int64{group}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func autoWebStates(t *testing.T, db *database.DB) map[int64]database.ChatGPTWebAccountState {
	t.Helper()
	rows, err := ListChatGPTWebAccountStates(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]database.ChatGPTWebAccountState{}
	for _, row := range rows {
		out[row.AccountID] = row
	}
	return out
}

func TestChatGPTWebAutoEnableExistingKeyAndFutureMembers(t *testing.T) {
	db, group := autoWebDB(t)
	ctx := t.Context()
	id := autoWebAccount(t, db, group, nil)
	if got := autoWebStates(t, db); len(got) != 0 {
		t.Fatal("unbound group enabled", got)
	}
	key, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "web", Key: "auto-test-key", AllowedGroupIDs: []int64{group}, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb}})
	if err != nil {
		t.Fatal(err)
	}
	states := autoWebStates(t, db)
	s := states[id]
	if len(states) != 1 || !s.Enabled || s.Status != "unverified" || s.ObservedAt != nil || s.ImageQuotaRemaining != nil || len(s.Capabilities) != 0 {
		t.Fatalf("default initialization invented readiness: %+v", states)
	}
	// A later member of an already configured key requires no key rewrite.
	later := autoWebAccount(t, db, group, nil)
	if got := autoWebStates(t, db); len(got) != 2 || !got[later].Enabled {
		t.Fatal("missed later member", got)
	}
	if _, err := db.SetChatGPTWebAccountEnabled(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if autoWebStates(t, db)[id].Enabled {
		t.Fatal("manual disable overridden")
	}
	// Keep an authenticated observation and environment byte-for-byte on reload.
	ready, err := db.GetChatGPTWebAccountState(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	hash := func(v string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(v))) }
	now, quota := time.Now().UTC().Add(-time.Second), 3
	ready.Status, ready.ObservedAt, ready.ImageQuotaRemaining = "ready", &now, &quota
	ready.Capabilities = []string{"generate"}
	ready.WorkspaceID = "auto-workspace"
	ready.WorkspaceFingerprint, ready.WorkspaceBindingFingerprint = hash("auto-workspace"), hash("auto-workspace")
	ready.ProxyFingerprint = hash("direct")
	if applied, err := db.SaveChatGPTWebAccountObservation(ctx, ready, "auto-test-token"); err != nil || !applied {
		t.Fatalf("save observation: %v %v", applied, err)
	}
	before, _ := db.GetChatGPTWebAccountState(ctx, later)
	after := autoWebStates(t, db)[later]
	if !reflect.DeepEqual(*before, after) {
		t.Fatalf("observation reset: before=%+v after=%+v", *before, after)
	}
	// Read fresh scope after a key is moved; no scheduler cache is involved.
	other, err := db.CreateAccountGroup(ctx, "other-auto", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	moved := autoWebAccount(t, db, other, nil)
	if err := db.UpdateAPIKeyAllowedGroups(ctx, key, []int64{other}); err != nil {
		t.Fatal(err)
	}
	oldGroupNew := autoWebAccount(t, db, group, nil)
	states = autoWebStates(t, db)
	if !states[moved].Enabled {
		t.Fatal("missed updated key scope")
	}
	if _, ok := states[oldGroupNew]; ok {
		t.Fatal("used obsolete key scope")
	}
}

func TestChatGPTWebAutoEnableScopeBoundaries(t *testing.T) {
	for _, name := range []string{"bound", "native", "unrestricted", "ungrouped", "expired", "key_disabled", "images_blocked", "plan_denied", "account_key_denied", "wrong_group", "no_affinity_only", "relay", "agent_identity", "deleted", "account_disabled", "manual_disabled"} {
		t.Run(name, func(t *testing.T) {
			db, group := autoWebDB(t)
			ctx := t.Context()
			input := database.APIKeyInput{Name: "fixture", Key: "auto-scope-fixture", AllowedGroupIDs: []int64{group}, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb}}
			switch name {
			case "native":
				input.Limits.UpstreamChannel = database.UpstreamChannelCodex
			case "unrestricted":
				input.AllowedGroupIDs = nil
			case "ungrouped":
				input.AllowedGroupIDs = []int64{0}
			case "expired":
				input.ExpiresAt = sql.NullTime{Valid: true, Time: time.Now().Add(-time.Hour)}
			case "images_blocked":
				input.Limits.ImageGenerationPolicy = database.ImageGenerationPolicyBlock
			case "plan_denied":
				input.Limits.PlanAllow = []string{"plus"}
			case "no_affinity_only":
				input.AllowedGroupIDs = nil
				input.Limits.NoAffinityGroupIDs = []int64{group}
			}
			key, err := db.InsertAPIKeyWithOptions(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			extra := map[string]any{}
			switch name {
			case "account_key_denied":
				extra["allowed_api_key_ids"] = []int64{key + 100}
			case "relay":
				extra["base_url"] = "https://fixture.invalid"
			case "agent_identity":
				extra["auth_mode"] = "agentIdentity"
			}
			accountGroup := group
			if name == "wrong_group" {
				accountGroup = 0
			}
			id := autoWebAccount(t, db, accountGroup, extra)
			switch name {
			case "key_disabled":
				err = db.UpdateAPIKey(ctx, key, database.APIKeyUpdate{EnabledSet: true, Enabled: false})
			case "deleted":
				err = db.SoftDeleteAccount(ctx, id)
			case "account_disabled":
				err = db.SetAccountEnabled(ctx, id, false)
			case "manual_disabled":
				_, err = db.SetChatGPTWebAccountEnabled(ctx, id, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			states := autoWebStates(t, db)
			if states[id].Enabled != (name == "bound") {
				t.Fatalf("unexpected default: %+v", states)
			}
			if name != "bound" && name != "manual_disabled" && len(states) != 0 {
				t.Fatal("out-of-scope state created", states)
			}
		})
	}
}

func TestChatGPTWebAutoAccountEligible(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		change func(*database.AccountRow)
		want   bool
	}{
		{"active", func(r *database.AccountRow) {}, true},
		{"locked", func(r *database.AccountRow) { r.Locked = true }, false},
		{"disabled", func(r *database.AccountRow) { r.Enabled = false }, false},
		{"deleted", func(r *database.AccountRow) { r.DeletedAt = sql.NullTime{Valid: true, Time: now} }, false},
		{"error", func(r *database.AccountRow) { r.Status = "error" }, false},
		{"unknown status", func(r *database.AccountRow) { r.Status = "mystery" }, false},
		{"non oauth", func(r *database.AccountRow) { r.Type = "apikey" }, false},
		{"auth cooldown", func(r *database.AccountRow) {
			r.CooldownReason = "unauthorized"
			r.CooldownUntil = sql.NullTime{Valid: true, Time: now.Add(time.Hour)}
		}, false},
		{"unknown cooldown", func(r *database.AccountRow) { r.CooldownReason = "unknown" }, false},
		{"native quota", func(r *database.AccountRow) {
			r.CooldownReason = "rate_limited"
			r.CooldownUntil = sql.NullTime{Valid: true, Time: now.Add(time.Hour)}
		}, true},
		{"native cooldown status", func(r *database.AccountRow) { r.Status = "cooldown"; r.CooldownReason = "dispatch_count_limit" }, true},
		{"missing access", func(r *database.AccountRow) { r.Credentials["access_token"] = " " }, false},
		{"expired", func(r *database.AccountRow) { r.Credentials["expires_at"] = now.Format(time.RFC3339) }, false},
		{"expired refreshable", func(r *database.AccountRow) {
			r.Credentials["expires_at"] = now.Format(time.RFC3339)
			r.Credentials["refresh_token"] = "rt"
		}, true},
		{"expired session", func(r *database.AccountRow) {
			r.Credentials["expires_at"] = now.Format(time.RFC3339)
			r.Credentials["session_token"] = "st"
		}, true},
		{"future", func(r *database.AccountRow) { r.Credentials["expires_at"] = now.Add(time.Second).Format(time.RFC3339) }, true},
		{"bad expiry", func(r *database.AccountRow) {
			r.Credentials["expires_at"] = "bad"
			r.Credentials["refresh_token"] = "rt"
		}, false},
		{"bad workspace", func(r *database.AccountRow) { r.Credentials["account_id"] = "bad workspace" }, false},
		{"default workspace", func(r *database.AccountRow) { r.Credentials["account_id"] = "" }, true},
		{"bad workspace header", func(r *database.AccountRow) {
			r.Credentials["custom_headers"] = map[string]interface{}{"ChatGPT-Account-ID": "bad workspace"}
		}, false},
	}
	if ChatGPTWebAutoAccountEligible(nil, now) {
		t.Fatal("nil account eligible")
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := &database.AccountRow{ID: 1, Type: "oauth", Platform: "openai", Enabled: true, Status: "active", Credentials: map[string]interface{}{"access_token": "token", "account_id": "workspace"}}
			tt.change(row)
			if got := ChatGPTWebAutoAccountEligible(row, now); got != tt.want {
				t.Fatalf("eligible = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChatGPTWebAutoEnableSkipsPaused(t *testing.T) {
	db, group := autoWebDB(t)
	ctx := t.Context()
	id := autoWebAccount(t, db, group, nil)
	if _, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "web", Key: "pause-key", AllowedGroupIDs: []int64{group}, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb}}); err != nil {
		t.Fatal(err)
	}
	settings, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.Action, settings.DegradeThreshold = "pause", 1
	if _, err = db.SaveIntelligenceCheckSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = db.EnqueueIntelligenceChecks(ctx, []int64{id}, now); err != nil {
		t.Fatal(err)
	}
	run, err := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if err != nil || run == nil {
		t.Fatalf("claim: %v %v", run, err)
	}
	ok, err := db.FinishIntelligenceCheck(ctx, run.ID, "worker", ic.Observation{Complete: true, Output: "29", DurationMS: 1}, now.Add(time.Second))
	if err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	paused, err := db.IsIntelligenceCheckPaused(ctx, id)
	if err != nil || !paused {
		t.Fatalf("pause: %v %v", paused, err)
	}
	if states := autoWebStates(t, db); len(states) != 0 {
		t.Fatalf("paused account initialized: %+v", states)
	}
}

func TestChatGPTWebAutoEnableFiltersPersistedHealth(t *testing.T) {
	db, group := autoWebDB(t)
	ctx := t.Context()
	if _, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "web", Key: "health-key", AllowedGroupIDs: []int64{group}, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb}}); err != nil {
		t.Fatal(err)
	}
	good := autoWebAccount(t, db, group, nil)
	missing := autoWebAccount(t, db, group, map[string]any{"access_token": ""})
	expired := autoWebAccount(t, db, group, map[string]any{"expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339)})
	recoverable := autoWebAccount(t, db, group, map[string]any{"expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339), "refresh_token": "rt"})
	unauthorized := autoWebAccount(t, db, group, nil)
	quota := autoWebAccount(t, db, group, nil)
	if err := db.SetCooldown(ctx, unauthorized, "unauthorized", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCooldown(ctx, quota, "rate_limited", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	states := autoWebStates(t, db)
	for _, id := range []int64{good, recoverable, quota} {
		if !states[id].Enabled {
			t.Fatalf("eligible account %d missing: %+v", id, states)
		}
	}
	for _, id := range []int64{missing, expired, unauthorized} {
		if _, ok := states[id]; ok {
			t.Fatalf("ineligible account %d initialized: %+v", id, states)
		}
	}
}
