package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
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
