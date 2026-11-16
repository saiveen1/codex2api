package auth

import (
	"context"
	"log"
	"strings"

	"github.com/codex2api/internal/openaiidentity"
)

// Run after the existing exchange, before its durable publication. Metadata
// failure must never discard a rotating RT or start a second refresh exchange.
func (s *Store) hydrateCodexRefreshedWorkspace(ctx context.Context, acc *Account, td *TokenData, updates map[string]any, proxyURL string) {
	acc.mu.RLock()
	identity := CodexWorkspaceIdentity{AccountID: acc.AccountID, Email: acc.Email, Headers: cloneCodexWorkspaceHeaders(acc.CustomHeaders)}
	id := acc.DBID
	acc.mu.RUnlock()
	if s.db != nil {
		row, err := s.db.GetAccountByID(ctx, id)
		if err != nil {
			updates["codex_workspace_error"] = "workspace identity: credential_snapshot_unavailable"
			for _, key := range []string{"email", "account_id", "workspace_id", "user_id"} {
				delete(updates, key)
			}
			return
		}
		identity.WorkspaceID, identity.UserID = row.GetCredential("workspace_id"), row.GetCredential("user_id")
	}
	if identity.WorkspaceID == "" {
		identity.WorkspaceID = identity.AccountID
	}
	identity.TrustedWorkspace = openaiidentity.ValidWebWorkspaceID(identity.WorkspaceID)
	identity = NormalizeCodexWorkspaceIdentity(identity, td.IDToken, td.AccessToken)
	if identity.TrustedWorkspace {
		identity.AccountID = identity.WorkspaceID
	}
	if email, workspace := openaiidentity.TokenIdentity(td.IDToken, td.AccessToken); workspace != "" && identity.Email != "" && !strings.EqualFold(email, identity.Email) {
		updates["codex_workspace_error"] = "workspace identity: login_identity_mismatch"
		for _, key := range []string{"email", "account_id", "workspace_id", "user_id"} {
			delete(updates, key)
		}
		return
	}
	identity, err := HydrateCodexWorkspaceIdentity(ctx, identity, td.AccessToken, proxyURL)
	if err != nil {
		updates["codex_workspace_error"] = err.Error()
		log.Printf("[account %d] refresh identity pending: %v", id, err)
		// New tokens still persist. Never replace a verified old identity with
		// partial/mismatched discovery, and never touch per-route headers.
		if WorkspaceIdentityRejected(err) {
			for _, key := range []string{"email", "account_id", "workspace_id", "user_id"} {
				delete(updates, key)
			}
		}
		return
	}
	updates["codex_workspace_error"] = ""
	if identity.AccountID != "" || identity.UserID != "" {
		updates["account_id"] = identity.AccountID
	}
	if identity.WorkspaceID != "" || identity.UserID != "" {
		updates["workspace_id"] = identity.WorkspaceID
	}
	if identity.UserID != "" {
		updates["user_id"] = identity.UserID
	}
	if identity.Email != "" {
		updates["email"] = identity.Email
	}
}

func cloneCodexWorkspaceHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		out[key] = value
	}
	return out
}
