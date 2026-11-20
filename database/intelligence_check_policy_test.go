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
