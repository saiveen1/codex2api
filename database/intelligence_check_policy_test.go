package database

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	ic "github.com/codex2api/internal/intelligencecheck"
)

func TestIntelligenceCheckIndependentPolicy(t *testing.T) {
	for _, scenario := range []string{"removed_rule", "manual_group", "manual_disable", "missing_origins"} {
		t.Run(scenario, func(t *testing.T) {
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
			original, other := makeGroup("production"), makeGroup("priority")
			isolation, manual := makeGroup("isolation"), makeGroup("manual")
			unmatched, id := intelligenceAccount(t, db), intelligenceAccount(t, db)
			if err := db.SetAccountGroups(ctx, id, []int64{original, other}); err != nil {
				t.Fatal(err)
			}
			settings, err := db.GetIntelligenceCheckSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			settings.Enabled, settings.RulesOnly = true, true
			settings.IntervalSeconds = 5
			settings.Rules = []ic.Rule{{ID: "isolating", Name: "Isolate", Channel: "codex", GroupID: original, Automatic: true, DegradeThreshold: 1, RecoverThreshold: 2, Action: ic.ActionPauseAndMove, TargetGroupID: isolation}}
			if _, err = db.SaveIntelligenceCheckSettings(ctx, settings); err != nil {
				t.Fatal(err)
			}
			check := func(output string) IntelligenceCheckRun {
				t.Helper()
				now = now.Add(10 * time.Second)
				count, err := db.EnqueueDueIntelligenceChecks(ctx, now, 1)
				if err != nil || count != 1 {
					t.Fatalf("scheduled %d: %v", count, err)
				}
				run, err := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
				if err != nil || run == nil || run.AccountID != id || run.Source != "automatic" {
					t.Fatal(run, err)
				}
				ok, err := db.FinishIntelligenceCheck(ctx, run.ID, "worker", ic.Observation{Complete: true, Output: output}, now.Add(time.Second))
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
				record, err := db.GetIntelligenceCheckRun(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				return record
			}
			assertState := func(paused, enabled bool, wantGroups ...int64) {
				t.Helper()
				groups, err := db.GetAccountGroupIDs(ctx, id)
				if err != nil || !slices.Equal(groups, wantGroups) {
					t.Fatalf("groups = %v, want %v, error %v", groups, wantGroups, err)
				}
				accounts, _, err := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{AccountID: id})
				if err != nil || len(accounts) != 1 || accounts[0].QualityPaused != paused || accounts[0].ManualEnabled != enabled {
					t.Fatalf("state after action = %+v, %v", accounts, err)
				}
			}
			if r := check("wrong"); r.Action != ic.ActionPauseAndMove || r.RuleID != "isolating" {
				t.Fatal(r)
			}
			assertState(true, true, isolation)
			switch scenario {
			case "removed_rule":
				if r := check("21"); r.RuleID != "isolating" || r.Action != "" {
					t.Fatal(r)
				}
				settings, err = db.GetIntelligenceCheckSettings(ctx)
				if err != nil {
					t.Fatal(err)
				}
				settings.Rules = []ic.Rule{}
				// An older client can edit unrelated settings without reopening global detection.
				settings.RulesOnly, settings.RulesOnlySupplied = false, false
				settings, err = db.SaveIntelligenceCheckSettings(ctx, settings)
				if err != nil || !settings.RulesOnly {
					t.Fatal(settings.RulesOnly, err)
				}
				if r := check("21"); r.Action != "" {
					t.Fatal(r)
				}
				assertState(true, true, isolation)
				if r := check("21"); r.Action != ic.ActionResumeAndRestore {
					t.Fatal(r)
				}
				assertState(false, true, original, other)
				outside, err := db.ActiveIntelligenceCheckAccounts(ctx, []int64{unmatched})
				if err != nil || outside[unmatched] {
					t.Fatal(outside, err)
				}
				if n, err := db.EnqueueDueIntelligenceChecks(ctx, now.Add(time.Minute), 10); n != 0 || err != nil {
					t.Fatal(n, err)
				}
			case "manual_group":
				if err := db.SetAccountGroups(ctx, id, []int64{manual}); err != nil {
					t.Fatal(err)
				}
				if r := check("21"); r.Action != "" {
					t.Fatal(r)
				}
				assertState(true, true, manual)
				if r := check("21"); r.Action != "resume" {
					t.Fatal(r)
				}
				assertState(false, true, manual)
			case "manual_disable":
				if err := db.SetAccountEnabled(ctx, id, false); err != nil {
					t.Fatal(err)
				}
				if n, err := db.EnqueueDueIntelligenceChecks(ctx, now.Add(time.Minute), 10); n != 0 || err != nil {
					t.Fatal(n, err)
				}
				intelligenceRun(t, db, id, now.Add(time.Minute), "21")
				assertState(false, false, isolation)
			case "missing_origins":
				for _, group := range []int64{original, other} {
					if err := db.DeleteAccountGroup(ctx, group); err != nil {
						t.Fatal(err)
					}
				}
				check("21")
				if r := check("21"); r.Action != "" || r.ActionMessage == "" {
					t.Fatal(r)
				}
				assertState(true, true, isolation)
			}
		})
	}
}

func TestIntelligenceCheckNarrowScopeClearsQueuedFallback(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	freeRunning, freeQueued, team, manual := intelligenceAccount(t, db), intelligenceAccount(t, db), intelligenceAccount(t, db), intelligenceAccount(t, db)
	group, err := db.CreateAccountGroup(ctx, "team", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountGroups(ctx, team, []int64{group}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueIntelligenceChecks(ctx, []int64{manual}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	s, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Enabled = true
	s.Rules = []ic.Rule{{ID: "team", Name: "Team only", Channel: "codex", GroupID: group, Automatic: true}}
	s, err = db.SaveIntelligenceCheckSettings(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 10); err != nil || n != 3 {
		t.Fatal(n, err)
	}
	if r, err := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute); err != nil || r == nil || r.AccountID != freeRunning {
		t.Fatal(r, err)
	}
	s.RulesOnly = true
	if _, err := db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{freeRunning: "running", freeQueued: "interrupted", team: "queued", manual: "queued"} {
		runs, total, err := db.ListIntelligenceCheckRuns(ctx, IntelligenceCheckRunFilter{AccountID: id})
		if err != nil || total != 1 || len(runs) != 1 || runs[0].Status != want {
			t.Fatal(id, runs, total, err)
		}
		if id == freeQueued && (!runs[0].Interrupted || runs[0].Error == "" || runs[0].StartedAt != nil) {
			t.Fatal(runs[0])
		}
	}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now.Add(time.Minute), 10); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if r, err := db.ClaimIntelligenceCheck(ctx, "worker2", now.Add(time.Second), time.Minute); err != nil || r == nil || r.AccountID != team || r.RuleID != "team" {
		t.Fatal(r, err)
	}
}

func TestIntelligenceCheckNormalOnlyRule(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	bad, unauthorized, healthy := intelligenceAccount(t, db), intelligenceAccount(t, db), intelligenceAccount(t, db)
	group, err := db.CreateAccountGroup(ctx, "team", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{bad, unauthorized, healthy} {
		if err := db.SetAccountGroups(ctx, id, []int64{group}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET status='error' WHERE id=$1`, bad); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCooldown(ctx, unauthorized, "unauthorized", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Enabled = true
	s.Rules = []ic.Rule{{ID: "team", Name: "Team", Channel: "codex", GroupID: group, Automatic: true, Action: "pause", DegradeThreshold: 1}}
	s, err = db.SaveIntelligenceCheckSettings(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 2); n != 2 || err != nil {
		t.Fatal(n, err)
	}
	s.Rules[0].NormalOnly = true
	if _, err = db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{bad, unauthorized} {
		runs, _, err := db.ListIntelligenceCheckRuns(ctx, IntelligenceCheckRunFilter{AccountID: id})
		if err != nil || len(runs) != 1 || runs[0].Status != "interrupted" || runs[0].StartedAt != nil {
			t.Fatal(runs, err)
		}
	}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 1); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	run, err := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if err != nil || run == nil || run.AccountID != healthy || !run.RequireNormal {
		t.Fatal(run, err)
	}
	if ok, err := db.FinishIntelligenceCheck(ctx, run.ID, "worker", ic.Observation{Complete: true, Output: "wrong"}, now.Add(time.Second)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET status='error' WHERE id=$1`, healthy); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 1); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	run, err = db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if err != nil || run == nil || run.AccountID != healthy || run.RequireNormal {
		t.Fatal(run, err)
	}
	if _, err := db.EnqueueIntelligenceChecks(ctx, []int64{bad}, now); err != nil {
		t.Fatal(err)
	}
	manual, err := db.ClaimIntelligenceCheck(ctx, "manual", now, time.Minute)
	if err != nil || manual == nil || manual.AccountID != bad || manual.RequireNormal {
		t.Fatal(manual, err)
	}
}

func TestIntelligenceCheckPreflightDefersWithoutBlockingGroup(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	blocked, ready := intelligenceAccount(t, db), intelligenceAccount(t, db)
	group, err := db.CreateAccountGroup(ctx, "group", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Enabled, s.RulesOnly = true, true
	s.Rules = []ic.Rule{{ID: "group", Name: "Group", Channel: "codex", GroupID: group, Automatic: true, NormalOnly: true}}
	if _, err := db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	preflight := IntelligenceCheckPreflight{IsAvailable: func(id int64) bool { return id != blocked }}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 1, preflight); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if err := db.BatchSetAccountGroups(ctx, []int64{blocked, ready}, []int64{group}); err != nil {
		t.Fatal(err)
	}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 1, preflight); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	state := intelligenceHoldState(t, db, blocked)
	if state.NextDueAt == nil || !state.NextDueAt.Equal(now.Add(15*time.Second)) || state.Status != "unknown" {
		t.Fatalf("unavailable account was not deferred: %+v", state)
	}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 1, preflight); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if run, err := db.ClaimIntelligenceCheck(ctx, "ready", now, time.Minute); err != nil || run == nil || run.AccountID != ready || !run.RequireNormal {
		t.Fatal(run, err)
	}
	preflight.IsAvailable = func(int64) bool { return true }
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, *state.NextDueAt, 1, preflight); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if run, err := db.ClaimIntelligenceCheck(ctx, "recovered", *state.NextDueAt, time.Minute); err != nil || run == nil || run.AccountID != blocked {
		t.Fatal(run, err)
	}
}
