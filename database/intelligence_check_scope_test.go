package database

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	ic "github.com/codex2api/internal/intelligencecheck"
)

func TestIntelligenceCheckScopedRecoveryAndHistory(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	makeGroup := func(name string) int64 {
		t.Helper()
		id, err := db.CreateAccountGroup(ctx, name, "", "", 0, 0, sql.NullInt64{})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	main, isolation := makeGroup("active"), makeGroup("isolation")
	free, pro := intelligenceAccount(t, db), intelligenceAccount(t, db)
	for id, plan := range map[int64]string{free: "free", pro: " PRO "} {
		if err := db.SetAccountGroups(ctx, id, []int64{main}); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateCredentials(ctx, id, map[string]any{"plan_type": plan}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Enabled = true
	s.Rules = []ic.Rule{
		{ID: "specific", Name: "Pro isolation", Channel: "codex", GroupID: main, Plan: " PRO ", Automatic: true, Model: "gpt-6-sol", IntervalSeconds: 10, DegradeThreshold: 1, Action: "switch_group", TargetGroupID: isolation},
		{ID: "rest", Name: "Disable other active accounts", Channel: "codex", GroupID: main},
		{ID: "quarantine", Name: "Exclude actual isolation group", Channel: "codex", GroupID: isolation},
	}
	if _, err = db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	accounts, n, err := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{GroupID: main, Plan: "pro"})
	if err != nil || n != 1 || len(accounts) != 1 || accounts[0].RuleID != "specific" || !accounts[0].AutomaticEffective {
		t.Fatalf("account filter or effective rule: %+v %d %v", accounts, n, err)
	}
	for step, output := range []string{"incorrect", "21"} {
		at := now.Add(time.Duration(step) * 20 * time.Second)
		checkedModel := ""
		queued, err := db.EnqueueDueIntelligenceChecks(ctx, at, 1, func(_ int64, model string) (time.Time, bool) { checkedModel = model; return time.Time{}, false })
		if err != nil || queued != 1 || checkedModel != "gpt-6-sol" {
			t.Fatalf("queue step %d: %d %q %v", step, queued, checkedModel, err)
		}
		run, err := db.ClaimIntelligenceCheck(ctx, "worker", at, time.Minute)
		if err != nil || run == nil || run.AccountID != pro || run.RuleID != "specific" || run.Model != "gpt-6-sol" || run.Plan != "pro" {
			t.Fatalf("claim step %d: %+v %v", step, run, err)
		}
		wantSnapshot, wantAfter := main, isolation
		if step == 1 {
			wantSnapshot, wantAfter = isolation, main
		}
		if len(run.Groups) != 1 || run.Groups[0].ID != wantSnapshot {
			t.Fatalf("incorrect group snapshot: %+v", run.Groups)
		}
		if ok, err := db.FinishIntelligenceCheck(ctx, run.ID, "worker", ic.Observation{Complete: true, Output: output}, at.Add(time.Second)); !ok || err != nil {
			t.Fatal(ok, err)
		}
		if ids, err := db.GetAccountGroupIDs(ctx, pro); !slices.Equal(ids, []int64{wantAfter}) || err != nil {
			t.Fatal(ids, err)
		}
	}
	if err := db.UpdateCredentials(ctx, pro, map[string]any{"plan_type": "free"}); err != nil {
		t.Fatal(err)
	}
	history, count, err := db.ListIntelligenceCheckRuns(ctx, IntelligenceCheckRunFilter{GroupID: isolation, Plan: "pro", Source: "automatic", Status: "normal", From: now.Add(time.Second), Before: now.Add(time.Minute), Summary: true})
	if err != nil || count != 1 || len(history) != 1 || history[0].Action != "restore_groups" || history[0].Output != "" || history[0].Question.Prompt != "" || history[0].Question.Name == "" {
		t.Fatalf("history: %+v %d %v", history, count, err)
	}
	// A plan-only override can enroll a new account even when the base target belongs to another source.
	s, err = db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Action, s.TargetGroupID = "switch_group", isolation
	s.Rules = append(s.Rules, ic.Rule{ID: "claude-plan", Name: "Claude plan", Channel: "claude", Plan: "max", Automatic: true, Action: "none"})
	if _, err = db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	claude, err := db.InsertAccountWithUpstream(ctx, "new Claude", "anthropic", "oauth", map[string]any{"upstream_type": "claude", "plan_type": "max"}, "")
	if err != nil {
		t.Fatal(err)
	}
	results, _, err := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{AccountID: claude})
	if err != nil || len(results) != 1 || !results[0].AutomaticEffective || results[0].RuleID != "claude-plan" {
		t.Fatal(results, err)
	}
	detail, err := db.GetIntelligenceCheckRun(ctx, history[0].ID)
	if err != nil || detail.Output != "21" || detail.Question.Prompt == "" {
		t.Fatalf("detail: %+v %v", detail, err)
	}
}

func testIntelligenceCheckPostgresPlanLock(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	id := intelligenceAccount(t, db)
	fence, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fence.Rollback()
	if _, err := db.intelligenceSettingsTx(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := db.intelligenceAccountTx(ctx, fence, id); err != nil {
		t.Fatal(err)
	}
	writer, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err = writer.ExecContext(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = writer.ExecContext(ctx, `UPDATE accounts SET credentials=credentials || '{"plan_type":"free"}'::jsonb WHERE id=$1`, id)
	var lockError interface{ SQLState() string }
	if !errors.As(err, &lockError) || lockError.SQLState() != "55P03" {
		t.Fatalf("plan write did not wait for detector transition: %v", err)
	}
}

func TestIntelligenceCheckRecordOnlyIsolation(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	id := intelligenceAccount(t, db)
	s, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Action, s.DegradeThreshold, s.Retention = "pause", 1, 1
	if _, err := db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	live := intelligenceRun(t, db, id, now, "21")
	readState := func() (state, status string, due int64, cursor int, paused bool) {
		t.Helper()
		if err := db.conn.QueryRowContext(ctx, `SELECT state_json,status,next_due_ms,question_cursor,quality_paused FROM intelligence_check_accounts WHERE account_id=$1`, id).Scan(&state, &status, &due, &cursor, &paused); err != nil {
			t.Fatal(err)
		}
		return
	}
	before, beforeStatus, beforeDue, beforeCursor, beforePaused := readState()
	for i := range 2 {
		at := now.Add(time.Duration(i+1) * 10 * time.Minute)
		ids, skipped, err := db.SubmitIntelligenceChecks(ctx, []int64{id}, at, true)
		if err != nil || len(ids) != 1 || len(skipped) != 0 {
			t.Fatal(ids, skipped, err)
		}
		if got, skipped, err := db.SubmitIntelligenceChecks(ctx, []int64{id}, at, true); err != nil || len(got) != 0 || !slices.Equal(skipped, []int64{id}) {
			t.Fatal(got, skipped, err)
		}
		if _, err := db.EnqueueIntelligenceChecks(ctx, []int64{id}, at); !errors.Is(err, ErrIntelligenceCheckTrialActive) {
			t.Fatal(err)
		}
		run, err := db.ClaimIntelligenceCheck(ctx, "trial", at, time.Minute)
		if err != nil || run == nil || !run.RecordOnly || run.ID != ids[0] {
			t.Fatal(run, err)
		}
		if i == 0 {
			if ok, err := db.FinishIntelligenceCheck(ctx, run.ID, "trial", ic.Observation{Complete: true, Output: "wrong"}, at.Add(time.Second)); !ok || err != nil {
				t.Fatal(ok, err)
			}
		} else if n, err := db.ExpireIntelligenceCheckLeases(ctx, at.Add(5*time.Minute)); n != 1 || err != nil {
			t.Fatal(n, err)
		}
	}
	state, status, due, cursor, paused := readState()
	if state != before || status != beforeStatus || due != beforeDue || cursor != beforeCursor || paused != beforePaused {
		t.Fatal("trial mutated detection state")
	}
	bars, err := db.IntelligenceCheckBars(ctx, []int64{id})
	if err != nil || len(bars[id].Records) != 1 || bars[id].Records[0].ID != live.ID {
		t.Fatal(bars, err)
	}
	only := true
	trials, count, err := db.ListIntelligenceCheckRuns(ctx, IntelligenceCheckRunFilter{RecordOnly: &only, AccountID: id})
	if err != nil || count != 1 || len(trials) != 1 || trials[0].Status != "interrupted" {
		t.Fatal(trials, count, err)
	}
	at := now.Add(time.Hour)
	ids, err := db.EnqueueIntelligenceChecks(ctx, []int64{id}, at)
	if len(ids) != 1 || err != nil {
		t.Fatal(ids, err)
	}
	if got, skipped, err := db.SubmitIntelligenceChecks(ctx, []int64{id}, at, true); err != nil || len(got) != 0 || !slices.Equal(skipped, []int64{id}) {
		t.Fatal(got, skipped, err)
	}
	if run, err := db.GetIntelligenceCheckRun(ctx, ids[0]); err != nil || run.RecordOnly || run.Status != "queued" {
		t.Fatal(run, err)
	}
}
