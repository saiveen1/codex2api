package auth

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/openaiidentity"
)

// ChatGPTWebOAuthAccountEligible checks account shape, not token/quota readiness.
func ChatGPTWebOAuthAccountEligible(row *database.AccountRow) bool {
	if row == nil || row.ID <= 0 || row.DeletedAt.Valid || row.Status == "deleted" || row.ErrorMessage == "deleted" ||
		(row.Platform != "" && row.Platform != "openai") || row.Type != "oauth" {
		return false
	}
	upstream := strings.ToLower(strings.TrimSpace(row.GetCredential("upstream_type")))
	mode := strings.ToLower(strings.TrimSpace(row.GetCredential("auth_mode")))
	return (upstream == "" || upstream == "codex") && (mode == "" || mode == "oauth") &&
		row.GetCredential("api_key") == "" && row.GetCredential("base_url") == ""
}

// ChatGPTWebAutoAccountEligible checks persisted account fences for automatic use.
// It does not replace the Web state, pause, authorization, cache or lease checks.
func ChatGPTWebAutoAccountEligible(row *database.AccountRow, now time.Time) bool {
	if !ChatGPTWebOAuthAccountEligible(row) || !row.Enabled || row.Locked ||
		(row.Status != "active" && row.Status != "cooldown") ||
		strings.TrimSpace(row.GetCredential("access_token")) == "" {
		return false
	}
	if row.CooldownReason != "" && !chatGPTWebNativeCooldown(row.CooldownReason) {
		return false
	}
	if row.Status == "cooldown" && !chatGPTWebNativeCooldown(row.CooldownReason) {
		return false
	}
	if raw := strings.TrimSpace(row.GetCredential("expires_at")); raw != "" {
		expires, err := time.Parse(time.RFC3339, raw)
		if err != nil || (!expires.After(now) && strings.TrimSpace(row.GetCredential("refresh_token")) == "" && strings.TrimSpace(row.GetCredential("session_token")) == "") {
			return false
		}
	}
	_, err := openaiidentity.RequestedWebWorkspace(row.GetCredential("account_id"), row.GetCredentialStringMap("custom_headers"))
	return err == nil
}

// ListChatGPTWebAccountStates initializes unconfigured members of explicitly
// bound Web-key groups on use. Existing rows (including manual disables) always
// win. This neither checks upstream nor grants readiness or dispatch permission.
func ListChatGPTWebAccountStates(ctx context.Context, db *database.DB) ([]database.ChatGPTWebAccountState, error) {
	states, err := db.ListChatGPTWebAccountStates(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := db.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	var eligibleKeys []*database.APIKeyRow
	var groupIDs []int64
	now := time.Now()
	for _, key := range keys {
		if !key.Enabled || (key.ExpiresAt.Valid && !key.ExpiresAt.Time.After(now)) ||
			key.Limits.ResolveUpstreamChannel() != database.UpstreamChannelChatGPTWeb ||
			key.Limits.ResolveImageGenerationPolicy() != database.ImageGenerationPolicyAllow {
			continue
		}
		// An unrestricted key or the ungrouped sentinel is not whole-pool consent.
		bound := false
		for _, id := range key.AllowedGroupIDs {
			if id > 0 {
				groupIDs = append(groupIDs, id)
				bound = true
			}
		}
		if bound {
			eligibleKeys = append(eligibleKeys, key)
		}
	}
	if len(eligibleKeys) == 0 {
		return states, nil
	}
	slices.Sort(groupIDs)
	ids, err := db.ListAccountIDsInGroups(ctx, slices.Compact(groupIDs))
	if err != nil {
		return nil, err
	}
	configured := make(map[int64]bool, len(states))
	for _, state := range states {
		configured[state.AccountID] = true
	}
	for _, id := range ids {
		if configured[id] {
			continue
		}
		row, err := db.GetAccountByID(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !ChatGPTWebAutoAccountEligible(row, now) {
			continue
		}
		paused, err := db.IsIntelligenceCheckPaused(ctx, id)
		if err != nil {
			return nil, err
		}
		if paused {
			continue
		}
		groups, err := db.GetAccountGroupIDs(ctx, id)
		if err != nil {
			return nil, err
		}
		account := &Account{DBID: id, PlanType: row.GetCredential("plan_type"), GroupIDs: groups}
		account.SetAllowedAPIKeyIDs(row.GetCredentialInt64Slice("allowed_api_key_ids"))
		for _, key := range eligibleKeys {
			bound := slices.ContainsFunc(key.AllowedGroupIDs, func(group int64) bool { return group > 0 && slices.Contains(groups, group) })
			if !bound || !APIKeyRowAllowsChatGPTWebAccount(key, account) {
				continue
			}
			state, err := db.InitChatGPTWebAccountEnabled(ctx, id)
			if errors.Is(err, sql.ErrNoRows) {
				break // Account deleted after selection; never revive it.
			}
			if err != nil {
				return nil, err
			}
			states = append(states, *state)
			break
		}
	}
	slices.SortFunc(states, func(a, b database.ChatGPTWebAccountState) int {
		if a.AccountID < b.AccountID {
			return -1
		}
		if a.AccountID > b.AccountID {
			return 1
		}
		return 0
	})
	return states, nil
}
