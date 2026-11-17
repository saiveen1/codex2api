package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/codex2api/internal/oauthgateway/service"
	"github.com/codex2api/internal/oauthgateway/upstream"
)

func TestOAuthRecovery401AndRefreshClassification(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
		want   bool
	}{
		{`{"error":{"code":"token_expired"}}`, 401, true},
		{`{"error":{"code":"invalid_access_token"}}`, 401, true},
		{`{"error":{"code":"token_invalidated"}}`, 401, true},
		{`{"error":{"code":"missing_scope"}}`, 401, false},
		{`{"error":{"code":"account_deactivated"}}`, 401, false},
		{`{"error":{"code":"workspace_deactivated"}}`, 401, false},
		{`{"error":{"code":"token_expired","message":"phone verification required"}}`, 401, false},
		{`{"error":{"code":"token_expired","message":"account suspended"}}`, 401, false},
		{`{"error":{"code":"token_expired","message":"gateway_auth failed"}}`, 401, false},
		{`{"error":{"code":"unauthorized"}}`, 401, false},
		{`{"error":{"code":"token_expired"}}`, 407, false},
		{`proxy returned status 401`, 401, false},
		{`{"error":"unauthorized"}`, 401, false},
	} {
		if got := IsOAuthRecoveryUnauthorized(tc.status, []byte(tc.body)); got != tc.want {
			t.Fatalf("unauthorized classification %s = %v", tc.body, got)
		}
	}
	for _, tc := range []struct {
		code, body string
		status     int
		want       bool
	}{
		{"invalid_grant", "", 400, true}, {"refresh_token_reused", "", 400, true}, {"refresh_token_expired", "", 401, true},
		{"invalid_client", "", 401, false}, {"unauthorized_client", "", 400, false}, {"access_denied", "", 400, false},
		{"invalid_grant", "account_deactivated", 400, false}, {"invalid_grant", "workspace_deactivated", 400, false},
		{"invalid_grant", "verification required", 400, false}, {"invalid_grant", "proxy authentication failed", 407, false},
		{"invalid_grant", "gateway failed", 502, false}, {"", "401 unauthorized", 401, false},
	} {
		err := fmt.Errorf("wrapped: %w", &oauthRefreshHTTPError{status: tc.status, code: tc.code, body: tc.body})
		if got := IsOAuthRecoveryRefreshFailure(err); got != tc.want {
			t.Fatalf("refresh classification %s = %v", tc.code, got)
		}
	}
	if IsOAuthRecoveryRefreshFailure(errors.New("401 invalid_grant")) {
		t.Fatal("untyped string triggered recovery")
	}
	if IsOAuthRecoveryRefreshFailure(&oauthRefreshUncertainError{err: &oauthRefreshHTTPError{status: 400, code: "invalid_grant"}}) {
		t.Fatal("uncertain refresh triggered a new authorization")
	}
}

func TestOAuthRecoveryCleanupGuardAndManualDelete(t *testing.T) {
	opted := newFastSchedulerTestAccount(1, HealthTierBanned, 120, 1)
	ordinary := newFastSchedulerTestAccount(2, HealthTierBanned, 120, 1)
	store := &Store{accounts: []*Account{opted, ordinary}}
	if store.OAuthRecoveryProtected(1) {
		t.Fatal("absent module changed cleanup")
	}
	var signals int
	store.SetOAuthRecoveryHooks(&OAuthRecoveryHooks{Enabled: func(id int64) bool { return id == 1 }, Signal: func(id, generation int64, refresh bool) { signals++ }})
	store.mu.Lock()
	protected := store.OAuthRecoveryProtected(1) // Hook must never reenter Store or encrypted/DB state.
	store.mu.Unlock()
	if !protected {
		t.Fatal("opted-in guard missing")
	}
	if !store.NotifyOAuthRecoveryUnauthorized(opted, 401, []byte(`{"error":{"code":"token_expired"}}`)) || signals != 1 {
		t.Fatal("real 401 not queued")
	}
	store.NotifyOAuthRecoveryUnauthorized(opted, 401, []byte(`{"error":{"code":"missing_scope"}}`))
	if signals != 1 || store.NotifyOAuthRecoveryUnauthorized(ordinary, 401, []byte(`{"error":{"code":"token_expired"}}`)) {
		t.Fatal("non-opt-in or excluded error triggered")
	}
	store.SetAutoCleanUnauthorized(true)
	store.runAutoCleanupSweep(context.Background())
	if store.FindByID(1) == nil || store.FindByID(2) != nil {
		t.Fatal("automatic cleanup ignored consent")
	}
	if err := store.SoftDeleteForClean(context.Background(), opted, "manual_clean"); err != nil {
		t.Fatal(err)
	}
	if store.FindByID(1) != nil {
		t.Fatal("manual deletion blocked")
	}
	store.SetOAuthRecoveryHooks(nil)
	if store.OAuthRecoveryProtected(1) {
		t.Fatal("removed module left a guard")
	}
}

type failedOAuthRecoveryStore struct {
	state service.State
	fail  bool
}

func (s *failedOAuthRecoveryStore) View() (service.State, error) {
	if s.fail {
		return service.State{}, errors.New("synthetic state failure")
	}
	return s.state, nil
}
func (s *failedOAuthRecoveryStore) Update(fn func(*service.State) error) error {
	if s.fail {
		return errors.New("synthetic state failure")
	}
	return fn(&s.state)
}
func (s *failedOAuthRecoveryStore) Close() error { return nil }

type failedOAuthRecoveryTarget struct{}

func (failedOAuthRecoveryTarget) Groups(context.Context) ([]upstream.Group, error) { return nil, nil }
func (failedOAuthRecoveryTarget) Import(context.Context, json.RawMessage, []int64, string) (upstream.ImportResult, error) {
	return upstream.ImportResult{}, nil
}

func TestOAuthRecoveryFailedWorkerProtects401AndSweep(t *testing.T) {
	durable := &failedOAuthRecoveryStore{state: service.State{Version: 3, Recoveries: []service.Recovery{{Snapshot: service.RecoverySnapshot{AccountID: 1}, Enabled: true, Epoch: 1}}}}
	app, err := service.New(durable, failedOAuthRecoveryTarget{}, false)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{accounts: []*Account{newFastSchedulerTestAccount(1, HealthTierBanned, 120, 1), newFastSchedulerTestAccount(2, HealthTierBanned, 120, 1)}}
	store.SetOAuthRecoveryHooks(&OAuthRecoveryHooks{Enabled: app.RecoveryEnabled, Signal: app.RequestRecovery})
	durable.fail = true
	if err = app.Run(context.Background()); err == nil {
		t.Fatal("worker did not fail")
	}
	if err = app.Close(); err != nil {
		t.Fatal(err)
	}
	opted := store.FindByID(1)
	if !store.NotifyOAuthRecoveryUnauthorized(opted, 401, []byte(`{"error":{"code":"token_expired"}}`)) {
		t.Fatal("closed worker lost 401 protection")
	}
	if store.NotifyOAuthRecoveryUnauthorized(store.FindByID(2), 401, []byte(`{"error":{"code":"token_expired"}}`)) {
		t.Fatal("ordinary account was protected")
	}
	store.SetAutoCleanUnauthorized(true)
	store.runAutoCleanupSweep(context.Background())
	if store.FindByID(1) == nil || store.FindByID(2) != nil {
		t.Fatal("sweep lost closed worker protection")
	}
	if err = store.SoftDeleteForClean(context.Background(), opted, "manual_clean"); err != nil || store.FindByID(1) != nil {
		t.Fatal("manual delete blocked", err)
	}
}
