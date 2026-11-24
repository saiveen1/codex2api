package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/internal/oauthgateway/service"
)

func oauthGatewayTestKey(t *testing.T, value string) {
	t.Helper()
	previous := append([]byte(nil), credCipherKey()...)
	setCredEncryptionKeyForTest(value)
	t.Cleanup(func() { credKey = previous })
}
func newOAuthGatewayStoreDB(t *testing.T) *DB {
	t.Helper()
	conn, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	return &DB{conn: conn, driver: "sqlite", sqliteWriteSem: make(chan struct{}, 1)}
}
func TestOAuthGatewayRequiresEncryption(t *testing.T) {
	oauthGatewayTestKey(t, "")
	db := newOAuthGatewayStoreDB(t)
	if _, err := db.OpenOAuthGatewayStore(t.Context()); !errors.Is(err, ErrOAuthGatewayEncryption) {
		t.Fatalf("missing key: %v", err)
	}
	var count int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='oauth_gateway_state'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("disabled module wrote state: count=%d err=%v", count, err)
	}
}
func TestOAuthGatewayEncryptedRoundTripAndWrongKey(t *testing.T) {
	oauthGatewayTestKey(t, "synthetic-encryption-root")
	db := newOAuthGatewayStoreDB(t)
	s, err := db.OpenOAuthGatewayStore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Update(func(state *service.State) error {
		state.Settings = service.Settings{OAuthURL: "https://gateway.example.invalid", OAuthToken: "synthetic-secret-CDK"}
		state.Jobs = []service.Job{{ID: "original-task", Fingerprint: "original-fingerprint"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err = db.conn.QueryRow(`SELECT payload FROM oauth_gateway_state WHERE id=1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"synthetic-secret-CDK", "original-task", "original-fingerprint", "gateway.example.invalid"} {
		if strings.Contains(payload, secret) {
			t.Fatal("plaintext in persisted state")
		}
	}
	view, err := s.View()
	if err != nil {
		t.Fatal(err)
	}
	view.Jobs[0].ID = "tampered"
	view, _ = s.View()
	if view.Jobs[0].ID != "original-task" {
		t.Fatal("View leaked mutable store memory")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	setCredEncryptionKeyForTest("wrong-root")
	if _, err = db.OpenOAuthGatewayStore(t.Context()); !errors.Is(err, ErrOAuthGatewayState) {
		t.Fatalf("wrong key accepted: %v", err)
	}
	setCredEncryptionKeyForTest("synthetic-encryption-root")
	reopened, err := db.OpenOAuthGatewayStore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	view, err = reopened.View()
	if err != nil || view.Settings.OAuthToken != "synthetic-secret-CDK" || view.Jobs[0].ID != "original-task" {
		t.Fatalf("restart lost state: %v", err)
	}
}
func TestOAuthGatewayLeaseAndCASFence(t *testing.T) {
	oauthGatewayTestKey(t, "synthetic-encryption-root")
	db := newOAuthGatewayStoreDB(t)
	first, err := db.OpenOAuthGatewayStore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err = db.OpenOAuthGatewayStore(t.Context()); !errors.Is(err, ErrOAuthGatewayOwner) {
		t.Fatalf("concurrent owner accepted: %v", err)
	}
	if _, err = db.conn.Exec(`UPDATE oauth_gateway_state SET lease_until=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	second, err := db.OpenOAuthGatewayStore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err = first.Update(func(s *service.State) error { s.Settings.OAuthToken = "must-not-write"; return nil }); err == nil {
		t.Fatal("old writer overwrote new owner")
	}
	if _, err = first.View(); err == nil {
		t.Fatal("poisoned store still readable")
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	if err = second.Update(func(s *service.State) error { return nil }); err != nil {
		t.Fatalf("old close released new lease: %v", err)
	}
	if _, err = db.conn.Exec(`UPDATE oauth_gateway_state SET revision=revision+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err = second.Update(func(s *service.State) error { return nil }); err == nil {
		t.Fatal("stale revision accepted")
	}
	if _, err = second.View(); err == nil {
		t.Fatal("CAS failure did not poison store")
	}
}
func TestOAuthGatewayExpiredLeaseCannotRenew(t *testing.T) {
	oauthGatewayTestKey(t, "synthetic-encryption-root")
	db := newOAuthGatewayStoreDB(t)
	s, err := db.OpenOAuthGatewayStore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.leaseUntil = 0
	if _, err = db.conn.Exec(`UPDATE oauth_gateway_state SET lease_until=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.View(); !errors.Is(err, ErrOAuthGatewayOwner) {
		t.Fatalf("expired lease renewed: %v", err)
	}
}
func TestOAuthGatewayAccountAndGroupsAtomic(t *testing.T) {
	oauthGatewayTestKey(t, "synthetic-encryption-root")
	db, err := New("sqlite", filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	group, err := db.CreateAccountGroup(ctx, "OAuth test", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	credentials := map[string]any{"access_token": "synthetic-AT", "refresh_token": "synthetic-RT", "email": "fixture@example.invalid", "workspace_id": "workspace-fixture"}
	id, err := db.InsertOAuthGatewayAccount(ctx, "fixture", credentials, "", []int64{group, group}, false)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := db.GetAccountGroupIDs(ctx, id)
	if err != nil || len(groups) != 1 || groups[0] != group {
		t.Fatalf("groups not committed: %v %v", groups, err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil || row.GetCredential("access_token") != "synthetic-AT" || row.CredentialFamilyID == "" {
		t.Fatalf("account defaults lost: %v", err)
	}
	var raw string
	if err = db.conn.QueryRow(`SELECT credentials FROM accounts WHERE id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "synthetic-AT") || strings.Contains(raw, "synthetic-RT") {
		t.Fatal("credentials persisted in plaintext")
	}
	// A binding failure after INSERT must roll the account back too.
	if _, err = db.conn.Exec(`CREATE TRIGGER fail_gateway_group BEFORE INSERT ON account_group_members BEGIN SELECT RAISE(ABORT, 'synthetic binding failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.InsertOAuthGatewayAccount(ctx, "must-rollback", credentials, "", []int64{group}, false); err == nil {
		t.Fatal("binding failure accepted")
	}
	count, err := db.CountAll(ctx)
	if err != nil || count != 1 {
		t.Fatalf("partial account insert: count=%d err=%v", count, err)
	}
}

func TestOAuthGatewayAccountSummaryLookups(t *testing.T) {
	db := newOAuthGatewayStoreDB(t)
	ctx := t.Context()
	_, err := db.conn.ExecContext(ctx, `CREATE TABLE accounts (id INTEGER PRIMARY KEY, status TEXT, enabled BOOLEAN);
		CREATE TABLE proxies (id INTEGER PRIMARY KEY, url TEXT);
		INSERT INTO accounts VALUES (1,'active',FALSE),(2,'cooldown',TRUE),(3,'deleted',FALSE),(4,'disabled',TRUE),(5,'active',FALSE);
		INSERT INTO proxies VALUES (13,'http://proxy.example.invalid:8080');`)
	if err != nil {
		t.Fatal(err)
	}
	disabled := db.OAuthGatewayDisabledAccounts(ctx, []int64{1, 2, 3, 4, 6})
	if len(disabled) != 2 || !disabled[1] || !disabled[4] {
		t.Fatalf("wrong persisted disabled accounts: %v", disabled)
	}
	if id := db.OAuthGatewayProxyID(ctx, ""); id == nil || *id != 0 {
		t.Fatalf("missing unbound proxy result: %v", id)
	}
	if id := db.OAuthGatewayProxyID(ctx, "http://proxy.example.invalid:8080"); id == nil || *id != 13 {
		t.Fatalf("missing committed proxy result: %v", id)
	}
	if id := db.OAuthGatewayProxyID(ctx, "http://unknown.example.invalid"); id != nil {
		t.Fatalf("invented proxy result: %v", id)
	}
}
