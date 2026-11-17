package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func TestChatGPTWebCanCheckOnlyRelaxesTokenExpiry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Store, *Account)
		want   bool
	}{
		{"ready", func(*Store, *Account) {}, true},
		{"expired", func(_ *Store, a *Account) { a.ExpiresAt = time.Now().Add(-time.Hour) }, true},
		{"native_quota", func(_ *Store, a *Account) {
			a.Status, a.CooldownReason = StatusCooldown, "rate_limited_5h"
			a.UsagePercent5h, a.UsagePercent7d = 100, 100
			a.UsagePercent5hValid, a.UsagePercent7dValid = true, true
		}, true},
		{"disabled", func(_ *Store, a *Account) { atomic.StoreInt32(&a.Disabled, 1) }, false},
		{"locked", func(_ *Store, a *Account) { atomic.StoreInt32(&a.Locked, 1) }, false},
		{"dispatch_pause", func(_ *Store, a *Account) { atomic.StoreInt32(&a.DispatchPaused, 1) }, false},
		{"intelligence_pause", func(_ *Store, a *Account) { atomic.StoreInt32(&a.IntelligencePaused, 1) }, false},
		{"banned", func(_ *Store, a *Account) { a.HealthTier = HealthTierBanned }, false},
		{"auth_cooldown", func(_ *Store, a *Account) { a.CooldownReason = "unauthorized" }, false},
		{"error", func(_ *Store, a *Account) { a.Status = StatusError }, false},
		{"missing_token", func(_ *Store, a *Account) { a.AccessToken = "" }, false},
		{"relay", func(_ *Store, a *Account) { a.UpstreamType = UpstreamOpenAIResponses }, false},
		{"wrong_channel", func(s *Store, _ *Account) { s.SetAPIKeyUpstreamChannel(42, database.UpstreamChannelCodex) }, false},
		{"group", func(s *Store, _ *Account) { s.SetAPIKeyAllowedGroups(42, []int64{99}) }, false},
		{"plan", func(s *Store, _ *Account) { s.SetAPIKeyAllowedPlans(42, []string{"plus"}) }, false},
		{"account_key", func(_ *Store, a *Account) { a.SetAllowedAPIKeyIDs([]int64{99}) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, account := chatGPTWebAuthFixture()
			tc.change(s, account)
			if got := s.ChatGPTWebAccountCanCheck(42, account); got != tc.want {
				t.Fatalf("CanCheck=%t want=%t", got, tc.want)
			}
			if got := s.ChatGPTWebAccountCanCheckContext(t.Context(), 42, account); got != tc.want {
				t.Fatalf("CanCheckContext=%t want=%t", got, tc.want)
			}
			if tc.name == "expired" {
				if s.ChatGPTWebAccountVisibleToKey(42, account) || s.TakeChatGPTWebAccount(account.ID(), 42, nil) != nil {
					t.Fatal("check-only expiry exception leaked to visibility/dispatch")
				}
			}
			if account.ActiveRequests.Load() != 0 || account.OccupiedRequests.Load() != 0 {
				t.Fatal("check eligibility acquired generation capacity")
			}
		})
	}
}

func TestChatGPTWebCanCheckKeepsCachedAuthFence(t *testing.T) {
	s, account := chatGPTWebAuthFixture()
	s.tokenCache = cache.NewMemory(1)
	defer s.tokenCache.Close()
	s.cacheAccountCooldownRecord(account.ID(), runtimeCooldownRecord{Reason: "unauthorized", ResetAt: time.Now().Add(time.Minute), UpdatedAt: time.Now()})
	if s.ChatGPTWebAccountCanCheck(42, account) || s.ChatGPTWebAccountCanCheckContext(t.Context(), 42, account) {
		t.Fatal("check bypassed persisted authentication fence")
	}
}

func TestChatGPTWebCanCheckContextCancellation(t *testing.T) {
	s, account := chatGPTWebAuthFixture()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if s.ChatGPTWebAccountCanCheckContext(ctx, 42, account) {
		t.Fatal("canceled eligibility check admitted an account")
	}
}
