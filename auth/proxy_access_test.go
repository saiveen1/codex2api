package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func TestProxyAccessExplicitPinOnlyAcrossRoutesAndSticky(t *testing.T) {
	const private = "http://private.example:8080"
	const shared = "http://shared.example:8080"
	a := &Account{DBID: 1, ProxyURL: private, GroupIDs: []int64{5}}
	b := &Account{DBID: 2, ProxyURL: private, GroupIDs: []int64{5}}
	rows := []*database.ProxyRow{{URL: private, Exclusive: true, AllowedAccountIDs: []int64{1}}, {URL: shared}}
	s := &Store{
		accounts: []*Account{a, b}, accountsByID: map[int64]*Account{1: a, 2: b},
		proxyPoolEnabled: true, globalProxy: private,
		proxyPoolLoader: func(context.Context) ([]*database.ProxyRow, error) { return rows, nil },
		sessionBindings: map[string]sessionAffinity{"b": {accountID: 2, proxyURL: private, lastUsedAt: time.Now()}},
	}
	s.SetGroupProxyURLs(5, []string{private})
	if err := s.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	if proxy, usable := s.ResolveUsableProxyForAccount(a); proxy != private || !usable {
		t.Fatalf("allowed pin: %q %v", proxy, usable)
	}
	if proxy, usable := s.ResolveUsableProxyForAccount(b); proxy != "" || usable {
		t.Fatalf("denied pin must not become direct: %q %v", proxy, usable)
	}
	if proxy, usable := s.ResolveChatGPTWebProxy(b); proxy != "" || usable {
		t.Fatalf("web denied pin: %q %v", proxy, usable)
	}
	if s.affinityProxyStillValid(2, private) || !s.affinityProxyStillValid(1, private) {
		t.Fatal("sticky pin access mismatch")
	}
	if _, exists := s.sessionBindings["b"]; exists {
		t.Fatal("incompatible sticky binding survived reload")
	}
	for _, account := range []*Account{a, b} {
		s.ApplyAccountProxyURL(account.DBID, "")
		if proxy, usable := s.ResolveUsableProxyForAccount(account); proxy != shared || !usable {
			t.Fatalf("implicit route: %q %v", proxy, usable)
		}
		if s.resolveGroupProxyForAccount(account) != "" || s.resolveFallbackProxyForAccount(account.DBID) != shared || s.affinityProxyStillValid(account.DBID, private) {
			t.Fatal("group/fallback/sticky bypassed exclusive policy")
		}
	}
	for i := 0; i < 5; i++ {
		if got := s.NextProxy(); got != shared {
			t.Fatalf("NextProxy selected %q", got)
		}
	}
	s.SetProxyPoolEnabled(false)
	if s.NextProxy() != "" || s.resolveFallbackProxyForAccount(1) != "" {
		t.Fatal("global fallback leaked exclusive node")
	}
	if proxy, usable := s.ResolveUsableProxyForAccount(a); proxy != "" || usable {
		t.Fatalf("exclusive global became direct: %q %v", proxy, usable)
	}
	s.ApplyAccountProxyURL(2, private)
	if proxy, usable := s.ResolveUsableProxyForAccount(b); proxy != "" || usable {
		t.Fatalf("pool-off pin bypass: %q %v", proxy, usable)
	}
}

func TestProxyAccessReloadToggleAndHealth(t *testing.T) {
	const url = "http://toggle.example:8080"
	a := &Account{DBID: 1}
	row := &database.ProxyRow{URL: url}
	s := &Store{accounts: []*Account{a}, accountsByID: map[int64]*Account{1: a}, proxyPoolEnabled: true}
	s.proxyPoolLoader = func(context.Context) ([]*database.ProxyRow, error) { return []*database.ProxyRow{row}, nil }
	s.proxyInventoryLoader = s.proxyPoolLoader
	if err := s.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	if s.NextProxy() != url {
		t.Fatal("shared node unavailable")
	}
	row.Exclusive, row.AllowedAccountIDs = true, []int64{1}
	if err := s.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	if s.NextProxy() != "" || s.affinityProxyStillValid(1, url) {
		t.Fatal("toggle retained implicit/sticky access")
	}
	s.ApplyAccountProxyURL(1, url)
	if proxy, usable := s.ResolveUsableProxyForAccount(a); proxy != url || !usable {
		t.Fatalf("allowed exclusive pin unavailable: %q %v", proxy, usable)
	}
	s.proxyPoolLoader = func(context.Context) ([]*database.ProxyRow, error) { return nil, nil }
	if err := s.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	if _, usable := s.ResolveUsableProxyForAccount(a); usable {
		t.Fatal("disabled exclusive pin became usable")
	}
	row.Exclusive = false
	s.proxyPoolLoader = s.proxyInventoryLoader
	if err := s.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	if s.NextProxy() != url {
		t.Fatal("shared toggle did not restore pool")
	}
}

func TestProxyAccessUpdatePublishesBeforeReloadFailure(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "access.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	const url = "http://atomic.example:8080"
	id, err := db.InsertProxy(ctx, url, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.InsertAccount(ctx, "allowed", "rt-a", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, proxyPoolEnabled: true, proxyPool: []string{url}, proxyPoolSet: buildProxyPoolSet([]string{url}), managedProxySet: buildProxyPoolSet([]string{url})}
	s.proxyPoolLoader = func(context.Context) ([]*database.ProxyRow, error) {
		return nil, errors.New("synthetic reload failure")
	}
	if _, err := s.UpdateProxyAccess(ctx, id, true, []int64{a}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReloadProxyPool(); err == nil {
		t.Fatal("expected synthetic failure")
	}
	if s.NextProxy() != "" || s.ProxyAllowedForAccount(a+1, url) || !s.ProxyAllowedForAccount(a, url) {
		t.Fatal("failed reload reopened restriction")
	}
	if proxy, usable := s.ResolveUsableProxyForAccount(&Account{DBID: a + 1, ProxyURL: url}); proxy != "" || usable {
		t.Fatalf("direct fallback guard failed: %q %v", proxy, usable)
	}
	s.RemoveProxyURLs([]string{url})
	s.SetProxyPoolEnabled(false)
	if s.ProxyAllowedForAccount(a, url) {
		t.Fatal("retired URL became a custom exclusive bypass")
	}
}

func TestProxyAccessCachedAffinityCannotResurrectRestrictedExit(t *testing.T) {
	tc := cache.NewMemory(1)
	defer tc.Close()
	const url = "http://cached.example:8080"
	a := &Account{DBID: 1}
	b := &Account{DBID: 2, ProxyURL: url}
	s := &Store{accounts: []*Account{a, b}, tokenCache: tc, exclusiveProxyAccounts: map[string][]int64{url: {1}}}
	ctx := context.Background()
	for _, id := range []int64{1, 2} {
		if err := tc.SetSessionAffinity(ctx, "stale", cache.SessionAffinityBinding{AccountID: id, ProxyURL: url}, time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.getCachedSessionAffinity("stale"); ok {
			t.Fatalf("cached exclusive exit resurrected for account %d", id)
		}
		if _, ok, err := tc.GetSessionAffinity(ctx, "stale"); err != nil || ok {
			t.Fatalf("incompatible cache entry survived: ok=%v err=%v", ok, err)
		}
	}
	s.ApplyAccountProxyURL(1, url)
	if err := tc.SetSessionAffinity(ctx, "allowed", cache.SessionAffinityBinding{AccountID: 1, ProxyURL: url}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.getCachedSessionAffinity("allowed"); !ok {
		t.Fatal("allowed explicit pin was rejected")
	}
}

func TestProxyAccessAccountOnlyExecutorsFailClosed(t *testing.T) {
	const url = "http://executor.example:8080"
	s := &Store{exclusiveProxyAccounts: map[string][]int64{url: {1}}}
	allowed := &Account{DBID: 1, ProxyURL: url, proxyStore: s}
	denied := &Account{DBID: 2, ProxyURL: url, proxyStore: s}
	if got, err := allowed.ResolveProxyForRequest(""); err != nil || got != url {
		t.Fatalf("allowed pin: %q %v", got, err)
	}
	for _, override := range []string{"", url, "http://other.example:8080"} {
		if got, err := denied.ResolveProxyForRequest(override); !errors.Is(err, database.ErrProxyAccessDenied) || got != "" {
			t.Fatalf("denied override %q: %q %v", override, got, err)
		}
	}
	if _, err := denied.KiroRequestAuth(); !errors.Is(err, database.ErrProxyAccessDenied) {
		t.Fatalf("Kiro snapshot bypass: %v", err)
	}
	if err := s.refreshAccountWithOptions(context.Background(), denied, false); !errors.Is(err, database.ErrProxyAccessDenied) {
		t.Fatalf("refresh dispatch bypass: %v", err)
	}
	unpinned := &Account{DBID: 1, proxyStore: s}
	if _, err := unpinned.ResolveProxyForRequest(url); !errors.Is(err, database.ErrProxyAccessDenied) {
		t.Fatalf("exclusive override without pin accepted: %v", err)
	}
}
