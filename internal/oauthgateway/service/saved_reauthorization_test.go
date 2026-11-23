package service

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestOAuthRecoverySavedReauthorization(t *testing.T) {
	a, store, target := newRecoveryFixture(t)
	enrollRecovery(t, a)
	const id = "saved-fixture-1"
	ctx := t.Context()
	entries, _ := a.Recoveries()
	if len(entries) != 1 || !entries[0].CanReauthorize {
		t.Fatal("ready enrollment should accept a manual request")
	}
	if _, _, err := a.ReauthorizeSaved(ctx, 43, "saved-other"); err == nil {
		t.Fatal("unenrolled account was accepted")
	}
	target.unavailable = true
	if _, _, err := a.ReauthorizeSaved(ctx, 42, "saved-disabled"); err == nil {
		t.Fatal("unavailable original account was accepted")
	}
	target.unavailable = false
	target.snapshot.Identity.ChatGPTAccountID = "wrong-workspace"
	if _, _, err := a.ReauthorizeSaved(ctx, 42, "saved-mismatch"); err == nil {
		t.Fatal("changed workspace was accepted")
	}
	target.snapshot.Identity.ChatGPTAccountID = "workspace-original"
	job, existing, err := a.ReauthorizeSaved(ctx, 42, id)
	if err != nil || existing || job.ID != id || job.AutoRecovery || job.JobType != "manual_recovery" || target.submits != 1 {
		t.Fatalf("manual request did not safely queue: job=%+v replay=%v err=%v", job.JobSummary, existing, err)
	}
	if _, existing, err = a.ReauthorizeSaved(ctx, 42, id); err != nil || !existing {
		t.Fatal("same request was not idempotent", err)
	}
	if _, _, err = a.ReauthorizeSaved(ctx, 42, "saved-overlap"); err == nil {
		t.Fatal("concurrent recovery accepted")
	}
	a.RequestRecovery(42, 1, true)
	job = drive(t, a, id)
	if job.Imported != 1 || target.submits != 2 || target.refreshCalls != 0 || target.recovered != 1 {
		t.Fatalf("manual reauthorization duplicated, refreshed or failed: %+v", job.JobSummary)
	}
	state, err := store.View()
	if err != nil {
		t.Fatal(err)
	}
	r := state.Recoveries[0]
	if r.JobID != id || r.State != "ready" || r.Attempts != 1 || r.NextAttemptAt.IsZero() || r.LastOutcome != reauthorizationSucceeded {
		t.Fatal("successful manual task lost recovery accounting")
	}
	if _, _, err = a.ReauthorizeSaved(ctx, 42, "saved-cooldown"); err == nil {
		t.Fatal("manual request bypassed cooldown")
	}
	var found bool
	for _, j := range state.Jobs {
		if j.ID == id {
			request := target.requests[j.Groups[0].Batch.BatchID]
			found = len(request.Accounts) == 1 && request.Accounts[0].ExpectedIdentity != nil && *request.Accounts[0].ExpectedIdentity == target.snapshot.Identity
		}
	}
	if len(target.requests) != 2 || !found {
		t.Fatal("manual task did not send the original identity assertion")
	}
	if _, err := a.Archive(id, false); err != nil {
		t.Fatal(err)
	}
	if archived, replay, err := a.ReauthorizeSaved(ctx, 42, id); err != nil || !replay || !archived.Archived {
		t.Fatal("archived request did not reconcile", err)
	}
	raw, _ := json.Marshal(job)
	if bytes.Contains(raw, []byte("JBSWY3DPEHPK3PXP")) || bytes.Contains(raw, []byte("keep spaces")) {
		t.Fatal("manual job response exposed saved material")
	}
}
