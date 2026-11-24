package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

func TestLocalImportRejectionRequiresExplicitDiscard(t *testing.T) {
	for _, code := range []string{"existing_account_requires_review", "gateway_identity_mismatch", "invalid_gateway_account"} {
		t.Run(code, func(t *testing.T) {
			f := newPair(t)
			a, _, _ := configuredApp(t, f)
			a.target = summaryTarget{Target: f, result: upstream.ImportResult{Total: 1, Failed: 1}, err: LocalImportError(code)}
			input := exampleInput("rejected")
			if _, _, err := a.Create(input); err != nil {
				t.Fatal(err)
			}
			view := drive(t, a, input.RequestID)
			if view.Imported != 0 || view.Failed != 1 || view.CanRetryImport || !view.CanArchive || !view.RetainedMaterial || view.Groups[0].Items[0].ImportState != "import_rejected" {
				t.Fatalf("rejection lost: %+v", view)
			}
			if view.Groups[0].Items[0].Error.Code != code {
				t.Fatal("local error lost its classification")
			}
			if _, err := a.Archive(input.RequestID, false); err == nil {
				t.Fatal("result discarded without confirmation")
			}
			if _, err := a.Archive(input.RequestID, true); err != nil {
				t.Fatal(err)
			}
			if f.submits != 1 {
				t.Fatal("rejection resubmitted OAuth")
			}
		})
	}
}
func TestLocalImportErrorNeverEchoesArbitraryMessage(t *testing.T) {
	local := LocalImportError("existing_account_requires_review")
	local.Message = "synthetic-sensitive-token"
	problem := publicError(local)
	if problem.Code != local.Code || problem.Source != "codex" || strings.Contains(problem.Message, "synthetic-sensitive-token") {
		t.Fatal("unsafe local projection")
	}
	raw, _ := json.Marshal(local)
	var decoded upstream.Error
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if localImportProblem(&decoded) != nil {
		t.Fatal("external response spoofed local adapter error")
	}
	if !LocalImportError("unknown-local-code").Uncertain {
		t.Fatal("unknown local error permitted retry")
	}
}

func TestLegacyReadyDeliveryUsesSavedBatchNotRequestOrResult(t *testing.T) {
	for _, returned := range []string{"", "chrome_macos"} {
		g := Group{Lines: []int{1}, Masks: []string{"fixture"}, Stage: "running", Deliveries: []Delivery{{Line: 1, State: "ready", Account: json.RawMessage(`{"credentials":{"email":"fixture@example.invalid"},"oauth_gateway_environment":"chrome_windows"}`)}}}
		g.Request.Options.Environment = "chrome_windows"
		if returned != "" {
			g.Batch.Options = &upstream.BatchProfile{Environment: returned}
		}
		job := Job{ID: "fixture", Groups: []Group{g}}
		store := &memoryStore{state: State{Version: 1, Jobs: []Job{job}}}
		f := &fakePair{t: t, importMode: "failed"}
		a := &App{store: store, target: f}
		if err := a.importOne(t.Context(), f, job, 0, 0); err != nil {
			t.Fatal(err)
		}
		state, err := store.View()
		if err != nil {
			t.Fatal(err)
		}
		d := state.Jobs[0].Groups[0].Deliveries[0]
		var account map[string]json.RawMessage
		if !d.EnvironmentChecked || d.State != "import_failed" || json.Unmarshal(d.Account, &account) != nil || string(account[upstream.EnvironmentCredential]) != `"`+returned+`"` {
			t.Fatalf("retry did not retain trusted environment %q", returned)
		}
	}
}

func TestDeliveryTimezoneUsesSavedBatchForCheckedAndLegacy(t *testing.T) {
	for _, checked := range []bool{false, true} {
		for _, returned := range []string{"", "America/New_York", "Local"} {
			g := Group{Lines: []int{1}, Masks: []string{"fixture"}, Stage: "running", Deliveries: []Delivery{{Line: 1, State: "ready", EnvironmentChecked: checked, Account: json.RawMessage(`{"credentials":{"email":"fixture@example.invalid"},"oauth_gateway_timezone":"Asia/Tokyo"}`)}}}
			g.Request.Options.Timezone = "Europe/Berlin"
			g.Batch.Options = &upstream.BatchProfile{Timezone: returned}
			job := Job{ID: "fixture", Groups: []Group{g}}
			store := &memoryStore{state: State{Version: 1, Jobs: []Job{job}}}
			f := &fakePair{t: t, importMode: "failed"}
			a := &App{store: store, target: f}
			if err := a.importOne(t.Context(), f, job, 0, 0); err != nil {
				t.Fatal(err)
			}
			state, err := store.View()
			if err != nil {
				t.Fatal(err)
			}
			var account map[string]json.RawMessage
			d := state.Jobs[0].Groups[0].Deliveries[0]
			if d.State != "import_failed" || json.Unmarshal(d.Account, &account) != nil || string(account[upstream.TimezoneCredential]) != `"`+returned+`"` {
				t.Fatalf("checked=%v returned=%q: wrong persisted timezone", checked, returned)
			}
		}
	}
}

func TestKeepReturnedTimezoneSameBatch(t *testing.T) {
	previous := upstream.Batch{BatchID: "batch", Options: &upstream.BatchProfile{Timezone: "America/New_York"}}
	for _, tc := range []struct{ id, timezone, want string }{
		{"batch", "", "America/New_York"},
		{"batch", "Asia/Tokyo", "Asia/Tokyo"},
		{"other", "", ""},
	} {
		next := upstream.Batch{BatchID: tc.id, Options: &upstream.BatchProfile{Timezone: tc.timezone}}
		keepReturnedEnvironment(&next, previous)
		if next.Options.Timezone != tc.want {
			t.Fatalf("batch=%q got %q, want %q", tc.id, next.Options.Timezone, tc.want)
		}
	}
}
