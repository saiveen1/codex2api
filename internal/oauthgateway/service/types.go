package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

// Store must provide isolated snapshots and atomic, durable updates. An error
// must not commit a partial mutation. The host owns fail-closed encryption.
type Store interface {
	View() (State, error)
	Update(func(*State) error) error
	Close() error
}

// Target imports locally without overwriting existing accounts. A confirmed
// failure needs {Total:1, Failed:1} and *upstream.Error{Uncertain:false}; all
// other ambiguous failures require manual review, never an automatic replay.
type Target interface {
	Groups(context.Context) ([]upstream.Group, error)
	Import(context.Context, json.RawMessage, []int64, string) (upstream.ImportResult, error)
}

type Fault struct {
	Status        int
	Code, Message string
}

func (e *Fault) Error() string                     { return e.Message }
func fault(status int, code, message string) error { return &Fault{status, code, message} }

const DefaultOAuthURL = "https://oauth.cala.lol"

type Settings struct {
	OAuthURL   string `json:"oauth_url"`
	OAuthToken string `json:"oauth_token"`
}
type SettingsView struct {
	OAuthURL              string `json:"oauth_url"`
	OAuthTokenSet         bool   `json:"oauth_token_set"`
	Configured            bool   `json:"configured"`
	AutoRecoverySupported bool   `json:"auto_recovery_supported"`
	AutoRecoveryReason    string `json:"auto_recovery_reason"`
}
type InputAccount struct {
	Identifier   string          `json:"identifier"`
	Password     string          `json:"password"`
	TOTPSecret   string          `json:"totp_secret"`
	Proxy        json.RawMessage `json:"proxy,omitempty"`
	RuntimeProxy string          `json:"runtime_proxy,omitempty"`
}
type Input struct {
	RequestID         string           `json:"request_id,omitempty"`
	SourceText        string           `json:"source_text,omitempty"`
	Accounts          []InputAccount   `json:"accounts,omitempty"`
	Options           upstream.Options `json:"options"`
	GroupIDs          []int64          `json:"group_ids"`
	SaveCredentials   bool             `json:"save_credentials"`
	RuntimeProxy      string           `json:"runtime_proxy,omitempty"`
	RecoveryAccountID int64            `json:"recovery_account_id,omitempty"`
}
type PreviewRow struct {
	Line             int    `json:"line"`
	Identifier       string `json:"identifier"`
	IdentifierMasked string `json:"identifier_masked"`
	ProxyMode        string `json:"proxy_mode"`
}
type Preview struct {
	Valid  bool                    `json:"valid"`
	Count  int                     `json:"count"`
	Rows   []PreviewRow            `json:"rows"`
	Errors []upstream.PreviewIssue `json:"errors"`
	Groups int                     `json:"groups"`
}
type Delivery struct {
	ItemID     string          `json:"item_id"`
	Line       int             `json:"line"`
	Account    json.RawMessage `json:"account,omitempty"`
	State      string          `json:"state"`
	Error      *Problem        `json:"error,omitempty"`
	CreatedIDs []int64         `json:"created_ids,omitempty"`
}
type Group struct {
	Request        upstream.SubmitRequest `json:"request"`
	Lines          []int                  `json:"lines"`
	Masks          []string               `json:"masks"`
	RuntimeProxies []string               `json:"runtime_proxies,omitempty"`
	Stage          string                 `json:"stage"`
	Batch          upstream.Batch         `json:"batch"`
	Deliveries     []Delivery             `json:"deliveries"`
	Error          *Problem               `json:"error,omitempty"`
	NextCheck      time.Time              `json:"next_check"`
	CancelSent     bool                   `json:"cancel_sent"`
	ReadOperation  string                 `json:"read_operation,omitempty"`
	ReadFailures   int                    `json:"read_failures,omitempty"`
}
type Job struct {
	ID              string            `json:"id"`
	Fingerprint     string            `json:"fingerprint"`
	Settings        Settings          `json:"settings"`
	Groups          []Group           `json:"groups"`
	GroupIDs        []int64           `json:"group_ids"`
	SaveCredentials bool              `json:"save_credentials"`
	RuntimeProxy    string            `json:"runtime_proxy,omitempty"`
	CancelRequested bool              `json:"cancel_requested"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Recovery        *RecoverySnapshot `json:"recovery,omitempty"`
	RecoveryEpoch   int64             `json:"recovery_epoch,omitempty"`
	AutoRecovery    bool              `json:"auto_recovery,omitempty"`
}
type State struct {
	Version    int           `json:"version"`
	Settings   Settings      `json:"settings"`
	Jobs       []Job         `json:"jobs"`
	Archived   []ArchivedJob `json:"archived,omitempty"`
	Recoveries []Recovery    `json:"recoveries,omitempty"`
}
type ItemView struct {
	Line             int      `json:"line"`
	Identifier       string   `json:"identifier"`
	IdentifierMasked string   `json:"identifier_masked"`
	State            string   `json:"state"`
	ImportState      string   `json:"import_state"`
	Error            *Problem `json:"error,omitempty"`
}
type GroupView struct {
	Index         int        `json:"index"`
	State         string     `json:"state"`
	BatchID       string     `json:"batch_id"`
	RequestID     string     `json:"request_id"`
	Total         int        `json:"total"`
	Imported      int        `json:"imported"`
	Error         *Problem   `json:"error,omitempty"`
	Items         []ItemView `json:"items"`
	ReadOperation string     `json:"read_operation,omitempty"`
	ReadFailures  int        `json:"read_failures,omitempty"`
	NextCheckAt   *time.Time `json:"next_check_at,omitempty"`
}
type JobView struct {
	ID                string      `json:"id"`
	State             string      `json:"state"`
	Total             int         `json:"total"`
	Imported          int         `json:"imported"`
	Failed            int         `json:"failed"`
	Pending           int         `json:"pending"`
	CreatedAt         time.Time   `json:"created_at"`
	UpdatedAt         time.Time   `json:"updated_at"`
	CanRefresh        bool        `json:"can_refresh"`
	CanCancel         bool        `json:"can_cancel"`
	CanRetryImport    bool        `json:"can_retry_import"`
	Groups            []GroupView `json:"groups"`
	CanArchive        bool        `json:"can_archive"`
	RetainedMaterial  bool        `json:"retained_material"`
	Archived          bool        `json:"archived"`
	ArchivedAt        *time.Time  `json:"archived_at,omitempty"`
	RecoveryAccountID int64       `json:"recovery_account_id,omitempty"`
	AutoRecovery      bool        `json:"auto_recovery,omitempty"`
}

// Problem is already sanitized; restoring it must not run a remote decoder.
type Problem upstream.Error

// ArchivedJob keeps idempotency and outcome facts, never account material.
type ArchivedJob struct {
	ID          string    `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	State       string    `json:"state"`
	Total       int       `json:"total"`
	Imported    int       `json:"imported"`
	Failed      int       `json:"failed"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ArchivedAt  time.Time `json:"archived_at"`
}
