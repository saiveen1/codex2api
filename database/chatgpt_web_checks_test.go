package database

import (
	"context"
	"testing"
	"time"
)

func TestChatGPTWebDiscoveredWorkspaceAndFailureSnapshot(t *testing.T) {
	db, _, id := newChatGPTWebTestDB(t)
	ctx := context.Background()
	if err := db.UpdateCredentials(ctx, id, map[string]any{"account_id": "user-legacy"}); err != nil {
		t.Fatal(err)
	}
	state, err := db.SetChatGPTWebAccountEnabled(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().Add(-time.Minute).UTC()
	quota := 3
	state.Status, state.Capabilities, state.ImageQuotaRemaining, state.ObservedAt = "ready", []string{"generate"}, &quota, &observed
	state.WorkspaceID, state.WorkspaceFingerprint = "resolved-default", chatGPTWebHash("resolved-default")
	state.WorkspaceBindingFingerprint, state.ProxyFingerprint = chatGPTWebHash(""), chatGPTWebHash("")
	state.LastCheckAt, state.LastCheckStage = &observed, "complete"
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, state, "test-AT"); !ok || err != nil {
		t.Fatalf("legacy discovery: %v %v", ok, err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil || row.GetCredential("account_id") != "user-legacy" {
		t.Fatal("Web observation rewrote native identity")
	}
	failed := observed.Add(10 * time.Second)
	if ok, err := db.RecordChatGPTWebCheckFailure(ctx, state, "test-AT", chatGPTWebHash(""), failed, "auth_required", "identity", 401); !ok || err != nil {
		t.Fatalf("failure: %v %v", ok, err)
	}
	stored, err := db.GetChatGPTWebAccountState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "auth_required" || stored.LastCheckErrorCode != "auth_required" || stored.LastCheckStage != "identity" || stored.LastCheckHTTPStatus != 401 || stored.LastCheckAt == nil || !stored.LastCheckAt.Equal(failed) {
		t.Fatalf("missing classified failure: %+v", stored)
	}
	if stored.ImageQuotaRemaining == nil || *stored.ImageQuotaRemaining != quota || stored.ObservedAt == nil || !stored.ObservedAt.Equal(observed) || stored.WorkspaceID != "resolved-default" {
		t.Fatal("failed refresh lost prior observation")
	}
	if ok, err := db.RecordChatGPTWebCheckFailure(ctx, state, "test-AT", chatGPTWebHash(""), observed, "upstream_error", "quota", 502); ok || err != nil {
		t.Fatalf("older failure won: %v %v", ok, err)
	}
	if err := db.UpdateAccountCustomHeaders(ctx, id, map[string]string{"ChatGPT-Account-ID": "team-workspace"}); err != nil {
		t.Fatal(err)
	}
	fresh := time.Now().UTC()
	state.ObservedAt, state.LastCheckAt = &fresh, &fresh
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, state, "test-AT"); ok || err != nil {
		t.Fatalf("default observation survived explicit route change: %v %v", ok, err)
	}
	if ok, err := db.RecordChatGPTWebCheckFailure(ctx, state, "test-AT", chatGPTWebHash(""), fresh, "upstream_error", "quota", 502); ok || err != nil {
		t.Fatalf("old route failure won: %v %v", ok, err)
	}
}

func TestChatGPTWebCheckSchemaUpgradePreservesAccount(t *testing.T) {
	db, _, id := newChatGPTWebTestDB(t)
	ctx := context.Background()
	state, err := db.SetChatGPTWebAccountEnabled(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.conn.ExecContext(ctx, `UPDATE chatgpt_web_account_states SET status='ready';
		ALTER TABLE chatgpt_web_account_states DROP COLUMN workspace_id;
		ALTER TABLE chatgpt_web_account_states DROP COLUMN workspace_binding_fingerprint;
		ALTER TABLE chatgpt_web_account_states DROP COLUMN last_check_at;
		ALTER TABLE chatgpt_web_account_states DROP COLUMN last_check_stage;
		ALTER TABLE chatgpt_web_account_states DROP COLUMN last_check_error_code;
		ALTER TABLE chatgpt_web_account_states DROP COLUMN rejected_credential_revision;
		ALTER TABLE chatgpt_web_account_states DROP COLUMN last_check_http_status`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := db.ensureChatGPTWebSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := db.GetChatGPTWebAccountState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Enabled || stored.Status != "unverified" || stored.DeviceID != state.DeviceID || stored.EnvironmentRevision != state.EnvironmentRevision || stored.WorkspaceID != "" || stored.LastCheckAt != nil {
		t.Fatalf("unsafe upgrade: %+v", stored)
	}
}
