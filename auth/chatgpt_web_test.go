package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func chatGPTWebAuthFixture() (*Store, *Account) {
	acc := &Account{DBID: 17, AccessToken: "test-AT", ExpiresAt: time.Now().Add(time.Hour), PlanType: "free", Status: StatusReady, HealthTier: HealthTierHealthy, GroupIDs: []int64{3}}
	s := (&Store{accounts: []*Account{acc}, apiKeyUpstreamChannels: map[int64]string{42: database.UpstreamChannelChatGPTWeb}}).withMaxConcurrency(1)
	s.rebuildAccountIndex()
	return s, acc
}

func TestChatGPTWebAdmissionBypassesOnlyNativeQuota(t *testing.T) {
	s, acc := chatGPTWebAuthFixture()
	acc.UsagePercent5h, acc.UsagePercent7d = 100, 100
	acc.UsagePercent5hValid, acc.UsagePercent7dValid = true, true
	acc.Reset5hAt, acc.Reset7dAt = time.Now().Add(time.Hour), time.Now().Add(24*time.Hour)
	acc.Status, acc.CooldownReason, acc.CooldownUtil = StatusCooldown, "rate_limited_5h", time.Now().Add(time.Hour)
	acc.HealthTier, acc.DynamicConcurrencyLimit = HealthTierRisky, 0
	acc.AutoPause5hThreshold, acc.AutoPause7dThreshold = 0.5, 0.5
	acc.effectiveAutoPause5h, acc.effectiveAutoPause7d = 0.5, 0.5
	acc.DispatchCountLimit, acc.dispatchWindowUsed = 1, 1
	acc.dispatchWindowResetAt = time.Now().Add(time.Hour)
	acc.ModelCooldowns = map[string]ModelCooldown{"gpt-image-2": {Model: "gpt-image-2", Reason: "rate_limited", ResetAt: time.Now().Add(time.Hour)}}
	acc.Models = []string{"not-an-image-model"}
	if acc.IsAvailable() {
		t.Fatal("fixture must fail native availability")
	}
	if s.APIKeyAllowsAccount(42, acc) {
		t.Fatal("web key leaked into native selection")
	}
	beforeCooldown, beforeScore, beforeCount, beforeTotal := acc.CooldownUtil, acc.SchedulerScore, acc.dispatchWindowUsed, acc.TotalRequests.Load()
	if got := s.TakeChatGPTWebAccount(acc.DBID, 42, nil); got != acc {
		t.Fatal("web blocked by native quota/cooldown")
	}
	if acc.ActiveRequests.Load() != 1 || acc.OccupiedRequests.Load() != 1 {
		t.Fatal("web did not reserve shared capacity")
	}
	if got := s.TakeChatGPTWebAccount(acc.DBID, 42, nil); got != nil {
		t.Fatal("capacity not enforced")
	}
	// Native requests occupy the exact same counters, not an isolated web pool.
	if reserveOccupiedAccountSlot(acc, 1) {
		t.Fatal("native ignored web occupancy")
	}
	s.Release(acc)
	if acc.ActiveRequests.Load() != 0 || acc.OccupiedRequests.Load() != 0 {
		t.Fatal("release leaked capacity")
	}
	if acc.UsagePercent5h != 100 || acc.UsagePercent7d != 100 || !acc.CooldownUtil.Equal(beforeCooldown) || acc.SchedulerScore != beforeScore || acc.dispatchWindowUsed != beforeCount || acc.TotalRequests.Load() != beforeTotal || acc.PlanType != "free" || acc.Status != StatusCooldown {
		t.Fatal("web admission polluted native observations/counters")
	}
	if acc.IsAvailable() {
		t.Fatal("web cleared native quota")
	}
}

func TestChatGPTWebAdmissionSecurityGuards(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Store, *Account)
	}{
		{"disabled", func(s *Store, a *Account) { atomic.StoreInt32(&a.Disabled, 1) }},
		{"locked", func(s *Store, a *Account) { atomic.StoreInt32(&a.Locked, 1) }},
		{"dispatch_paused", func(s *Store, a *Account) { atomic.StoreInt32(&a.DispatchPaused, 1) }},
		{"intelligence_paused", func(s *Store, a *Account) { atomic.StoreInt32(&a.IntelligencePaused, 1) }},
		{"expired", func(s *Store, a *Account) { a.ExpiresAt = time.Now().Add(-time.Second) }},
		{"missing_token", func(s *Store, a *Account) { a.AccessToken = "" }},
		{"unauthorized", func(s *Store, a *Account) {
			a.Status = StatusCooldown
			a.CooldownReason = "unauthorized"
			a.CooldownUtil = time.Now().Add(-time.Hour)
		}},
		{"error", func(s *Store, a *Account) { a.Status = StatusError }},
		{"banned", func(s *Store, a *Account) { a.HealthTier = HealthTierBanned }},
		{"unknown_cooldown", func(s *Store, a *Account) { a.Status = StatusCooldown; a.CooldownReason = "unknown" }},
		{"relay", func(s *Store, a *Account) { a.UpstreamType = UpstreamOpenAIResponses }},
		{"grok", func(s *Store, a *Account) { a.UpstreamType = UpstreamGrok }},
		{"claude", func(s *Store, a *Account) { a.UpstreamType = UpstreamClaude }},
		{"antigravity", func(s *Store, a *Account) { a.UpstreamType = UpstreamAntigravity }},
		{"agent_identity", func(s *Store, a *Account) { a.CodexAuthMode = CodexAuthModeAgentIdentity }},
		{"api_key", func(s *Store, a *Account) { a.APIKey = "relay-secret" }},
		{"base_url", func(s *Store, a *Account) { a.BaseURL = "https://relay.invalid" }},
		{"account_whitelist", func(s *Store, a *Account) { a.SetAllowedAPIKeyIDs([]int64{99}) }},
		{"key_groups", func(s *Store, a *Account) { s.SetAPIKeyAllowedGroups(42, []int64{4}) }},
		{"key_plans", func(s *Store, a *Account) { s.SetAPIKeyAllowedPlans(42, []string{"plus"}) }},
		{"native_key", func(s *Store, a *Account) { s.SetAPIKeyUpstreamChannel(42, database.UpstreamChannelCodex) }},
		{"auto_key", func(s *Store, a *Account) { s.SetAPIKeyUpstreamChannel(42, "") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, a := chatGPTWebAuthFixture()
			tc.change(s, a)
			if got := s.TakeChatGPTWebAccount(a.DBID, 42, nil); got != nil {
				s.Release(got)
				t.Fatal("security guard bypassed")
			}
			if a.ActiveRequests.Load() != 0 || a.OccupiedRequests.Load() != 0 {
				t.Fatal("rejected admission leaked capacity")
			}
		})
	}
	s, a := chatGPTWebAuthFixture()
	if got := s.TakeChatGPTWebAccount(a.DBID, 42, func(*Account) bool { return false }); got != nil {
		t.Fatal("caller filter bypassed")
	}
	if got := s.TakeChatGPTWebAccount(a.DBID, 0, nil); got != nil {
		t.Fatal("zero key bypassed")
	}
	if got := s.TakeChatGPTWebAccount(999, 42, nil); got != nil {
		t.Fatal("missing account bypassed")
	}
	a.SetAllowedAPIKeyIDs([]int64{42})
	s.SetAPIKeyAllowedGroups(42, []int64{3})
	s.SetAPIKeyAllowedPlans(42, []string{"free"})
	if got := s.TakeChatGPTWebAccount(a.DBID, 42, nil); got != a {
		t.Fatal("matching authorization denied")
	} else {
		s.Release(got)
	}
}

func TestChatGPTWebAdmissionSharedConcurrency(t *testing.T) {
	s, a := chatGPTWebAuthFixture()
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.TakeChatGPTWebAccount(a.DBID, 42, nil) != nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("admission winners=%d", wins.Load())
	}
	s.Release(a)
	if !reserveOccupiedAccountSlot(a, 1) {
		t.Fatal("native could not reserve released slot")
	}
	if s.TakeChatGPTWebAccount(a.DBID, 42, nil) != nil {
		t.Fatal("web ignored native occupancy")
	}
	s.Release(a)
}

func TestChatGPTWebAdmissionCachedAuthFence(t *testing.T) {
	s, a := chatGPTWebAuthFixture()
	s.tokenCache = cache.NewMemory(1)
	defer s.tokenCache.Close()
	s.cacheAccountCooldownRecord(a.DBID, runtimeCooldownRecord{Reason: "unauthorized", ResetAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()})
	if s.TakeChatGPTWebAccount(a.DBID, 42, nil) != nil {
		t.Fatal("cached authentication fence bypassed")
	}
	if a.Status != StatusReady || a.HealthTier != HealthTierHealthy {
		t.Fatal("web mutated native state from cache")
	}
	s.deleteCachedAccountCooldown(a.DBID)
	s.cacheAccountCooldownRecord(a.DBID, runtimeCooldownRecord{Reason: "rate_limited_5h", ResetAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()})
	if got := s.TakeChatGPTWebAccount(a.DBID, 42, nil); got != a {
		t.Fatal("cached native quota blocked web")
	} else {
		s.Release(got)
	}
}
