package database

import (
	"context"
	"testing"
	"time"

	ic "github.com/codex2api/internal/intelligencecheck"
)

func TestIntelligenceModelSkipDoesNotRetryUntilMatchesChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		matches []string
		last    string
	}{
		{"no intersection", nil, ""},
		{"upstream rejected all", []string{"expensive"}, "expensive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := intelligenceDB(t), context.Background()
			s, err := db.GetIntelligenceCheckSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			s.Enabled, s.GPTModel = true, "cheap,expensive"
			if _, err := db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
				t.Fatal(err)
			}
			id := intelligenceAccount(t, db)
			now := time.Now().UTC().Truncate(time.Millisecond)
			matches := tc.matches
			preflight := IntelligenceCheckPreflight{MatchedModels: func(_ context.Context, accountID int64, configured string) []string {
				if accountID != id || configured != s.GPTModel {
					t.Fatalf("wrong account or candidate list: %d, %q", accountID, configured)
				}
				return matches
			}}
			enqueue := func(want int) {
				t.Helper()
				if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 10, preflight); err != nil || n != want {
					t.Fatalf("queued %d, want %d: %v", n, want, err)
				}
			}
			finish := func(o ic.Observation, status, model string) {
				t.Helper()
				r, err := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
				if err != nil || r == nil || r.AccountID != id {
					t.Fatalf("claim: %+v %v", r, err)
				}
				if ok, err := db.FinishIntelligenceCheck(ctx, r.ID, "worker", o, now); err != nil || !ok {
					t.Fatal(ok, err)
				}
				got, err := db.GetIntelligenceCheckRun(ctx, r.ID)
				if err != nil || got.Status != status || got.Model != model {
					t.Fatalf("record status/model: %q %q %v", got.Status, got.Model, err)
				}
			}
			enqueue(1)
			finish(ic.Observation{Skipped: true, Error: "无匹配模型", RequestedModel: tc.last, MatchedModels: tc.last}, "skipped", tc.last)
			interval := time.Duration(s.IntervalSeconds) * time.Second
			for i := 0; i < 3; i++ {
				now = now.Add(interval)
				enqueue(0)
			}
			if _, total, err := db.ListIntelligenceCheckRuns(ctx, IntelligenceCheckRunFilter{AccountID: id}); err != nil || total != 1 {
				t.Fatal("duplicate history", total, err)
			}
			a := intelligenceHoldState(t, db, id)
			if a.ModelSkip == nil || a.QuestionCursor != 0 || a.ErrorRetrySeconds != 0 || a.DegradeStreak != 0 || a.RecoverStreak != 0 || a.QualityPaused {
				t.Fatalf("skip was graded: %+v", a)
			}
			matches = []string{"cheap"}
			now = now.Add(interval)
			enqueue(1)
			finish(ic.Observation{RequestedModel: "cheap", Complete: true, Output: "21"}, "normal", "cheap")
			if a = intelligenceHoldState(t, db, id); a.ModelSkip != nil || a.QuestionCursor != 1 {
				t.Fatalf("matching capability did not resume normal checks: %+v", a)
			}
		})
	}
}
