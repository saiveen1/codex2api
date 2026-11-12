package auth

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
)

// TakeChatGPTWebAccount acquires shared in-flight/occupied capacity, not Codex
// dispatch/usage quota. The caller MUST first validate the opt-in DB web state,
// positive capability/quota evidence, current credential/environment/egress and
// acquire its durable exclusive account lease. Release with Store.Release.
//
// Deliberately do not use IsAvailable, model manifests/cooldowns, lazy probing,
// health scores, smart pacing or reserveDispatchCount: those are native Codex
// observations and a web request must neither consume nor rewrite them. Likewise
// do not call native success/failure/usage reporting for a web outcome. Generic
// occupied/active counters still enforce capacity across BOTH channels.
func (s *Store) TakeChatGPTWebAccount(accountID, apiKeyID int64, filter AccountFilter) *Account {
	if s == nil || accountID <= 0 || apiKeyID <= 0 || s.APIKeyUpstreamChannel(apiKeyID) != database.UpstreamChannelChatGPTWeb {
		return nil
	}
	s.mu.RLock()
	acc := s.lookupByIDLocked(accountID)
	s.mu.RUnlock()
	if acc == nil || accountDispatchBlocked(acc) || atomic.LoadInt32(&acc.Locked) != 0 {
		return nil
	}
	if !acc.AllowsAPIKey(apiKeyID) || !s.apiKeyAllowsAccount(apiKeyID, acc, true) {
		return nil
	}
	if _, usable := s.ResolveChatGPTWebProxy(acc); !usable {
		return nil
	}
	if filter != nil && !filter(acc) {
		return nil
	}
	// Preserve cross-process authentication fences without applying cached Codex
	// quota/cooldown observations to account memory as native selection does.
	if record, ok := s.getCachedAccountCooldown(acc.DBID); ok && !chatGPTWebNativeCooldown(record.Reason) {
		return nil
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if !acc.chatGPTWebDispatchEligibleLocked(time.Now()) {
		return nil
	}
	limit := acc.effectiveBaseConcurrencyLocked(s.maxConcurrency.Load())
	if !reserveOccupiedAccountSlot(acc, limit) {
		return nil
	}
	if atomic.LoadInt32(&acc.Locked) != 0 || !acc.chatGPTWebDispatchEligibleLocked(time.Now()) {
		releaseOccupiedAccountSlot(acc)
		return nil
	}
	return acc
}

// Only known native-only throttles are bypassed. Unknown/authentication reasons
// fail closed, including an unauthorized fence whose recovery time has passed.
func chatGPTWebNativeCooldown(reason string) bool {
	switch reason {
	case "rate_limited", "rate_limited_5h", "rate_limited_7d", "responses_rate_limited", "usage_limited", "usage_limit", "dispatch_count_limit":
		return true
	}
	return false
}

func (a *Account) chatGPTWebDispatchEligibleLocked(now time.Time) bool {
	upstream := strings.ToLower(strings.TrimSpace(a.UpstreamType))
	authMode := strings.ToLower(strings.TrimSpace(a.CodexAuthMode))
	if (upstream != "" && upstream != "codex") || (authMode != "" && authMode != "oauth") || a.APIKey != "" || a.BaseURL != "" ||
		strings.TrimSpace(a.AccessToken) == "" || (!a.ExpiresAt.IsZero() && !a.ExpiresAt.After(now)) ||
		accountDispatchBlocked(a) || atomic.LoadInt32(&a.Locked) != 0 || a.Status == StatusError || a.healthTierLocked() == HealthTierBanned {
		return false
	}
	if a.Status != StatusReady && a.Status != StatusCooldown {
		return false
	}
	if a.CooldownReason != "" && !chatGPTWebNativeCooldown(a.CooldownReason) {
		return false
	}
	if a.Status == StatusCooldown && !chatGPTWebNativeCooldown(a.CooldownReason) {
		return false
	}
	return true
}
