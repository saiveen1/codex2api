package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestKiroRefreshPreservesExclusiveProxyPolicy(t *testing.T) {
	const proxyURL = "http://exclusive.example:8080"
	for _, tc := range []struct {
		name      string
		accountID int64
		enabled   bool
		allowed   bool
	}{
		{name: "allowed explicit binding", accountID: 1, enabled: true, allowed: true},
		{name: "unauthorized binding", accountID: 2, enabled: true},
		{name: "disabled managed proxy", accountID: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, ProxyPoolEnabled: true})
			t.Cleanup(store.Stop)
			rows := []*database.ProxyRow{{URL: proxyURL, Exclusive: true, AllowedAccountIDs: []int64{1}}}
			store.proxyInventoryLoader = func(context.Context) ([]*database.ProxyRow, error) { return rows, nil }
			store.proxyPoolLoader = func(context.Context) ([]*database.ProxyRow, error) {
				if tc.enabled {
					return rows, nil
				}
				return nil, nil
			}
			if err := store.ReloadProxyPool(); err != nil {
				t.Fatal(err)
			}
			account := &Account{DBID: tc.accountID, UpstreamType: UpstreamKiro, AccessToken: "old-at", RefreshToken: "rt",
				ProxyURL: proxyURL, Status: StatusReady, ExpiresAt: time.Now().Add(time.Hour),
				Kiro: KiroCredential{AuthKind: KiroAuthKindSocial}}
			store.AddAccount(account)

			previous := kiroTokenRefreshFunc
			t.Cleanup(func() { kiroTokenRefreshFunc = previous })
			calls := 0
			kiroTokenRefreshFunc = func(_ context.Context, _ KiroCredential, rt, proxy string) (*KiroTokenData, error) {
				calls++
				if !tc.allowed || rt != "rt" || proxy != proxyURL {
					t.Fatalf("unexpected refresh: allowed=%t token=%q proxy=%q", tc.allowed, rt, proxy)
				}
				return &KiroTokenData{AccessToken: "new-at", ExpiresAt: time.Now().Add(time.Hour)}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := store.RefreshKiroAccount(ctx, account)
			if tc.allowed {
				if err != nil || calls != 1 || account.AccessToken != "new-at" {
					t.Fatalf("allowed refresh failed: err=%v calls=%d", err, calls)
				}
			} else if err == nil || !strings.Contains(err.Error(), "proxy access denied or unavailable") || calls != 0 || account.AccessToken != "old-at" {
				t.Fatalf("restricted refresh did not fail closed: err=%v calls=%d", err, calls)
			}
		})
	}
}
