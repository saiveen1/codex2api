package service

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

func TestImportFlowMappingAndIdempotency(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	in := exampleInput("flow")
	in.Accounts = append(in.Accounts, InputAccount{Identifier: "two@example.invalid", Password: "other", TOTPSecret: "JBSWY3DPEHPK3PXP"})
	preview, err := a.Preview(context.Background(), in)
	if err != nil || !preview.Valid {
		t.Fatalf("preview: %v", err)
	}
	if _, existing, err := a.Create(in); err != nil || existing {
		t.Fatal("create failed")
	}
	if _, existing, err := a.Create(in); err != nil || !existing {
		t.Fatal("idempotent repeat failed")
	}
	changed := in
	changed.RuntimeProxy = "http://other.invalid:80"
	if _, _, err := a.Create(changed); err == nil {
		t.Fatal("changed request reused ID")
	}
	v := drive(t, a, "flow")
	if v.State != "completed" || v.Imported != 2 || f.submits != 1 || f.imports != 2 {
		t.Fatalf("flow state=%s submitted=%d imported=%d", v.State, f.submits, f.imports)
	}
	if len(f.imported) != 2 || f.imported[0] != "one@example.invalid" || f.imported[1] != "two@example.invalid" {
		t.Fatal("delivery mapping was ignored")
	}
	raw, _ := json.Marshal(v)
	for _, secret := range []string{"keep spaces", "JBSWY3DPEHPK3PXP", "synthetic-result-token", "synthetic-oauth"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("secret leaked in task projection")
		}
	}
	state, _ := a.store.View()
	if state.Jobs[0].Groups[0].Request.Accounts[0].Password != "" || state.Jobs[0].Settings.OAuthToken != "" {
		t.Fatal("completed task retained temporary secrets")
	}
}
func TestProxyFailureDoesNotAuthorize(t *testing.T) {
	f := newPair(t)
	f.proxyFails = true
	a, _, _ := configuredApp(t, f)
	in := exampleInput("proxy-failed")
	in.Options.ProxyMode = "custom"
	in.Options.Proxy = json.RawMessage(`"http://proxy.example.invalid:8080"`)
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if v.State != "failed" || f.checks != 1 || f.submits != 0 || f.imports != 0 {
		t.Fatal("failed proxy caused authorization")
	}
	if v.Groups[0].Error == nil || v.Groups[0].Error.Code != "proxy_check_failed" {
		t.Fatal("local error was lost during persistence")
	}
}
func TestLostSubmitFindsOriginalWithoutReplay(t *testing.T) {
	f := newPair(t)
	f.lostSubmit = true
	a, _, _ := configuredApp(t, f)
	in := exampleInput("lost")
	if _, _, e := a.Create(in); e != nil {
		t.Fatal(e)
	}
	v := drive(t, a, in.RequestID)
	if v.State != "completed" || f.submits != 1 || f.lookups != 1 || f.imports != 1 {
		t.Fatalf("lost response: state=%s submits=%d lookups=%d imports=%d", v.State, f.submits, f.lookups, f.imports)
	}
}
func TestUnknownImportNeverReplays(t *testing.T) {
	for _, mode := range []string{"unknown", "generic"} {
		t.Run(mode, func(t *testing.T) {
			f := newPair(t)
			f.importMode = mode
			a, _, _ := configuredApp(t, f)
			in := exampleInput("unknown-import")
			if _, _, err := a.Create(in); err != nil {
				t.Fatal(err)
			}
			v := drive(t, a, in.RequestID)
			if v.State != "needs_review" || v.CanRetryImport || v.Groups[0].Items[0].ImportState != "import_unknown" {
				t.Fatal("unknown import became retryable")
			}
			if _, err := a.Action(in.RequestID, "retry-import"); err == nil {
				t.Fatal("unknown import retry accepted")
			}
			if _, err := a.Action(in.RequestID, "refresh"); err != nil {
				t.Fatal(err)
			}
			drive(t, a, in.RequestID)
			if f.imports != 1 || f.submits != 1 {
				t.Fatal("unknown outcome replayed")
			}
		})
	}
}
func TestAcceptedImportWarningIsNotRetryable(t *testing.T) {
	f := newPair(t)
	f.importMode = "warning"
	a, _, _ := configuredApp(t, f)
	in := exampleInput("warning")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if v.Imported != 1 || v.CanRetryImport || v.Groups[0].Items[0].Error == nil {
		t.Fatal("accepted warning treated as unaccepted import")
	}
	raw, _ := json.Marshal(v)
	if bytes.Contains(raw, []byte("synthetic warning")) {
		t.Fatal("local target error leaked")
	}
}
func TestKnownImportFailureRetriesOnlyImport(t *testing.T) {
	f := newPair(t)
	f.importMode = "failed"
	a, _, _ := configuredApp(t, f)
	in := exampleInput("retry-import")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if !v.CanRetryImport {
		t.Fatal("confirmed failed import cannot be retried")
	}
	f.importMode = ""
	if _, err := a.Action(in.RequestID, "retry-import"); err != nil {
		t.Fatal(err)
	}
	v = drive(t, a, in.RequestID)
	if v.Imported != 1 || f.submits != 1 || f.imports != 2 {
		t.Fatal("import retry repeated OAuth")
	}
}
func TestIncompleteDeliveryStopsInsteadOfSpinning(t *testing.T) {
	f := newPair(t)
	f.missingDelivery = true
	a, _, _ := configuredApp(t, f)
	in := exampleInput("missing-delivery")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if v.State != "needs_review" || f.imports != 0 {
		t.Fatal("incomplete delivery did not stop")
	}
}
func TestRestartDoesNotReplaySubmitting(t *testing.T) {
	f := newPair(t)
	a, store, target := configuredApp(t, f)
	in := exampleInput("restart")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	state, _ := a.store.View()
	req := state.Jobs[0].Groups[0].Request
	o, _ := upstream.NewOAuth(f.server.URL, "synthetic-oauth", false)
	if _, err := o.Submit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := a.mutateGroup(in.RequestID, 0, func(g *Group) { g.Stage = "submitting" }); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	reopened, err := New(store.reopen(t), target, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v := drive(t, reopened, in.RequestID)
	if v.Imported != 1 || f.submits != 1 || f.lookups != 1 {
		t.Fatal("restart replayed a submission")
	}
}
func TestParsePreservesPasswordAndSplitsProxy(t *testing.T) {
	in := exampleInput("parse")
	in.Accounts = nil
	in.SourceText = "\ufeff\nOne@example.invalid---- keep spaces ----jbswy3dpehpk3pxp\n"
	groups, p, err := normalize(in)
	if err != nil || !p.Valid || groups[0].Request.Accounts[0].Password != " keep spaces " || groups[0].Lines[0] != 2 {
		t.Fatal("parser changed material or physical line")
	}
	in.SourceText = "one@example.invalid----ambiguous----password----JBSWY3DPEHPK3PXP"
	_, p, err = normalize(in)
	if err == nil && p.Valid {
		t.Fatal("ambiguous separator accepted")
	}
	in = exampleInput("split")
	in.Accounts = append(in.Accounts, InputAccount{Identifier: "two@example.invalid", Password: "other", TOTPSecret: "JBSWY3DPEHPK3PXP", Proxy: json.RawMessage(`"socks5h://proxy.example.invalid:1080"`)})
	groups, p, err = normalize(in)
	if err != nil || !p.Valid || len(groups) != 2 || groups[1].Request.Options.ProxyMode != "custom" {
		t.Fatal("per-account proxy not split")
	}
}
func TestQueuedCancelAndDestinationChanges(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	in := exampleInput("cancel")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SaveSettings(Settings{OAuthURL: "https://changed.example.invalid"}); err == nil {
		t.Fatal("credentials silently rebound to a new host")
	}
	if _, err := a.Action(in.RequestID, "cancel"); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if v.State != "cancelled" || f.submits != 0 {
		t.Fatal("queued cancel ran OAuth")
	}
}
