package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/oauthgateway/service"
	"github.com/codex2api/internal/oauthgateway/upstream"
)

func TestOAuthGatewayRecoveryAdminContract(t *testing.T) {
	h, r, state := newOAuthGatewayAdminFixture(t)
	response := oauthGatewayRequest(r, "GET", "recovery", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"data":[]`) {
		t.Fatal("empty recovery contract", response.Code, response.Body.String())
	}
	response = oauthGatewayRequest(r, "GET", "settings", "")
	if !strings.Contains(response.Body.String(), `"auto_recovery_supported":true`) {
		t.Fatal("controlled recovery support not exposed")
	}
	for _, body := range []string{`{"recovery_account_id":42,"group_ids":[1]}`, `{"recovery_account_id":42,"runtime_proxy":"http://fixture.invalid:80"}`} {
		for _, endpoint := range []string{"preview", "jobs"} {
			response = oauthGatewayRequest(r, "POST", endpoint, body)
			if response.Code != 400 || !strings.Contains(response.Body.String(), "invalid_recovery_destination") {
				t.Fatal("repair destination accepted", response.Code)
			}
		}
	}
	if err := state.Update(func(s *service.State) error {
		s.Recoveries = []service.Recovery{{Snapshot: service.RecoverySnapshot{AccountID: 42, Identifier: "fixture@example.invalid"}, Enabled: true, Epoch: 1, State: "ready", Material: upstream.Account{Identifier: "fixture@example.invalid", Password: "secret-password", TOTPSecret: "JBSWY3DPEHPK3PXP"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response = oauthGatewayRequest(r, "GET", "recovery", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "fixture@example.invalid") || strings.Contains(response.Body.String(), "secret-password") || strings.Contains(response.Body.String(), "JBSWY3DPEHPK3PXP") {
		t.Fatal("unsafe recovery view")
	}
	response = oauthGatewayRequest(r, "POST", "recovery/42/disable", `{}`)
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("disable contract", response.Code)
	}
	saved, _ := state.View()
	if saved.Recoveries[0].Enabled || saved.Recoveries[0].Material.Password != "" || saved.Recoveries[0].Material.TOTPSecret != "" {
		t.Fatal("disable did not erase material")
	}
	response = oauthGatewayRequest(r, "POST", "recovery/42/disable", `{"unexpected":true}`)
	if response.Code != 400 {
		t.Fatal("disable accepted unknown fields")
	}
	h.oauthGateway = nil
	h.oauthGatewayInitErr = database.ErrOAuthGatewayEncryption
	response = oauthGatewayRequest(r, "GET", "recovery", "")
	if response.Code != 503 {
		t.Fatal("unavailable recovery lost 503 contract")
	}
}

func TestOAuthGatewayRecoveryNativeIdentityCASAndNoUpsert(t *testing.T) {
	h, _, _ := newOAuthGatewayAdminFixture(t)
	ctx := t.Context()
	group, err := h.db.CreateAccountGroup(ctx, "original-group", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	h.store.SetUsageProbeConcurrency(1)
	credentials := map[string]any{"email": "fixture@example.invalid", "user_id": "user-original", "workspace_id": "workspace-original", "account_id": "workspace-original", "refresh_token": "old-rt", "access_token": "old-at", "expires_at": "2099-01-01T00:00:00Z", "plan_type": "plus"}
	id, err := h.db.InsertOAuthGatewayAccount(ctx, "original", credentials, "http://original.invalid:80", []int64{group}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	originalRuntime := h.store.FindByID(id)
	originalRuntime.TotalRequests.Store(17)
	h.store.MarkCooldownWithErrorExactDuration(originalRuntime, time.Hour, "unauthorized", "synthetic 401")
	channel := database.UpstreamChannelCodex
	before, err := h.getAccountListSnapshot(ctx, channel)
	if err != nil || len(before.Items) != 1 || before.Items[0].Status != "unauthorized" || before.Items[0].PlanType != "plus" {
		t.Fatal("initial unauthorized snapshot missing", err)
	}
	started, release := make(chan struct{}), make(chan struct{}, 1)
	defer close(release)
	var probes atomic.Int32
	h.probeUsage = func(probeCtx context.Context, acc *auth.Account) error {
		if probes.Add(1) == 1 {
			close(started)
		}
		if acc != originalRuntime || acc.GetAccessToken() != "new-AT" || acc.RuntimeStatus() != "active" {
			t.Error("probe did not use the recovered runtime account")
		}
		select {
		case <-release:
			acc.SetUsagePercent7d(1)
		case <-probeCtx.Done():
			return probeCtx.Err()
		}
		return nil
	}
	target := oauthGatewayTarget{h}
	snap, err := target.RecoverySnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	account := json.RawMessage(`{"platform":"openai","type":"oauth","name":"must-not-replace","credentials":{"email":"fixture@example.invalid","access_token":"new-AT","refresh_token":"new-RT","chatgpt_user_id":"user-original","chatgpt_account_id":"workspace-original","plan_type":"self_serve_business_prolite"},"extra":{"oauth_workspace":{"id":"workspace-original","name":"Recovered Workspace"}}}`)
	wrongMetadata := json.RawMessage(strings.Replace(string(account), `"id":"workspace-original"`, `"id":"wrong-workspace"`, 1))
	if oauthGatewayWorkspaceName(wrongMetadata, snap.Identity.ChatGPTAccountID) != "" {
		t.Fatal("accepted a name from a different workspace")
	}
	wrong := json.RawMessage(strings.ReplaceAll(string(account), "workspace-original", "wrong-workspace"))
	if result, err := target.Recover(ctx, snap, wrong, "wrong", true); err == nil || result.Failed != 1 {
		t.Fatal("workspace mismatch accepted")
	}
	result, err := target.Recover(ctx, snap, account, "repair", false)
	if err != nil || result.Updated != 1 || result.Success != 0 {
		t.Fatal("native recovery failed", err)
	}
	waitImportProbeSignal(t, started)
	ready, err := h.getAccountListSnapshot(ctx, channel)
	if err != nil || len(ready.Items) != 1 || ready.Items[0].Status != "active" || ready.Items[0].PlanType != "self_serve_business_prolite" || accountListNormal(ready.Items[0]) {
		t.Fatal("first list refresh must see recovered state and plan without faking usage", err)
	}
	if _, ok, err := h.cache.GetRuntime(ctx, "account-cooldown", strconv.FormatInt(id, 10)); err != nil || ok {
		t.Fatal("stale 401 cache survived recovery", err)
	}
	release <- struct{}{}
	drainImportProbeQueue(t, h)
	sampled, err := h.getAccountListSnapshot(ctx, channel)
	filter := accountPageQuery{Status: "normal", Plan: "team", GroupInclude: []int64{group}}
	if err != nil || len(sampled.Items) != 1 || !accountListItemMatches(sampled.Items[0], filter, channel) || probes.Load() != 1 {
		t.Fatal("recovered account did not enter normal + team + original group after sampling", err)
	}
	after, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := h.db.GetAccountGroupIDs(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "original" || after.ProxyURL != "http://original.invalid:80" || len(groups) != 1 || groups[0] != group || h.store.FindByID(id) != originalRuntime || originalRuntime.TotalRequests.Load() != 17 {
		t.Fatal("recovery changed routing/history/runtime identity")
	}
	if after.GetCredential("access_token") != "new-AT" || h.store.FindByID(id).GetCredentialGeneration() != snap.Generation+1 {
		t.Fatal("recovered credentials not published")
	}
	if after.GetCredential("workspace_name") != "Recovered Workspace" {
		t.Fatal("recovery did not save the verified workspace name")
	}
	if result, err = target.Recover(ctx, snap, account, "stale", false); err == nil || result.Failed != 1 {
		t.Fatal("stale recovery overwrote new credentials")
	}
	if result, err = target.Import(ctx, account, nil, "", false); err == nil || result.Failed != 1 {
		t.Fatal("ordinary import became an overwrite")
	}
	if err = h.db.SoftDeleteAccount(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = target.RecoverySnapshot(ctx, id); err == nil {
		t.Fatal("deleted account accepted for repair")
	}
}

func TestOAuthGatewayRecoveryRefresh401AndConcurrentFences(t *testing.T) {
	for _, scenario := range []string{"success", "new-429", "generation", "deleted", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			h, _, _ := newOAuthGatewayAdminFixture(t)
			h.store.SetUsageProbeConcurrency(1)
			var samples atomic.Int32
			h.probeUsage = func(_ context.Context, acc *auth.Account) error {
				samples.Add(1)
				if acc.GetAccessToken() != "new-at" {
					t.Error("refresh follow-up used stale credentials")
				}
				acc.SetUsagePercent7d(1)
				return nil
			}
			ctx := context.Background()
			credentials := map[string]any{"email": "fixture@example.invalid", "user_id": "user-original", "workspace_id": "workspace-original", "account_id": "workspace-original", "refresh_token": "old-rt", "access_token": "old-at"}
			id, err := h.db.InsertOAuthGatewayAccount(ctx, "original", credentials, "", nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if err = h.store.LoadAccountByID(ctx, id); err != nil {
				t.Fatal(err)
			}
			acc := h.store.FindByID(id)
			atomic.StoreInt32(&acc.Disabled, 1)
			h.store.MarkCooldownWithErrorExactDuration(acc, 5*time.Minute, "unauthorized", "synthetic 401")
			snap, err := (oauthGatewayTarget{h}).RecoverySnapshot(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch scenario {
				case "new-429":
					h.store.MarkCooldownWithErrorExactDuration(acc, time.Hour, "rate_limited", "new 429")
				case "generation":
					if e := h.db.UpdateCredentials(ctx, id, map[string]any{"access_token": "manual-winner"}); e != nil {
						t.Error(e)
					}
				case "deleted":
					if e := h.db.SoftDeleteAccount(ctx, id); e != nil {
						t.Error(e)
					}
				case "disabled":
					if e := h.db.SetAccountEnabled(ctx, id, false); e != nil {
						t.Error(e)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"new-at","refresh_token":"new-rt","expires_in":3600}`))
			}))
			defer provider.Close()
			old := auth.ResinRequestDecorator
			auth.ResinRequestDecorator = func(string, string) string { return provider.URL }
			defer func() { auth.ResinRequestDecorator = old }()
			eligible, err := (oauthGatewayTarget{h}).RefreshBeforeRecovery(ctx, snap)
			if calls.Load() != 1 || eligible {
				t.Fatal("wrong refresh path", calls.Load(), eligible, err)
			}
			drainImportProbeQueue(t, h)
			if scenario == "success" && samples.Load() != 1 || err != nil && samples.Load() != 0 {
				t.Fatal("invalid post-refresh sampling", scenario, samples.Load(), err)
			}
			row, e := h.db.GetAccountByIDIncludingDeleted(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			switch scenario {
			case "success":
				if err != nil || atomic.LoadInt32(&acc.Disabled) != 0 || acc.RuntimeStatus() != "active" || row.CooldownReason != "" || row.CooldownUntil.Valid {
					t.Fatalf("successful refresh remains blocked: err=%v runtime=%s db=%s", err, acc.RuntimeStatus(), row.CooldownReason)
				}
			case "new-429":
				if err != nil || row.CooldownReason != "rate_limited" || !acc.HasActiveCooldown() {
					t.Fatal("concurrent 429 erased", err)
				}
			default:
				if err == nil || atomic.LoadInt32(&acc.Disabled) == 0 {
					t.Fatal("stale completion re-enabled account", scenario, err)
				}
			}
		})
	}
}
