package auth

import (
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/openaiidentity"
)

// ChatGPTWebAccountVisibleToKey checks the same authorization boundary as web
// dispatch without acquiring capacity or applying Codex usage/model gates.
func (s *Store) ChatGPTWebAccountVisibleToKey(apiKeyID int64, account *Account) bool {
	if s == nil || account == nil || apiKeyID <= 0 || s.APIKeyUpstreamChannel(apiKeyID) != database.UpstreamChannelChatGPTWeb {
		return false
	}
	if !account.AllowsAPIKey(apiKeyID) || !s.apiKeyAllowsAccount(apiKeyID, account, true) {
		return false
	}
	if record, ok := s.getCachedAccountCooldown(account.DBID); ok && !chatGPTWebNativeCooldown(record.Reason) {
		return false
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	return account.chatGPTWebDispatchEligibleLocked(time.Now())
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
