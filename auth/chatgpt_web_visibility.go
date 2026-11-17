package auth

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/openaiidentity"
)

// ChatGPTWebAccountVisibleToKey checks the same authorization boundary as web
// dispatch without acquiring capacity or applying Codex usage/model gates.
func (s *Store) ChatGPTWebAccountVisibleToKey(apiKeyID int64, account *Account) bool {
	return s.chatGPTWebAccountVisibleToKey(apiKeyID, account, false, nil)
}

// ChatGPTWebAccountCanCheck preserves Web dispatch's safety/authorization fences
// but permits the existing credential lifecycle to recover an expired token.
// It does not acquire generation capacity or bypass the durable Web lease.
func (s *Store) ChatGPTWebAccountCanCheck(apiKeyID int64, account *Account) bool {
	return s.chatGPTWebAccountCanCheck(nil, apiKeyID, account)
}

// ChatGPTWebAccountCanCheckContext also bounds shared auth-cache I/O by the
// caller's automatic admission deadline; no detached cache timeout may extend it.
func (s *Store) ChatGPTWebAccountCanCheckContext(ctx context.Context, apiKeyID int64, account *Account) bool {
	return s.chatGPTWebAccountCanCheck(ctx, apiKeyID, account)
}

func (s *Store) chatGPTWebAccountCanCheck(ctx context.Context, apiKeyID int64, account *Account) bool {
	if !s.chatGPTWebAccountVisibleToKey(apiKeyID, account, true, ctx) {
		return false
	}
	_, usable := s.ResolveChatGPTWebProxy(account)
	return usable
}

func (s *Store) chatGPTWebAccountVisibleToKey(apiKeyID int64, account *Account, allowExpired bool, ctx context.Context) bool {
	if s == nil || account == nil || apiKeyID <= 0 || s.APIKeyUpstreamChannel(apiKeyID) != database.UpstreamChannelChatGPTWeb {
		return false
	}
	if !account.AllowsAPIKey(apiKeyID) || !s.apiKeyAllowsAccount(apiKeyID, account, true) {
		return false
	}
	if ctx != nil {
		if ctx.Err() != nil {
			return false
		}
		if s.tokenCache != nil {
			cacheCtx, cancel := context.WithTimeout(ctx, runtimeCooldownCacheTimeout)
			defer cancel()
			payload, ok, err := s.tokenCache.GetRuntime(cacheCtx, accountCooldownCacheNamespace, accountCooldownRuntimeKey(account.DBID))
			if err != nil || cacheCtx.Err() != nil {
				return false
			}
			if ok && len(payload) > 0 {
				var record runtimeCooldownRecord
				if json.Unmarshal(payload, &record) != nil || (record.ResetAt.After(time.Now()) && !chatGPTWebNativeCooldown(normalizeCooldownReason(record.Reason))) {
					return false
				}
			}
		}
	} else if record, ok := s.getCachedAccountCooldown(account.DBID); ok && !chatGPTWebNativeCooldown(record.Reason) {
		return false
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	return account.chatGPTWebEligibleLocked(time.Now(), allowExpired)
}

// ChatGPTWebCredentials returns the requested route; an empty workspace means
// authenticated default discovery, never an invented workspace from a user ID.
func (a *Account) ChatGPTWebCredentials() (string, string) {
	token, workspace, _, _, err := a.ChatGPTWebIdentity()
	if err != nil {
		return "", ""
	}
	return token, workspace
}

func (a *Account) ChatGPTWebIdentity() (token, workspace, email string, expired bool, err error) {
	if a == nil {
		return
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	workspace, err = openaiidentity.RequestedWebWorkspace(a.AccountID, a.CustomHeaders)
	return strings.TrimSpace(a.AccessToken), workspace, strings.TrimSpace(a.Email), !a.ExpiresAt.IsZero() && !a.ExpiresAt.After(time.Now()), err
}
