package auth

import "github.com/codex2api/database"

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
