package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/oauthgateway/service"
	"github.com/codex2api/internal/oauthgateway/upstream"
	"github.com/codex2api/internal/openaiidentity"
)

func (t oauthGatewayTarget) RecoverySnapshot(ctx context.Context, id int64) (service.RecoverySnapshot, error) {
	row, err := t.h.db.GetAccountByIDIncludingDeleted(ctx, id)
	if err != nil {
		return service.RecoverySnapshot{}, service.LocalImportError("recovery_account_unavailable")
	}
	return database.OAuthRecoverySnapshot(row)
}

func (t oauthGatewayTarget) RefreshBeforeRecovery(ctx context.Context, snap service.RecoverySnapshot) (bool, error) {
	if t.h.store == nil {
		return false, service.LocalImportError("recovery_refresh_requires_review")
	}
	current, err := t.RecoverySnapshot(ctx, snap.AccountID)
	if err != nil {
		return false, err
	}
	if current != snap {
		return false, service.LocalImportError("recovery_credentials_changed")
	}
	err = t.h.store.RefreshOAuthRecoveryAccount(ctx, snap)
	if err == nil {
		return false, nil
	}
	if auth.IsOAuthRecoveryRefreshFailure(err) {
		return true, nil
	}
	return false, service.LocalImportError("recovery_refresh_requires_review")
}

func (t oauthGatewayTarget) Recover(ctx context.Context, snap service.RecoverySnapshot, raw json.RawMessage, jobID string) (upstream.ImportResult, error) {
	var result struct {
		Platform string `json:"platform"`
		Type     string `json:"type"`
		sub2apiAccountEntry
	}
	if len(raw) > 1<<20 || json.Unmarshal(raw, &result) != nil || result.Platform != "openai" || result.Type != "oauth" {
		return oauthGatewayImportFailure("invalid_gateway_account")
	}
	c := result.Credentials
	if c.AuthMode != "" || c.AgentIdentity != nil || c.AgentPrivateKey != "" || c.AgentRuntimeID != "" || strings.TrimSpace(c.RefreshToken) == "" || strings.TrimSpace(c.AccessToken) == "" {
		return oauthGatewayImportFailure("invalid_gateway_account")
	}
	if strings.TrimSpace(c.ChatGPTUserID) != snap.Identity.ChatGPTUserID || openaiidentity.NormalizeWorkspaceID(c.ChatGPTAccountID) != snap.Identity.ChatGPTAccountID {
		return oauthGatewayImportFailure("recovery_identity_mismatch")
	}
	tokens := sub2apiAccountEntryToTokens(result.sub2apiAccountEntry)
	if len(tokens) != 1 {
		return oauthGatewayImportFailure("invalid_gateway_account")
	}
	seed := importTokenSeed(tokens[0], nil)
	if !strings.EqualFold(seed.email, snap.Identifier) {
		return oauthGatewayImportFailure("recovery_identity_mismatch")
	}
	mismatch := func(user, workspace, email string) bool {
		return (user != "" && user != snap.Identity.ChatGPTUserID) || (workspace != "" && openaiidentity.NormalizeWorkspaceID(workspace) != snap.Identity.ChatGPTAccountID) || (email != "" && !strings.EqualFold(email, snap.Identifier))
	}
	if info := auth.ParseIDToken(c.IDToken); info != nil && mismatch(info.UserID, info.ChatGPTAccountID, info.Email) {
		return oauthGatewayImportFailure("recovery_identity_mismatch")
	}
	if info := auth.ParseAccessToken(c.AccessToken); info != nil && mismatch(info.UserID, info.ChatGPTAccountID, info.Email) {
		return oauthGatewayImportFailure("recovery_identity_mismatch")
	}
	seed.accountID, seed.workspaceID, seed.userID = snap.Identity.ChatGPTAccountID, snap.Identity.ChatGPTAccountID, snap.Identity.ChatGPTUserID
	parsed := tokenCredentialMap(seed)
	updates := map[string]any{"user_id": snap.Identity.ChatGPTUserID, "workspace_id": snap.Identity.ChatGPTAccountID, "account_id": snap.Identity.ChatGPTAccountID, "session_token": ""}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "expires_at", "access_token_type"} {
		if value, ok := parsed[key]; ok {
			updates[key] = value
		}
	}
	if name := oauthGatewayWorkspaceName(raw); name != "" {
		updates["workspace_name"] = name
	}
	if err := t.h.db.RecoverOAuthGatewayAccount(ctx, snap, updates, jobID); err != nil {
		var known *upstream.Error
		if errors.As(err, &known) && !known.Uncertain {
			return upstream.ImportResult{Total: 1, Failed: 1}, known
		}
		return upstream.ImportResult{}, service.LocalImportError("import_outcome_unknown")
	}
	accepted := upstream.ImportResult{Total: 1, Updated: 1}
	t.h.invalidateAccountSnapshotCaches()
	t.h.db.InsertAccountEventAsync(snap.AccountID, "oauth_recovered", "oauth_gateway")
	if t.h.store != nil {
		if err := t.h.store.ReloadOAuthRecoveredAccount(ctx, snap.AccountID); err != nil {
			return accepted, service.LocalImportError("runtime_load_pending")
		}
	}
	return accepted, nil
}

var _ service.RecoveryTarget = oauthGatewayTarget{}
