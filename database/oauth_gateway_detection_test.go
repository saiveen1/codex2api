package database

import (
	"context"
	"database/sql"
	"testing"
	"time"

	ic "github.com/codex2api/internal/intelligencecheck"
)

func TestOAuthGatewayDetectionPause(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	s, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Enabled, s.NewAccountsEnabled = true, true
	group, err := db.CreateAccountGroup(ctx, "team", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	s.Rules = []ic.Rule{{ID: "codex", Name: "Codex", Channel: "codex", GroupID: group, Automatic: true}}
	if _, err = db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	creds := map[string]any{"email": "fixture@example.invalid", "user_id": "user-original", "workspace_id": "workspace-original", "account_id": "workspace-original", "refresh_token": "old-rt", "access_token": "old-at", "plan_type": "pro"}
	paused, err := db.InsertOAuthGatewayAccount(ctx, "paused", creds, "", []int64{group}, true)
	if err != nil {
		t.Fatal(err)
	}
	defaultID, err := db.InsertOAuthGatewayAccount(ctx, "default", creds, "", []int64{group}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertState := func(id int64, enabled bool, due int64) {
		t.Helper()
		var got bool
		var gotDue int64
		if err := db.conn.QueryRowContext(ctx, `SELECT enabled,next_due_ms FROM intelligence_check_accounts WHERE account_id=$1`, id).Scan(&got, &gotDue); err != nil || got != enabled || gotDue != due {
			t.Fatalf("account %d: enabled=%v due=%d err=%v", id, got, gotDue, err)
		}
	}
	assertState(paused, false, 0)
	assertState(defaultID, true, 0)
	now := time.Now()
	for _, check := range []struct {
		paused bool
		id     int64
	}{{true, paused}, {false, defaultID}} {
		participants, count, err := db.ListIntelligenceRuleParticipants(ctx, "codex", 0, 20, now, check.paused, IntelligenceCheckPreflight{})
		if err != nil || count != 1 || len(participants) != 1 || participants[0].AccountID != check.id {
			t.Fatalf("paused=%v participants=%+v count=%d err=%v", check.paused, participants, count, err)
		}
	}
	if err := db.SetIntelligenceCheckEnabled(ctx, []int64{paused}, true); err != nil {
		t.Fatal(err)
	}
	assertState(paused, true, 0)
	if _, count, err := db.ListIntelligenceRuleParticipants(ctx, "codex", 0, 20, now, false, IntelligenceCheckPreflight{}); err != nil || count != 2 {
		t.Fatalf("restored account missing from active rule: count=%d err=%v", count, err)
	}

	row, err := db.GetAccountByID(ctx, paused)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := OAuthRecoverySnapshot(row)
	if err != nil {
		t.Fatal(err)
	}
	updates := map[string]any{"user_id": "user-original", "workspace_id": "workspace-original", "access_token": "new-at", "refresh_token": "new-rt"}
	if _, err := db.conn.ExecContext(ctx, `UPDATE intelligence_check_accounts SET next_due_ms=123 WHERE account_id=$1`, paused); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverOAuthGatewayAccount(ctx, snap, updates, "job", true); err != nil {
		t.Fatal(err)
	}
	assertState(paused, false, 0)
	row, err = db.GetAccountByID(ctx, paused)
	if err != nil {
		t.Fatal(err)
	}
	snap, err = OAuthRecoverySnapshot(row)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE intelligence_check_accounts SET next_due_ms=123 WHERE account_id=$1`, paused); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverOAuthGatewayAccount(ctx, snap, updates, "job-again", false); err != nil {
		t.Fatal(err)
	}
	assertState(paused, false, 123)
}

func TestOAuthGatewayDetectionPauseFailureRollsBack(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	creds := map[string]any{"email": "fixture@example.invalid", "user_id": "user-original", "workspace_id": "workspace-original", "account_id": "workspace-original", "refresh_token": "old-rt", "access_token": "old-at"}
	id, err := db.InsertOAuthGatewayAccount(ctx, "original", creds, "", nil, false)
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
	if _, err := db.conn.ExecContext(ctx, `CREATE TRIGGER fail_detection_pause BEFORE UPDATE OF enabled ON intelligence_check_accounts BEGIN SELECT RAISE(ABORT, 'pause failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOAuthGatewayAccount(ctx, "rollback", creds, "", nil, true); err == nil {
		t.Fatal("insert accepted failed pause")
	}
	count, err := db.CountAll(ctx)
	if err != nil || count != 1 {
		t.Fatalf("insert not rolled back: count=%d err=%v", count, err)
	}
	updates := map[string]any{"user_id": "user-original", "workspace_id": "workspace-original", "access_token": "new-at", "refresh_token": "new-rt"}
	if err := db.RecoverOAuthGatewayAccount(ctx, snap, updates, "job", true); err == nil {
		t.Fatal("recovery accepted failed pause")
	}
	after, err := db.GetAccountByID(ctx, id)
	if err != nil || after.CredentialGeneration != snap.Generation || after.GetCredential("access_token") != "old-at" {
		t.Fatalf("recovery not rolled back: %+v %v", after, err)
	}
}
