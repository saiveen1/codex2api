package auth

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestChatGPTWebScopeRowAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name       string
		groups     []int64
		noAffinity []int64
		plans      []string
		accGroups  []int64
		plan       string
		allowed    []int64
		want       bool
	}{
		{name: "unrestricted", want: true},
		{name: "group exact", groups: []int64{4}, accGroups: []int64{4}, want: true},
		{name: "group mismatch", groups: []int64{4}, accGroups: []int64{40}},
		{name: "no affinity membership", groups: []int64{4}, noAffinity: []int64{8}, accGroups: []int64{8}, want: true},
		{name: "no affinity retains unrestricted semantics", noAffinity: []int64{8}, want: true},
		{name: "plan normalized", plans: []string{" FREE "}, plan: " Free ", want: true},
		{name: "plan mismatch", plans: []string{"free"}, plan: "plus"},
		{name: "plan unknown fails closed", plans: []string{"free"}},
		{name: "groups and plans AND", groups: []int64{4}, accGroups: []int64{4}, plans: []string{"free"}, plan: "plus"},
		{name: "allowed key exact", allowed: []int64{17}, want: true},
		{name: "allowed key mismatch", allowed: []int64{170}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := &database.APIKeyRow{ID: 17, AllowedGroupIDs: tc.groups, Limits: database.APIKeyLimits{
				UpstreamChannel: database.UpstreamChannelChatGPTWeb, NoAffinityGroupIDs: tc.noAffinity, PlanAllow: tc.plans,
			}}
			acc := &Account{GroupIDs: tc.accGroups, PlanType: tc.plan}
			acc.SetAllowedAPIKeyIDs(tc.allowed)
			if got := APIKeyRowAllowsChatGPTWebAccount(row, acc); got != tc.want {
				t.Fatalf("allowed=%v want=%v", got, tc.want)
			}
			if APIKeyRowAllowsAccount(row, acc) {
				t.Fatal("native helper admitted a Web key")
			}
		})
	}
}

func TestChatGPTWebScopeFreshRowsWithoutReadiness(t *testing.T) {
	row := &database.APIKeyRow{ID: 17, AllowedGroupIDs: []int64{4}, Limits: database.APIKeyLimits{
		UpstreamChannel: database.UpstreamChannelChatGPTWeb, PlanAllow: []string{"free"},
	}}
	// No AT, native error, expired tokens/key and disabled key are not membership gates.
	row.ExpiresAt.Valid, row.ExpiresAt.Time = true, time.Now().Add(-time.Hour)
	acc := &Account{GroupIDs: []int64{4}, PlanType: "free", Status: StatusError, ExpiresAt: time.Now().Add(-time.Hour)}
	acc.SetAllowedAPIKeyIDs([]int64{17})
	if !APIKeyRowAllowsChatGPTWebAccount(row, acc) {
		t.Fatal("runtime gates leaked into administrative selection")
	}
	row.AllowedGroupIDs = []int64{5}
	if APIKeyRowAllowsChatGPTWebAccount(row, acc) {
		t.Fatal("ignored fresh group restriction")
	}
	acc.GroupIDs = []int64{5}
	row.Limits.PlanAllow = []string{"plus"}
	if APIKeyRowAllowsChatGPTWebAccount(row, acc) {
		t.Fatal("ignored fresh plan restriction")
	}
	acc.PlanType = "plus"
	acc.SetAllowedAPIKeyIDs([]int64{18})
	if APIKeyRowAllowsChatGPTWebAccount(row, acc) {
		t.Fatal("ignored fresh account key restriction")
	}
	acc.SetAllowedAPIKeyIDs(nil)
	if !APIKeyRowAllowsChatGPTWebAccount(row, acc) {
		t.Fatal("did not use current row configuration")
	}
	for _, channel := range []string{"", database.UpstreamChannelCodex, database.UpstreamChannelGrok} {
		row.Limits.UpstreamChannel = channel
		if APIKeyRowAllowsChatGPTWebAccount(row, acc) {
			t.Fatalf("accepted non-Web channel %q", channel)
		}
	}
	if APIKeyRowAllowsChatGPTWebAccount(nil, acc) || APIKeyRowAllowsChatGPTWebAccount(row, nil) {
		t.Fatal("nil authorization accepted")
	}
	row.ID = 0
	row.Limits.UpstreamChannel = database.UpstreamChannelChatGPTWeb
	if APIKeyRowAllowsChatGPTWebAccount(row, acc) {
		t.Fatal("invalid key ID accepted")
	}
}
