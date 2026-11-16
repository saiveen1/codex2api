package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/codex2api/internal/openaiidentity"
)

func (db *DB) ensureChatGPTWebCheckSchema(ctx context.Context) error {
	columns := []struct{ name, definition string }{
		{"workspace_id", "TEXT NOT NULL DEFAULT ''"},
		{"workspace_binding_fingerprint", "TEXT NOT NULL DEFAULT ''"},
		{"last_check_at", "BIGINT NULL"},
		{"last_check_stage", "TEXT NOT NULL DEFAULT ''"},
		{"last_check_error_code", "TEXT NOT NULL DEFAULT ''"},
		{"last_check_http_status", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		if db.isSQLite() {
			if err := db.ensureSQLiteColumn(ctx, "chatgpt_web_account_states", column.name, column.definition); err != nil {
				return err
			}
		} else if _, err := db.conn.ExecContext(ctx, "ALTER TABLE chatgpt_web_account_states ADD COLUMN IF NOT EXISTS "+column.name+" "+column.definition); err != nil {
			return err
		}
	}
	return nil
}

func requestedChatGPTWebWorkspace(creds map[string]any) (string, error) {
	return openaiidentity.RequestedWebWorkspace(credentialStringFromMap(creds, "account_id"), (&AccountRow{Credentials: creds}).GetCredentialStringMap("custom_headers"))
}

func chatGPTWebCheckStage(stage string) bool {
	switch stage {
	case "", "admission", "bootstrap", "identity", "workspace", "quota", "persist", "complete":
		return true
	}
	return false
}

// RecordChatGPTWebCheckFailure keeps the previous quota observation intact but
// revokes Web readiness. It never alters native credentials/status or another
// environment's newer observation. Busy/stale attempts must not call this.
func (db *DB) RecordChatGPTWebCheckFailure(ctx context.Context, state *ChatGPTWebAccountState, expectedAT, expectedBinding string, checked time.Time, code, stage string, upstreamStatus int) (bool, error) {
	if state == nil || state.AccountID <= 0 || state.EnvironmentRevision <= 0 || code == "" || !chatGPTWebSafeError(code) || !chatGPTWebCheckStage(stage) || stage == "" || checked.IsZero() || checked.After(time.Now().Add(time.Minute)) || upstreamStatus < 0 || upstreamStatus > 599 {
		return false, errors.New("invalid web check failure")
	}
	status := "unverified"
	switch code {
	case "auth_required", "token_expired", "identity_mismatch":
		status = "auth_required"
	case "environment_challenge":
		status = "challenge"
	case "quota_exhausted":
		status = "quota_exhausted"
	}
	var applied bool
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		creds, err := db.chatGPTWebAccountCredentials(ctx, tx, state.AccountID)
		if err != nil {
			return err
		}
		if expectedAT != "" && credentialStringFromMap(creds, "access_token") != expectedAT {
			return nil
		}
		if expectedBinding != "" {
			requested, err := requestedChatGPTWebWorkspace(creds)
			if err != nil || chatGPTWebHash(requested) != expectedBinding {
				return nil
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE chatgpt_web_account_states SET
			status=CASE WHEN enabled THEN $3 ELSE status END, capabilities='[]', error_code=$4,
			last_check_at=$5, last_check_stage=$6, last_check_error_code=$4, last_check_http_status=$7, updated_at=$8
			WHERE account_id=$1 AND environment_revision=$2 AND (last_check_at IS NULL OR last_check_at <= $5) AND (observed_at IS NULL OR observed_at <= $5)`,
			state.AccountID, state.EnvironmentRevision, status, code, checked.UnixNano(), stage, upstreamStatus, time.Now().UTC().UnixNano())
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		applied = n == 1
		return err
	})
	return applied && err == nil, err
}
