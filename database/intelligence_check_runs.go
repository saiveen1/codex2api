package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	ic "github.com/codex2api/internal/intelligencecheck"
	"math/rand/v2"
	"strings"
	"time"
)

const intelligenceRunColumns = `id,account_id,status,run_json,lease_owner,lease_until_ms`

func scanIntelligenceRun(row interface{ Scan(...interface{}) error }) (IntelligenceCheckRun, error) {
	var r IntelligenceCheckRun
	var raw, status, owner string
	var id, account, until int64
	err := row.Scan(&id, &account, &status, &raw, &owner, &until)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &r)
	}
	r.ID = id
	r.AccountID = account
	r.Status = status
	r.LeaseOwner = owner
	r.LeaseUntil = until
	return r, err
}
func (db *DB) intelligenceRunTx(ctx context.Context, tx *sql.Tx, id int64) (IntelligenceCheckRun, error) {
	q := `SELECT ` + intelligenceRunColumns + ` FROM intelligence_check_runs WHERE id=$1`
	if !db.isSQLite() {
		q += ` FOR UPDATE`
	}
	return scanIntelligenceRun(tx.QueryRowContext(ctx, q, id))
}
func (db *DB) GetIntelligenceCheckRun(ctx context.Context, id int64) (IntelligenceCheckRun, error) {
	return scanIntelligenceRun(db.conn.QueryRowContext(ctx, `SELECT `+intelligenceRunColumns+` FROM intelligence_check_runs WHERE id=$1`, id))
}

func (db *DB) ActiveIntelligenceCheckAccounts(ctx context.Context, ids []int64) (map[int64]bool, error) {
	active := make(map[int64]bool)
	if len(ids) == 0 {
		return active, nil
	}
	ph := strings.Join(dbPlaceholders(db.isSQLite(), 1, len(ids)), ",")
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id FROM intelligence_check_runs WHERE account_id IN (`+ph+`) AND status IN ('queued','running')`, argsFromInt64s(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		active[id] = true
	}
	return active, rows.Err()
}

var ErrIntelligenceCheckNoEnabledQuestion = errors.New("no enabled question")
var ErrIntelligenceCheckTrialActive = errors.New("该账号已有只记录试跑正在等待或执行")

// intelligenceCheckSnapshot freezes the source-specific model, question and policy for a run.
func intelligenceCheckSnapshot(s ic.Settings, a IntelligenceCheckAccount, source string, created time.Time) (IntelligenceCheckRun, error) {
	// Remember the last stable question ID, rather than indexing the enabled
	// subset: disabling an earlier question must not shift the next question.
	start := 0
	foundLast := false
	for i, q := range s.Questions {
		if q.ID == a.LastQuestionID {
			start = (i + 1) % len(s.Questions)
			foundLast = true
			break
		}
	}
	if !foundLast {
		for i, q := range s.Questions {
			if q.ID == a.NextQuestionID {
				start = i
				break
			}
		}
	}

	var question ic.Question
	found := false
	for i := 0; i < len(s.Questions); i++ {
		candidate := s.Questions[(start+i)%len(s.Questions)]
		if candidate.Enabled {
			question = candidate
			found = true
			break
		}
	}
	if !found {
		return IntelligenceCheckRun{}, ErrIntelligenceCheckNoEnabledQuestion
	}
	model, effort := s.ModelForChannel(a.Channel)
	snapshot := s
	snapshot.Questions = nil
	r := IntelligenceCheckRun{CredentialGeneration: a.CredentialGeneration, AccountID: a.AccountID, AccountName: a.AccountName, Channel: a.Channel, Source: source, Status: "queued", ConfigRevision: s.Revision, ManualRevision: a.ManualRevision, Model: model, ReasoningEffort: effort, JudgeMode: s.JudgeMode, Question: question, Settings: snapshot, CreatedAt: created.UTC()}
	r.IntelligenceCheckScope = a.IntelligenceCheckScope
	return r, nil
}

func (db *DB) intelligenceEnqueueTx(ctx context.Context, tx *sql.Tx, s ic.Settings, a IntelligenceCheckAccount, source string, recordOnly bool, now time.Time) (int64, error) {
	if !recordOnly {
		if err := validateIntelligenceAccountSettings(ctx, tx, s, a.Channel, a.OwnedAction); err != nil {
			return 0, err
		}
	}
	existing, err := scanIntelligenceRun(tx.QueryRowContext(ctx, `SELECT `+intelligenceRunColumns+` FROM intelligence_check_runs WHERE account_id=$1 AND status IN ('queued','running')`, a.AccountID))
	if err == nil {
		if recordOnly {
			return 0, nil
		}
		if existing.RecordOnly {
			return 0, ErrIntelligenceCheckTrialActive
		}
		if source == "manual" && existing.Source == "automatic" && existing.Status == "queued" {
			existing.Source = "manual"
			raw, _ := json.Marshal(existing)
			if _, err = tx.ExecContext(ctx, `UPDATE intelligence_check_runs SET run_json=$1 WHERE id=$2`, string(raw), existing.ID); err != nil {
				return 0, err
			}
		}
		return existing.ID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	r, err := intelligenceCheckSnapshot(s, a, source, now)
	if err != nil {
		return 0, err
	}
	r.RecordOnly = recordOnly
	raw, err := json.Marshal(r)
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO intelligence_check_runs(account_id,status,run_json,created_ms,ready_ms,record_only) VALUES($1,'queued',$2,$3,$3,$4) RETURNING id`, a.AccountID, string(raw), now.UnixMilli(), recordOnly).Scan(&id)
	return id, err
}
func (db *DB) EnqueueIntelligenceChecks(ctx context.Context, ids []int64, now time.Time) ([]int64, error) {
	out, _, err := db.SubmitIntelligenceChecks(ctx, ids, now, false)
	return out, err
}

func (db *DB) SubmitIntelligenceChecks(ctx context.Context, ids []int64, now time.Time, recordOnly bool) ([]int64, []int64, error) {
	skipped := []int64{}
	out := []int64{}
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		s, err := db.intelligenceSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, id := range normalizeIDSlice(ids) {
			a, err := db.intelligenceAccountTx(ctx, tx, id)
			if err != nil {
				return err
			}
			policy, rule := a.resolve(s)
			a.RuleID, a.RuleName = rule.ID, rule.Name
			runID, err := db.intelligenceEnqueueTx(ctx, tx, policy, a, "manual", recordOnly, now)
			if err != nil {
				return err
			}
			if runID == 0 {
				skipped = append(skipped, id)
				continue
			}
			out = append(out, runID)
		}
		return nil
	})
	return out, skipped, err
}

// IntelligenceCheckHold reports whether an account's automatic check should
// wait because the upstream would reject it for rate-limit or quota reasons.
// until is the expected recovery time, zero when unknown.
type IntelligenceCheckHold func(accountID int64, model string) (until time.Time, held bool)

type intelligenceQueryer interface {
	intelligenceQueryRower
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

// intelligenceDueAccounts returns a bounded, ordered window of due accounts.
// intelligenceDueAccounts lists accounts whose automatic check is due, with
// the source that selects the detector model.
func (db *DB) intelligenceDueAccounts(ctx context.Context, q intelligenceQueryer, s ic.Settings, now time.Time, limit int) ([]IntelligenceCheckAccount, error) {
	filter := ""
	args := []interface{}{now.UnixMilli(), limit}
	if ic.MovesGroups(s.Action) && len(s.Rules) == 0 && !s.RulesOnly {
		channel, e := intelligenceTargetGroupChannel(ctx, q, s)
		owned := "COALESCE(" + db.intelligenceStateActionExpr("s") + ",'')<>''"
		if e != nil {
			if !errors.Is(e, ErrIntelligenceCheckTargetGroup) {
				return nil, e
			}
			filter = " AND (" + owned + ")"
		} else {
			filter = " AND (" + db.intelligenceChannelExpr() + "=$3 OR " + owned + ")"
			args = append(args, channel)
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT `+db.intelligenceAccountSelect()+` FROM accounts a JOIN intelligence_check_accounts s ON s.account_id=a.id WHERE `+db.intelligenceEligibleSQL()+filter+` AND `+db.intelligenceAutomaticSQL("a", "s")+` AND s.next_due_ms<=$1 AND NOT EXISTS(SELECT 1 FROM intelligence_check_runs r WHERE r.account_id=a.id AND r.status IN ('queued','running')) ORDER BY s.next_due_ms,a.id LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	due := []IntelligenceCheckAccount{}
	for rows.Next() {
		a, err := scanIntelligenceAccount(rows)
		if err != nil {
			return nil, err
		}
		due = append(due, a)
	}
	return due, rows.Err()
}

// intelligenceHoldTx postpones an automatic check without grading it: streaks,
// retry backoff and the latest status are untouched, so a quota wait is
// neither a pass nor a failure. The account is examined again at the expected
// recovery time, or after one regular interval when that is unknown or later,
// so an early reset is noticed.
func (db *DB) intelligenceHoldTx(ctx context.Context, tx *sql.Tx, s ic.Settings, a IntelligenceCheckAccount, until, now time.Time) error {
	delay := time.Duration(s.IntervalSeconds) * time.Second
	if wait := until.Sub(now); !until.IsZero() && wait < delay {
		delay = max(wait, time.Second)
	}
	state := a.State
	state.ProbePhase = ic.PhaseQuotaWait
	state.NextIntervalSeconds = int((delay + time.Second - 1) / time.Second)
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE intelligence_check_accounts SET state_json=$1,next_due_ms=$2 WHERE account_id=$3`, string(raw), now.Add(delay).UnixMilli(), a.AccountID)
	return err
}

// EnqueueDueIntelligenceChecks queues due automatic checks. With hold, accounts
// it reports as rate limited or out of quota are postponed instead of queued.
func (db *DB) EnqueueDueIntelligenceChecks(ctx context.Context, now time.Time, limit int, hold ...IntelligenceCheckHold) (int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// hold reads runtime account state, so it runs before the write transaction
	// rather than under database locks. An account that becomes due in between
	// is simply queued; the runner checks again before sending.
	type holdResult struct {
		until time.Time
		model string
	}
	held := map[int64]holdResult{}
	if len(hold) > 0 && hold[0] != nil {
		s, err := db.GetIntelligenceCheckSettings(ctx)
		if err != nil {
			return 0, err
		}
		if !s.Enabled {
			return 0, nil
		}
		due, err := db.intelligenceDueAccounts(ctx, db.conn, s, now, limit)
		if err != nil {
			return 0, err
		}
		for _, a := range due {
			policy, _ := a.resolve(s)
			model, _ := policy.ModelForChannel(a.Channel)
			if until, ok := hold[0](a.AccountID, model); ok {
				held[a.AccountID] = holdResult{until, model}
			}
		}
	}
	count := 0
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		s, err := db.intelligenceSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		if !s.Enabled {
			return nil
		}
		due, err := db.intelligenceDueAccounts(ctx, tx, s, now, limit)
		if err != nil {
			return err
		}
		for _, a := range due {
			policy, rule := a.resolve(s)
			a.RuleID, a.RuleName = rule.ID, rule.Name
			model, _ := policy.ModelForChannel(a.Channel)
			if err = validateIntelligenceAccountSettings(ctx, tx, policy, a.Channel, a.OwnedAction); err != nil {
				if !errors.Is(err, ErrIntelligenceCheckTargetGroup) {
					return err
				}
				// A removed target must not monopolize the bounded due window.
				if _, err = tx.ExecContext(ctx, `UPDATE intelligence_check_accounts SET next_due_ms=$1 WHERE account_id=$2`, now.Add(time.Duration(policy.IntervalSeconds)*time.Second).UnixMilli(), a.AccountID); err != nil {
					return err
				}
				continue
			}
			if h, ok := held[a.AccountID]; ok && h.model == model {
				if err = db.intelligenceHoldTx(ctx, tx, policy, a, h.until, now); err != nil {
					return err
				}
				continue
			}
			if _, err = db.intelligenceEnqueueTx(ctx, tx, policy, a, "automatic", false, now); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

// ClaimIntelligenceCheck leases eligible work and refreshes its snapshot against current policy.
func (db *DB) ClaimIntelligenceCheck(ctx context.Context, owner string, now time.Time, lease time.Duration) (*IntelligenceCheckRun, error) {
	if owner == "" || lease <= 0 {
		return nil, errors.New("nonempty owner and positive lease are required")
	}
	var claimed *IntelligenceCheckRun
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		s, err := db.intelligenceSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		var active int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM intelligence_check_runs WHERE status='running'`).Scan(&active); err != nil {
			return err
		}
		if active >= s.Concurrency {
			return nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+intelligenceRunColumns+` FROM intelligence_check_runs WHERE status='queued' AND ready_ms<=$1 AND (`+db.intelligenceJSONField("source")+`='manual' OR ($2 AND account_id IN (SELECT s.account_id FROM intelligence_check_accounts s JOIN accounts a ON a.id=s.account_id WHERE `+db.intelligenceAutomaticSQL("a", "s")+`))) ORDER BY ready_ms,id LIMIT 100`, now.UnixMilli(), s.Enabled)
		if err != nil {
			return err
		}
		candidates := []IntelligenceCheckRun{}
		for rows.Next() {
			r, e := scanIntelligenceRun(rows)
			if e != nil {
				rows.Close()
				return e
			}
			candidates = append(candidates, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, r := range candidates {
			a, e := db.intelligenceAccountTx(ctx, tx, r.AccountID)
			if e != nil {
				if errors.Is(e, ErrIntelligenceCheckUnsupportedAccount) {
					r.Observation = ic.Observation{Interrupted: true, Error: "account is no longer eligible"}
					if e = db.intelligenceTerminalTx(ctx, tx, &r, now, "interrupted"); e != nil {
						return e
					}
					continue
				}
				return e
			}
			policy, rule := a.resolve(s)
			a.RuleID, a.RuleName = rule.ID, rule.Name
			if r.Source == "automatic" && (!s.Enabled || !a.Enabled || !a.ManualEnabled || !s.ChannelEnabled(a.Channel) || !intelligenceRuleAllowsAutomatic(s, a, rule)) {
				continue
			}
			fresh, snapshotErr := intelligenceCheckSnapshot(policy, a, r.Source, r.CreatedAt)
			if snapshotErr == nil {
				fresh.ID, fresh.RecordOnly = r.ID, r.RecordOnly
				fresh.RequireNormal = r.Source == "automatic" && rule.NormalOnly && a.OwnedAction == ""
				r = fresh
				if !r.RecordOnly {
					snapshotErr = validateIntelligenceAccountSettings(ctx, tx, policy, a.Channel, a.OwnedAction)
				}
			}
			if e = snapshotErr; e != nil {
				if !errors.Is(e, ErrIntelligenceCheckTargetGroup) && !errors.Is(e, ErrIntelligenceCheckNoEnabledQuestion) {
					return e
				}
				r.Observation = ic.Observation{Interrupted: true, Error: "configuration error: " + e.Error()}
				if e = db.intelligenceTerminalTx(ctx, tx, &r, now, "interrupted"); e != nil {
					return e
				}
				if e = db.intelligenceRetainTx(ctx, tx, r, s.Retention); e != nil {
					return e
				}
				continue
			}
			if minimum := time.Duration(r.Settings.TimeoutSeconds+30) * time.Second; lease < minimum {
				lease = minimum
			}
			started := now.UTC()
			r.StartedAt = &started
			r.Status = "running"
			r.LeaseOwner = owner
			r.LeaseUntil = now.Add(lease).UnixMilli()
			raw, _ := json.Marshal(r)
			if _, err = tx.ExecContext(ctx, `UPDATE intelligence_check_runs SET status='running',run_json=$1,lease_owner=$2,lease_until_ms=$3 WHERE id=$4 AND status='queued'`, string(raw), owner, r.LeaseUntil, r.ID); err != nil {
				return err
			}
			claimed = &r
			break
		}
		return nil
	})
	return claimed, err
}

// Requeue is reserved for local account-capacity contention before any upstream
// request was sent. The short delay prevents a busy head account starving peers.
func (db *DB) RequeueIntelligenceCheck(ctx context.Context, id int64, owner string, now time.Time) error {
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := db.intelligenceSettingsTx(ctx, tx); err != nil {
			return err
		}
		r, err := db.intelligenceRunTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.Status != "running" || r.LeaseOwner != owner || r.LeaseUntil <= now.UnixMilli() {
			return nil
		}
		r.Status = "queued"
		r.StartedAt = nil
		raw, _ := json.Marshal(r)
		_, err = tx.ExecContext(ctx, `UPDATE intelligence_check_runs SET status='queued',run_json=$1,lease_owner='',lease_until_ms=0,ready_ms=$2 WHERE id=$3`, string(raw), now.Add(5*time.Second).UnixMilli(), id)
		return err
	})
}

// Save holds the settings lock here, so a queued run cannot be claimed or
// promoted to manual while we close it. Preserve historical and live runs.
func (db *DB) interruptIntelligenceOutsideScopeTx(ctx context.Context, tx *sql.Tx, retention int, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+intelligenceRunColumns+` FROM intelligence_check_runs WHERE status='queued' AND `+db.intelligenceJSONField("source")+`='automatic' AND account_id NOT IN (
		SELECT st.account_id FROM intelligence_check_accounts st JOIN accounts a ON a.id=st.account_id CROSS JOIN intelligence_check_settings cfg WHERE cfg.id=1 AND `+db.intelligenceRuleAutomaticSQL("a", "st", "cfg")+`)`)
	if err != nil {
		return err
	}
	var runs []IntelligenceCheckRun
	for rows.Next() {
		r, err := scanIntelligenceRun(rows)
		if err != nil {
			rows.Close()
			return err
		}
		runs = append(runs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range runs {
		r.Observation = ic.Observation{Interrupted: true, Error: "账号不在当前自动检测范围，未发送请求"}
		if err := db.intelligenceTerminalTx(ctx, tx, &r, now, "interrupted"); err != nil {
			return err
		}
		if err := db.intelligenceRetainTx(ctx, tx, r, retention); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) intelligenceTerminalTx(ctx context.Context, tx *sql.Tx, r *IntelligenceCheckRun, now time.Time, status string) error {
	r.Status = status
	completed := now.UTC()
	r.CompletedAt = &completed
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE intelligence_check_runs SET status=$1,run_json=$2,completed_ms=$3,lease_owner='',lease_until_ms=0,question_name=$5,duration_ms=$6,summary_version=1 WHERE id=$4`, status, string(raw), now.UnixMilli(), r.ID, r.Question.Name, r.DurationMS)
	return err
}
func (db *DB) intelligenceRetainTx(ctx context.Context, tx *sql.Tx, r IntelligenceCheckRun, retention int) error {
	const eligible = `account_id=$1 AND record_only=$3 AND status NOT IN ('queued','running')`
	_, err := tx.ExecContext(ctx, `DELETE FROM intelligence_check_runs WHERE `+eligible+` AND id NOT IN (SELECT id FROM intelligence_check_runs WHERE `+eligible+` ORDER BY id DESC LIMIT $2)`, r.AccountID, retention, r.RecordOnly)
	return err
}

// FinishIntelligenceCheck commits a fenced result and its scheduling or credential-block state.
func (db *DB) FinishIntelligenceCheck(ctx context.Context, id int64, owner string, o ic.Observation, now time.Time) (bool, error) {
	committed := false
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		s, err := db.intelligenceSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		r, err := db.intelligenceRunTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.Status != "running" || r.LeaseOwner != owner || r.LeaseUntil <= now.UnixMilli() {
			return nil
		}
		r.Observation = o
		grade := ic.Grade(r.Question, r.JudgeMode, o)
		r.AnswerPassed = grade.AnswerPassed
		r.LatencyPassed = grade.LatencyPassed
		r.Error = grade.Error
		if !r.RecordOnly {
			a, err := db.intelligenceAccountTx(ctx, tx, r.AccountID)
			if err != nil && !errors.Is(err, ErrIntelligenceCheckUnsupportedAccount) {
				return err
			}
			policy, rule := a.resolve(s)
			if err == nil && intelligenceRunMayUpdate(r, a, policy, rule) {
				s := policy
				prior := a.State
				if prior.PolicyRuleID != r.RuleID {
					prior.PolicyRuleID = r.RuleID
					prior.DegradeStreak, prior.RecoverStreak, prior.ErrorRetrySeconds = 0, 0, 0
				}
				next, action := ic.Transition(s, prior, grade.Status)
				if grade.Status != "interrupted" {
					next.LastQuestionID = r.Question.ID
					for i, q := range s.Questions {
						if q.ID == r.Question.ID {
							next.NextQuestionID = s.Questions[(i+1)%len(s.Questions)].ID
							break
						}
					}
				}
				if err = db.intelligenceApplyActionTx(ctx, tx, &a, &next, &r, action, s); err != nil {
					return err
				}
				next, delay := ic.Schedule(s, prior, next, grade.Status)
				if next.BlockedCredentialGeneration != nil && *next.BlockedCredentialGeneration != a.CredentialGeneration {
					next.BlockedCredentialGeneration = nil
				}
				if o.CredentialRejected && (s.AdaptiveEnabled || a.Channel == "antigravity") {
					generation := a.CredentialGeneration
					next.BlockedCredentialGeneration = &generation
				} else if grade.Status == "normal" {
					next.BlockedCredentialGeneration = nil
				}
				raw, _ := json.Marshal(next)
				cursor := a.QuestionCursor
				if grade.Status != "interrupted" {
					cursor++
				}
				if _, err = tx.ExecContext(ctx, `UPDATE intelligence_check_accounts SET state_json=$1,quality_paused=$2,next_due_ms=$3,question_cursor=$4,status=$5 WHERE account_id=$6`, string(raw), a.QualityPaused, intelligenceNextDue(s, now, delay), cursor, grade.Status, r.AccountID); err != nil {
					return err
				}
			} else {
				r.ActionMessage = "state changed after enqueue; historical result only"
			}
		}
		if err = db.intelligenceTerminalTx(ctx, tx, &r, now, grade.Status); err != nil {
			return err
		}
		if err = db.intelligenceRetainTx(ctx, tx, r, s.Retention); err != nil {
			return err
		}
		committed = true
		return nil
	})
	return committed, err
}

func intelligenceRunMayUpdate(r IntelligenceCheckRun, a IntelligenceCheckAccount, s ic.Settings, rule ic.Rule) bool {
	return r.ConfigRevision == s.Revision && r.ManualRevision == a.ManualRevision && r.CredentialGeneration == a.CredentialGeneration &&
		(r.Groups == nil || r.Plan == a.Plan && r.RuleID == rule.ID)
}
func intelligenceGroupIDsTx(ctx context.Context, tx *sql.Tx, id int64) ([]int64, map[int64]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT g.id,COALESCE(g.channel,'codex') FROM account_group_members m JOIN account_groups g ON g.id=m.group_id WHERE m.account_id=$1 ORDER BY g.id`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	ids := []int64{}
	channels := map[int64]string{}
	for rows.Next() {
		var id int64
		var channel string
		if err = rows.Scan(&id, &channel); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		channels[id] = strings.ToLower(strings.TrimSpace(channel))
	}
	return ids, channels, rows.Err()
}

func (db *DB) intelligenceApplyActionTx(ctx context.Context, tx *sql.Tx, a *IntelligenceCheckAccount, next *ic.State, r *IntelligenceCheckRun, action string, s ic.Settings) error {
	switch action {
	case "":
		return nil
	case "pause":
		a.QualityPaused = true
	case "resume":
		a.QualityPaused = false
	case "switch_group", ic.ActionPauseAndMove:
		var channel string
		err := tx.QueryRowContext(ctx, `SELECT channel FROM account_groups WHERE id=$1`, s.TargetGroupID).Scan(&channel)
		if errors.Is(err, sql.ErrNoRows) || err == nil && NormalizeAccountGroupChannel(channel) != a.Channel {
			next.OwnedAction, next.OwnedRecoverThreshold = "", 0
			r.ActionMessage = "target group missing or belongs to another channel"
			return nil
		}
		if err != nil {
			return err
		}
		groups, channels, err := intelligenceGroupIDsTx(ctx, tx, a.AccountID)
		if err != nil {
			return err
		}
		next.OriginalGroupIDs = groups
		next.OriginalGroupChannels = channels
		if err = db.batchReplaceAccountGroups(ctx, tx, []int64{a.AccountID}, []int64{s.TargetGroupID}); err != nil {
			return err
		}
		if action == ic.ActionPauseAndMove {
			a.QualityPaused = true
		}
	case "restore_groups", ic.ActionResumeAndRestore:
		surviving := []int64{}
		missing := []int64{}
		incompatible := []int64{}
		for _, gid := range a.OriginalGroupIDs {
			var channel string
			err := tx.QueryRowContext(ctx, `SELECT COALESCE(channel,'codex') FROM account_groups WHERE id=$1`, gid).Scan(&channel)
			if errors.Is(err, sql.ErrNoRows) {
				missing = append(missing, gid)
				continue
			}
			if err != nil {
				return err
			}
			if original, recorded := a.OriginalGroupChannels[gid]; recorded && strings.ToLower(strings.TrimSpace(channel)) != original {
				incompatible = append(incompatible, gid)
				continue
			}
			surviving = append(surviving, gid)
		}
		if action == ic.ActionResumeAndRestore && len(a.OriginalGroupIDs) > 0 && len(surviving) == 0 {
			// Never return a previously grouped account to the ungrouped business pool.
			next.OwnedAction, next.OwnedRecoverThreshold = a.OwnedAction, a.OwnedRecoverThreshold
			next.RecoverStreak = 0
			r.ActionMessage = "原分组均已删除或不兼容；保留业务禁用，请手动指定分组"
			return nil
		}
		if err := db.batchReplaceAccountGroups(ctx, tx, []int64{a.AccountID}, surviving); err != nil {
			return err
		}
		next.OriginalGroupIDs = nil
		next.OriginalGroupChannels = nil
		if action == ic.ActionResumeAndRestore {
			a.QualityPaused = false
		}
		if len(missing) > 0 || len(incompatible) > 0 {
			r.ActionMessage = fmt.Sprintf("original groups omitted: deleted=%v incompatible=%v", missing, incompatible)
		}

	}
	r.Action = action
	return insertSchedulerOutboxEventTx(ctx, tx, SchedulerEntityAccount, a.AccountID, "upsert")
}
func (db *DB) ExpireIntelligenceCheckLeases(ctx context.Context, now time.Time) (int, error) {
	count := 0
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		s, err := db.intelligenceSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+intelligenceRunColumns+` FROM intelligence_check_runs WHERE status='running' AND lease_until_ms<=$1 ORDER BY id LIMIT 500`, now.UnixMilli())
		if err != nil {
			return err
		}
		expired := []IntelligenceCheckRun{}
		for rows.Next() {
			r, e := scanIntelligenceRun(rows)
			if e != nil {
				rows.Close()
				return e
			}
			expired = append(expired, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, r := range expired {
			r.Observation.Interrupted = true
			r.Error = "execution lease expired; request was not replayed"
			if err = db.intelligenceTerminalTx(ctx, tx, &r, now, "interrupted"); err != nil {
				return err
			}
			if !r.RecordOnly && r.ConfigRevision == s.Revision {
				a, e := db.intelligenceAccountTx(ctx, tx, r.AccountID)
				if e != nil && !errors.Is(e, ErrIntelligenceCheckUnsupportedAccount) {
					return e
				}
				policy, rule := a.resolve(s)
				if e == nil && intelligenceRunMayUpdate(r, a, policy, rule) {
					s := policy
					next, _ := ic.Transition(s, a.State, "interrupted")
					next, delay := ic.Schedule(s, a.State, next, "interrupted")
					raw, _ := json.Marshal(next)
					if _, err = tx.ExecContext(ctx, `UPDATE intelligence_check_accounts SET next_due_ms=$1,status='interrupted',state_json=$2 WHERE account_id=$3`, intelligenceNextDue(s, now, delay), string(raw), r.AccountID); err != nil {
						return err
					}
				}
			}
			if err = db.intelligenceRetainTx(ctx, tx, r, s.Retention); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}
func (db *DB) intelligenceJSONField(field string) string {
	if db.isSQLite() {
		return "json_extract(run_json,'$." + field + "')"
	}
	return "(run_json::jsonb->>'" + field + "')"
}
func (db *DB) ListIntelligenceCheckRuns(ctx context.Context, f IntelligenceCheckRunFilter) ([]IntelligenceCheckRun, int, error) {
	where := "TRUE"
	args := []interface{}{}
	add := func(expr string, val interface{}) {
		args = append(args, val)
		where += fmt.Sprintf(" AND "+expr, "$"+fmt.Sprint(len(args)))
	}
	if f.AccountID > 0 {
		add(`account_id=%s`, f.AccountID)
	}
	if f.Search != "" {
		add(`LOWER(`+db.intelligenceJSONField("account_name")+`) LIKE %s`, "%"+strings.ToLower(f.Search)+"%")
	}
	if f.Channel != "" {
		add(db.intelligenceJSONField("channel")+`=%s`, f.Channel)
	}
	if f.Status != "" {
		add(`status=%s`, f.Status)
	}
	if f.GroupID > 0 {
		add(db.intelligenceHistoryGroupSQL(), f.GroupID)
	}
	if f.Plan != "" {
		add(db.intelligenceJSONField("plan")+`=%s`, ic.NormalizePlan(f.Plan))
	}
	if f.Source != "" {
		add(db.intelligenceJSONField("source")+`=%s`, f.Source)
	}
	if !f.From.IsZero() {
		add(`created_ms>=%s`, f.From.UnixMilli())
	}
	if !f.Before.IsZero() {
		add(`created_ms<%s`, f.Before.UnixMilli())
	}
	if f.RecordOnly != nil {
		add(`record_only=%s`, *f.RecordOnly)
	}
	var total int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM intelligence_check_runs WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	offset, limit := intelligencePage(f.Offset, f.Limit)
	args = append(args, limit, offset)
	columns := intelligenceRunColumns
	if f.Summary {
		summary := `(run_json::jsonb - ARRAY['output','settings','effective_context','error']::text[] #- '{question,prompt}' #- '{question,expected_answer}')::text`
		if db.isSQLite() {
			summary = `json_remove(run_json,'$.output','$.settings','$.effective_context','$.error','$.question.prompt','$.question.expected_answer')`
		}
		columns = strings.Replace(columns, "run_json", summary, 1)
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT `+columns+` FROM intelligence_check_runs WHERE `+where+fmt.Sprintf(` ORDER BY id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []IntelligenceCheckRun{}
	for rows.Next() {
		r, e := scanIntelligenceRun(rows)
		if e != nil {
			return nil, 0, e
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// IntelligenceCheckBars returns bounded history and source-aware automatic scheduling metadata.
func (db *DB) IntelligenceCheckBars(ctx context.Context, ids []int64) (map[int64]IntelligenceCheckBar, error) {
	ids = normalizeIDSlice(ids)
	out := map[int64]IntelligenceCheckBar{}
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > 500 {
		return nil, errors.New("at most 500 accounts per bars request")
	}
	ph := strings.Join(dbPlaceholders(db.isSQLite(), 1, len(ids)), ",")
	args := argsFromInt64s(ids)
	// A disabled automatic queue must not lock the manual action in a spinner.
	// Keep the task queued so a manual submission can promote it, or re-enabling
	// automatic checks can resume it. Running and manual queued tasks stay active.
	automaticEnabled := `(config_json::jsonb->>'enabled')::boolean`
	if db.isSQLite() {
		automaticEnabled = `json_extract(config_json,'$.enabled')`
	}
	active := `r.status='running' OR (r.status='queued' AND (` + db.intelligenceJSONField("source") + `='manual' OR (` + db.intelligenceAutomaticSQL("acc", "a") + ` AND (SELECT ` + automaticEnabled + ` FROM intelligence_check_settings WHERE id=1))))`
	rows, err := db.conn.QueryContext(ctx, `SELECT a.account_id,a.enabled,a.quality_paused,a.state_json,a.next_due_ms,COALESCE(acc.credential_generation,1),(COALESCE(acc.enabled,TRUE) AND (SELECT `+automaticEnabled+` AND `+db.intelligenceSourceEnabledSQL("acc", "cfg")+` AND `+db.intelligenceRuleAutomaticSQL("acc", "a", "cfg")+` FROM intelligence_check_settings cfg WHERE id=1)),COALESCE(r.id,0),COALESCE(r.status,'') FROM intelligence_check_accounts a JOIN accounts acc ON acc.id=a.account_id LEFT JOIN intelligence_check_runs r ON r.account_id=a.account_id AND r.status IN ('queued','running') AND (`+active+`) WHERE a.account_id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var b IntelligenceCheckBar
		var due, generation int64
		var automaticAllowed bool
		var raw string
		if err = rows.Scan(&id, &b.Enabled, &b.QualityPaused, &raw, &due, &generation, &automaticAllowed, &b.ActiveRunID, &b.ActiveStatus); err != nil {
			rows.Close()
			return nil, err
		}
		var s ic.State
		if err = json.Unmarshal([]byte(raw), &s); err != nil {
			rows.Close()
			return nil, err
		}
		b.OwnedAction = s.OwnedAction
		b.ProbePhase, b.NextIntervalSeconds, b.PassedStreak = s.ProbePhase, s.NextIntervalSeconds, s.RecoverStreak
		if due > 0 && automaticAllowed && b.Enabled {
			when := time.UnixMilli(due)
			b.NextDueAt = &when
		}
		// The stored deadline of a quota wait is only the next re-examination,
		// and the wait itself ends with automatic checks on the account.
		if b.ProbePhase == ic.PhaseQuotaWait {
			b.NextDueAt = nil
			if !automaticAllowed || !b.Enabled {
				b.ProbePhase = ""
			}
		}
		if s.BlockedCredentialGeneration != nil && *s.BlockedCredentialGeneration == generation {
			b.ProbePhase = "credential_blocked"
			b.NextDueAt = nil
		}
		b.Records = []IntelligenceCheckRunSummary{}
		out[id] = b
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Each branch performs one covering-index seek and reads at most twenty
	// summaries. Neither the database nor Go needs to parse run_json here.
	branches := make([]string, len(ids))
	for i, placeholder := range dbPlaceholders(db.isSQLite(), 1, len(ids)) {
		branches[i] = `SELECT * FROM (SELECT id,account_id,status,question_name,duration_ms,completed_ms FROM intelligence_check_runs WHERE account_id=` + placeholder + ` AND NOT record_only AND status NOT IN ('queued','running') ORDER BY id DESC LIMIT 20) recent`
	}
	rows, err = db.conn.QueryContext(ctx, `SELECT * FROM (`+strings.Join(branches, " UNION ALL ")+`) summaries ORDER BY account_id,id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID, completedMS int64
		var r IntelligenceCheckRunSummary
		if err = rows.Scan(&r.ID, &accountID, &r.Status, &r.QuestionName, &r.DurationMS, &completedMS); err != nil {
			return nil, err
		}
		if completedMS > 0 {
			at := time.UnixMilli(completedMS).UTC()
			r.CompletedAt = &at
		}
		b := out[accountID]
		b.Records = append(b.Records, r)
		out[accountID] = b
	}
	return out, rows.Err()
}

// Add bounded jitter without exceeding the configured interval or maximum.
// Persist the selected deadline; polling and restarts never redraw it.
func intelligenceNextDue(s ic.Settings, now time.Time, seconds int) int64 {
	delay := time.Duration(seconds) * time.Second
	if s.AdaptiveEnabled {
		delay -= time.Duration(rand.Int64N(max(1, int64(delay/10))))
	}
	return now.Add(delay).UnixMilli()
}
