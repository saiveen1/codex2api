package auth

import (
	"testing"

	"github.com/codex2api/database"
)

func TestAPIKeyRowAllowsAccountChannelIsolation(t *testing.T) {
	codex := &Account{DBID: 17, GroupIDs: []int64{3}, PlanType: "free"}
	kiro := &Account{DBID: 18, UpstreamType: UpstreamKiro, AccessToken: "synthetic-kiro-token", GroupIDs: []int64{3}, PlanType: "free"}
	for _, tt := range []struct {
		name    string
		channel string
		account *Account
		want    bool
	}{
		{"codex accepts codex", database.UpstreamChannelCodex, codex, true},
		{"codex rejects kiro", database.UpstreamChannelCodex, kiro, false},
		{"kiro accepts kiro", database.UpstreamChannelKiro, kiro, true},
		{"kiro rejects codex", database.UpstreamChannelKiro, codex, false},
		{"web rejects native codex", database.UpstreamChannelChatGPTWeb, codex, false},
		{"web rejects native kiro", database.UpstreamChannelChatGPTWeb, kiro, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			row := &database.APIKeyRow{ID: 42, AllowedGroupIDs: []int64{3}, Limits: database.APIKeyLimits{UpstreamChannel: tt.channel, PlanAllow: []string{"free"}}}
			if got := APIKeyRowAllowsAccount(row, tt.account); got != tt.want {
				t.Fatalf("APIKeyRowAllowsAccount = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChatGPTWebSharedConstraintsPreserveAdmissionBoundary(t *testing.T) {
	s, acc := chatGPTWebAuthFixture()
	if s.APIKeyAllowsAccount(42, acc) || !s.apiKeyAllowsAccount(42, acc, true) {
		t.Fatal("web admission must remain explicit after shared permission extraction")
	}
	s.apiKeyAllowedGroupSets = map[int64]map[int64]struct{}{42: {99: {}}}
	if s.apiKeyAllowsAccount(42, acc, true) {
		t.Fatal("web admission bypassed group restrictions")
	}
	s.apiKeyAllowedGroupSets[42] = map[int64]struct{}{3: {}}
	s.apiKeyAllowedPlanSets = map[int64]map[string]struct{}{42: {"pro": {}}}
	if s.apiKeyAllowsAccount(42, acc, true) {
		t.Fatal("web admission bypassed plan restrictions")
	}
	s.apiKeyAllowedPlanSets[42] = map[string]struct{}{"free": {}}
	if !s.apiKeyAllowsAccount(42, acc, true) || s.APIKeyAllowsAccount(42, acc) {
		t.Fatal("authorized web account must remain exclusive to explicit web admission")
	}
}

func TestSetAPIKeyUpstreamChannelPreservesKiroAndWeb(t *testing.T) {
	s, codex := chatGPTWebAuthFixture()
	kiro := &Account{DBID: 18, UpstreamType: UpstreamKiro, AccessToken: "synthetic-kiro-token"}
	s.SetAPIKeyUpstreamChannel(42, " Kiro ")
	if s.APIKeyUpstreamChannel(42) != database.UpstreamChannelKiro || !s.APIKeyAllowsAccount(42, kiro) || s.APIKeyAllowsAccount(42, codex) {
		t.Fatal("runtime key update lost the Kiro channel boundary")
	}
	s.SetAPIKeyUpstreamChannel(42, " ChatGPT_Web ")
	if s.APIKeyUpstreamChannel(42) != database.UpstreamChannelChatGPTWeb || s.APIKeyAllowsAccount(42, codex) || !s.apiKeyAllowsAccount(42, codex, true) {
		t.Fatal("runtime key update lost the explicit web admission boundary")
	}
}
