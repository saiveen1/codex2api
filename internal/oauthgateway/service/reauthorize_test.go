package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

func reauthorizationFixture(t *testing.T) (*App, *memoryStore, *fakePair, Input) {
	t.Helper()
	a, store, target := configuredApp(t, newPair(t))
	original := exampleInput("reauth-source")
	original.Accounts[0].Identifier = "imported@example.invalid"
	pending := original.Accounts[0]
	pending.Identifier = "import-failed@example.invalid"
	failed := original.Accounts[0]
	failed.Identifier = "one@example.invalid"
	failed.Proxy = json.RawMessage(`"http://proxy.example.invalid:8080"`)
	original.Accounts = append(original.Accounts, pending, failed)
	if _, _, err := a.Create(original); err != nil {
		t.Fatal(err)
	}
	if err := a.changeJob(original.RequestID, func(j *Job) error {
		g := &j.Groups[0]
		g.Stage = "complete"
		g.Batch = upstream.Batch{BatchID: "mixed-batch", RequestID: g.Request.RequestID, State: "completed", Total: 2, Succeeded: 2,
			Items: []upstream.BatchItem{{ItemID: "imported-item", Line: 1, State: "succeeded"}, {ItemID: "import-failed-item", Line: 2, State: "succeeded"}}}
		g.Deliveries = []Delivery{{ItemID: "imported-item", Line: 1, State: "imported", CreatedIDs: []int64{42}},
			{ItemID: "import-failed-item", Line: 2, State: "import_failed", Account: json.RawMessage(`{"synthetic":true}`)}}
		setConfirmedOAuthFailure(&j.Groups[1])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	in := exampleInput("reauth-child")
	in.RetryOf = &RetryReference{JobID: original.RequestID, Line: 3}
	in.Accounts[0] = InputAccount{Identifier: " ONE@example.invalid ", Password: "fresh-password", TOTPSecret: "AAAAAAAAAAAAAAAA"}
	return a, store, target, in
}

func setConfirmedOAuthFailure(g *Group) {
	g.Stage = "complete"
	g.Batch = upstream.Batch{BatchID: "failed-batch", RequestID: g.Request.RequestID, State: "failed", Total: 1, Failed: 1,
		Items: []upstream.BatchItem{{ItemID: "oauth-failed-item", Line: 1, State: "failed",
			Progress: &upstream.ItemProgress{Stage: "totp_verify"}, Error: &upstream.Error{Code: "oauth_failed", Source: "upstream_login", Action: "manual_review", Evidence: &upstream.FailureEvidence{UpstreamCode: "invalid_state", HTTPStatus: 409, Endpoint: "api_accounts_mfa_verify"}}}}}
}

func requireReauthorizationFault(t *testing.T, err error, code string) {
	t.Helper()
	var f *Fault
	if !errors.As(err, &f) || f.Code != code {
		t.Fatalf("expected %s, got %#v", code, err)
	}
}

func TestReauthorizationUsesGlobalLineAndKeepsImportRetry(t *testing.T) {
	a, store, _, in := reauthorizationFixture(t)
	source, err := a.Job(in.RetryOf.JobID)
	if err != nil || !source.CanRetryImport || source.Imported != 1 || source.Groups[0].Items[0].CanReauthorize || source.Groups[0].Items[1].CanReauthorize || !source.Groups[1].Items[0].CanReauthorize || source.Groups[1].Items[0].Line != 3 {
		t.Fatalf("unexpected source view: %#v, %v", source, err)
	}
	got := source.Groups[1].Items[0].Diagnostic
	if got == nil || *got != (ItemDiagnostic{UpstreamCode: "invalid_state", HTTPStatus: 409, Stage: "totp_verify", Endpoint: "api_accounts_mfa_verify"}) {
		t.Fatalf("terminal row lost diagnostic: %+v", got)
	}
	p, err := a.Preview(context.Background(), in)
	if err != nil || !p.Valid || p.Count != 1 || p.Rows[0].Identifier != "one@example.invalid" {
		t.Fatalf("preview rejected replacement material: %#v, %v", p, err)
	}
	child, existing, err := a.Create(in)
	if err != nil || existing || child.RetryOf == nil || *child.RetryOf != *in.RetryOf || child.Total != 1 {
		t.Fatalf("unexpected create: %#v, %t, %v", child, existing, err)
	}
	s, err := store.View()
	if err != nil || s.Version != 5 || len(s.Jobs) != 2 || s.Jobs[0].Groups[1].Request.Accounts[0].Password != "" {
		t.Fatalf("source login material was retained, or retry reference was not stored (err=%v)", err)
	}
	newGroup := s.Jobs[1].Groups[0]
	if newGroup.Request.RequestID == s.Jobs[0].Groups[1].Request.RequestID || newGroup.Request.Accounts[0].Password != in.Accounts[0].Password || newGroup.Request.Accounts[0].TOTPSecret != in.Accounts[0].TOTPSecret {
		t.Fatal("child reused the source upstream request or did not use new material")
	}
	jobs, err := a.Jobs()
	if err != nil || len(jobs) != 2 || jobs[0].RetryOf == nil || *jobs[0].RetryOf != *in.RetryOf || jobs[1].Groups[1].Items[0].CanReauthorize || !jobs[1].CanRetryImport {
		t.Fatalf("list did not expose the relation or preserve import retry: %#v, %v", jobs, err)
	}
	source, err = a.Action(source.ID, "retry-import")
	if err != nil || source.Imported != 1 || source.Groups[0].Items[0].ImportState != "imported" || source.Groups[0].Items[1].ImportState != "ready" || source.Groups[1].Items[0].State != "failed" {
		t.Fatalf("retry-import changed unrelated rows: %#v, %v", source, err)
	}
}

func TestReauthorizationRejectsUnsafeSources(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Job)
	}{
		{"preflight-failed", func(j *Job) { j.Groups[1].Stage, j.Groups[1].Batch = "failed", upstream.Batch{} }},
		{"submit-unknown", func(j *Job) { j.Groups[1].Stage = "submit_unknown" }},
		{"running", func(j *Job) { j.Groups[1].Stage = "running" }},
		{"pending", func(j *Job) { j.Groups[1].Batch.Pending = 1 }},
		{"remote-batch-review", func(j *Job) { j.Groups[1].Batch.State = "needs_review" }},
		{"remote-review", func(j *Job) { j.Groups[1].Batch.Items[0].State = "needs_review" }},
		{"remote-outcome-unknown", func(j *Job) { j.Groups[1].Batch.Items[0].Error.Code = "execution_outcome_unknown" }},
		{"remote-query-original", func(j *Job) { j.Groups[1].Batch.Items[0].Error.Action = "query_original" }},
		{"manual-account-review", func(j *Job) { j.Groups[1].Batch.Items[0].Error.Code = "account_requires_review" }},
		{"unidentified-failure", func(j *Job) { j.Groups[1].Batch.Items[0].Error = nil }},
		{"blocked", func(j *Job) { j.Groups[1].Batch.Items[0].State = "blocked" }},
		{"sibling-in-review", func(j *Job) { j.Groups[0].Deliveries[1].State = "import_unknown" }},
		{"delivery-exists", func(j *Job) {
			j.Groups[1].Deliveries = []Delivery{{ItemID: "oauth-failed-item", Line: 1, State: "imported"}}
		}},
		{"manual-recovery", func(j *Job) { j.Recovery = &RecoverySnapshot{AccountID: 9} }},
		{"auto-recovery", func(j *Job) { j.Recovery, j.AutoRecovery = &RecoverySnapshot{AccountID: 9}, true }},
		{"archived", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _, in := reauthorizationFixture(t)
			change := tc.change
			if change == nil {
				change = func(j *Job) {
					j.Groups[0].Deliveries[1].State, j.Groups[0].Deliveries[1].Account = "imported", nil
				}
			}
			if err := a.changeJob(in.RetryOf.JobID, func(j *Job) error { change(j); return nil }); err != nil {
				t.Fatal(err)
			}
			if tc.change == nil {
				if _, err := a.Archive(in.RetryOf.JobID, false); err != nil {
					t.Fatal(err)
				}
			}
			v, err := a.Job(in.RetryOf.JobID)
			if err != nil || (!v.Archived && v.Groups[1].Items[0].CanReauthorize) {
				t.Fatalf("unsafe source exposed retry: %#v, %v", v, err)
			}
			_, err = a.Preview(context.Background(), in)
			requireReauthorizationFault(t, err, "cannot_reauthorize")
			_, _, err = a.Create(in)
			requireReauthorizationFault(t, err, "cannot_reauthorize")
		})
	}
}

func TestReauthorizationRequiresFreshSingleMatchingMaterial(t *testing.T) {
	for _, tc := range []struct {
		name, createCode, previewCode string
		change                        func(*Input)
	}{
		{"nonpositive-global-line", "invalid_retry_of", "invalid_retry_of", func(in *Input) { in.RetryOf.Line = 0 }},
		{"success-at-local-line-number", "cannot_reauthorize", "cannot_reauthorize", func(in *Input) { in.RetryOf.Line = 1 }},
		{"no-such-source", "cannot_reauthorize", "cannot_reauthorize", func(in *Input) { in.RetryOf.JobID = "missing" }},
		{"wrong-identity", "reauthorization_identity_mismatch", "reauthorization_identity_mismatch", func(in *Input) { in.Accounts[0].Identifier = "other@example.invalid" }},
		{"combined-with-recovery", "invalid_retry_of", "invalid_retry_of", func(in *Input) { in.RecoveryAccountID = 42 }},
		{"missing-material", "one_material_source_required", "one_material_source_required", func(in *Input) { in.Accounts = nil }},
		{"missing-totp", "invalid_material", "", func(in *Input) { in.Accounts[0].TOTPSecret = "" }},
		{"multiple-accounts", "invalid_retry_material", "invalid_retry_material", func(in *Input) { in.Accounts = append(in.Accounts, in.Accounts[0]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _, in := reauthorizationFixture(t)
			tc.change(&in)
			p, err := a.Preview(context.Background(), in)
			if tc.previewCode != "" {
				requireReauthorizationFault(t, err, tc.previewCode)
			} else if err != nil || p.Valid || len(p.Errors) != 1 || p.Errors[0].Code != "invalid_totp_secret" {
				t.Fatalf("invalid material was not reported: %#v, %v", p, err)
			}
			_, _, err = a.Create(in)
			requireReauthorizationFault(t, err, tc.createCode)
		})
	}
}

func TestReauthorizationDeduplicatesAcrossConcurrentCreateAndArchive(t *testing.T) {
	a, store, target, in := reauthorizationFixture(t)
	type result struct {
		in       Input
		existing bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, id := range []string{"double-click-a", "double-click-b"} {
		candidate := in
		candidate.RequestID = id
		go func(candidate Input) {
			<-start
			_, existing, err := a.Create(candidate)
			results <- result{candidate, existing, err}
		}(candidate)
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil {
		first, second = second, first
	}
	if first.err != nil || first.existing || second.existing {
		t.Fatalf("expected one new task: %#v, %#v", first.err, second.err)
	}
	requireReauthorizationFault(t, second.err, "reauthorization_already_created")
	if _, existing, err := a.Create(first.in); err != nil || !existing {
		t.Fatalf("same request ID should replay idempotently: %t, %v", existing, err)
	}
	if err := a.changeJob(first.in.RequestID, func(j *Job) error { j.Groups[0].Stage = "submit_unknown"; return nil }); err != nil {
		t.Fatal(err)
	}
	_, _, err := a.Create(second.in)
	requireReauthorizationFault(t, err, "reauthorization_already_created")
	_, err = a.Preview(context.Background(), second.in)
	requireReauthorizationFault(t, err, "reauthorization_already_created")
	fromUnknown := second.in
	fromUnknown.RetryOf = &RetryReference{JobID: first.in.RequestID, Line: 1}
	_, _, err = a.Create(fromUnknown)
	requireReauthorizationFault(t, err, "cannot_reauthorize")

	if err := a.changeJob(first.in.RequestID, func(j *Job) error { setConfirmedOAuthFailure(&j.Groups[0]); return nil }); err != nil {
		t.Fatal(err)
	}
	known, err := a.Job(first.in.RequestID)
	if err != nil || !known.Groups[0].Items[0].CanReauthorize {
		t.Fatalf("confirmed failed attempt should be eligible on its own row: %#v, %v", known, err)
	}
	archived, err := a.Archive(first.in.RequestID, false)
	if err != nil || !archived.Archived || archived.RetryOf == nil || *archived.RetryOf != *in.RetryOf {
		t.Fatalf("archive lost retry linkage: %#v, %v", archived, err)
	}
	other := exampleInput("with-runtime-proxy")
	other.Accounts[0].Identifier = "unrelated@example.invalid"
	other.Accounts[0].RuntimeProxy = "http://runtime.example.invalid:8080"
	if _, _, err = a.Create(other); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(store.reopen(t), target, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, _, err = reopened.Create(second.in)
	requireReauthorizationFault(t, err, "reauthorization_already_created")
	if got, existing, err := reopened.Create(first.in); err != nil || !existing || !got.Archived || got.RetryOf == nil || *got.RetryOf != *in.RetryOf {
		t.Fatalf("archived idempotent replay lost linkage: %#v, %t, %v", got, existing, err)
	}
}
