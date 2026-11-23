package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/internal/openaiidentity"
)

// CodexWorkspaceIdentity keeps login identity separate from workspace routing.
// Headers remain caller-owned; discovery never rewrites a per-route override.
type CodexWorkspaceIdentity struct {
	AccountID, WorkspaceID, UserID, Email string
	WorkspaceName                         string
	Headers                               map[string]string
	TrustedWorkspace                      bool
}

// NormalizeCodexWorkspaceIdentity is deliberately network-free.
func NormalizeCodexWorkspaceIdentity(identity CodexWorkspaceIdentity, idToken, accessToken string) CodexWorkspaceIdentity {
	identity.AccountID = strings.TrimSpace(identity.AccountID)
	identity.WorkspaceID = strings.TrimSpace(identity.WorkspaceID)
	identity.UserID = strings.TrimSpace(identity.UserID)
	identity.Email = strings.TrimSpace(identity.Email)
	for _, value := range []string{identity.AccountID, identity.WorkspaceID} {
		if strings.HasPrefix(strings.ToLower(value), "user-") && identity.UserID == "" {
			identity.UserID = value
		}
	}
	identity.AccountID = openaiidentity.NormalizeWorkspaceID(identity.AccountID)
	if !openaiidentity.ValidWebWorkspaceID(identity.AccountID) {
		identity.AccountID = ""
	}
	identity.WorkspaceID = openaiidentity.NormalizeWorkspaceID(identity.WorkspaceID)
	if !openaiidentity.ValidWebWorkspaceID(identity.WorkspaceID) {
		identity.WorkspaceID = ""
	}
	if email, workspace := openaiidentity.TokenIdentity(idToken, accessToken); openaiidentity.ValidWebWorkspaceID(workspace) && (identity.Email == "" || strings.EqualFold(identity.Email, email)) {
		identity.WorkspaceID = workspace
		identity.TrustedWorkspace = true
		if identity.Email == "" {
			identity.Email = email
		}
	}
	if identity.AccountID == "" && identity.WorkspaceID != "" {
		identity.AccountID = identity.WorkspaceID
	}
	return identity
}

// WorkspaceIdentityError contains safe diagnostics only, never URLs, tokens or bodies.
// Rejected means an authenticated identity/explicit target conflict, not a network failure.
type WorkspaceIdentityError struct {
	Code     string
	Rejected bool
}

func (e *WorkspaceIdentityError) Error() string { return "workspace identity: " + e.Code }

const (
	codexWorkspaceAccountBudget = 2 * time.Second
	codexWorkspaceBatchBudget   = 8 * time.Second
	codexWorkspaceBatchAttempts = 16
)

type codexWorkspaceContextKey struct{}
type codexWorkspaceTransportKey struct{}
type codexWorkspaceBudget struct {
	mu       sync.Mutex
	deadline time.Time
	attempts int
	tripped  bool
}

// WithCodexWorkspaceHydration shares one bounded budget/circuit across an import.
func WithCodexWorkspaceHydration(ctx context.Context) context.Context {
	if _, ok := ctx.Value(codexWorkspaceContextKey{}).(*codexWorkspaceBudget); ok {
		return ctx
	}
	return context.WithValue(ctx, codexWorkspaceContextKey{}, &codexWorkspaceBudget{})
}

// WithCodexWorkspaceTransport injects only the metadata transport for offline tests.
// It does not alter OAuth exchange, proxy policy, endpoints, or other requests.
func WithCodexWorkspaceTransport(ctx context.Context, transport http.RoundTripper) context.Context {
	return context.WithValue(ctx, codexWorkspaceTransportKey{}, transport)
}

func workspaceIdentityFailure(code string, rejected bool) error {
	return &WorkspaceIdentityError{Code: code, Rejected: rejected}
}

// HydrateCodexWorkspaceIdentity authenticates missing identity using the same AT
// and fixed egress for both requests. Complete token identities need no probe.
func HydrateCodexWorkspaceIdentity(ctx context.Context, identity CodexWorkspaceIdentity, accessToken, proxyURL string) (CodexWorkspaceIdentity, error) {
	return hydrateCodexWorkspaceIdentity(ctx, identity, accessToken, proxyURL, false)
}

// LookupCodexWorkspaceName reads only the name for the already-selected workspace.
func LookupCodexWorkspaceName(ctx context.Context, identity CodexWorkspaceIdentity, accessToken, proxyURL string) string {
	if !openaiidentity.ValidWebWorkspaceID(identity.WorkspaceID) {
		return ""
	}
	identity.Headers = nil
	identity, err := hydrateCodexWorkspaceIdentity(ctx, identity, accessToken, proxyURL, true)
	if err != nil {
		return ""
	}
	return identity.WorkspaceName
}

func hydrateCodexWorkspaceIdentity(ctx context.Context, identity CodexWorkspaceIdentity, accessToken, proxyURL string, lookupName bool) (CodexWorkspaceIdentity, error) {
	original := identity
	target, err := openaiidentity.RequestedWebWorkspace(identity.WorkspaceID, identity.Headers)
	if err != nil {
		return original, workspaceIdentityFailure("invalid_target", true)
	}
	if !lookupName && identity.TrustedWorkspace && identity.Email != "" && identity.WorkspaceID != "" && target == identity.WorkspaceID {
		return identity, nil
	}
	info := ParseAccessToken(strings.TrimSpace(accessToken))
	// Opaque fixture tokens and credentials without a real expiry stay pure/pending.
	if info == nil || info.ExpiresAt.IsZero() {
		return identity, nil
	}
	if !time.Now().Before(info.ExpiresAt) {
		return original, workspaceIdentityFailure("expired_access_token", false)
	}
	// No metadata adapter exists for Resin's account-sticky reverse route yet.
	// Stay pending rather than silently switching this credential to direct egress.
	if ResinRequestDecorator != nil && ctx.Value(codexWorkspaceTransportKey{}) == nil {
		return original, workspaceIdentityFailure("metadata_egress_unavailable", false)
	}
	ctx = WithCodexWorkspaceHydration(ctx)
	budget := ctx.Value(codexWorkspaceContextKey{}).(*codexWorkspaceBudget)
	budget.mu.Lock()
	if budget.deadline.IsZero() {
		budget.deadline = time.Now().Add(codexWorkspaceBatchBudget)
	}
	deadline := budget.deadline
	if budget.tripped || budget.attempts >= codexWorkspaceBatchAttempts || !time.Now().Before(deadline) {
		budget.mu.Unlock()
		return original, workspaceIdentityFailure("metadata_budget_exhausted", false)
	}
	budget.attempts++
	budget.mu.Unlock()
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ctx, cancelAccount := context.WithTimeout(ctx, codexWorkspaceAccountBudget)
	defer cancelAccount()
	base, err := BuildHTTPClientChecked(proxyURL)
	if err != nil {
		budget.mu.Lock()
		budget.tripped = true
		budget.mu.Unlock()
		return original, workspaceIdentityFailure("proxy_unavailable", false)
	}
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if transport, ok := ctx.Value(codexWorkspaceTransportKey{}).(http.RoundTripper); ok {
		client.Transport = transport
	}
	request := func(path string, out any) error {
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/"+path, nil)
		if requestErr != nil {
			return workspaceIdentityFailure("invalid_request", false)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
		req.Header.Set("Accept", "application/json")
		// Never forward arbitrary import headers or a stale user-/workspace ID to discovery.
		resp, requestErr := client.Do(req)
		if requestErr != nil {
			budget.mu.Lock()
			budget.tripped = true
			budget.mu.Unlock()
			return workspaceIdentityFailure("metadata_transport_failed", false)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			if resp.StatusCode >= 500 {
				budget.mu.Lock()
				budget.tripped = true
				budget.mu.Unlock()
			}
			return workspaceIdentityFailure(fmt.Sprintf("metadata_http_%d", resp.StatusCode), false)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if readErr != nil || len(data) > 1<<20 || json.Unmarshal(data, out) != nil {
			return workspaceIdentityFailure("invalid_metadata", false)
		}
		return nil
	}
	var me struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err := request("me", &me); err != nil {
		return original, err
	}
	me.ID, me.Email = strings.TrimSpace(me.ID), strings.TrimSpace(me.Email)
	if me.ID == "" || me.Email == "" {
		return original, workspaceIdentityFailure("missing_login_identity", false)
	}
	if identity.Email != "" && !strings.EqualFold(identity.Email, me.Email) || identity.UserID != "" && identity.UserID != me.ID {
		return original, workspaceIdentityFailure("login_identity_mismatch", true)
	}
	var metadata struct {
		Accounts map[string]struct {
			Account struct {
				ID          string          `json:"account_id"`
				Name        json.RawMessage `json:"name"`
				Deactivated bool            `json:"is_deactivated"`
			} `json:"account"`
		} `json:"accounts"`
	}
	if err := request("accounts/check/v4-2023-04-27?timezone_offset_min=-480", &metadata); err != nil {
		return original, err
	}
	workspace := ""
	if target != "" {
		for _, entry := range metadata.Accounts {
			if entry.Account.ID == target && !entry.Account.Deactivated {
				workspace = target
				break
			}
		}
		if workspace == "" {
			return original, workspaceIdentityFailure("target_not_member", true)
		}
	} else if entry, ok := metadata.Accounts["default"]; ok && !entry.Account.Deactivated {
		workspace = strings.TrimSpace(entry.Account.ID)
	}
	if !openaiidentity.ValidWebWorkspaceID(workspace) {
		return original, workspaceIdentityFailure("missing_authenticated_default", false)
	}
	// Refresh publication may share base identity with sibling routes. A verified
	// explicit target must never replace the authenticated default for those routes.
	if identity.WorkspaceID == "" {
		entry, ok := metadata.Accounts["default"]
		if !ok || entry.Account.Deactivated || !openaiidentity.ValidWebWorkspaceID(strings.TrimSpace(entry.Account.ID)) {
			return original, workspaceIdentityFailure("missing_authenticated_default", false)
		}
		identity.WorkspaceID = strings.TrimSpace(entry.Account.ID)
	}
	// Prefer the record keyed by the base workspace ID, not an unrelated default.
	entry, ok := metadata.Accounts[identity.WorkspaceID]
	if !ok {
		entry = metadata.Accounts["default"]
	}
	if !entry.Account.Deactivated && strings.TrimSpace(entry.Account.ID) == identity.WorkspaceID {
		_ = json.Unmarshal(entry.Account.Name, &identity.WorkspaceName)
	}
	identity.Email, identity.UserID = me.Email, me.ID
	identity.TrustedWorkspace = true
	identity.AccountID = identity.WorkspaceID
	return identity, nil
}

func WorkspaceIdentityRejected(err error) bool {
	var identityErr *WorkspaceIdentityError
	return errors.As(err, &identityErr) && identityErr.Rejected
}
