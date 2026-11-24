package database

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	ic "github.com/codex2api/internal/intelligencecheck"
)

func TestIntelligenceCheckOncePreservesPolicyAndHistory(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	id, other := intelligenceAccount(t, db), intelligenceAccount(t, db)
	beforeSettings, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runID, err := db.SubmitIntelligenceCheckOnce(ctx, id, "gpt-6.2-sol", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.SubmitIntelligenceCheckOnce(ctx, id, "other", now); !errors.Is(err, ErrIntelligenceCheckTrialActive) {
		t.Fatalf("busy: %v", err)
	}
	run, err := db.ClaimIntelligenceCheck(ctx, "test", now, time.Minute)
	if err != nil || run == nil {
		t.Fatalf("claim: %v %v", run, err)
	}
	if run.ID != runID || run.AccountID != id || !run.Once || !run.RecordOnly || run.Model != "gpt-6.2-sol" || run.Question.Prompt != ic.CandyPrompt || run.Question.ExpectedAnswer != "21" {
		t.Fatalf("snapshot: %+v", run)
	}
	ok, err := db.FinishIntelligenceCheck(ctx, runID, "test", ic.Observation{Complete: true, Output: "21", RequestedModel: run.Model}, now.Add(time.Second))
	if err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	stored, err := db.GetIntelligenceCheckRun(ctx, runID)
	if err != nil || stored.Status != "normal" || !stored.RecordOnly || stored.Model != run.Model {
		t.Fatalf("history: %+v %v", stored, err)
	}
	afterSettings, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil || !reflect.DeepEqual(beforeSettings, afterSettings) {
		t.Fatalf("settings changed: %v", err)
	}
	after, _, err := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("account state changed: %v", err)
	}
	runs, total, err := db.ListIntelligenceCheckRuns(ctx, IntelligenceCheckRunFilter{AccountID: other})
	if err != nil || total != 0 || len(runs) != 0 {
		t.Fatalf("other account: %v %d %v", runs, total, err)
	}
}
