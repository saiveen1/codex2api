package admin

import (
	"context"
	"testing"
)

func TestIntelligenceRunnerSkipsUnmatchedAccountAndResumesOnPolicyChange(t *testing.T) {
	f := newIntelligenceHoldFixture(t)
	setModels := func(models string) {
		t.Helper()
		ctx := context.Background()
		s, err := f.db.GetIntelligenceCheckSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		s.GPTModel = models
		if _, err := f.db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	setModels("not-allowed，also-not-allowed")
	// A no-request skip must complete even when all dispatch slots are busy.
	f.account.ActiveRequests.Add(10000)
	for range 3 {
		f.tick()
	}
	f.account.ActiveRequests.Add(-10000)
	if runs := f.runs(); len(runs) != 1 || runs[0].Status != "skipped" || runs[0].Model != "" || f.requests.Load() != 0 {
		t.Fatalf("unexpected skips or upstream traffic: runs=%+v requests=%d", runs, f.requests.Load())
	}
	setModels("not-allowed\ngpt-5.4")
	f.tick()
	if runs := f.runs(); len(runs) != 2 || runs[0].Status != "normal" || runs[0].Model != "gpt-5.4" || f.requests.Load() != 1 {
		t.Fatalf("new matching model not used exactly once: runs=%+v requests=%d", runs, f.requests.Load())
	}
}
