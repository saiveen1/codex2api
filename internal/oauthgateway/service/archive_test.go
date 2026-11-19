package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

func TestArchiveRetainsIdempotencyAcrossRestart(t *testing.T) {
	f := newPair(t)
	a, store, target := configuredApp(t, f)
	in := exampleInput("archive-one")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if !v.CanArchive || v.RetainedMaterial {
		t.Fatal("completed task not archivable or retained login material")
	}
	// Preserve discard confirmation for unresolved task material; retained
	// account recovery material is stored separately from the archived job.
	if err := store.Update(func(s *State) error {
		s.Jobs[0].SaveCredentials = true
		s.Jobs[0].Groups[0].Request.Accounts[0].Password = " keep spaces "
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Archive(in.RequestID, false); err == nil {
		t.Fatal("saved material discarded without acknowledgement")
	}
	v, err := a.Archive(in.RequestID, true)
	if err != nil || !v.Archived || v.CanArchive || v.CanRefresh || v.Imported != 1 {
		t.Fatalf("archive outcome: %v", err)
	}
	state, err := a.store.View()
	if err != nil || len(state.Jobs) != 0 || len(state.Archived) != 1 || state.Version != 3 {
		t.Fatal("archive did not compact durable state")
	}
	archivedJSON, _ := json.Marshal(state.Archived)
	for _, secret := range []string{"keep spaces", "JBSWY3DPEHPK3PXP", "one@example.invalid", "synthetic-oauth"} {
		if bytes.Contains(archivedJSON, []byte(secret)) {
			t.Fatal("archive retained account material")
		}
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(store.reopen(t), target, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v, existing, err := reopened.Create(in)
	if err != nil || !existing || !v.Archived {
		t.Fatal("archived request replay was not idempotent")
	}
	if worked, err := reopened.Step(context.Background()); err != nil || worked {
		t.Fatal("archive replay scheduled OAuth")
	}
	changed := in
	changed.RuntimeProxy = "http://other.example.invalid:80"
	if _, _, err = reopened.Create(changed); err == nil {
		t.Fatal("archived ID rebound to new input")
	}
	if f.submits != 1 || f.imports != 1 {
		t.Fatal("archive caused a remote write")
	}
	if _, err = reopened.Archive(in.RequestID, true); err != nil {
		t.Fatal("archive not idempotent")
	}
	list, err := reopened.ArchivedJobs()
	if err != nil || len(list) != 1 || !list[0].Archived {
		t.Fatal("archive listing incorrect")
	}
	v, err = reopened.Job(in.RequestID)
	if err != nil || !v.Archived {
		t.Fatal("archived job lookup failed")
	}
}
func TestArchiveRejectsUnresolvedWork(t *testing.T) {
	for _, scenario := range []string{"queued", "unknown", "retryable"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPair(t)
			a, _, _ := configuredApp(t, f)
			if scenario == "unknown" {
				f.importMode = "unknown"
			}
			if scenario == "retryable" {
				f.importMode = "failed"
			}
			in := exampleInput("unresolved")
			if _, _, err := a.Create(in); err != nil {
				t.Fatal(err)
			}
			if scenario != "queued" {
				drive(t, a, in.RequestID)
			}
			before, _ := a.store.View()
			if _, err := a.Archive(in.RequestID, true); err == nil {
				t.Fatal("unresolved work archived")
			}
			after, _ := a.store.View()
			if len(after.Jobs) != len(before.Jobs) || len(after.Archived) != 0 {
				t.Fatal("rejected archive changed state")
			}
		})
	}
}
func TestArchiveFreesCapacityWithoutDeletingDeduplication(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	if err := a.store.Update(func(s *State) error {
		for i := 0; i < 200; i++ {
			s.Jobs = append(s.Jobs, Job{ID: fmt.Sprintf("finished-%d", i), Fingerprint: "synthetic", CreatedAt: time.Now(), UpdatedAt: time.Now(), Groups: []Group{{Stage: "cancelled", Lines: []int{1}, Masks: []string{"***"}}}})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	in := exampleInput("after-archive")
	if _, _, err := a.Create(in); err == nil {
		t.Fatal("full active record store accepted task")
	}
	if _, err := a.Archive("finished-0", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Create(in); err != nil {
		t.Fatal("archive did not free active capacity")
	}
	state, _ := a.store.View()
	if len(state.Jobs) != 200 || len(state.Archived) != 1 {
		t.Fatal("wrong archive capacity accounting")
	}
}

type archiveResultTarget struct {
	*recoveryTestTarget
	lookups int
}

func (t *archiveResultTarget) AccountResult(_ context.Context, _ int64) AccountResult {
	t.lookups++
	ids := []int64{7, 9}
	proxyID := int64(13)
	return AccountResult{PlanType: "self_serve_business_prolite", WorkspaceID: "final-workspace", GroupIDs: &ids, RuntimeProxyID: &proxyID, IdentifierMasked: "one@example.invalid"}
}

func TestArchiveRetainsOnlySafeJobResults(t *testing.T) {
	a, s, target := newRecoveryFixture(t)
	adapter := &archiveResultTarget{recoveryTestTarget: target}
	a.target = adapter
	in := exampleInput("result-archive")
	in.RuntimeProxy = "http://synthetic-proxy.example.invalid:80"
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if v.JobType != "import" || v.RequestedCountry != "US" || v.GroupIDs == nil || len(*v.GroupIDs) != 1 || (*v.GroupIDs)[0] != 1 || len(v.AccountResults) != 1 {
		t.Fatalf("wrong submitted summary: %+v", v.JobSummary)
	}
	r := v.AccountResults[0]
	if r.Line != 1 || r.AccountID != 42 || r.PlanType != "self_serve_business_prolite" || r.WorkspaceID != "final-workspace" || r.GroupIDs == nil || len(*r.GroupIDs) != 2 || (*r.GroupIDs)[0] != 7 || r.RuntimeProxyID == nil || *r.RuntimeProxyID != 13 || r.Outcome != "imported" || r.IdentifierMasked != "o***@example.invalid" {
		t.Fatalf("wrong committed result: %+v", r)
	}
	archived, err := a.Archive(in.RequestID, false)
	if err != nil || len(archived.AccountResults) != 1 || archived.AccountResults[0].AccountID != 42 {
		t.Fatalf("archive lost summary: %+v, %v", archived, err)
	}
	state, _ := s.View()
	for _, value := range []any{archived, state.Archived} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"one@example.invalid", "keep spaces", "JBSWY3DPEHPK3PXP", "synthetic-oauth", "synthetic-result-token", in.RuntimeProxy, "observed_country"} {
			if bytes.Contains(raw, []byte(forbidden)) {
				t.Fatalf("archive included forbidden data: %q", forbidden)
			}
		}
	}
	if adapter.lookups != 1 {
		t.Fatalf("account result looked up %d times; expected one at commit", adapter.lookups)
	}
}

func TestArchiveLegacySummaryAndAutomaticRecoveryReconciliation(t *testing.T) {
	a, _, _ := newRecoveryFixture(t)
	var legacy ArchivedJob
	if err := json.Unmarshal([]byte(`{"id":"legacy","state":"completed","total":1,"imported":1,"fingerprint":"previous"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(s *State) error {
		s.Archived = append(s.Archived, legacy)
		s.Recoveries = append(s.Recoveries, Recovery{Snapshot: RecoverySnapshot{AccountID: 21}, Enabled: true, State: "recovering", JobID: "legacy"})
		s.Recoveries = append(s.Recoveries, Recovery{Snapshot: RecoverySnapshot{AccountID: 22}, Enabled: true, State: "recovering", JobID: "resolved"})
		s.Jobs = append(s.Jobs, Job{ID: "resolved", AutoRecovery: true, Recovery: &RecoverySnapshot{AccountID: 22}, Groups: []Group{{Stage: "failed", Lines: []int{1}, Masks: []string{"***"}, Request: upstream.SubmitRequest{Options: upstream.Options{Country: "http://not-a-country.example.invalid"}}}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	v, err := a.Job("legacy")
	if err != nil || !v.Archived || v.JobType != "" || v.GroupIDs != nil || v.AccountResults != nil || v.RequestedCountry != "" {
		t.Fatalf("invented legacy metadata: %+v, %v", v, err)
	}
	if v, err = a.Archive("resolved", false); err != nil || v.JobType != "auto_recovery" || v.RequestedCountry != "" {
		t.Fatalf("automatic recovery archive failed: %+v, %v", v, err)
	}
	if worked, err := a.Step(t.Context()); !worked || err != nil {
		t.Fatalf("legacy recovery not reconciled: %v", err)
	}
	list, err := a.Recoveries()
	if err != nil || len(list) != 2 || list[0].State != "ready" || list[0].LastOutcome != "" || list[1].State != "manual_review" || list[1].LastOutcome != "" {
		t.Fatalf("recovery got stuck or acquired an invented outcome: %+v, %v", list, err)
	}
}
