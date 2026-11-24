package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

// memoryStore deliberately models isolated snapshots and transactional failure.
// It is test-only; production encryption belongs to the host database store.
type memoryStore struct {
	mu           sync.Mutex
	state        State
	closed       bool
	beforeCommit func(*State) error
}

func cloneState(s State) (State, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return State{}, err
	}
	var copy State
	err = json.Unmarshal(raw, &copy)
	return copy, err
}
func (s *memoryStore) View() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return State{}, errors.New("store closed")
	}
	return cloneState(s.state)
}
func (s *memoryStore) Update(change func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("store closed")
	}
	next, err := cloneState(s.state)
	if err != nil {
		return err
	}
	if err = change(&next); err != nil {
		return err
	}
	if s.beforeCommit != nil {
		if err = s.beforeCommit(&next); err != nil {
			return err
		}
	}
	next, err = cloneState(next)
	if err == nil {
		s.state = next
	}
	return err
}
func (s *memoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
func (s *memoryStore) reopen(t *testing.T) *memoryStore {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	copy, err := cloneState(s.state)
	if err != nil {
		t.Fatal(err)
	}
	return &memoryStore{state: copy}
}

type fakePair struct {
	t                                       *testing.T
	mu                                      sync.Mutex
	server                                  *httptest.Server
	requests                                map[string]upstream.SubmitRequest
	batches                                 map[string]upstream.Batch
	submits, imports, checks, lookups       int
	lostSubmit, proxyFails, missingDelivery bool
	importMode                              string
	imported                                []string
	importGroups                            [][]int64
	importProxies                           []string
	onImport                                func()
}

func newPair(t *testing.T) *fakePair {
	f := &fakePair{t: t, requests: map[string]upstream.SubmitRequest{}, batches: map[string]upstream.Batch{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}
func capabilities() upstream.Capabilities {
	var c upstream.Capabilities
	c.Protocol.Name = "codex-account-gateway.oauth"
	c.Protocol.Version = 1
	c.Protocol.Revision = 3
	c.OAuthMethods = []string{"password_totp"}
	c.Features = map[string]bool{"result_item_mapping": true, "request_id_lookup": true, "proxy_check": true, "expected_identity_guard": true}
	c.Countries = []string{"US"}
	c.Environments = []string{"chrome_windows"}
	c.ProxyModes = []string{"system", "custom"}
	c.WorkspacePolicies = []string{"team_preferred"}
	c.MaxBatchSize = 100
	c.MaxUploadBytes = 1 << 20
	return c
}
func (f *fakePair) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	send := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": v}) }
	if strings.HasPrefix(r.URL.Path, "/api/v1/oauth/") && r.Header.Get("Authorization") != "Bearer synthetic-oauth" {
		f.t.Error("wrong OAuth auth header")
		w.WriteHeader(401)
		return
	}
	switch {
	case r.URL.Path == "/api/v1/oauth/capabilities":
		send(capabilities())
	case r.URL.Path == "/api/v1/oauth/quota":
		send(upstream.Quota{Total: 100, Remaining: 100})
	case r.URL.Path == "/api/v1/oauth/preview":
		var req upstream.SubmitRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			f.t.Error("bad preview")
			return
		}
		rows := []upstream.PreviewRow{}
		for i := range req.Accounts {
			rows = append(rows, upstream.PreviewRow{Line: i + 1, IdentifierMasked: "s***@example.invalid"})
		}
		send(upstream.Preview{Valid: true, Count: len(req.Accounts), Rows: rows, Errors: []upstream.PreviewIssue{}})
	case r.URL.Path == "/api/v1/oauth/proxy-check":
		f.checks++
		var raw map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		if len(raw) != 1 || raw["options"] == nil {
			f.t.Error("proxy check leaked material")
		}
		var opts map[string]any
		_ = json.Unmarshal(raw["options"], &opts)
		if _, ok := opts["workspace_policy"]; ok {
			f.t.Error("workspace policy sent to proxy check")
		}
		send(upstream.ProxyCheck{Usable: !f.proxyFails, RequestedCountry: "US", OpenAIReachable: !f.proxyFails})
	case r.URL.Path == "/api/v1/oauth/batches" && r.Method == "POST":
		f.submits++
		var req upstream.SubmitRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			f.t.Error("bad submit")
			return
		}
		id := fmt.Sprintf("gb_%032x", f.submits)
		batch := upstream.Batch{BatchID: id, RequestID: req.RequestID, State: "completed", Total: len(req.Accounts), Succeeded: len(req.Accounts), Items: []upstream.BatchItem{}}
		for i := range req.Accounts {
			batch.Items = append(batch.Items, upstream.BatchItem{ItemID: fmt.Sprintf("gi_%032x", f.submits*1000+i), Line: i + 1, IdentifierMasked: "s***@example.invalid", State: "succeeded"})
		}
		f.requests[id] = req
		f.batches[id] = batch
		if f.lostSubmit {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(202)
		send(batch)
	case strings.HasPrefix(r.URL.Path, "/api/v1/oauth/requests/"):
		f.lookups++
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/oauth/requests/")
		for _, b := range f.batches {
			if b.RequestID == id {
				send(b)
				return
			}
		}
		readError(w, 404, "not_found", "query_original")
	case strings.HasPrefix(r.URL.Path, "/api/v1/oauth/batches/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/oauth/batches/"), "/")
		batch, ok := f.batches[parts[0]]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if len(parts) == 1 {
			send(batch)
			return
		}
		if parts[1] == "result" {
			req := f.requests[batch.BatchID]
			accounts := make([]map[string]any, len(req.Accounts))
			mapping := []upstream.DeliveryItem{}
			for i, account := range req.Accounts {
				idx := len(accounts) - 1 - i
				accounts[idx] = map[string]any{"platform": "openai", "type": "oauth", "credentials": map[string]any{"email": account.Identifier, "access_token": "synthetic-result-token", "refresh_token": "synthetic-result-refresh", "chatgpt_user_id": "user-original", "chatgpt_account_id": "workspace-original"}}
				mapping = append(mapping, upstream.DeliveryItem{ItemID: batch.Items[i].ItemID, Line: i + 1, AccountIndex: idx})
			}
			if f.missingDelivery {
				accounts = []map[string]any{}
				mapping = []upstream.DeliveryItem{}
			}
			data := map[string]any{"type": "sub2api-data", "version": 1, "proxies": []any{}, "accounts": accounts}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data, "delivery": upstream.Delivery{IncludedCount: len(accounts), Items: mapping, ExpiredItemIDs: []string{}}})
			return
		}
		if parts[1] == "cancel" {
			send(batch)
			return
		}
		w.WriteHeader(404)
	default:
		f.t.Errorf("unexpected HTTP endpoint %s", r.URL.Path)
		w.WriteHeader(404)
	}
}
func (f *fakePair) Groups(context.Context) ([]upstream.Group, error) {
	return []upstream.Group{{ID: 1, Name: "free"}}, nil
}
func (f *fakePair) Import(ctx context.Context, account json.RawMessage, ids []int64, proxy string, pauseAutoDetection bool) (upstream.ImportResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return upstream.ImportResult{}, err
	}
	if f.onImport != nil {
		f.onImport()
	}
	f.imports++
	var data struct {
		Credentials struct {
			Email string `json:"email"`
		} `json:"credentials"`
	}
	if json.Unmarshal(account, &data) != nil || data.Credentials.Email == "" {
		f.t.Error("not a single-account import")
	}
	f.imported = append(f.imported, data.Credentials.Email)
	f.importGroups = append(f.importGroups, append([]int64(nil), ids...))
	f.importProxies = append(f.importProxies, proxy)
	switch f.importMode {
	case "unknown":
		return upstream.ImportResult{}, &upstream.Error{Code: "transport_error", Message: "secret", Source: "execution", Action: "query_original", Uncertain: true}
	case "generic":
		return upstream.ImportResult{Total: 1, Failed: 1}, errors.New("synthetic-result-token")
	case "failed":
		return upstream.ImportResult{Total: 1, Failed: 1}, &upstream.Error{Code: "upstream_error", Message: "secret", Source: "execution", Action: "fix_configuration"}
	case "warning":
		return upstream.ImportResult{Total: 1, Success: 1}, &upstream.Error{Code: "upstream_error", Message: "synthetic warning never expose", Source: "execution", Action: "fix_configuration"}
	case "duplicate":
		return upstream.ImportResult{Total: 1, Duplicate: 1}, nil
	}
	return upstream.ImportResult{Total: 1, Success: 1}, nil
}
func configuredApp(t *testing.T, f *fakePair) (*App, *memoryStore, *fakePair) {
	t.Helper()
	store := &memoryStore{state: State{Version: 1, Jobs: []Job{}}}
	a, err := New(store, f, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	_, err = a.SaveSettings(Settings{OAuthURL: f.server.URL, OAuthToken: "synthetic-oauth"})
	if err != nil {
		t.Fatal(err)
	}
	return a, store, f
}
func exampleInput(id string) Input {
	return Input{RequestID: id, Accounts: []InputAccount{{Identifier: "one@example.invalid", Password: " keep spaces ", TOTPSecret: "JBSWY3DPEHPK3PXP"}}, Options: upstream.Options{Country: "US"}, GroupIDs: []int64{1}}
}
func drive(t *testing.T, a *App, id string) JobView {
	t.Helper()
	for n := 0; n < 20; n++ {
		state, err := a.store.View()
		if err != nil {
			t.Fatal(err)
		}
		active := false
		for _, job := range state.Jobs {
			if job.ID != id {
				continue
			}
			for _, g := range job.Groups {
				switch g.Stage {
				case "queued", "checking", "running", "submitting", "submit_unknown":
					active = true
				}
			}
		}
		if !active {
			v, e := a.Job(id)
			if e != nil {
				t.Fatal(e)
			}
			return v
		}
		stepNow(t, a, id)
	}
	t.Fatal("task did not reach a stable state")
	return JobView{}
}
func useWrapper(t *testing.T, a *App, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	_, err := a.SaveSettings(Settings{OAuthURL: server.URL, OAuthToken: "synthetic-oauth"})
	if err != nil {
		t.Fatal(err)
	}
	return server
}
func stepNow(t *testing.T, a *App, id string) {
	t.Helper()
	if err := a.changeJob(id, func(j *Job) error {
		for i := range j.Groups {
			j.Groups[i].NextCheck = time.Time{}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}
