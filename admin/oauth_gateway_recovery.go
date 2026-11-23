package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"time"

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
		t.refreshRecoveredAccountViews(snap.AccountID, snap.Generation+1)
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
	for _, key := range []string{"access_token", "refresh_token", "id_token", "expires_at", "access_token_type", "plan_type"} {
		if value, ok := parsed[key]; ok {
			updates[key] = value
		}
	}
	if name := oauthGatewayWorkspaceName(raw, snap.Identity.ChatGPTAccountID); name != "" {
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
	t.h.db.InsertAccountEventAsync(snap.AccountID, "oauth_recovered", "oauth_gateway")
	if t.h.store != nil {
		if err := t.h.store.ReloadOAuthRecoveredAccount(ctx, snap.AccountID); err != nil {
			t.h.invalidateAccountSnapshotCaches()
			return accepted, service.LocalImportError("runtime_load_pending")
		}
	}
	t.refreshRecoveredAccountViews(snap.AccountID, snap.Generation+1)
	return accepted, nil
}

func (t oauthGatewayTarget) refreshRecoveredAccountViews(id, generation int64) {
	h := t.h
	// Invalidate after publishing runtime state, then again after real usage is observed.
	h.invalidateAccountSnapshotCaches()
	if h.store == nil || !h.store.ImportUsageProbeEnabled() {
		return
	}
	h.runImportProbeTask(func(ctx context.Context) {
		acc := h.store.FindByID(id)
		if ctx.Err() != nil || !h.store.ImportUsageProbeEnabled() || acc == nil || acc.GetCredentialGeneration() != generation || atomic.LoadInt32(&acc.Disabled) != 0 || acc.GetAccessToken() == "" || !acc.TryBeginUsageProbe() {
			return
		}
		defer acc.FinishUsageProbe()
		defer h.invalidateAccountSnapshotCaches()
		// An existing account must never go through the import-time merge/soft-delete path.
		probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		if err := h.usageProbeFunc()(probeCtx, acc); err != nil {
			log.Printf("恢复账号 %d 用量采样失败: %v", id, err)
		}
	})
}

var _ service.RecoveryTarget = oauthGatewayTarget{}
