package auth

import (
	"testing"

	"github.com/codex2api/database"
)

func TestCodexContextTimezoneDefaults(t *testing.T) {
	ctx := t.Context()
	db := newProxyPoolReloadTestDB(t)
	const pin = "http://fixed.example:8080"
	id, err := db.InsertProxy(ctx, pin, "")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	account := &Account{DBID: 7, ProxyURL: pin, proxyStore: store}
	check := func(override, want string) {
		t.Helper()
		if got := account.EffectiveCodexContextTimezone(override); got != want {
			t.Fatalf("context timezone = %q, want %q", got, want)
		}
	}
	check("", "")
	store.SetAccountTimezoneDefaults("UTC", true)
	check("", "UTC")
	if err := db.UpdateProxyTestResultWithTimezone(ctx, id, pin, database.ProxyTestStatusSuccess, "192.0.2.1", "", 1, "Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	check("", "Asia/Tokyo")
	check(pin, "Asia/Tokyo")
	check("http://other.example:8080", "UTC")
	if got := account.EffectiveCodexTimezone(); got != "" {
		t.Fatalf("inherited timezone must not change the explicit binding: %q", got)
	}
	account.Timezone = "Europe/Berlin"
	check("", "Europe/Berlin")
	account.Timezone = ""
	store.SetAccountTimezoneDefaults("UTC", false)
	check("", "UTC")
	store.SetAccountTimezoneDefaults("UTC", true)
	store.RemoveProxyURLs([]string{pin})
	check("", "UTC")
}
