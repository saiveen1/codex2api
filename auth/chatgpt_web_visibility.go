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

// ChatGPTWebCredentials snapshots the token and effective workspace together.
func (a *Account) ChatGPTWebCredentials() (string, string) {
	if a == nil {
		return "", ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	workspace := strings.TrimSpace(a.AccountID)
	if override := openaiidentity.WorkspaceOverrideFromHeaders(a.CustomHeaders); override != "" {
		workspace = override
	}
	return strings.TrimSpace(a.AccessToken), workspace
}
