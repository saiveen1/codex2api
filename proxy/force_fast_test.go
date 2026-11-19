package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestForceFastDispatchAndAccounting(t *testing.T) {
	withPayloadRules(t, `{"override":[{"params":{"service_tier":"ultrafast"}}]}`)
	previousResin, previousWS := resinCfg.Load(), WebsocketExecuteFunc
	t.Cleanup(func() { resinCfg.Store(previousResin); WebsocketExecuteFunc = previousWS })
	t.Setenv("CODEX_DISABLE_ROUTING_HINT", "")
	var wireTier, hint string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wireTier = extractServiceTier(readUpstreamRequestBody(r))
		hint = r.Header.Get(codexRoutingHintHeader)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	WebsocketExecuteFunc = func(_ context.Context, _ *auth.Account, body []byte, _, _, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		wireTier = extractServiceTier(body)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}

	account := &auth.Account{DBID: 621, AccessToken: "test"}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	h := &Handler{}
	for _, force := range []bool{true, false} {
		c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 1, Limits: database.APIKeyLimits{ForceFast: force}})
		policy := h.codexAttemptPolicy(c, h.payloadRuleIdentity(c), account)
		want := "ultrafast"
		if force {
			want = "priority"
		}
		for _, raw := range []string{`{"model":"gpt-test","input":"hello"}`, `{"model":"gpt-test","input":"hello","service_tier":"default"}`} {
			body := []byte(raw)
			if got := policy.requestedTier(body, "gpt-test", nil); got != want {
				t.Fatalf("force=%t: requested tier=%q, want %q", force, got, want)
			}
			ctx := policy.withContext(context.Background())
			resp, err := ExecuteCompactRequest(ctx, account, body, "session", "", "local", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			compactTier := extractServiceTier(body)
			if force {
				compactTier = "priority"
			}
			if wireTier != compactTier {
				t.Fatalf("compact force=%t wire=%q, want %q", force, wireTier, compactTier)
			}
			for _, ws := range []bool{false, true} {
				wireTier, hint = "", ""
				resp, err := ExecuteRequest(policy.withContext(context.Background()), account, body, "session", "", "local", nil, nil, ws)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if wireTier != want || (!ws && hint != "model=gpt-test;tier="+want) || string(body) != raw {
					t.Fatalf("force=%t ws=%t wire=%q hint=%q body=%s", force, ws, wireTier, hint, body)
				}
			}
		}
		if h.codexAttemptPolicy(c, nil, &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "test"}).forcesFast() {
			t.Fatal("Codex tier policy leaked to a relay account")
		}
	}
}

func TestForceFastReevaluatesSelectedAccount(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	h := &Handler{store: store}
	groupAccount := &auth.Account{DBID: 1, GroupIDs: []int64{2, 7}}
	other := &auth.Account{DBID: 2, GroupIDs: []int64{3}}
	store.AddAccount(other)
	store.SetGroupForceFast(7, true)
	base := &PayloadRuleIdentity{APIKeyID: 9, GroupIDs: []int64{7}}
	if !h.codexAttemptPolicy(nil, nil, groupAccount).forcesFast() || h.codexAttemptPolicy(nil, base, other).forcesFast() {
		t.Fatal("match any actual account group, not the groups a key is allowed to use")
	}
	store.ApplyAccountForceFast(other.DBID, true)
	store.SetGroupForceFast(7, false)
	if h.codexAttemptPolicy(nil, base, groupAccount).forcesFast() || !h.codexAttemptPolicy(nil, nil, other).forcesFast() {
		t.Fatal("a retry must use the selected account's own current settings")
	}
	store.ApplyAccountForceFast(other.DBID, false)
	if h.codexAttemptPolicy(nil, base, other).forcesFast() {
		t.Fatal("disabling the last scope must restore normal tier selection")
	}
}

func TestForceFastPrecedence(t *testing.T) {
	withPayloadRules(t, `{"override":[{"params":{"service_tier":"ultrafast"}}]}`)
	store := auth.NewStore(nil, nil, nil)
	h := &Handler{store: store}
	account := &auth.Account{DBID: 1, GroupIDs: []int64{2, 7}}
	store.AddAccount(account)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := []byte(`{"model":"gpt-test","service_tier":"default"}`)
	for _, tc := range []struct {
		key, group, account  bool
		wantSource, wantTier string
	}{
		{false, false, false, "", "ultrafast"},
		{false, false, true, "account", "priority"},
		{false, true, false, "group", "priority"},
		{false, true, true, "group", "priority"},
		{true, false, false, "key", "priority"},
		{true, false, true, "key", "priority"},
		{true, true, false, "key", "priority"},
		{true, true, true, "key", "priority"},
	} {
		c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 1, Limits: database.APIKeyLimits{ForceFast: tc.key}})
		store.SetGroupForceFast(7, tc.group)
		store.ApplyAccountForceFast(account.DBID, tc.account)
		policy := h.codexAttemptPolicy(c, nil, account)
		if tier := policy.requestedTier(body, "gpt-test", nil); policy.forceFastSource != tc.wantSource || tier != tc.wantTier {
			t.Errorf("key=%t group=%t account=%t: source=%q tier=%q, want %q %q", tc.key, tc.group, tc.account, policy.forceFastSource, tier, tc.wantSource, tc.wantTier)
		}
	}
}
