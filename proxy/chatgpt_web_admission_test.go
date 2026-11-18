package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const webAdmissionReady = `{"status":"ready","capabilities":["generate","edit"],"image_quota_remaining":10,"workspace_id":"fixture-upstream-account","stage":"complete","upstream_status":200}`

func webAdmissionContext(f *webFixture, ctx context.Context) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil).WithContext(ctx)
	c.Set(contextAPIKeyRow, f.key)
	c.Set(contextAPIKeyID, f.key.ID)
	return c
}

func webAdmissionSQL(t *testing.T, f *webFixture, query string, args ...any) {
	t.Helper()
	conn, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func webAdmissionGroup(t *testing.T, f *webFixture, name string) int64 {
	t.Helper()
	id, err := f.db.CreateAccountGroup(t.Context(), name, "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func webAdmissionKey(t *testing.T, f *webFixture, groups []int64, limits database.APIKeyLimits) {
	t.Helper()
	if err := f.db.UpdateAPIKeyAllowedGroups(t.Context(), f.key.ID, groups); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateAPIKeyLimits(t.Context(), f.key.ID, limits); err != nil {
		t.Fatal(err)
	}
	var err error
	f.key, err = f.db.GetAPIKeyByID(t.Context(), f.key.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.syncAPIKeyAllowedGroups(f.key)
}

func webAdmissionAddAccount(t *testing.T, f *webFixture, group int64, allowed []int64) *auth.Account {
	t.Helper()
	id, err := f.db.InsertAccountWithCredentials(t.Context(), "admission-candidate", map[string]any{
		"access_token": "fixture-access-token", "account_id": "fixture-upstream-account", "plan_type": "free", "allowed_api_key_ids": allowed,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetAccountGroups(t.Context(), id, []int64{group}); err != nil {
		t.Fatal(err)
	}
	account := &auth.Account{DBID: id, AccessToken: "fixture-access-token", AccountID: "fixture-upstream-account", PlanType: "free", Status: auth.StatusReady, GroupIDs: []int64{group}}
	f.handler.store.AddAccount(account)
	return account
}

func webAdmissionUnverified(t *testing.T, f *webFixture) {
	t.Helper()
	webAdmissionSQL(t, f, `UPDATE chatgpt_web_account_states SET status='unverified', observed_at=NULL, last_check_at=NULL, last_check_error_code='', error_code='' WHERE account_id=?`, f.account.ID())
}

func TestChatGPTWebPrepareStaleObservation(t *testing.T) {
	f := newWebFixture(t, nil)
	old := time.Now().Add(-31 * time.Minute).UnixNano()
	webAdmissionSQL(t, f, `UPDATE chatgpt_web_account_states SET observed_at=?,last_check_at=? WHERE account_id=?`, old, old, f.account.ID())
	var checks atomic.Int32
	setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		if r.URL.Path != "/v1/accounts/check" {
			t.Error("prepare submitted an image")
		}
		fmt.Fprint(w, webAdmissionReady)
	})
	c := webAdmissionContext(f, t.Context())
	if err := f.handler.prepareChatGPTWebAccounts(c, "generate", "gpt-image-2"); err != nil {
		t.Fatal(err)
	}
	state, err := f.db.GetChatGPTWebAccountState(t.Context(), f.account.ID())
	if err != nil || !chatGPTWebStateReady(*state, "generate", time.Now()) || checks.Load() != 1 || f.calls.Load() != 0 {
		t.Fatalf("readiness=%+v checks=%d err=%v", state, checks.Load(), err)
	}
	// n<=4 executions share the Gin request's single admission allowance.
	for range 3 {
		_ = f.handler.prepareChatGPTWebAccounts(c, "generate", "gpt-image-2")
	}
	if checks.Load() != 1 {
		t.Fatal("one HTTP request repeated admission")
	}
}

func TestChatGPTWebPrepareSelectedOnlyAttemptsAndBackoff(t *testing.T) {
	f := newWebFixture(t, nil)
	group := webAdmissionGroup(t, f, "selected-only")
	webAdmissionKey(t, f, []int64{group}, f.key.Limits)
	var accounts []*auth.Account
	for range 5 {
		accounts = append(accounts, webAdmissionAddAccount(t, f, group, nil))
	}
	var checks atomic.Int32
	setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		fmt.Fprint(w, `{"status":"auth_required","error_code":"auth_required","stage":"identity","upstream_status":401,"message":"do-not-expose-secret"}`)
	})
	c := webAdmissionContext(f, t.Context())
	err := f.handler.prepareChatGPTWebAccounts(c, "generate", "gpt-image-2")
	if err == nil || strings.Contains(err.Error(), "do-not-expose") || checks.Load() != 3 {
		t.Fatalf("attempt bound: %v checks=%d", err, checks.Load())
	}
	for i, account := range accounts {
		state, err := f.db.GetChatGPTWebAccountState(t.Context(), account.ID())
		if i >= 3 {
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("unselected candidate initialized", i, err)
			}
		} else if err != nil || state.Status == "ready" || state.LastCheckErrorCode != "auth_required" {
			t.Fatalf("failure not persisted: %+v %v", state, err)
		}
	}
	_ = f.handler.prepareChatGPTWebAccounts(c, "generate", "gpt-image-2")
	if checks.Load() != 3 {
		t.Fatal("request repeated its attempts")
	}
	_ = f.handler.prepareChatGPTWebAccounts(webAdmissionContext(f, t.Context()), "generate", "gpt-image-2")
	if checks.Load() != 5 {
		t.Fatalf("backoff did not advance fairly to untouched candidates: %d", checks.Load())
	}
	// Even without an earlier ready observation, rejected tokens stay skipped.
	webAdmissionSQL(t, f, `UPDATE chatgpt_web_account_states SET last_check_at=? WHERE last_check_error_code='auth_required'`, time.Now().Add(-time.Hour).UnixNano())
	_ = f.handler.prepareChatGPTWebAccounts(webAdmissionContext(f, t.Context()), "generate", "gpt-image-2")
	if checks.Load() != 5 {
		t.Fatal("retried previously rejected credentials")
	}
	// Explicit manual checks retain their behavior and are not backoff-gated.
	setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) { checks.Add(1); fmt.Fprint(w, webAdmissionReady) })
	if _, err := f.handler.CheckChatGPTWebAccount(t.Context(), accounts[0].ID()); err != nil {
		t.Fatal(err)
	}
	if checks.Load() != 6 {
		t.Fatal("manual check was suppressed by automatic backoff")
	}
}

func TestChatGPTWebPrepareFreshAuthorization(t *testing.T) {
	for _, scenario := range []string{"group", "plan", "account_key", "affinity", "no_affinity", "scope_budget", "key_disabled", "key_expired", "wrong_channel", "model_denied", "image_policy", "account_disabled", "web_disabled", "credentials_changed", "workspace_changed", "proxy_changed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWebFixture(t, nil)
			webAdmissionUnverified(t, f)
			group := webAdmissionGroup(t, f, "authorized")
			other := webAdmissionGroup(t, f, "other")
			if err := f.db.SetAccountGroups(t.Context(), f.account.ID(), []int64{group}); err != nil {
				t.Fatal(err)
			}
			f.account.GroupIDs = []int64{group}
			limits := f.key.Limits
			limits.PlanAllow = []string{"free"}
			if scenario == "affinity" || scenario == "no_affinity" {
				limits.NoAffinityGroupIDs = []int64{other}
			}
			webAdmissionKey(t, f, []int64{group}, limits)
			c := webAdmissionContext(f, t.Context())
			switch scenario {
			case "group":
				if err := f.db.SetAccountGroups(t.Context(), f.account.ID(), []int64{other}); err != nil {
					t.Fatal(err)
				}
			case "plan":
				if err := f.db.UpdateCredentials(t.Context(), f.account.ID(), map[string]any{"plan_type": "plus"}); err != nil {
					t.Fatal(err)
				}
			case "account_key":
				if err := f.db.UpdateCredentials(t.Context(), f.account.ID(), map[string]any{"allowed_api_key_ids": []int64{f.key.ID + 99}}); err != nil {
					t.Fatal(err)
				}
			case "affinity":
				c.Request.Header.Set("X-Codex2API-Affinity-Key", "synthetic-affinity")
				if err := f.db.SetAccountGroups(t.Context(), f.account.ID(), []int64{other}); err != nil {
					t.Fatal(err)
				}
			case "no_affinity":
				f.account.GroupIDs = []int64{other} // Cheap filter passes; fresh original-group account must not.
			case "scope_budget":
				c.Set(contextScopeBudgetGate, &scopeBudgetGate{blockedAccounts: map[int64]struct{}{f.account.ID(): {}}})
			case "key_disabled":
				webAdmissionSQL(t, f, `UPDATE api_keys SET enabled=FALSE WHERE id=?`, f.key.ID)
			case "key_expired":
				if err := f.db.UpdateAPIKeyExpiresAt(t.Context(), f.key.ID, sql.NullTime{Time: time.Now().Add(-time.Minute), Valid: true}); err != nil {
					t.Fatal(err)
				}
			case "wrong_channel", "model_denied", "image_policy":
				changed := f.key.Limits
				if scenario == "wrong_channel" {
					changed.UpstreamChannel = database.UpstreamChannelCodex
				}
				if scenario == "model_denied" {
					changed.ModelDeny = []string{"gpt-image-2"}
				}
				if scenario == "image_policy" {
					changed.ImageGenerationPolicy = database.ImageGenerationPolicyBlock
				}
				if err := f.db.UpdateAPIKeyLimits(t.Context(), f.key.ID, changed); err != nil {
					t.Fatal(err)
				}
			case "account_disabled":
				webAdmissionSQL(t, f, `UPDATE accounts SET enabled=FALSE WHERE id=?`, f.account.ID())
			case "web_disabled":
				if _, err := f.db.SetChatGPTWebAccountEnabled(t.Context(), f.account.ID(), false); err != nil {
					t.Fatal(err)
				}
			case "credentials_changed":
				if err := f.db.UpdateCredentials(t.Context(), f.account.ID(), map[string]any{"access_token": "rotated-synthetic-token"}); err != nil {
					t.Fatal(err)
				}
			case "workspace_changed":
				if err := f.db.UpdateCredentials(t.Context(), f.account.ID(), map[string]any{"account_id": "changed-workspace"}); err != nil {
					t.Fatal(err)
				}
			case "proxy_changed":
				if err := f.db.UpdateAccountProxyURL(t.Context(), f.account.ID(), "http://proxy.invalid:1234"); err != nil {
					t.Fatal(err)
				}
			}
			var checks atomic.Int32
			setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) { checks.Add(1); fmt.Fprint(w, webAdmissionReady) })
			if err := f.handler.prepareChatGPTWebAccounts(c, "generate", "gpt-image-2"); err == nil || checks.Load() != 0 {
				t.Fatalf("out-of-scope check: %v checks=%d", err, checks.Load())
			}
			state, err := f.db.GetChatGPTWebAccountState(t.Context(), f.account.ID())
			if err != nil || state.LastCheckAt != nil || (scenario == "web_disabled" && state.Enabled) {
				t.Fatalf("forbidden account mutated: %+v %v", state, err)
			}
		})
	}
}

func TestChatGPTWebPrepareMissingRequiresPositiveBoundGroup(t *testing.T) {
	for _, groups := range [][]int64{nil, {0}} {
		f := newWebFixture(t, nil)
		group := webAdmissionGroup(t, f, "not-consent")
		account := webAdmissionAddAccount(t, f, group, nil)
		webAdmissionKey(t, f, groups, f.key.Limits)
		var checks atomic.Int32
		setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) { checks.Add(1); fmt.Fprint(w, webAdmissionReady) })
		_ = f.handler.prepareChatGPTWebAccounts(webAdmissionContext(f, t.Context()), "generate", "gpt-image-2")
		if _, err := f.db.GetChatGPTWebAccountState(t.Context(), account.ID()); !errors.Is(err, sql.ErrNoRows) || checks.Load() != 0 {
			t.Fatal("unrestricted/sentinel key enabled pool", err, checks.Load())
		}
	}
}

func TestChatGPTWebPrepareFreshCandidateBound(t *testing.T) {
	f := newWebFixture(t, nil)
	group := webAdmissionGroup(t, f, "bounded-fresh-validation")
	webAdmissionKey(t, f, []int64{group}, f.key.Limits)
	for range 32 {
		webAdmissionAddAccount(t, f, group, []int64{f.key.ID + 99})
	}
	last := webAdmissionAddAccount(t, f, group, nil)
	var checks atomic.Int32
	setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) { checks.Add(1); fmt.Fprint(w, webAdmissionReady) })
	_ = f.handler.prepareChatGPTWebAccounts(webAdmissionContext(f, t.Context()), "generate", "gpt-image-2")
	if checks.Load() != 0 {
		t.Fatal("more than 32 stale runtime candidates were fresh-validated")
	}
	if _, err := f.db.GetChatGPTWebAccountState(t.Context(), last.ID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("33rd candidate initialized", err)
	}
}

func TestChatGPTWebPrepareDeadlineAndSlots(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprint("blocked_slots=", blocked), func(t *testing.T) {
			f := newWebFixture(t, nil)
			webAdmissionUnverified(t, f)
			var checks atomic.Int32
			setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
				checks.Add(1)
				var request chatGPTWebAccountRequest
				_ = json.NewDecoder(r.Body).Decode(&request)
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
					t.Error("worker request not canceled")
				}
			})
			if blocked {
				f.handler.chatGPTWebAdmissionOnce.Do(func() { f.handler.chatGPTWebAdmissionSlots = make(chan struct{}, 7) })
				for range 7 {
					f.handler.chatGPTWebAdmissionSlots <- struct{}{}
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			err := f.handler.prepareChatGPTWebAccounts(webAdmissionContext(f, ctx), "generate", "gpt-image-2")
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
				t.Fatalf("deadline swallowed/extended: %v elapsed=%s", err, time.Since(started))
			}
			if blocked && checks.Load() != 0 {
				t.Fatal("eighth process check entered worker")
			}
			if !blocked && checks.Load() != 1 {
				t.Fatal("fixture did not reach worker")
			}
			if !blocked {
				state, readErr := f.db.GetChatGPTWebAccountState(t.Context(), f.account.ID())
				if readErr != nil || state.LastCheckAt == nil || state.LastCheckErrorCode != "worker_timeout" {
					t.Fatalf("canceled check lost failure/backoff record: state=%+v err=%v", state, readErr)
				}
				acquired, leaseErr := f.db.AcquireChatGPTWebAccountLease(t.Context(), f.account.ID(), "deadline-cleanup", time.Now().Add(time.Minute))
				if leaseErr != nil || !acquired {
					t.Fatalf("canceled check retained lease: acquired=%t err=%v", acquired, leaseErr)
				}
				if err := f.db.ReleaseChatGPTWebAccountLease(t.Context(), f.account.ID(), "deadline-cleanup"); err != nil {
					t.Fatal(err)
				}
				if err := f.handler.prepareChatGPTWebAccounts(webAdmissionContext(f, t.Context()), "generate", "gpt-image-2"); err == nil || checks.Load() != 1 {
					t.Fatal("next request ignored canceled-check backoff")
				}
				if len(f.handler.chatGPTWebAdmissionSlots) != 0 {
					t.Fatal("canceled check retained process slot")
				}
			}
		})
	}
	if chatGPTWebAdmissionBudget != 20*time.Second {
		t.Fatal("automatic check budget changed")
	}
}

func TestChatGPTWebPrepareLeasePreventsConcurrentWorkerChecks(t *testing.T) {
	f := newWebFixture(t, nil)
	webAdmissionUnverified(t, f)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var checks atomic.Int32
	setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			fmt.Fprint(w, webAdmissionReady)
		case <-r.Context().Done():
		}
	})
	other := NewHandler(f.handler.store, f.db, nil, nil)
	other.chatGPTWebRuntime = f.handler.chatGPTWebRuntime
	first := make(chan error, 1)
	go func() {
		first <- f.handler.prepareChatGPTWebAccounts(webAdmissionContext(f, t.Context()), "generate", "gpt-image-2")
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first checker stalled before worker")
	}
	if err := other.prepareChatGPTWebAccounts(webAdmissionContext(f, t.Context()), "generate", "gpt-image-2"); err == nil {
		t.Fatal("leased account was checked twice")
	}
	if checks.Load() != 1 {
		t.Fatalf("lease allowed concurrent checks=%d", checks.Load())
	}
	close(release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first checker did not finish")
	}
}

func TestChatGPTWebPrepareConcurrentKeyRevocationFailsClosed(t *testing.T) {
	f := newWebFixture(t, nil)
	webAdmissionUnverified(t, f)
	setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
		webAdmissionSQL(t, f, `UPDATE api_keys SET enabled=FALSE WHERE id=?`, f.key.ID)
		fmt.Fprint(w, webAdmissionReady)
	})
	if err := f.handler.prepareChatGPTWebAccounts(webAdmissionContext(f, t.Context()), "generate", "gpt-image-2"); err == nil {
		t.Fatal("revoked key returned successful readiness")
	}
	state, err := f.db.GetChatGPTWebAccountState(t.Context(), f.account.ID())
	if err != nil || state.Status == "ready" {
		t.Fatalf("revoked observation was marked ready: %+v %v", state, err)
	}
}

func TestChatGPTWebAutoCheckNeededEvidence(t *testing.T) {
	f := newWebFixture(t, nil)
	state, err := f.db.GetChatGPTWebAccountState(t.Context(), f.account.ID())
	if err != nil {
		t.Fatal(err)
	}
	token, requested := f.account.ChatGPTWebCredentials()
	proxyURL, _ := f.handler.store.ResolveChatGPTWebProxy(f.account)
	now := time.Now()
	for _, tc := range []struct {
		name      string
		change    func(*database.ChatGPTWebAccountState)
		operation string
		want      bool
	}{
		{"ready_busy", func(*database.ChatGPTWebAccountState) {}, "generate", false},
		{"no_edit", func(s *database.ChatGPTWebAccountState) { s.Capabilities = []string{"generate"} }, "edit", false},
		{"zero_quota", func(s *database.ChatGPTWebAccountState) {
			zero := 0
			reset := now.Add(time.Hour)
			s.Status = "quota_exhausted"
			s.ImageQuotaRemaining = &zero
			s.QuotaResetAt = &reset
		}, "generate", false},
		{"stale_zero_with_future_reset", func(s *database.ChatGPTWebAccountState) {
			zero := 0
			old, reset := now.Add(-time.Hour), now.Add(time.Hour)
			s.Status, s.ImageQuotaRemaining, s.ObservedAt, s.QuotaResetAt = "quota_exhausted", &zero, &old, &reset
		}, "generate", false},
		{"reset_due", func(s *database.ChatGPTWebAccountState) {
			zero := 0
			reset := now.Add(-time.Second)
			s.Status = "quota_exhausted"
			s.ImageQuotaRemaining = &zero
			s.QuotaResetAt = &reset
		}, "generate", true},
		{"stale", func(s *database.ChatGPTWebAccountState) { old := now.Add(-31 * time.Minute); s.ObservedAt = &old }, "generate", true},
		{"credentials", func(s *database.ChatGPTWebAccountState) { s.CredentialRevision = chatGPTWebHash("rotated") }, "generate", true},
		{"workspace", func(s *database.ChatGPTWebAccountState) { s.WorkspaceBindingFingerprint = chatGPTWebHash("other") }, "generate", true},
		{"proxy", func(s *database.ChatGPTWebAccountState) { s.ProxyFingerprint = chatGPTWebHash("other") }, "generate", true},
		{"recent_failure", func(s *database.ChatGPTWebAccountState) {
			s.Status = "auth_required"
			s.LastCheckAt = &now
			s.LastCheckErrorCode = "auth_required"
		}, "generate", false},
		{"backoff_due", func(s *database.ChatGPTWebAccountState) {
			old := now.Add(-3 * time.Minute)
			s.Status = "auth_required"
			s.LastCheckAt = &old
			s.LastCheckErrorCode = "auth_required"
		}, "generate", true},
		{"disabled", func(s *database.ChatGPTWebAccountState) { s.Enabled = false }, "generate", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *state
			tc.change(&copy)
			if got := chatGPTWebAutoCheckNeeded(&copy, tc.operation, token, requested, proxyURL, false, now); got != tc.want {
				t.Fatalf("needed=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestChatGPTWebPreparePrefersCurrentToken(t *testing.T) {
	f := newWebFixture(t, nil)
	webAdmissionUnverified(t, f)
	group := webAdmissionGroup(t, f, "current-token-first")
	webAdmissionKey(t, f, []int64{group}, f.key.Limits)
	if err := f.db.SetAccountGroups(t.Context(), f.account.ID(), []int64{group}); err != nil {
		t.Fatal(err)
	}
	f.account.GroupIDs = []int64{group}
	f.account.ExpiresAt = time.Now().Add(-time.Hour)
	if err := f.db.UpdateCredentials(t.Context(), f.account.ID(), map[string]any{"expires_at": f.account.ExpiresAt.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	current := webAdmissionAddAccount(t, f, group, nil)
	var checks atomic.Int32
	setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
		var request chatGPTWebAccountRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.AccountID != current.ID() {
			t.Error("old expired account blocked a current token candidate")
		}
		checks.Add(1)
		fmt.Fprint(w, webAdmissionReady)
	})
	if err := f.handler.prepareChatGPTWebAccounts(webAdmissionContext(f, t.Context()), "generate", "gpt-image-2"); err != nil {
		t.Fatal(err)
	}
	old, err := f.db.GetChatGPTWebAccountState(t.Context(), f.account.ID())
	if err != nil || old.LastCheckAt != nil || checks.Load() != 1 {
		t.Fatalf("expired token was needlessly refreshed/checked: %+v %v checks=%d", old, err, checks.Load())
	}
}

func TestChatGPTWebPrepareCanceledBeforeAdmission(t *testing.T) {
	f := newWebFixture(t, nil)
	webAdmissionUnverified(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.handler.prepareChatGPTWebAccounts(webAdmissionContext(f, ctx), "generate", "gpt-image-2"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation swallowed: %v", err)
	}
	state, err := f.db.GetChatGPTWebAccountState(t.Context(), f.account.ID())
	if err != nil || state.LastCheckAt != nil {
		t.Fatalf("canceled request mutated check state: %+v %v", state, err)
	}
}

func TestChatGPTWebPrecheckOriginStatusMessage(t *testing.T) {
	for _, tc := range []struct {
		origin int
		want   string
	}{{0, "未记录上游 HTTP 响应"}, {403, "上游 HTTP 403"}} {
		check := &ChatGPTWebCheckError{Code: "worker_unavailable", Stage: "bootstrap", HTTPStatus: 503, UpstreamStatus: tc.origin}
		body, status := chatGPTWebErrorFields(errors.Join(&chatGPTWebFailure{check.Code, check.HTTPStatus, false}, check), "fixture")
		msg := fmt.Sprint(body["message"])
		if status != 503 || body["precheck_stage"] != "bootstrap" || !strings.Contains(msg, tc.want) || !strings.Contains(msg, "生图未提交") || !strings.Contains(msg, check.Code) {
			t.Fatalf("unexpected precheck response: %v %v", status, body)
		}
	}
}

func TestChatGPTWebPrepareSystemFailureStopsEarly(t *testing.T) {
	for _, code := range []string{"worker_unavailable", "internal_error"} {
		t.Run(code, func(t *testing.T) {
			f := newWebFixture(t, nil)
			group := webAdmissionGroup(t, f, "system-error")
			webAdmissionKey(t, f, []int64{group}, f.key.Limits)
			first := webAdmissionAddAccount(t, f, group, nil)
			second := webAdmissionAddAccount(t, f, group, nil)
			var checks atomic.Int32
			setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
				checks.Add(1)
				if code == "worker_unavailable" {
					fmt.Fprint(w, `{"status":"unverified","error_code":"worker_unavailable","stage":"bootstrap","exception_kind":"CurlError","curl_code":56,"transport_hint":"peer_reset"}`)
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, `{"code":%q,"stage":"admission"}`, code)
			})
			err := f.handler.prepareChatGPTWebAccounts(webAdmissionContext(f, t.Context()), "generate", "gpt-image-2")
			var failure *chatGPTWebFailure
			if !errors.As(err, &failure) || failure.code != code || failure.unknown || checks.Load() != 1 {
				t.Fatalf("system failure not bounded/classified: %v checks=%d", err, checks.Load())
			}
			if code == "worker_unavailable" {
				body, _ := chatGPTWebErrorFields(err, "fixture")
				if body["transport_cause"] != "curl(56) 接收数据失败：对端重置连接" || !strings.Contains(fmt.Sprint(body["message"]), "对端重置连接") {
					t.Fatalf("transport evidence lost: %v", body)
				}
			}
			if _, err := f.db.GetChatGPTWebAccountState(t.Context(), second.ID()); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("system failure initialized another account", err)
			}
			state, err := f.db.GetChatGPTWebAccountState(t.Context(), first.ID())
			if err != nil || state.LastCheckErrorCode != code || state.Status == "ready" {
				t.Fatalf("system failure lost diagnostics: %+v %v", state, err)
			}
		})
	}
}
