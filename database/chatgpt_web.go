package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/codex2api/internal/openaiidentity"
	"github.com/codex2api/internal/webasset"
	"github.com/google/uuid"
)

var (
	ErrChatGPTWebExecutionConflict = errors.New("chatgpt web execution binding conflict")
	ErrChatGPTWebTransition        = errors.New("invalid chatgpt web execution transition")
	ErrChatGPTWebLease             = errors.New("chatgpt web account lease is not owned")
)

// ChatGPTWebAccountState is opt-in, secret-free web state, never a projection of
// Codex usage. An absent row means disabled, not an implicitly eligible account.
// Ready observations must also match the current AT fingerprint and egress at
// dispatch time; a stored observation is not an authorization grant.
type ChatGPTWebAccountState struct {
	AccountID                   int64      `json:"account_id"`
	Enabled                     bool       `json:"enabled"`
	EnvironmentRevision         int64      `json:"environment_revision"`
	DeviceID                    string     `json:"device_id"`
	SessionID                   string     `json:"session_id"`
	Status                      string     `json:"status"`
	Capabilities                []string   `json:"capabilities"`
	ImageQuotaRemaining         *int       `json:"image_quota_remaining"`
	QuotaResetAt                *time.Time `json:"quota_reset_at"`
	ObservedAt                  *time.Time `json:"observed_at"`
	CredentialRevision          string     `json:"credential_revision"`
	WorkspaceFingerprint        string     `json:"workspace_fingerprint"`
	ProxyFingerprint            string     `json:"proxy_fingerprint"`
	ErrorCode                   string     `json:"error_code"`
	UpdatedAt                   time.Time  `json:"updated_at"`
	WorkspaceID                 string     `json:"workspace_id"`
	WorkspaceBindingFingerprint string     `json:"workspace_binding_fingerprint"`
	LastCheckAt                 *time.Time `json:"last_check_at"`
	LastCheckStage              string     `json:"last_check_stage"`
	LastCheckErrorCode          string     `json:"last_check_error_code"`
	LastCheckHTTPStatus         int        `json:"last_check_http_status"`
}

type ChatGPTWebExecution struct {
	ID                   string    `json:"id"`
	APIKeyID             int64     `json:"api_key_id"`
	AccountID            int64     `json:"account_id"`
	RequestHash          string    `json:"request_hash"`
	CredentialRevision   string    `json:"credential_revision"`
	EnvironmentRevision  int64     `json:"environment_revision"`
	WorkspaceFingerprint string    `json:"workspace_fingerprint"`
	ProxyFingerprint     string    `json:"proxy_fingerprint"`
	Operation            string    `json:"operation"`
	Stage                string    `json:"stage"`
	ConversationID       string    `json:"conversation_id"`
	TaskID               string    `json:"task_id"`
	AssetIDs             []string  `json:"asset_ids"`
	ErrorCode            string    `json:"error_code"`
	ResultUnknown        bool      `json:"result_unknown"`
	LeaseOwner           string    `json:"lease_owner"`
	LeaseUntil           time.Time `json:"lease_until"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// Both bootstrap paths use this additive, idempotent schema. Epoch nanoseconds
// avoid driver-dependent TIMESTAMP comparisons/scanning and preserve lease CAS
// precision on SQLite and PostgreSQL. There is deliberately no account backfill.
func (db *DB) ensureChatGPTWebSchema(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chatgpt_web_account_states (
			account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			enabled BOOLEAN NOT NULL DEFAULT FALSE,
			environment_revision BIGINT NOT NULL DEFAULT 1,
			device_id TEXT NOT NULL, session_id TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'disabled', capabilities TEXT NOT NULL DEFAULT '[]',
			image_quota_remaining INTEGER NULL, quota_reset_at BIGINT NULL, observed_at BIGINT NULL,
			credential_revision TEXT NOT NULL DEFAULT '', proxy_fingerprint TEXT NOT NULL DEFAULT '', workspace_fingerprint TEXT NOT NULL DEFAULT '',
			error_code TEXT NOT NULL DEFAULT '', updated_at BIGINT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS chatgpt_web_account_leases (
			account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			owner TEXT NOT NULL, expires_at BIGINT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS chatgpt_web_executions (
			id TEXT PRIMARY KEY, api_key_id BIGINT NOT NULL, account_id BIGINT NOT NULL,
			request_hash TEXT NOT NULL, credential_revision TEXT NOT NULL,
			environment_revision BIGINT NOT NULL, proxy_fingerprint TEXT NOT NULL, workspace_fingerprint TEXT NOT NULL DEFAULT '', operation TEXT NOT NULL,
			stage TEXT NOT NULL, conversation_id TEXT NOT NULL DEFAULT '', task_id TEXT NOT NULL DEFAULT '',
			asset_ids TEXT NOT NULL DEFAULT '[]', error_code TEXT NOT NULL DEFAULT '',
			result_unknown BOOLEAN NOT NULL DEFAULT FALSE, lease_owner TEXT NOT NULL, lease_until BIGINT NOT NULL,
			created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_chatgpt_web_executions_account ON chatgpt_web_executions(account_id, created_at, id);
	`)
	if err != nil {
		return err
	}
	// Upgrade previously created Web tables without inventing a workspace.
	if db.isSQLite() {
		for _, table := range []string{"chatgpt_web_account_states", "chatgpt_web_executions"} {
			if err := db.ensureSQLiteColumn(ctx, table, "workspace_fingerprint", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
	} else {
		_, err = db.conn.ExecContext(ctx, `ALTER TABLE chatgpt_web_account_states ADD COLUMN IF NOT EXISTS workspace_fingerprint TEXT NOT NULL DEFAULT '';
			ALTER TABLE chatgpt_web_executions ADD COLUMN IF NOT EXISTS workspace_fingerprint TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return err
		}
	}
	if err := db.ensureChatGPTWebCheckSchema(ctx); err != nil {
		return err
	}
	// Legacy readiness has no authenticated resolved workspace and must be rechecked.
	_, err = db.conn.ExecContext(ctx, `UPDATE chatgpt_web_account_states SET status='unverified' WHERE status='ready' AND (workspace_fingerprint='' OR workspace_id='' OR workspace_binding_fingerprint='')`)
	return err
}

const chatGPTWebStateColumns = `account_id, enabled, environment_revision, device_id, session_id, status, capabilities, image_quota_remaining, quota_reset_at, observed_at, credential_revision, proxy_fingerprint, workspace_fingerprint, error_code, updated_at, workspace_id, workspace_binding_fingerprint, last_check_at, last_check_stage, last_check_error_code, last_check_http_status`
const chatGPTWebExecutionColumns = `id, api_key_id, account_id, request_hash, credential_revision, environment_revision, proxy_fingerprint, workspace_fingerprint, operation, stage, conversation_id, task_id, asset_ids, error_code, result_unknown, lease_owner, lease_until, created_at, updated_at`

type chatGPTWebScanner interface{ Scan(...any) error }

func scanChatGPTWebState(row chatGPTWebScanner) (*ChatGPTWebAccountState, error) {
	var s ChatGPTWebAccountState
	var caps string
	var quota, reset, observed, checked sql.NullInt64
	var updated int64
	if err := row.Scan(&s.AccountID, &s.Enabled, &s.EnvironmentRevision, &s.DeviceID, &s.SessionID, &s.Status, &caps, &quota, &reset, &observed, &s.CredentialRevision, &s.ProxyFingerprint, &s.WorkspaceFingerprint, &s.ErrorCode, &updated, &s.WorkspaceID, &s.WorkspaceBindingFingerprint, &checked, &s.LastCheckStage, &s.LastCheckErrorCode, &s.LastCheckHTTPStatus); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(caps), &s.Capabilities); err != nil {
		return nil, err
	}
	if s.Capabilities == nil {
		s.Capabilities = []string{}
	}
	if quota.Valid {
		n := int(quota.Int64)
		s.ImageQuotaRemaining = &n
	}
	s.QuotaResetAt, s.ObservedAt = chatGPTWebTimePtr(reset), chatGPTWebTimePtr(observed)
	s.LastCheckAt = chatGPTWebTimePtr(checked)
	s.UpdatedAt = time.Unix(0, updated).UTC()
	return &s, nil
}

func chatGPTWebTimePtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(0, n.Int64).UTC()
	return &t
}
func chatGPTWebTimeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

func (db *DB) GetChatGPTWebAccountState(ctx context.Context, id int64) (*ChatGPTWebAccountState, error) {
	return scanChatGPTWebState(db.conn.QueryRowContext(ctx, `SELECT `+chatGPTWebStateColumns+` FROM chatgpt_web_account_states WHERE account_id=$1`, id))
}

func (db *DB) ListChatGPTWebAccountStates(ctx context.Context) ([]ChatGPTWebAccountState, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT `+chatGPTWebStateColumns+` FROM chatgpt_web_account_states ORDER BY account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatGPTWebAccountState{}
	for rows.Next() {
		s, err := scanChatGPTWebState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// Lock the credential owner before checking its AT: an administrative token
// replacement/refresh must win over an observation made using stale credentials.
func (db *DB) chatGPTWebAccountCredentials(ctx context.Context, tx *sql.Tx, id int64) (map[string]any, error) {
	query := `SELECT credentials, COALESCE(platform,''), COALESCE(type,'') FROM accounts WHERE id=$1 AND status <> 'deleted' AND deleted_at IS NULL`
	if !db.isSQLite() {
		query += ` FOR UPDATE`
	}
	var raw any
	var platform, kind string
	if err := tx.QueryRowContext(ctx, query, id).Scan(&raw, &platform, &kind); err != nil {
		return nil, err
	}
	creds := decodeCredentials(raw)
	upstream := strings.ToLower(strings.TrimSpace(credentialStringFromMap(creds, "upstream_type")))
	authMode := strings.ToLower(strings.TrimSpace(credentialStringFromMap(creds, "auth_mode")))
	if (platform != "" && platform != "openai") || kind != "oauth" || (upstream != "" && upstream != "codex") ||
		(authMode != "" && authMode != "oauth") || credentialStringFromMap(creds, "api_key") != "" || credentialStringFromMap(creds, "base_url") != "" {
		return nil, errors.New("chatgpt web requires an OpenAI OAuth account")
	}
	return creds, nil
}

func (db *DB) SetChatGPTWebAccountEnabled(ctx context.Context, id int64, enabled bool) (*ChatGPTWebAccountState, error) {
	var out *ChatGPTWebAccountState
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := db.chatGPTWebAccountCredentials(ctx, tx, id); err != nil {
			return err
		}
		status := "disabled"
		if enabled {
			status = "unverified"
		}
		now := time.Now().UTC().UnixNano()
		_, err := tx.ExecContext(ctx, `INSERT INTO chatgpt_web_account_states(account_id, enabled, device_id, session_id, status, updated_at)
			VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(account_id) DO NOTHING`, id, enabled, uuid.NewString(), uuid.NewString(), status, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE chatgpt_web_account_states SET enabled=$2, environment_revision=environment_revision+1,
			status=$3, capabilities='[]', image_quota_remaining=NULL, quota_reset_at=NULL, observed_at=NULL,
			credential_revision='', proxy_fingerprint='', workspace_fingerprint='', workspace_id='', workspace_binding_fingerprint='', last_check_at=NULL, last_check_stage='', last_check_error_code='', last_check_http_status=0, error_code='', updated_at=$4 WHERE account_id=$1 AND enabled <> $2`, id, enabled, status, now)
		if err != nil {
			return err
		}
		out, err = scanChatGPTWebState(tx.QueryRowContext(ctx, `SELECT `+chatGPTWebStateColumns+` FROM chatgpt_web_account_states WHERE account_id=$1`, id))
		return err
	})
	return out, err
}

func chatGPTWebHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func chatGPTWebIsHash(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func chatGPTWebSafeID(s string, allowEmpty bool) bool {
	if len(s) == 0 {
		return allowEmpty
	}
	if len(s) > 256 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}
func chatGPTWebSafeError(code string) bool {
	switch code {
	case "workspace_unavailable", "identity_mismatch", "quota_unknown", "token_expired", "account_disabled", "account_busy", "account_ineligible", "egress_unavailable", "observation_stale":
		return true
	case "", "invalid_request", "auth_required", "environment_challenge", "quota_exhausted", "unsupported_model", "unsupported_parameter", "submission_unknown", "image_stream_interrupted", "image_poll_timeout", "image_download_failed", "content_policy_violation", "upstream_text_reply", "upstream_error", "execution_conflict", "worker_unavailable", "worker_timeout", "worker_protocol_error", "cancelled", "internal_error":
		return true
	}
	return false
}

// SaveChatGPTWebAccountObservation is only for trusted check results, not admin
// PUT payloads. Enablement, environment identity/revision are never writable here.
// A false return is a stale AT/environment/observation, never a partial update.
func (db *DB) SaveChatGPTWebAccountObservation(ctx context.Context, state *ChatGPTWebAccountState, expectedAT string) (bool, error) {
	if state == nil || expectedAT == "" {
		return false, errors.New("missing chatgpt web observation binding")
	}
	s := *state
	if s.EnvironmentRevision <= 0 || !chatGPTWebIsHash(s.ProxyFingerprint) || !openaiidentity.ValidWebWorkspaceID(s.WorkspaceID) || s.WorkspaceFingerprint != chatGPTWebHash(s.WorkspaceID) || !chatGPTWebIsHash(s.WorkspaceBindingFingerprint) || !chatGPTWebSafeError(s.ErrorCode) {
		return false, errors.New("invalid chatgpt web observation")
	}
	revision := chatGPTWebHash(expectedAT)
	if s.CredentialRevision != "" && s.CredentialRevision != revision {
		return false, errors.New("invalid chatgpt web credential revision")
	}
	caps := slices.Clone(s.Capabilities)
	slices.Sort(caps)
	caps = slices.Compact(caps)
	for _, c := range caps {
		if c != "generate" && c != "edit" {
			return false, errors.New("invalid chatgpt web capability")
		}
	}
	if s.ImageQuotaRemaining != nil && *s.ImageQuotaRemaining < 0 {
		return false, errors.New("invalid chatgpt web quota")
	}
	switch s.Status {
	case "ready":
		if len(caps) == 0 || s.ImageQuotaRemaining == nil || *s.ImageQuotaRemaining <= 0 || s.ErrorCode != "" {
			return false, errors.New("ready requires positive web capability and quota evidence")
		}
	case "unverified", "auth_required", "challenge", "quota_exhausted":
	default:
		return false, errors.New("invalid chatgpt web observation status")
	}
	now := time.Now().UTC()
	observed := now
	if s.ObservedAt != nil {
		observed = s.ObservedAt.UTC()
	}
	if observed.IsZero() || observed.After(now.Add(time.Minute)) {
		return false, errors.New("invalid chatgpt web observation time")
	}
	checked := observed
	if s.LastCheckAt != nil {
		checked = s.LastCheckAt.UTC()
	}
	if checked.IsZero() || checked.After(now.Add(time.Minute)) || !chatGPTWebCheckStage(s.LastCheckStage) || !chatGPTWebSafeError(s.LastCheckErrorCode) || s.LastCheckHTTPStatus < 0 || s.LastCheckHTTPStatus > 599 {
		return false, errors.New("invalid web check diagnostic")
	}
	if caps == nil {
		caps = []string{}
	}
	encoded, _ := json.Marshal(caps)
	var applied bool
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		creds, err := db.chatGPTWebAccountCredentials(ctx, tx, s.AccountID)
		if err != nil {
			return err
		}
		if credentialStringFromMap(creds, "access_token") != expectedAT {
			return nil
		}
		requested, err := requestedChatGPTWebWorkspace(creds)
		if err != nil || chatGPTWebHash(requested) != s.WorkspaceBindingFingerprint || (requested != "" && requested != s.WorkspaceID) {
			return nil
		}
		res, err := tx.ExecContext(ctx, `UPDATE chatgpt_web_account_states SET status=$3, capabilities=$4,
			image_quota_remaining=$5, quota_reset_at=$6, observed_at=$7, credential_revision=$8, proxy_fingerprint=$9, workspace_fingerprint=$10, error_code=$11, updated_at=$12,
			workspace_id=$13, workspace_binding_fingerprint=$14, last_check_at=$15, last_check_stage=$16, last_check_error_code=$17, last_check_http_status=$18
			WHERE account_id=$1 AND environment_revision=$2 AND enabled=TRUE AND (observed_at IS NULL OR observed_at < $7) AND (last_check_at IS NULL OR last_check_at <= $15)`,
			s.AccountID, s.EnvironmentRevision, s.Status, string(encoded), s.ImageQuotaRemaining, chatGPTWebTimeArg(s.QuotaResetAt), observed.UnixNano(), revision, s.ProxyFingerprint, s.WorkspaceFingerprint, s.ErrorCode, now.UnixNano(), s.WorkspaceID, s.WorkspaceBindingFingerprint, checked.UnixNano(), s.LastCheckStage, s.LastCheckErrorCode, s.LastCheckHTTPStatus)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		applied = n == 1
		return err
	})
	return applied && err == nil, err
}

// A lease expiry grants ownership only, never permission to resubmit a fenced
// execution. Owners must be fresh opaque per-run IDs, not reusable host names.
func (db *DB) AcquireChatGPTWebAccountLease(ctx context.Context, accountID int64, owner string, expiresAt time.Time) (bool, error) {
	if accountID <= 0 || !chatGPTWebSafeID(owner, false) || !expiresAt.After(time.Now()) {
		return false, ErrChatGPTWebLease
	}
	var acquired bool
	err := db.withSQLiteWriteLock(ctx, func() error {
		now := time.Now().UTC().UnixNano()
		res, err := db.conn.ExecContext(ctx, `INSERT INTO chatgpt_web_account_leases(account_id,owner,expires_at) VALUES($1,$2,$3)
			ON CONFLICT(account_id) DO UPDATE SET owner=excluded.owner, expires_at=excluded.expires_at
			WHERE chatgpt_web_account_leases.expires_at <= $4 OR chatgpt_web_account_leases.owner=excluded.owner`, accountID, owner, expiresAt.UTC().UnixNano(), now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		acquired = n == 1
		return err
	})
	return acquired, err
}
func (db *DB) ReleaseChatGPTWebAccountLease(ctx context.Context, accountID int64, owner string) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `DELETE FROM chatgpt_web_account_leases WHERE account_id=$1 AND owner=$2`, accountID, owner)
		return err
	})
}

func scanChatGPTWebExecution(row chatGPTWebScanner) (*ChatGPTWebExecution, error) {
	var e ChatGPTWebExecution
	var assets string
	var until, created, updated int64
	if err := row.Scan(&e.ID, &e.APIKeyID, &e.AccountID, &e.RequestHash, &e.CredentialRevision, &e.EnvironmentRevision, &e.ProxyFingerprint, &e.WorkspaceFingerprint, &e.Operation, &e.Stage, &e.ConversationID, &e.TaskID, &assets, &e.ErrorCode, &e.ResultUnknown, &e.LeaseOwner, &until, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(assets), &e.AssetIDs); err != nil {
		return nil, err
	}
	if e.AssetIDs == nil {
		e.AssetIDs = []string{}
	}
	e.LeaseUntil, e.CreatedAt, e.UpdatedAt = time.Unix(0, until).UTC(), time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	return &e, nil
}
func chatGPTWebSameBinding(a, b *ChatGPTWebExecution) bool {
	return a.ID == b.ID && a.APIKeyID == b.APIKeyID && a.AccountID == b.AccountID && a.RequestHash == b.RequestHash && a.CredentialRevision == b.CredentialRevision && a.EnvironmentRevision == b.EnvironmentRevision && a.ProxyFingerprint == b.ProxyFingerprint && a.WorkspaceFingerprint == b.WorkspaceFingerprint && a.Operation == b.Operation
}
func validateChatGPTWebExecution(e *ChatGPTWebExecution) error {
	if e == nil || !chatGPTWebSafeID(e.ID, false) || e.APIKeyID <= 0 || e.AccountID <= 0 || e.EnvironmentRevision <= 0 ||
		!chatGPTWebIsHash(e.RequestHash) || !chatGPTWebIsHash(e.CredentialRevision) || !chatGPTWebIsHash(e.ProxyFingerprint) || !chatGPTWebIsHash(e.WorkspaceFingerprint) ||
		(e.Operation != "generate" && e.Operation != "edit") || !chatGPTWebSafeID(e.LeaseOwner, false) ||
		!chatGPTWebSafeID(e.ConversationID, true) || !chatGPTWebSafeID(e.TaskID, true) || !chatGPTWebSafeError(e.ErrorCode) || len(e.AssetIDs) > 64 {
		return errors.New("invalid chatgpt web execution metadata")
	}
	for _, id := range e.AssetIDs {
		if !webasset.Valid(id) {
			return errors.New("invalid chatgpt web asset ID")
		}
	}
	return nil
}
func (db *DB) chatGPTWebOwnedLease(ctx context.Context, tx *sql.Tx, e *ChatGPTWebExecution, now time.Time) error {
	query := `SELECT owner, expires_at FROM chatgpt_web_account_leases WHERE account_id=$1`
	if !db.isSQLite() {
		query += ` FOR UPDATE`
	}
	var owner string
	var until int64
	err := tx.QueryRowContext(ctx, query, e.AccountID).Scan(&owner, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrChatGPTWebLease
	}
	if err != nil {
		return err
	}
	if owner != e.LeaseOwner || until <= now.UnixNano() || !e.LeaseUntil.After(now) || e.LeaseUntil.UnixNano() > until {
		return ErrChatGPTWebLease
	}
	return nil
}

// Create accepts prepared only. Reusing an ID is an immutable-binding check,
// never a lease acquisition or retry instruction, including after restart.
func (db *DB) CreateChatGPTWebExecution(ctx context.Context, execution *ChatGPTWebExecution) (bool, error) {
	if err := validateChatGPTWebExecution(execution); err != nil {
		return false, err
	}
	e := *execution
	if e.Stage == "" {
		e.Stage = "prepared"
	}
	var created bool
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		old, err := scanChatGPTWebExecution(tx.QueryRowContext(ctx, `SELECT `+chatGPTWebExecutionColumns+` FROM chatgpt_web_executions WHERE id=$1`, e.ID))
		if err == nil {
			if !chatGPTWebSameBinding(old, &e) {
				return ErrChatGPTWebExecutionConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if e.Stage != "prepared" || e.ResultUnknown || e.ConversationID != "" || e.TaskID != "" || len(e.AssetIDs) != 0 {
			return ErrChatGPTWebTransition
		}
		now := time.Now().UTC()
		if err := db.chatGPTWebOwnedLease(ctx, tx, &e, now); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO chatgpt_web_executions(id,api_key_id,account_id,request_hash,credential_revision,environment_revision,proxy_fingerprint,workspace_fingerprint,operation,stage,lease_owner,lease_until,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'prepared',$10,$11,$12,$12) ON CONFLICT(id) DO NOTHING`, e.ID, e.APIKeyID, e.AccountID, e.RequestHash, e.CredentialRevision, e.EnvironmentRevision, e.ProxyFingerprint, e.WorkspaceFingerprint, e.Operation, e.LeaseOwner, e.LeaseUntil.UnixNano(), now.UnixNano())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		created = n == 1
		if !created {
			old, err = scanChatGPTWebExecution(tx.QueryRowContext(ctx, `SELECT `+chatGPTWebExecutionColumns+` FROM chatgpt_web_executions WHERE id=$1`, e.ID))
			if err != nil {
				return err
			}
			if !chatGPTWebSameBinding(old, &e) {
				return ErrChatGPTWebExecutionConflict
			}
		}
		return nil
	})
	return created && err == nil, err
}
func (db *DB) GetChatGPTWebExecution(ctx context.Context, id string, apiKeyID int64) (*ChatGPTWebExecution, error) {
	if apiKeyID <= 0 {
		return nil, sql.ErrNoRows
	}
	return scanChatGPTWebExecution(db.conn.QueryRowContext(ctx, `SELECT `+chatGPTWebExecutionColumns+` FROM chatgpt_web_executions WHERE id=$1 AND api_key_id=$2`, id, apiKeyID))
}
func (db *DB) ListChatGPTWebExecutions(ctx context.Context, accountID int64, limit int) ([]ChatGPTWebExecution, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT `+chatGPTWebExecutionColumns+` FROM chatgpt_web_executions WHERE (account_id=$1 OR $1=0) ORDER BY created_at DESC,id DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatGPTWebExecution{}
	for rows.Next() {
		e, err := scanChatGPTWebExecution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// Transitions are adjacent and monotonic; same-stage saves add IDs/renew a
// lease, never clear or change known IDs. failed/unknown are submission fences.
// They may resume only READ/POLL/DOWNLOAD using IDs already in the ledger.
// An expired submitting execution without known IDs can only become unknown,
// even for its old owner: a process restart is not evidence of non-submission.
func chatGPTWebTransition(old, next *ChatGPTWebExecution, now time.Time) bool {
	stages := map[string]string{"prepared": "submitting", "submitting": "submitted", "submitted": "generated", "generated": "downloading", "downloading": "completed"}
	if old.Stage == "completed" {
		return false
	}
	if old.Stage == "submitting" && (!old.LeaseUntil.After(now) || old.LeaseOwner != next.LeaseOwner) && old.ConversationID == "" && old.TaskID == "" {
		return next.Stage == "unknown"
	}
	if old.Stage == "failed" || old.Stage == "unknown" {
		if next.Stage == old.Stage {
			return true
		}
		if next.Stage == "submitted" {
			return old.ConversationID != "" || old.TaskID != ""
		}
		if next.Stage == "generated" || next.Stage == "downloading" {
			return len(old.AssetIDs) > 0
		}
		return false
	}
	if _, ok := stages[old.Stage]; !ok {
		return false
	}
	return next.Stage == old.Stage || next.Stage == stages[old.Stage] || next.Stage == "failed" || next.Stage == "unknown"
}

// Save uses expected stage + owner + immutable bindings under a write lock.
// A different owner may resume only after the execution lease expired AND after
// acquiring the exclusive account lease. No API ever resets a submission fence.
func (db *DB) SaveChatGPTWebExecution(ctx context.Context, execution *ChatGPTWebExecution, expectedStage string) (bool, error) {
	if err := validateChatGPTWebExecution(execution); err != nil {
		return false, err
	}
	e := *execution
	var saved bool
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT ` + chatGPTWebExecutionColumns + ` FROM chatgpt_web_executions WHERE id=$1`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		old, err := scanChatGPTWebExecution(tx.QueryRowContext(ctx, query, e.ID))
		if err != nil {
			return err
		}
		if !chatGPTWebSameBinding(old, &e) {
			return ErrChatGPTWebExecutionConflict
		}
		if old.Stage != expectedStage {
			return nil
		}
		now := time.Now().UTC()
		if old.LeaseOwner != e.LeaseOwner && old.LeaseUntil.After(now) {
			return nil
		}
		if err := db.chatGPTWebOwnedLease(ctx, tx, &e, now); err != nil {
			return err
		}
		if !chatGPTWebTransition(old, &e, now) {
			return ErrChatGPTWebTransition
		}
		if old.ConversationID != "" && old.ConversationID != e.ConversationID || old.TaskID != "" && old.TaskID != e.TaskID {
			return ErrChatGPTWebExecutionConflict
		}
		for _, id := range old.AssetIDs {
			if !slices.Contains(e.AssetIDs, id) {
				return ErrChatGPTWebExecutionConflict
			}
		}
		if (e.Stage == "submitted" && e.ConversationID == "" && e.TaskID == "") || ((e.Stage == "generated" || e.Stage == "downloading" || e.Stage == "completed") && len(e.AssetIDs) == 0) {
			return fmt.Errorf("%w: stage requires known upstream IDs", ErrChatGPTWebTransition)
		}
		if e.Stage == "prepared" && (e.ConversationID != "" || e.TaskID != "" || len(e.AssetIDs) != 0) {
			return ErrChatGPTWebTransition
		}
		if e.Stage == "unknown" {
			e.ResultUnknown = true
		} else if e.Stage != "failed" {
			e.ResultUnknown = false
		}
		assets := e.AssetIDs
		if assets == nil {
			assets = []string{}
		}
		encoded, _ := json.Marshal(assets)
		res, err := tx.ExecContext(ctx, `UPDATE chatgpt_web_executions SET stage=$2,conversation_id=$3,task_id=$4,asset_ids=$5,error_code=$6,result_unknown=$7,lease_owner=$8,lease_until=$9,updated_at=$10
			WHERE id=$1 AND stage=$11 AND lease_owner=$12`, e.ID, e.Stage, e.ConversationID, e.TaskID, string(encoded), e.ErrorCode, e.ResultUnknown, e.LeaseOwner, e.LeaseUntil.UnixNano(), now.UnixNano(), expectedStage, old.LeaseOwner)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		saved = n == 1
		return err
	})
	return saved && err == nil, err
}
