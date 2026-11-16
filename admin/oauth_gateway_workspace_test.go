package admin

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOAuthGatewayLegacyWorkspaceDiscoveryKeepsDuplicateGuard(t *testing.T) {
	h, db := importWorkspaceFixture(t)
	calls := 0
	ctx := importWorkspaceContext(t, "legacy@example.com", "workspace-default", &calls)
	token := importWorkspaceToken(t, "gateway-legacy", time.Now().Add(time.Hour))
	raw, err := json.Marshal(map[string]any{"platform": "openai", "type": "oauth", "name": "gateway legacy", "credentials": map[string]any{"email": "legacy@example.com", "access_token": token, "refresh_token": "synthetic-gateway-rt", "chatgpt_account_id": "user-legacy", "chatgpt_user_id": "user-legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (oauthGatewayTarget{h}).Import(ctx, raw, nil, "")
	if err != nil || result.Success != 1 || len(result.CreatedIDs) != 1 || calls != 2 {
		t.Fatalf("legacy gateway import: %+v %v metadata=%d", result, err, calls)
	}
	row, err := db.GetAccountByID(ctx, result.CreatedIDs[0])
	if err != nil || row.GetCredential("workspace_id") != "workspace-default" || row.GetCredential("account_id") != "workspace-default" || row.GetCredential("user_id") != "user-legacy" {
		t.Fatal("gateway did not preserve separate authenticated identity")
	}
	duplicate, err := (oauthGatewayTarget{h}).Import(ctx, raw, nil, "")
	if err == nil || duplicate.Success != 0 || duplicate.Failed != 1 {
		t.Fatal("gateway compatibility bypassed duplicate/no-recovery guard")
	}
	rows, err := db.ListActive(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatal("duplicate gateway import mutated account pool")
	}
}

func TestOAuthGatewayLegacyIdentityMismatchStillRejects(t *testing.T) {
	h, db := importWorkspaceFixture(t)
	calls := 0
	ctx := importWorkspaceContext(t, "another@example.com", "workspace-default", &calls)
	token := importWorkspaceToken(t, "gateway-mismatch", time.Now().Add(time.Hour))
	raw, err := json.Marshal(map[string]any{"platform": "openai", "type": "oauth", "credentials": map[string]any{"email": "legacy@example.com", "access_token": token, "refresh_token": "synthetic-gateway-rt", "chatgpt_account_id": "user-legacy", "chatgpt_user_id": "user-legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (oauthGatewayTarget{h}).Import(ctx, raw, nil, "")
	if err == nil || result.Success != 0 {
		t.Fatal("gateway accepted mismatched authenticated identity")
	}
	rows, readErr := db.ListActive(ctx)
	if readErr != nil || len(rows) != 0 {
		t.Fatal("rejected identity changed account pool")
	}
}
