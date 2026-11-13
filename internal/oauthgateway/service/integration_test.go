package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

func TestSettingsDefaultMaskAndManualOnly(t *testing.T) {
	f := newPair(t)
	store := &memoryStore{state: State{Version: 1}}
	a, err := New(store, f, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	v, err := a.Settings()
	if err != nil || v.OAuthURL != DefaultOAuthURL || v.Configured || v.OAuthTokenSet || v.AutoRecoverySupported {
		t.Fatal("incorrect unconfigured defaults")
	}
	if !strings.Contains(v.AutoRecoveryReason, "manual import only") || !strings.Contains(v.AutoRecoveryReason, "no automatic replacement") {
		t.Fatal("manual-only reason missing")
	}
	v, err = a.SaveSettings(Settings{OAuthToken: "synthetic-cdk"})
	if err != nil || !v.Configured || !v.OAuthTokenSet || v.OAuthURL != "https://oauth.cala.lol" {
		t.Fatal("default host token configuration failed")
	}
	raw, _ := json.Marshal(v)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if len(fields) != 5 || bytes.Contains(raw, []byte("synthetic-cdk")) || fields["oauth_token"] != nil {
		t.Fatal("settings exposed credentials or old remote-target fields")
	}
	if _, err = a.SaveSettings(Settings{OAuthURL: "https://other.example.invalid"}); err == nil {
		t.Fatal("URL changed without new token")
	}
	state, _ := store.View()
	if state.Settings.OAuthURL != DefaultOAuthURL || state.Settings.OAuthToken != "synthetic-cdk" {
		t.Fatal("rejected URL change mutated settings")
	}
	for _, token := range []string{"bad\r\nheader", "bad\x00token", strings.Repeat("x", 4097)} {
		if _, err := a.SaveSettings(Settings{OAuthToken: token}); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
	if groups, err := a.Groups(context.Background()); err != nil || len(groups) != 1 {
		t.Fatal("local target required remote admin settings")
	}
}
func TestDestinationChangesNeverRebindExistingJobOrOldToken(t *testing.T) {
	f := newPair(t)
	a, store, _ := configuredApp(t, f)
	in := exampleInput("pinned-destination")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	var newHostCalls int
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newHostCalls++
		if r.Header.Get("Authorization") != "Bearer replacement-cdk" {
			t.Error("old key sent to new host")
		}
		if r.URL.Path != "/prefix/api/v1/oauth/capabilities" {
			t.Error("base path changed")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": capabilities()})
	}))
	defer next.Close()
	if _, err := a.SaveSettings(Settings{OAuthURL: next.URL + "/prefix"}); err == nil {
		t.Fatal("new host reused old token")
	}
	if newHostCalls != 0 {
		t.Fatal("saving settings contacted new host")
	}
	if _, err := a.SaveSettings(Settings{OAuthURL: next.URL + "/prefix/", OAuthToken: "replacement-cdk"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SaveSettings(Settings{}); err != nil {
		t.Fatal("masked update failed")
	}
	if _, err := a.Capabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if newHostCalls != 1 {
		t.Fatal("unexpected connection check count")
	}
	state, _ := store.View()
	if state.Settings.OAuthURL != next.URL+"/prefix" || state.Jobs[0].Settings.OAuthURL != f.server.URL || state.Jobs[0].Settings.OAuthToken != "synthetic-oauth" {
		t.Fatal("job destination was rebound")
	}
	v := drive(t, a, in.RequestID)
	if v.Imported != 1 || f.submits != 1 || newHostCalls != 1 {
		t.Fatal("original job did not retain its gateway")
	}
}
func TestSaveCredentialsRejectedBeforeRemoteWork(t *testing.T) {
	f := newPair(t)
	a, store, _ := configuredApp(t, f)
	in := exampleInput("no-reusable-login")
	in.SaveCredentials = true
	if _, err := a.Preview(context.Background(), in); err == nil {
		t.Fatal("preview allowed credential retention")
	}
	if _, _, err := a.Create(in); err == nil {
		t.Fatal("create allowed credential retention")
	}
	state, _ := store.View()
	if len(state.Jobs) != 0 || f.submits != 0 || f.imports != 0 {
		t.Fatal("rejected retention caused work")
	}
}
func TestRevision3AndRequiredCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*upstream.Capabilities)
	}{
		{"name", func(c *upstream.Capabilities) { c.Protocol.Name = "other" }},
		{"major", func(c *upstream.Capabilities) { c.Protocol.Version = 2 }},
		{"revision", func(c *upstream.Capabilities) { c.Protocol.Revision = 2 }},
		{"mapping", func(c *upstream.Capabilities) { delete(c.Features, "result_item_mapping") }},
		{"lookup", func(c *upstream.Capabilities) { delete(c.Features, "request_id_lookup") }},
		{"method", func(c *upstream.Capabilities) { c.OAuthMethods = []string{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPair(t)
			a, _, _ := configuredApp(t, f)
			useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/oauth/capabilities" {
					c := capabilities()
					tc.mutate(&c)
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": c})
					return
				}
				f.handle(w, r)
			}))
			if _, err := a.Capabilities(context.Background()); err == nil {
				t.Fatal("incompatible capabilities accepted")
			}
			in := exampleInput("contract")
			if _, err := a.Preview(context.Background(), in); err == nil {
				t.Fatal("preview accepted incompatible contract")
			}
			if _, _, err := a.Create(in); err != nil {
				t.Fatal(err)
			}
			v := drive(t, a, in.RequestID)
			if v.State != "failed" || f.submits != 0 || f.imports != 0 {
				t.Fatal("incompatible contract authorized")
			}
		})
	}
	c := capabilities()
	c.Protocol.Revision = 4
	if err := compatible(c); err != nil {
		t.Fatal("backward-compatible revision rejected")
	}
}
func TestConnectionsKeepsLocalCodexCompatibilityShape(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	v, err := a.Connections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o, c := v["oauth"].(map[string]any), v["codex"].(map[string]any)
	if o["ok"] != true || o["capabilities"] == nil || o["quota"] == nil || c["ok"] != true || c["groups"] == nil {
		t.Fatal("connection shape changed")
	}
	useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { readError(w, 401, "unauthorized", "authenticate") }))
	v, err = a.Connections(context.Background())
	if err != nil || v["oauth"].(map[string]any)["ok"] != false || v["codex"].(map[string]any)["ok"] != true {
		t.Fatal("OAuth auth failure disabled local target")
	}
}
func TestLocalImportGetsOnlyExplicitGroupsAndRuntimeProxy(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	in := exampleInput("local-parameters")
	in.GroupIDs = []int64{1, 1}
	in.RuntimeProxy = "http://runtime-proxy.example.invalid:8080"
	in.Options.ProxyMode = "custom"
	in.Options.Proxy = json.RawMessage(`"http://oauth-proxy.example.invalid:8080"`)
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if v.Imported != 1 || !reflect.DeepEqual(f.importGroups, [][]int64{{1}}) || !reflect.DeepEqual(f.importProxies, []string{in.RuntimeProxy}) {
		t.Fatal("local import parameters changed")
	}
}
func TestDurableCheckpointFailurePreventsSideEffects(t *testing.T) {
	for _, checkpoint := range []string{"submitting", "importing"} {
		t.Run(checkpoint, func(t *testing.T) {
			f := newPair(t)
			a, store, _ := configuredApp(t, f)
			in := exampleInput("checkpoint")
			if _, _, err := a.Create(in); err != nil {
				t.Fatal(err)
			}
			if checkpoint == "importing" {
				stepNow(t, a, in.RequestID)
				stepNow(t, a, in.RequestID)
			}
			store.beforeCommit = func(s *State) error {
				g := s.Jobs[0].Groups[0]
				if g.Stage == checkpoint || (len(g.Deliveries) > 0 && g.Deliveries[0].State == checkpoint) {
					return errors.New("synthetic durable write failure")
				}
				return nil
			}
			if _, err := a.Step(context.Background()); err == nil {
				t.Fatal("checkpoint failure ignored")
			}
			if f.imports != 0 || (checkpoint == "submitting" && f.submits != 0) {
				t.Fatal("side effect preceded durable checkpoint")
			}
		})
	}
}
func TestRestartAfterSubmitAcknowledgementCommitFailure(t *testing.T) {
	f := newPair(t)
	a, store, target := configuredApp(t, f)
	in := exampleInput("lost-commit")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	store.beforeCommit = func(s *State) error {
		if s.Jobs[0].Groups[0].Batch.BatchID != "" {
			return errors.New("synthetic acknowledgement commit failure")
		}
		return nil
	}
	if _, err := a.Step(context.Background()); err == nil {
		t.Fatal("acknowledgement commit failure ignored")
	}
	state, _ := store.View()
	if f.submits != 1 || state.Jobs[0].Groups[0].Stage != "submitting" {
		t.Fatal("submission checkpoint missing")
	}
	_ = a.Close()
	reopened, err := New(store.reopen(t), target, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v := drive(t, reopened, in.RequestID)
	if v.Imported != 1 || f.submits != 1 || f.lookups != 1 {
		t.Fatal("lost commit replayed authorization")
	}
}
func TestRestartAfterImportOutcomeCommitFailure(t *testing.T) {
	f := newPair(t)
	a, store, target := configuredApp(t, f)
	in := exampleInput("import-commit")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	stepNow(t, a, in.RequestID)
	stepNow(t, a, in.RequestID)
	f.onImport = func() {
		state, err := store.View()
		if err != nil || state.Jobs[0].Groups[0].Deliveries[0].State != "importing" {
			t.Error("import checkpoint not durable")
		}
	}
	store.beforeCommit = func(s *State) error {
		if s.Jobs[0].Groups[0].Deliveries[0].State == "imported" {
			return errors.New("synthetic import outcome commit failure")
		}
		return nil
	}
	if _, err := a.Step(context.Background()); err == nil {
		t.Fatal("import outcome failure ignored")
	}
	_ = a.Close()
	reopened, err := New(store.reopen(t), target, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v, err := reopened.Job(in.RequestID)
	if err != nil || v.State != "needs_review" || v.CanRetryImport || v.Groups[0].Items[0].ImportState != "import_unknown" {
		t.Fatal("restart did not reconcile importing to unknown")
	}
	if _, err := reopened.Action(in.RequestID, "refresh"); err != nil {
		t.Fatal(err)
	}
	drive(t, reopened, in.RequestID)
	if f.imports != 1 || f.submits != 1 {
		t.Fatal("restart replayed import")
	}
}
func TestConstructorStateVersionsAndLegacyRetention(t *testing.T) {
	f := newPair(t)
	for _, state := range []State{{Version: 0}, {Version: 3}, {Version: 1, Archived: []ArchivedJob{{ID: "bad"}}}, {Version: 1, Settings: Settings{OAuthToken: "orphaned-cdk"}}} {
		store := &memoryStore{state: state}
		if _, err := New(store, f, false); err == nil || !store.closed {
			t.Fatal("invalid state accepted or failed store left open")
		}
	}
	for _, version := range []int{1, 2} {
		settings := Settings{OAuthURL: "https://configured.example.invalid", OAuthToken: "synthetic-cdk"}
		request := upstream.SubmitRequest{Accounts: []upstream.Account{{Password: "legacy-password", TOTPSecret: "JBSWY3DPEHPK3PXP"}}}
		group := Group{Stage: "cancelled", Lines: []int{1}, Masks: []string{"***"}, Request: request}
		job := Job{ID: "legacy", SaveCredentials: true, Settings: settings, Groups: []Group{group}}
		store := &memoryStore{state: State{Version: version, Settings: settings, Jobs: []Job{job}}}
		a, err := New(store, f, false)
		if err != nil {
			t.Fatal(err)
		}
		state, _ := store.View()
		if state.Version != version || state.Settings.OAuthURL != "https://configured.example.invalid" || state.Jobs[0].SaveCredentials || hasRetainedMaterial(state.Jobs[0]) || state.Jobs[0].Settings.OAuthToken != "" {
			t.Fatal("constructor changed host/version or retained reusable login")
		}
		_ = a.Close()
	}
}

type summaryTarget struct {
	Target
	result upstream.ImportResult
	err    error
}

func (s summaryTarget) Import(context.Context, json.RawMessage, []int64, string) (upstream.ImportResult, error) {
	return s.result, s.err
}
func TestInjectedImportSummaryValidation(t *testing.T) {
	known := &upstream.Error{Code: "upstream_error", Source: "execution", Action: "fix_configuration"}
	for _, tc := range []struct {
		name   string
		result upstream.ImportResult
		err    error
		want   string
	}{
		{"created", upstream.ImportResult{Total: 1, Success: 1, CreatedIDs: []int64{9}}, nil, "imported"},
		{"updated-summary-compatibility", upstream.ImportResult{Total: 1, Updated: 1}, nil, "imported"},
		{"duplicate", upstream.ImportResult{Total: 1, Duplicate: 1}, nil, "imported"},
		{"failed", upstream.ImportResult{Total: 1, Failed: 1}, known, "import_failed"},
		{"missing-summary", upstream.ImportResult{}, nil, "import_unknown"},
		{"mixed", upstream.ImportResult{Total: 1, Success: 1, Failed: 1}, known, "import_unknown"},
		{"negative", upstream.ImportResult{Total: 1, Success: 2, Updated: -1}, nil, "import_unknown"},
		{"invalid-id", upstream.ImportResult{Total: 1, Success: 1, CreatedIDs: []int64{0}}, nil, "import_unknown"},
		{"generic-even-with-success", upstream.ImportResult{Total: 1, Success: 1}, errors.New("synthetic-token"), "import_unknown"},
		{"unconfirmed-failed", upstream.ImportResult{Total: 1, Failed: 1}, nil, "import_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPair(t)
			a, _, _ := configuredApp(t, f)
			a.target = summaryTarget{Target: f, result: tc.result, err: tc.err}
			in := exampleInput("summary")
			if _, _, err := a.Create(in); err != nil {
				t.Fatal(err)
			}
			v := drive(t, a, in.RequestID)
			if v.Groups[0].Items[0].ImportState != tc.want {
				t.Fatalf("got %s, want %s", v.Groups[0].Items[0].ImportState, tc.want)
			}
		})
	}
}
func TestRunHonorsCanceledContext(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx); err != nil || f.submits != 0 || f.imports != 0 {
		t.Fatal("canceled worker performed work")
	}
}
