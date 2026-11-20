package auth

import (
	"maps"
	"sort"
	"strings"

	"github.com/codex2api/database"
)

type accountConcurrencyKey struct{ channel, plan string }

// NormalizeAccountConcurrencyPlan returns the stable rule key for an account plan.
func NormalizeAccountConcurrencyPlan(channel, plan string) string {
	plan = strings.ToLower(strings.TrimSpace(plan))
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case database.UpstreamChannelCodex:
		switch plan {
		case "pro_lite", "pro-lite":
			return "prolite"
		case "pro_max", "pro-max":
			return "promax"
		case "self_serve_business_prolite":
			return "team5x"
		case "self_serve_business_usage_based":
			return "business"
		case "ent26", "enterprise_cbp_usage_based":
			return "enterprise"
		}
	case database.UpstreamChannelClaude:
		switch plan {
		case "claude-free", "claude-pro", "claude-max", "claude-max-5x", "claude-max-20x", "claude-team", "claude-enterprise", "claude-business":
			return strings.TrimPrefix(plan, "claude-")
		}
	case database.UpstreamChannelGrok:
		return CanonicalGrokLivePlanFilter(plan)
	case database.UpstreamChannelAntigravity:
		return CanonicalAntigravityPlan(plan)
	case database.UpstreamChannelKiro:
		if key := KiroPlanKey(plan); key != "" {
			return key
		}
	}
	return plan
}

func accountConcurrencyChannel(upstreamType string) string {
	// Match the account-list channels, including legacy and Responses accounts in Codex.
	return database.NormalizeAccountGroupChannel(upstreamType)
}

func (a *Account) accountConcurrencyKeyLocked() accountConcurrencyKey {
	channel := accountConcurrencyChannel(a.UpstreamType)
	plan := a.PlanType
	if strings.TrimSpace(plan) == "" {
		plan = a.storedConcurrencyPlan
	}
	switch {
	case channel == database.UpstreamChannelAntigravity && a.AntigravityPlanKey != "":
		plan = a.AntigravityPlanKey
	case channel == database.UpstreamChannelGrok && strings.TrimSpace(a.APIKey) != "":
		plan = "api"
	case strings.EqualFold(strings.TrimSpace(a.UpstreamType), UpstreamOpenAIResponses) && strings.TrimSpace(plan) == "":
		plan = "api"
	}
	return accountConcurrencyKey{channel, NormalizeAccountConcurrencyPlan(channel, plan)}
}

func (a *Account) accountConcurrencyDefaultLocked() int64 {
	if a.concurrencyStore != nil {
		if defaults := a.concurrencyStore.accountConcurrencyDefaults.Load(); defaults != nil {
			return (*defaults)[a.accountConcurrencyKeyLocked()]
		}
	}
	return 0
}

// AccountRowBaseConcurrency resolves the fallback shown for rows not loaded in
// the runtime pool. Group membership is only available from the runtime entry.
func (s *Store) AccountRowBaseConcurrency(row *database.AccountRow) int64 {
	if row.BaseConcurrencyOverride.Valid && row.BaseConcurrencyOverride.Int64 > 0 {
		return row.BaseConcurrencyOverride.Int64
	}
	upstream := row.GetCredential("upstream_type")
	plan := row.GetCredential("plan_type")
	apiKey := row.GetCredential("api_key")
	account := Account{
		concurrencyStore:    s,
		UpstreamType:        upstream,
		PlanType:            plan,
		APIKey:              apiKey,
		claudeSessionWindow: claudeSessionWindowForRow(upstream, s.ClaudeSessionWindowLimit()),
	}
	switch accountConcurrencyChannel(upstream) {
	case database.UpstreamChannelAntigravity:
		account.AntigravityPlanKey = antigravityPlanKeyFromRow(row)
	case database.UpstreamChannelGrok:
		if strings.TrimSpace(apiKey) == "" {
			if tokenPlan := GrokPlanTypeFromAccessToken(row.GetCredential("access_token")); tokenPlan != "" {
				account.PlanType = tokenPlan
			}
		}
	}
	return account.effectiveBaseConcurrencyLocked(s.maxConcurrency.Load())
}

// SetAccountConcurrencyDefaults replaces the channel/plan defaults and refreshes
// existing accounts and routing schedulers. An empty list clears all rules.
func (s *Store) SetAccountConcurrencyDefaults(rules []database.AccountConcurrencyRule) {
	if s == nil {
		return
	}
	defaults := make(map[accountConcurrencyKey]int64, len(rules))
	for _, rule := range rules {
		channel := strings.ToLower(strings.TrimSpace(rule.Channel))
		switch channel {
		case database.UpstreamChannelCodex, database.UpstreamChannelClaude, database.UpstreamChannelAntigravity, database.UpstreamChannelGrok, database.UpstreamChannelKiro:
		default:
			continue
		}
		if plan := NormalizeAccountConcurrencyPlan(channel, rule.Plan); plan != "" && rule.Concurrency > 0 {
			defaults[accountConcurrencyKey{channel, plan}] = int64(rule.Concurrency)
		}
	}

	s.accountMutationMu.Lock()
	defer s.accountMutationMu.Unlock()
	old := s.accountConcurrencyDefaults.Load()
	if (old == nil && len(defaults) == 0) || (old != nil && maps.Equal(*old, defaults)) {
		return
	}
	if len(defaults) == 0 {
		s.accountConcurrencyDefaults.Store(nil)
	} else {
		s.accountConcurrencyDefaults.Store(&defaults)
	}
	accounts := s.accountSnapshotAccounts()
	for _, account := range accounts {
		if account != nil {
			account.mu.Lock()
			account.recomputeSchedulerLocked(s.maxConcurrency.Load())
			account.mu.Unlock()
		}
	}
	if scheduler := s.getFastScheduler(); scheduler != nil {
		scheduler.Rebuild(accounts)
	}
	s.invalidateRoutingSchedulers()
	s.notifySchedulerAvailability()
}

// AccountConcurrencyPlanChoices lists normalized plans present in the runtime pool.
func (s *Store) AccountConcurrencyPlanChoices() map[string][]string {
	choices := make(map[string][]string)
	if s == nil {
		return choices
	}
	seen := make(map[accountConcurrencyKey]struct{})
	for _, account := range s.accountSnapshotAccounts() {
		if account == nil {
			continue
		}
		account.mu.RLock()
		key := account.accountConcurrencyKeyLocked()
		account.mu.RUnlock()
		if _, exists := seen[key]; exists || key.plan == "" {
			continue
		}
		seen[key] = struct{}{}
		choices[key.channel] = append(choices[key.channel], key.plan)
	}
	for _, plans := range choices {
		sort.Strings(plans)
	}
	return choices
}
