package database

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/internal/oauthgateway/service"
	"github.com/codex2api/internal/oauthgateway/upstream"
)

func TestOAuthRecoveryCASPreservesOriginalAndRejectsRaces(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, scenario := range []string{"success", "refresh", "manual-edit", "identity", "deleted", "disabled", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			creds := map[string]any{"email": "fixture@example.invalid", "user_id": "user-original", "workspace_id": "workspace-original", "account_id": "workspace-original", "refresh_token": "old-rt-" + scenario, "access_token": "old-at", "custom_headers": map[string]string{"X-Keep": "yes"}}
			id, err := db.InsertAccountWithCredentials(ctx, "original-name", creds, "http://original-proxy.invalid:80")
			if err != nil {
				t.Fatal(err)
			}
			row, err := db.GetAccountByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := OAuthRecoverySnapshot(row)
			if err != nil {
				t.Fatal(err)
			}
			updates := map[string]any{"user_id": "user-original", "workspace_id": "workspace-original", "account_id": "workspace-original", "access_token": "new-at", "refresh_token": "new-rt-" + scenario}
			switch scenario {
			case "refresh":
				attempt, err := db.BeginCodexRefresh(ctx, id, snap.Generation, creds["refresh_token"].(string))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.FinishCodexRefresh(ctx, attempt, map[string]any{"access_token": "refresh-won", "refresh_token": "refresh-rt"}); err != nil {
					t.Fatal(err)
				}
			case "manual-edit":
				if err = db.UpdateCredentials(ctx, id, map[string]any{"access_token": "manual-won"}); err != nil {
					t.Fatal(err)
				}
			case "identity":
				updates["workspace_id"] = "different-workspace"
			case "deleted":
				if err = db.SoftDeleteAccount(ctx, id); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if _, err = db.conn.ExecContext(ctx, `UPDATE accounts SET enabled=FALSE WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				var successes atomic.Int32
				var wg sync.WaitGroup
				for range 2 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if db.RecoverOAuthGatewayAccount(ctx, snap, updates, "job-race", false) == nil {
							successes.Add(1)
						}
					}()
				}
				wg.Wait()
				if successes.Load() != 1 {
					t.Fatal("CAS did not serialize duplicate result", successes.Load())
				}
				return
			}
			err = db.RecoverOAuthGatewayAccount(ctx, snap, updates, "job-"+scenario, false)
			if scenario != "success" {
				var classified *upstream.Error
				if !errors.As(err, &classified) || classified.Uncertain {
					t.Fatal("race not a confirmed refusal", err)
				}
				after, e := db.GetAccountByIDIncludingDeleted(ctx, id)
				if e != nil {
					t.Fatal(e)
				}
				if after.GetCredential("access_token") == "new-at" {
					t.Fatal("conflicting result written")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := db.GetAccountByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.ID != id || after.Name != row.Name || after.ProxyURL != row.ProxyURL || after.CredentialFamilyID != snap.Family || after.CredentialGeneration != snap.Generation+1 || after.GetCredentialStringMap("custom_headers")["X-Keep"] != "yes" || after.GetCredential("access_token") != "new-at" {
				t.Fatal("original account metadata not preserved")
			}
			if err = db.RecoverOAuthGatewayAccount(ctx, snap, updates, "job-success", false); err == nil {
				t.Fatal("same snapshot wrote twice")
			}
		})
	}
}

func TestOAuthRecoveryEncryptedMaterialRoundTrip(t *testing.T) {
	oauthGatewayTestKey(t, "synthetic-recovery-root")
	db := newOAuthGatewayStoreDB(t)
	store, err := db.OpenOAuthGatewayStore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(s *service.State) error {
		s.Version = 3
		s.Recoveries = []service.Recovery{{Snapshot: service.RecoverySnapshot{AccountID: 42}, Enabled: true, Epoch: 1, State: "pending", Material: upstream.Account{Identifier: "private@example.invalid", Password: "private-password", TOTPSecret: "JBSWY3DPEHPK3PXP"}, Options: upstream.Options{Proxy: []byte(`"http://secret:pass@proxy.invalid:80"`)}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err = db.conn.QueryRow(`SELECT payload FROM oauth_gateway_state WHERE id=1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private@example.invalid", "private-password", "JBSWY3DPEHPK3PXP", "secret:pass"} {
		if strings.Contains(payload, secret) {
			t.Fatal("recovery material stored as plaintext")
		}
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.OpenOAuthGatewayStore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err := reopened.View()
	if err != nil || state.Version != 3 || len(state.Recoveries) != 1 || state.Recoveries[0].Material.Password != "private-password" || state.Recoveries[0].State != "pending" {
		t.Fatal("recovery restart lost state", err)
	}
}

func TestOAuthRecoveryRefreshCompletionFence(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "refresh-fence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, scenario := range []string{"success", "new-429", "new-401", "generation", "deleted", "disabled", "identity", "receipt", "token"} {
		t.Run(scenario, func(t *testing.T) {
			oldRT := "old-" + scenario
			id, err := db.InsertAccountWithCredentials(ctx, "original", map[string]any{"email": "fixture@example.invalid", "user_id": "user-original", "workspace_id": "workspace-original", "account_id": "workspace-original", "access_token": "old-at", "refresh_token": oldRT}, "")
			if err != nil {
				t.Fatal(err)
			}
			row, err := db.GetAccountByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := OAuthRecoverySnapshot(row)
			if err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(time.Hour).Truncate(time.Second)
			if err = db.SetCooldown(ctx, id, "unauthorized", until); err != nil {
				t.Fatal(err)
			}
			attempt, err := db.BeginCodexRefresh(ctx, id, snap.Generation, oldRT)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.FinishCodexRefresh(ctx, attempt, map[string]any{"access_token": "new-at", "refresh_token": "new-rt"}); err != nil {
				t.Fatal(err)
			}
			attemptID, access := attempt.ID, "new-at"
			switch scenario {
			case "new-429":
				err = db.SetCooldown(ctx, id, "rate_limited", until.Add(time.Hour))
			case "new-401":
				err = db.SetCooldown(ctx, id, "unauthorized", until.Add(time.Minute))
			case "generation":
				err = db.UpdateCredentials(ctx, id, map[string]any{"access_token": "manual-winner"})
			case "deleted":
				err = db.SoftDeleteAccount(ctx, id)
			case "disabled":
				err = db.SetAccountEnabled(ctx, id, false)
			case "identity":
				snap.Identity.ChatGPTUserID = "wrong-user"
			case "receipt":
				attemptID = "not-the-commit"
			case "token":
				access = "not-the-token"
			}
			if err != nil {
				t.Fatal(err)
			}
			cleared, err := db.CompleteOAuthRecoveryRefresh(ctx, snap, attemptID, access, "new-rt", until)
			if scenario == "success" {
				if err != nil || !cleared {
					t.Fatal("matching refresh not completed", err)
				}
				return
			}
			if scenario == "new-429" {
				if err != nil || cleared {
					t.Fatal("new 429 erased", err)
				}
				return
			}
			if err == nil || cleared {
				t.Fatal("stale completion accepted", scenario)
			}
		})
	}
}
