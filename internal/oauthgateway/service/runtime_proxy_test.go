package service

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAccountRuntimeProxiesStayLocalAndFollowDeliveryMapping(t *testing.T) {
	f := newPair(t)
	a, store, _ := configuredApp(t, f)
	in := exampleInput("account-runtime-proxies")
	in.RuntimeProxy = "http://default-runtime.example.invalid:8080"
	in.Accounts[0].RuntimeProxy = "http://user:private-runtime@first-runtime.example.invalid:8080"
	in.Accounts = append(in.Accounts,
		InputAccount{Identifier: "two@example.invalid", Password: "second", TOTPSecret: "JBSWY3DPEHPK3PXP"},
		InputAccount{Identifier: "three@example.invalid", Password: "third", TOTPSecret: "JBSWY3DPEHPK3PXP", RuntimeProxy: "socks5://third-runtime.example.invalid:1080", Proxy: json.RawMessage(`"http://oauth-only.example.invalid:8080"`)},
	)
	groups, preview, err := normalize(in)
	if err != nil || !preview.Valid || len(groups) != 2 {
		t.Fatalf("normalize: %v %+v groups=%d", err, preview, len(groups))
	}
	// Two different runtime proxies do not create separate OAuth batches.
	if !reflect.DeepEqual(groups[0].RuntimeProxies, []string{in.Accounts[0].RuntimeProxy, in.RuntimeProxy}) {
		t.Fatal("runtime mapping lost")
	}
	for _, g := range groups {
		raw, _ := json.Marshal(g.Request)
		if strings.Contains(string(raw), "runtime") {
			t.Fatalf("runtime proxy sent to OAuth: %s", raw)
		}
	}
	if _, _, err = a.Create(in); err != nil {
		t.Fatal(err)
	}
	state, _ := store.View()
	if state.Version != 4 {
		t.Fatalf("old worker could ignore pins: version=%d", state.Version)
	}
	// Simulate restart from a serialized checkpoint before executing any side effect.
	restored, err := cloneState(state)
	if err != nil {
		t.Fatal(err)
	}
	nextStore := &memoryStore{state: restored}
	next, err := New(nextStore, f, false)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	v := drive(t, next, in.RequestID)
	if v.Imported != 3 {
		t.Fatalf("not imported: %+v", v)
	}
	got := map[string]string{}
	for i, account := range f.imported {
		got[account] = f.importProxies[i]
	}
	want := map[string]string{"one@example.invalid": in.Accounts[0].RuntimeProxy, "two@example.invalid": in.RuntimeProxy, "three@example.invalid": in.Accounts[2].RuntimeProxy}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("account proxies crossed: got=%v want=%v", got, want)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "private-runtime") || strings.Contains(string(raw), "runtime.example") {
		t.Fatal("proxy credentials leaked into task response")
	}
	final, _ := nextStore.View()
	for _, g := range final.Jobs[0].Groups {
		if len(g.RuntimeProxies) != 0 {
			t.Fatal("terminal runtime secrets retained")
		}
	}
	if _, err := next.Archive(in.RequestID, false); err != nil {
		t.Fatal(err)
	}
	final, _ = nextStore.View()
	if final.Version != 4 {
		t.Fatal("archive downgraded state version")
	}
}

func TestAccountRuntimeProxyRetryAndFingerprint(t *testing.T) {
	f := newPair(t)
	f.importMode = "failed"
	a, store, _ := configuredApp(t, f)
	in := exampleInput("runtime-retry")
	in.Accounts[0].RuntimeProxy = "http://retry-runtime.example.invalid:8080"
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if !v.CanRetryImport {
		t.Fatalf("retry not offered: %+v", v)
	}
	state, _ := store.View()
	if state.Jobs[0].Groups[0].RuntimeProxies[0] != in.Accounts[0].RuntimeProxy {
		t.Fatal("retry pin purged")
	}
	changed := in
	changed.Accounts = append([]InputAccount(nil), in.Accounts...)
	changed.Accounts[0].RuntimeProxy = "http://other-runtime.example.invalid:8080"
	if _, _, err := a.Create(changed); err == nil {
		t.Fatal("same request ID accepted changed runtime pin")
	}
	f.importMode = ""
	if _, err := a.Action(in.RequestID, "retry-import"); err != nil {
		t.Fatal(err)
	}
	v = drive(t, a, in.RequestID)
	if v.Imported != 1 || len(f.importProxies) != 2 || f.importProxies[0] != f.importProxies[1] {
		t.Fatalf("retry changed proxy: %+v %v", v, f.importProxies)
	}
}

func TestAccountRuntimeProxyValidationAndRecoveryIsolation(t *testing.T) {
	in := exampleInput("runtime-invalid")
	in.Accounts[0].RuntimeProxy = "http://example.invalid\nprivate"
	_, p, err := normalize(in)
	if err != nil || p.Valid || len(p.Errors) != 1 || p.Errors[0].Code != "invalid_runtime_proxy" {
		t.Fatalf("bad runtime accepted: %+v %v", p, err)
	}
	in.Accounts[0].RuntimeProxy = "http://proxy.example.invalid:8080"
	in.GroupIDs = nil
	in.RecoveryAccountID = 42
	if _, _, err = normalize(in); err == nil {
		t.Fatal("repair allowed proxy mutation")
	}
}

func TestAccountRuntimeProxyMalformedMappingRejectsImport(t *testing.T) {
	f := newPair(t)
	a, store, _ := configuredApp(t, f)
	in := exampleInput("runtime-mapping")
	in.Accounts[0].RuntimeProxy = "http://runtime.example.invalid:8080"
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(s *State) error { s.Jobs[0].Groups[0].RuntimeProxies = []string{"one", "two"}; return nil }); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if f.imports != 0 || v.Failed != 1 {
		t.Fatalf("invalid mapping imported: %+v imports=%d", v, f.imports)
	}
}
