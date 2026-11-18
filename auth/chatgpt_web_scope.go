package auth

import (
	"strconv"
	"strings"

	"github.com/codex2api/database"
)

// WebReferenceCountAllowed uses explicit per-account evidence, not a plan guess.
func (a *Account) WebReferenceCountAllowed(count int) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, tag := range a.Tags {
		if value, ok := strings.CutPrefix(tag, "web-max-refs:"); ok {
			if n, err := strconv.Atoi(value); err == nil && n >= 0 && count > n {
				return false
			}
		}
	}
	return true
}

// APIKeyRowAllowsChatGPTWebAccount applies fresh key/account authorization only.
// Callers must validate the account's OAuth shape; readiness and token expiry
// deliberately do not gate administrative enable/disable selection.
func APIKeyRowAllowsChatGPTWebAccount(row *database.APIKeyRow, acc *Account) bool {
	if row == nil || acc == nil || row.ID <= 0 || row.Limits.ResolveUpstreamChannel() != database.UpstreamChannelChatGPTWeb || !acc.AllowsAPIKey(row.ID) {
		return false
	}
	groups := int64Set(normalizeAllowedGroupIDs(row.AllowedGroupIDs))
	noAffinity := int64Set(normalizeAllowedGroupIDs(row.Limits.NoAffinityGroupIDs))
	plans := stringSet(normalizeAllowedPlans(row.Limits.PlanAllow))
	return apiKeyAllowsAccountConstraints(acc, groups, noAffinity, plans, database.UpstreamChannelChatGPTWeb, true)
}
