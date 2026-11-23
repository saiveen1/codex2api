package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/oauthgateway/service"
)

// Callbacks must be memory-only; runtime and account locks may be held by callers.
type OAuthRecoveryHooks struct {
	Enabled func(int64) bool
	Signal  func(id, generation int64, refreshFirst bool)
}

func (s *Store) SetOAuthRecoveryHooks(hooks *OAuthRecoveryHooks) { s.oauthRecovery.Store(hooks) }
func (s *Store) OAuthRecoveryProtected(id int64) bool {
	hooks := s.oauthRecovery.Load()
	return hooks != nil && hooks.Enabled != nil && hooks.Enabled(id)
}

func recoveryDenied(body string) bool {
	body = strings.ToLower(body)
	for _, word := range []string{"missing_scope", "deactivated", "suspended", "banned", "account_disabled", "workspace_disabled", "phone", "verification", "verify", "captcha", "challenge", "invalid_client", "unauthorized_client", "access_denied", "proxy_authentication", "gateway_auth"} {
		if strings.Contains(body, word) {
			return true
		}
	}
	return false
}

// A bare HTTP status or arbitrary text containing "401" is not credential proof.
func IsOAuthRecoveryUnauthorized(status int, body []byte) bool {
	if status != http.StatusUnauthorized || len(body) > 1<<20 || recoveryDenied(string(body)) {
		return false
	}
	type codedError struct{ Code string }
	var response struct {
		Code     string
		Error    *codedError
		Detail   *codedError
		Response struct {
			Error         *codedError
			StatusDetails struct{ Error *codedError } `json:"status_details"`
		}
	}
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	code := response.Code
	for _, field := range []*codedError{response.Error, response.Response.Error, response.Response.StatusDetails.Error, response.Detail} {
		if field != nil {
			code = field.Code
			break
		}
	}
	switch code {
	case "token_expired", "token_invalidated", "token_revoked", "invalid_token", "invalid_access_token", "access_token_expired", "session_expired", "session_invalidated":
		return true
	}
	return false
}

func IsOAuthRecoveryRefreshFailure(err error) bool {
	if err == nil || isOAuthRefreshUncertain(err) {
		return false
	}
	var remote *oauthRefreshHTTPError
	if !errors.As(err, &remote) || (remote.status != 400 && remote.status != 401) || recoveryDenied(remote.body) {
		return false
	}
	switch remote.code {
	case "invalid_grant", "refresh_token_reused", "refresh_token_invalidated", "refresh_token_expired", "refresh_token_revoked", "token_invalidated":
		return true
	}
	return false
}

// MarkUpstreamUnauthorized applies the cooldown before the OAuth worker can refresh.
func (s *Store) MarkUpstreamUnauthorized(acc *Account, body []byte, detail string) {
	s.MarkCooldownWithError(acc, 24*time.Hour, "unauthorized", detail)
	if acc != nil && !acc.IsRelayStyle() && !acc.IsCodexAgentIdentity() {
		s.NotifyOAuthRecoveryUnauthorized(acc, http.StatusUnauthorized, body)
	}
}

func (s *Store) NotifyOAuthRecoveryUnauthorized(acc *Account, status int, body []byte) bool {
	if acc == nil || !s.OAuthRecoveryProtected(acc.ID()) {
		return false
	}
	if IsOAuthRecoveryUnauthorized(status, body) {
		if hooks := s.oauthRecovery.Load(); hooks != nil && hooks.Signal != nil {
			hooks.Signal(acc.ID(), acc.GetCredentialGeneration(), true)
		}
	}
	// Opted-in accounts are not deleted by 401 cleanup, even if manual review is
	// required. This does not bypass manual deletion, disabled or CAS checks.
	return true
}

func (s *Store) notifyOAuthRecoveryRefreshFailure(acc *Account, generation int64, err error) {
	if !IsOAuthRecoveryRefreshFailure(err) || !s.OAuthRecoveryProtected(acc.ID()) {
		return
	}
	if hooks := s.oauthRecovery.Load(); hooks != nil && hooks.Signal != nil {
		hooks.Signal(acc.ID(), generation, false)
	}
}

type oauthRecoveryRefreshReceiptKey struct{}
type oauthRecoveryRefreshReceipt struct {
	attemptID                 string
	generation                int64
	accessToken, refreshToken string
}

// RefreshOAuthRecoveryAccount uses the normal refresh path, then only clears
// the 401 that preceded its own committed rotation. A concurrent winner fails closed.
func (s *Store) RefreshOAuthRecoveryAccount(ctx context.Context, original service.RecoverySnapshot) error {
	acc := s.FindByID(original.AccountID)
	if acc == nil || s.db == nil {
		return database.ErrCodexCredentialsChanged
	}
	row, err := s.db.GetAccountByIDIncludingDeleted(ctx, original.AccountID)
	if err != nil {
		return err
	}
	current, err := database.OAuthRecoverySnapshot(row)
	if err != nil {
		return err
	}
	if current != original {
		return database.ErrCodexCredentialsChanged
	}
	acc.mu.RLock()
	until, unauthorizedAt := acc.CooldownUtil, acc.LastUnauthorizedAt
	if acc.CredentialGeneration != original.Generation || acc.CooldownReason != "unauthorized" {
		acc.mu.RUnlock()
		return database.ErrCodexCredentialsChanged
	}
	acc.mu.RUnlock()
	receipt := &oauthRecoveryRefreshReceipt{}
	err = s.RefreshSingle(context.WithValue(ctx, oauthRecoveryRefreshReceiptKey{}, receipt), original.AccountID)
	if err != nil {
		return err
	}
	if receipt.attemptID == "" || receipt.generation != original.Generation {
		return database.ErrCodexCredentialsChanged
	}
	acc.usageCooldownPersistMu.Lock()
	defer acc.usageCooldownPersistMu.Unlock()
	acc.mu.RLock()
	unchanged := acc.CredentialGeneration == original.Generation+1 && acc.AccessToken == receipt.accessToken && acc.RefreshToken == receipt.refreshToken && acc.LastUnauthorizedAt.Equal(unauthorizedAt)
	acc.mu.RUnlock()
	if !unchanged || s.FindByID(original.AccountID) != acc {
		return database.ErrCodexCredentialsChanged
	}
	clear, err := s.db.CompleteOAuthRecoveryRefresh(ctx, original, receipt.attemptID, receipt.accessToken, receipt.refreshToken, until)
	if err != nil {
		return err
	}
	acc.mu.Lock()
	unchanged = acc.CredentialGeneration == original.Generation+1 && acc.AccessToken == receipt.accessToken && acc.RefreshToken == receipt.refreshToken && acc.LastUnauthorizedAt.Equal(unauthorizedAt)
	if !unchanged {
		acc.mu.Unlock()
		return database.ErrCodexCredentialsChanged
	}
	clearedRuntime := false
	if clear && acc.CooldownReason == "unauthorized" && acc.CooldownUtil.Equal(until) {
		acc.CooldownReason = ""
		acc.CooldownUtil = time.Time{}
		acc.Status = StatusReady
		acc.ErrorMsg = ""
		acc.usageCooldownPersistPending = false
		acc.usageCooldownClearErrorPending = false
		clearedRuntime = true
		if acc.HealthTier == HealthTierBanned {
			acc.HealthTier = HealthTierWarm
		}
	}
	atomic.StoreInt32(&acc.Disabled, 0)
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	if clearedRuntime {
		s.deleteCachedAccountCooldown(acc.DBID)
	}
	return nil
}

// Reload in place so statistics and scheduling metadata are not reconstructed.
func (s *Store) ReloadOAuthRecoveredAccount(ctx context.Context, id int64) error {
	row, err := s.db.GetAccountByIDIncludingDeleted(ctx, id)
	if err != nil {
		return err
	}
	if !row.Enabled || row.Status == "deleted" || row.Status == "disabled" {
		return database.ErrCodexCredentialsChanged
	}
	acc := s.FindByID(id)
	if acc == nil {
		s.clearRecoveredOAuthCooldownCache(row, time.Time{})
		return s.LoadAccountByID(ctx, id)
	}
	acc.usageCooldownPersistMu.Lock()
	if !applyCodexCredentialValues(acc, row.Credentials, row.CredentialGeneration) {
		acc.usageCooldownPersistMu.Unlock()
		return nil
	}
	var clearedUntil time.Time
	acc.mu.Lock()
	if acc.CredentialGeneration == row.CredentialGeneration && row.CooldownReason != "unauthorized" {
		if acc.CooldownReason == "unauthorized" {
			clearedUntil = acc.CooldownUtil
			acc.CooldownReason = ""
			acc.CooldownUtil = time.Time{}
			acc.Status = StatusReady
			if row.CooldownUntil.Valid && row.CooldownUntil.Time.After(time.Now()) {
				acc.setCooldownUntilLocked(row.CooldownUntil.Time, row.CooldownReason)
			}
			acc.usageCooldownPersistPending = false
			acc.usageCooldownClearErrorPending = false
		}
		if acc.Status == StatusError {
			acc.Status = StatusReady
		}
		acc.ErrorMsg = ""
		if acc.HealthTier == HealthTierBanned {
			acc.HealthTier = HealthTierWarm
		}
		atomic.StoreInt32(&acc.Disabled, 0)
	}
	acc.mu.Unlock()
	s.clearRecoveredOAuthCooldownCache(row, clearedUntil)
	acc.usageCooldownPersistMu.Unlock()
	if s.tokenCache != nil {
		if err := s.tokenCache.DeleteAccessToken(ctx, id); err != nil {
			return err
		}
	}
	s.finishCodexRefresh(ctx, acc, "")
	s.invalidateRoutingSchedulers()
	return nil
}

func (s *Store) clearRecoveredOAuthCooldownCache(row *database.AccountRow, clearedUntil time.Time) {
	if row.CooldownReason == "unauthorized" {
		return
	}
	record, ok := s.getCachedAccountCooldown(row.ID)
	if ok && record.Reason == "unauthorized" && (record.ResetAt.Equal(clearedUntil) || !record.UpdatedAt.After(row.UpdatedAt)) {
		s.deleteCachedAccountCooldown(row.ID)
	}
}
