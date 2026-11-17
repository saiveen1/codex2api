package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/oauthgateway/service"
	"github.com/codex2api/internal/oauthgateway/upstream"
)

func TestOAuthRecovery401CleanupIntegration(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		store := auth.NewStore(nil, nil, nil)
		store.SetAutoCleanUnauthorized(true)
		account := &auth.Account{DBID: 42, AccessToken: "synthetic-at", RefreshToken: "synthetic-rt", Status: auth.StatusReady, CredentialGeneration: 1}
		store.AddAccount(account)
		signals := 0
		if optIn {
			store.SetOAuthRecoveryHooks(&auth.OAuthRecoveryHooks{Enabled: func(id int64) bool { return id == 42 }, Signal: func(id, generation int64, refresh bool) {
				if !refresh || generation != 1 {
					t.Error("401 must first refresh original token")
				}
				signals++
			}})
		}
		h := &Handler{store: store}
		h.applyCooldown(account, http.StatusUnauthorized, []byte(`{"error":{"code":"token_expired"}}`), nil)
		if (store.FindByID(42) != nil) != optIn {
			t.Fatal("401 cleanup guard not respected")
		}
		if optIn && signals != 1 {
			t.Fatal("true 401 not queued")
		}
	}
}

type stoppedRecoveryStore struct {
	state service.State
	fail  bool
}

func (s *stoppedRecoveryStore) View() (service.State, error) {
	if s.fail {
		return service.State{}, errors.New("synthetic persistence failure")
	}
	return s.state, nil
}
func (s *stoppedRecoveryStore) Update(fn func(*service.State) error) error {
	if s.fail {
		return errors.New("synthetic persistence failure")
	}
	return fn(&s.state)
}
func (s *stoppedRecoveryStore) Close() error { return nil }

type stoppedRecoveryTarget struct{}

func (stoppedRecoveryTarget) Groups(context.Context) ([]upstream.Group, error) { return nil, nil }
func (stoppedRecoveryTarget) Import(context.Context, json.RawMessage, []int64, string) (upstream.ImportResult, error) {
	return upstream.ImportResult{}, nil
}

func TestOAuthRecoveryStoppedWorkerPreservesProxy401Guard(t *testing.T) {
	durable := &stoppedRecoveryStore{state: service.State{Version: 3, Recoveries: []service.Recovery{{Snapshot: service.RecoverySnapshot{AccountID: 42}, Enabled: true, Epoch: 1}}}}
	app, err := service.New(durable, stoppedRecoveryTarget{}, false)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, nil)
	store.SetAutoCleanUnauthorized(true)
	store.SetOAuthRecoveryHooks(&auth.OAuthRecoveryHooks{Enabled: app.RecoveryEnabled, Signal: app.RequestRecovery})
	durable.fail = true
	if err = app.Run(context.Background()); err == nil {
		t.Fatal("worker did not fail")
	}
	if err = app.Close(); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: store}
	for _, id := range []int64{42, 43} {
		acc := &auth.Account{DBID: id, AccessToken: "at", RefreshToken: "rt", Status: auth.StatusReady, CredentialGeneration: 1}
		store.AddAccount(acc)
		h.applyCooldown(acc, http.StatusUnauthorized, []byte(`{"error":{"code":"token_expired"}}`), nil)
		if (store.FindByID(id) != nil) != (id == 42) {
			t.Fatalf("closed worker changed 401 cleanup for %d", id)
		}
	}
	if err = store.SoftDeleteForClean(context.Background(), store.FindByID(42), "manual_clean"); err != nil || store.FindByID(42) != nil {
		t.Fatal("manual deletion blocked", err)
	}
}
