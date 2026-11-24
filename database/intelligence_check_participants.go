package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	ic "github.com/codex2api/internal/intelligencecheck"
)

type IntelligenceRuleParticipant struct {
	AccountID        int64                    `json:"account_id"`
	AccountName      string                   `json:"account_name"`
	Groups           []IntelligenceCheckGroup `json:"groups"`
	OriginalGroupIDs []int64                  `json:"original_group_ids,omitempty"`
	Recovery         bool                     `json:"recovery"`
	NextDueAt        *time.Time               `json:"next_due_at,omitempty"`
	State            string                   `json:"state"`
	AvailableAfter   *time.Time               `json:"available_after,omitempty"`
}

// ListIntelligenceRuleParticipants reads the scheduler's saved eligibility scope,
// including future checks and owned recovery, without enqueuing or delaying work.
func (db *DB) ListIntelligenceRuleParticipants(ctx context.Context, id string, offset, limit int, now time.Time, paused bool, preflight IntelligenceCheckPreflight) ([]IntelligenceRuleParticipant, int, error) {
	items := []IntelligenceRuleParticipant{}
	s, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		return nil, 0, err
	}
	var found bool
	for _, rule := range s.Rules {
		if rule.ID == id {
			found = true
			break
		}
	}
	if !found {
		return nil, 0, sql.ErrNoRows
	}
	if !s.Enabled && !paused {
		return items, 0, nil
	}
	rule := db.intelligenceRuleSQL("a", "s", "cfg")
	scope := db.intelligenceAutomaticSQL("a", "s")
	if paused {
		// Keep opted-out accounts reachable even when other scheduling gates are closed.
		scope = `s.enabled=FALSE`
	}
	from := ` FROM accounts a JOIN intelligence_check_accounts s ON s.account_id=a.id CROSS JOIN intelligence_check_settings cfg WHERE cfg.id=1 AND cfg.revision=$2 AND ` + db.intelligenceEligibleSQL() + ` AND ` + scope + ` AND ` + db.intelligenceFieldSQL(rule, "id", "") + `=$1`
	var total int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*)`+from, id, s.Revision).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return items, 0, nil
	}
	offset, limit = intelligencePage(offset, limit)
	activeSQL := `,COALESCE((SELECT r.status FROM intelligence_check_runs r WHERE r.account_id=a.id AND r.status IN ('queued','running') LIMIT 1),'')`
	if paused {
		activeSQL = `,'paused'`
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT `+db.intelligenceAccountSelect()+activeSQL+from+` ORDER BY s.next_due_ms,a.id LIMIT $3 OFFSET $4`, id, s.Revision, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	type candidate struct {
		account IntelligenceCheckAccount
		active  string
	}
	var candidates []candidate
	for rows.Next() {
		var active string
		a, err := scanIntelligenceAccount(rows, &active)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		candidates = append(candidates, candidate{a, active})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var configChecked bool
	var configErr error
	for _, c := range candidates {
		a := c.account
		item := IntelligenceRuleParticipant{AccountID: a.AccountID, AccountName: a.AccountName, Groups: a.Groups, OriginalGroupIDs: a.OriginalGroupIDs, Recovery: a.OwnedAction != "", NextDueAt: a.NextDueAt, State: c.active}
		if paused {
			item.NextDueAt = nil
			items = append(items, item)
			continue
		}
		policy, matched := a.resolve(s)
		if !item.Recovery && !configChecked {
			configChecked = true
			configErr = validateIntelligenceAccountSettings(ctx, db.conn, policy, a.Channel)
			if configErr != nil && !errors.Is(configErr, ErrIntelligenceCheckTargetGroup) {
				return nil, 0, configErr
			}
		}
		if item.State == "" {
			model, _ := policy.ModelForChannel(a.Channel)
			if preflight.MatchedModels != nil {
				models := preflight.MatchedModels(ctx, a.AccountID, model)
				key := ic.ModelSkip{Revision: policy.Revision, Generation: a.CredentialGeneration, RuleID: matched.ID, Channel: a.Channel, Matches: strings.Join(models, ",")}
				if len(models) == 0 || a.ModelSkip != nil && *a.ModelSkip == key {
					item.State = "skipped"
					items = append(items, item)
					continue
				}
				model = models[0]
			}
			item.State, item.AvailableAfter = intelligenceParticipantState(now, a, matched, model, !item.Recovery && configErr != nil, preflight)
		}
		items = append(items, item)
	}
	return items, total, nil
}

func intelligenceParticipantState(now time.Time, a IntelligenceCheckAccount, rule ic.Rule, model string, invalidConfig bool, preflight IntelligenceCheckPreflight) (string, *time.Time) {
	if invalidConfig {
		return "config_error", nil
	}
	if preflight.QuotaHold != nil {
		if until, held := preflight.QuotaHold(a.AccountID, model); held {
			if until.After(now) {
				return "quota_wait", &until
			}
			return "quota_wait", nil
		}
	}
	if preflight.IsAvailable != nil && a.requiresNormal(rule) && !preflight.IsAvailable(a.AccountID) {
		return "not_dispatchable", nil
	}
	if a.NextDueAt != nil && a.NextDueAt.After(now) {
		return "scheduled", nil
	}
	return "due", nil
}
