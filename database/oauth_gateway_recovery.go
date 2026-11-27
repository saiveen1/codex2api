package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/codex2api/internal/oauthgateway/service"
	"github.com/codex2api/internal/oauthgateway/upstream"
	"github.com/codex2api/internal/openaiidentity"
)

func OAuthRecoverySnapshot(row *AccountRow) (service.RecoverySnapshot, error) {
	if row == nil || row.Status == "deleted" || row.Status == "disabled" || !row.Enabled {
		return service.RecoverySnapshot{}, service.LocalImportError("recovery_account_unavailable")
	}
	kind := strings.TrimSpace(row.GetCredential("upstream_type"))
	if (kind != "" && kind != "codex") || row.GetCredential("auth_mode") != "" || row.GetCredential("refresh_token") == "" {
		return service.RecoverySnapshot{}, service.LocalImportError("recovery_account_unavailable")
	}
	user := strings.TrimSpace(row.GetCredential("user_id"))
	workspace := openaiidentity.NormalizeWorkspaceID(row.GetCredential("workspace_id"))
	if workspace == "" {
		workspace = openaiidentity.NormalizeWorkspaceID(row.GetCredential("account_id"))
	}
	identifier := strings.ToLower(strings.TrimSpace(row.GetCredential("email")))
	if identifier == "" {
		identifier = strings.TrimSpace(row.GetCredential("phone_number"))
	}
	if user == "" || !openaiidentity.ValidWebWorkspaceID(workspace) || identifier == "" || row.CredentialGeneration <= 0 || row.CredentialFamilyID == "" {
		return service.RecoverySnapshot{}, service.LocalImportError("recovery_identity_unverified")
	}
	// Metadata and cooldown writes do not invalidate an otherwise unchanged token.
	version := map[string]string{}
	for _, key := range []string{"refresh_token", "access_token", "session_token", "id_token", "user_id", "workspace_id", "account_id", "email", "phone_number", "auth_mode", "upstream_type"} {
		version[key] = row.GetCredential(key)
	}
	raw, _ := json.Marshal(version)
	digest := sha256.Sum256(raw)
	return service.RecoverySnapshot{AccountID: row.ID, Identifier: identifier, Identity: upstream.Identity{ChatGPTUserID: user, ChatGPTAccountID: workspace}, Generation: row.CredentialGeneration, Family: row.CredentialFamilyID, CredentialVersion: hex.EncodeToString(digest[:])}, nil
}

// RecoverOAuthGatewayAccount uses the existing Codex generation/family fence.
// The row lock covers enabled/deleted and identity checks, not just generation.
// No insert and no mutation of proxy, groups, statistics or sibling routes.
func (db *DB) RecoverOAuthGatewayAccount(ctx context.Context, expected service.RecoverySnapshot, updates map[string]any, jobID string, pauseAutoDetection bool) error {
	return db.withOAuthGatewayWrite(ctx, func(conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		query := `SELECT credentials,credential_generation,credential_family_id,status,enabled FROM accounts WHERE id=$1`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		row := AccountRow{ID: expected.AccountID}
		var raw any
		if err = tx.QueryRowContext(ctx, query, row.ID).Scan(&raw, &row.CredentialGeneration, &row.CredentialFamilyID, &row.Status, &row.Enabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return service.LocalImportError("recovery_account_unavailable")
			}
			return err
		}
		row.Credentials = decodeCredentials(raw)
		current, err := OAuthRecoverySnapshot(&row)
		if err != nil {
			return err
		}
		if current != expected {
			return service.LocalImportError("recovery_credentials_changed")
		}
		if credentialStringFromMap(updates, "user_id") != expected.Identity.ChatGPTUserID || credentialStringFromMap(updates, "workspace_id") != expected.Identity.ChatGPTAccountID {
			return service.LocalImportError("recovery_identity_mismatch")
		}
		merged := mergeCredentialMaps(row.Credentials, updates)
		merged["credential_family_id"] = row.CredentialFamilyID
		merged["codex_refresh_error"] = ""
		merged["codex_workspace_error"] = ""
		merged["oauth_gateway_recovery_job"] = jobID
		if err = db.writeCodexRefreshCredentials(ctx, tx, row, merged, true); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE accounts SET cooldown_until=NULL,cooldown_reason='' WHERE id=$1 AND cooldown_reason='unauthorized'`, row.ID); err != nil {
			return err
		}
		if pauseAutoDetection {
			if _, err = tx.ExecContext(ctx, `UPDATE intelligence_check_accounts SET enabled=FALSE,next_due_ms=0 WHERE account_id=$1`, row.ID); err != nil {
				return err
			}
		}
		// An unknown COMMIT stays import_unknown at the worker; it is never replayed.
		return tx.Commit()
	})
}

// CompleteOAuthRecoveryRefresh clears only the 401 belonging to this exact
// successful refresh; a changed account, credential or newer cooldown wins.
func (db *DB) CompleteOAuthRecoveryRefresh(ctx context.Context, original service.RecoverySnapshot, attemptID, accessToken, refreshToken string, until time.Time) (bool, error) {
	clearRuntime := false
	err := db.withOAuthGatewayWrite(ctx, func(conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		query := `SELECT credentials,credential_generation,credential_family_id,status,enabled,cooldown_reason,cooldown_until FROM accounts WHERE id=$1`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		row := AccountRow{ID: original.AccountID}
		var raw any
		if err = tx.QueryRowContext(ctx, query, row.ID).Scan(&raw, &row.CredentialGeneration, &row.CredentialFamilyID, &row.Status, &row.Enabled, &row.CooldownReason, &row.CooldownUntil); err != nil {
			return err
		}
		row.Credentials = decodeCredentials(raw)
		current, err := OAuthRecoverySnapshot(&row)
		if err != nil {
			return err
		}
		if current.Identity != original.Identity || current.Family != original.Family || current.Identifier != original.Identifier || current.Generation != original.Generation+1 || attemptID == "" || row.GetCredential("codex_refresh_attempt_id") != attemptID || row.GetCredential("access_token") != accessToken || row.GetCredential("refresh_token") != refreshToken {
			return ErrCodexCredentialsChanged
		}
		if row.CooldownReason == "unauthorized" {
			if !row.CooldownUntil.Valid || !row.CooldownUntil.Time.Truncate(time.Microsecond).Equal(until.Truncate(time.Microsecond)) {
				return ErrCodexCredentialsChanged
			}
			if _, err = tx.ExecContext(ctx, `UPDATE accounts SET cooldown_reason='',cooldown_until=NULL,error_message='' WHERE id=$1 AND credential_generation=$2 AND enabled=TRUE AND status<>'deleted' AND status<>'disabled' AND cooldown_reason='unauthorized'`, row.ID, current.Generation); err != nil {
				return err
			}
			clearRuntime = true
		} else if row.CooldownReason == "" {
			clearRuntime = true
		}
		return tx.Commit()
	})
	return clearRuntime, err
}
