package proxy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestResolveProxyForAttemptAccessAndUnavailable(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "proxy-access.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ownerID, err := db.InsertAccountWithCredentials(ctx, "owner", map[string]interface{}{"access_token": "synthetic-owner"}, "")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := db.InsertAccountWithCredentials(ctx, "other", map[string]interface{}{"access_token": "synthetic-other"}, "")
	if err != nil {
		t.Fatal(err)
	}
	const private = "http://private.example.invalid:18080"
	const shared = "http://shared.example.invalid:18081"
	privateID, err := db.InsertProxy(ctx, private, "private")
	if err != nil {
		t.Fatal(err)
	}
	sharedID, err := db.InsertProxy(ctx, shared, "shared")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, ProxyPoolEnabled: true})
	t.Cleanup(store.Stop)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateProxyAccess(ctx, privateID, true, []int64{ownerID}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: store}
	owner := &auth.Account{DBID: ownerID, ProxyURL: private}
	denied := &auth.Account{DBID: otherID, ProxyURL: private}
	unpinnedOwner := &auth.Account{DBID: ownerID}
	unpinnedOther := &auth.Account{DBID: otherID}
	check := func(name string, account *auth.Account, sticky, want string, wantError bool) {
		t.Helper()
		got, err := h.resolveProxyForAttempt(account, sticky)
		if got != want || (err != nil) != wantError {
			t.Errorf("%s: got route %q error %v; want %q error=%v", name, got, err, want, wantError)
		}
	}
	check("owner explicit pin", owner, private, private, false)
	check("nonowner explicit pin", denied, shared, "", true)
	check("unauthorized sticky falls back", unpinnedOther, private, shared, false)
	check("allowlisted but unpinned cannot select exclusive", unpinnedOwner, private, shared, false)
	check("shared sticky", unpinnedOwner, shared, shared, false)

	disabled := false
	if err := db.UpdateProxy(ctx, privateID, nil, nil, &disabled); err != nil {
		t.Fatal(err)
	}
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	check("disabled pinned node", owner, shared, "", true)
	if err := db.UpdateProxyTestResult(ctx, sharedID, shared, database.ProxyTestStatusError, "", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	check("no usable proxy cannot become direct", unpinnedOther, shared, "", true)

	var noStore *Handler
	if got, err := noStore.resolveProxyForAttempt(owner, "  "+shared+"  "); got != shared || err != nil {
		t.Fatalf("nil store fallback: %q, %v", got, err)
	}
}
