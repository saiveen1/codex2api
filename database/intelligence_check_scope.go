package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ic "github.com/codex2api/internal/intelligencecheck"
)

type IntelligenceCheckGroup struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type IntelligenceCheckScope struct {
	Plan     string                   `json:"plan,omitempty"`
	Groups   []IntelligenceCheckGroup `json:"groups"`
	RuleID   string                   `json:"rule_id,omitempty"`
	RuleName string                   `json:"rule_name,omitempty"`
}

func (a IntelligenceCheckAccount) resolve(s ic.Settings) (ic.Settings, ic.Rule) {
	ids := a.OriginalGroupIDs
	if !ic.MovesGroups(a.OwnedAction) {
		ids = make([]int64, 0, len(a.Groups))
		for _, g := range a.Groups {
			ids = append(ids, g.ID)
		}
	}
	return s.Resolve(a.Channel, a.Plan, ids)
}

func intelligenceRuleAllowsAutomatic(s ic.Settings, a IntelligenceCheckAccount, r ic.Rule) bool {
	return a.OwnedAction != "" || (r.Automatic && (!r.NormalOnly || a.normalAccount)) || (!s.RulesOnly && r.ID == "")
}

func (a IntelligenceCheckAccount) requiresNormal(rule ic.Rule) bool {
	return rule.NormalOnly && a.OwnedAction == ""
}

// Keep hard account failures out of the queue; the runner also checks live
// dispatch state, including quota and credential availability, before sending.
func (db *DB) intelligenceNormalAccountSQL(account string) string {
	until := account + `.cooldown_until`
	expired := until + `<=CURRENT_TIMESTAMP`
	if db.isSQLite() {
		expired = `julianday(` + until + `)<=julianday('now')`
	}
	return `(` + account + `.status='active' AND (COALESCE(` + account + `.cooldown_reason,'') NOT IN ('unauthorized','overload_paused') OR COALESCE(` + expired + `,FALSE)))`
}

func (db *DB) intelligencePlanSQL(account string) string {
	plan := db.intelligenceFieldSQL(account+`.credentials`, "plan_type", "")
	return `LOWER(TRIM(COALESCE(` + plan + `,'')))`
}

func (db *DB) intelligenceGroupsSQL(account string) string {
	from := ` FROM account_group_members m JOIN account_groups g ON g.id=m.group_id WHERE m.account_id=` + account + `.id`
	if db.isSQLite() {
		return `(SELECT COALESCE(json_group_array(json_object('id',id,'name',name)),'[]') FROM (SELECT g.id,g.name` + from + ` ORDER BY g.id))`
	}
	return `(SELECT COALESCE(json_agg(json_build_object('id',g.id,'name',g.name) ORDER BY g.id)::text,'[]')` + from + `)`
}

func (db *DB) intelligenceScopeColumns(account string) string {
	return "," + db.intelligencePlanSQL(account) + "," + db.intelligenceGroupsSQL(account)
}

func (db *DB) intelligenceFieldSQL(value, key, pgType string) string {
	if db.isSQLite() {
		return `json_extract(` + value + `,'$.` + key + `')`
	}
	field := `((` + value + `)::jsonb->>'` + key + `')`
	if pgType != "" {
		field += `::` + pgType
	}
	return field
}

// Ordered matching in SQL keeps disabled rules out of the scheduler's bounded
// candidate window. Match the same fields and order as Settings.Resolve.
func (db *DB) intelligenceRuleSQL(account, state, config string) string {
	configJSON := config + `.config_json`
	from, order := `json_each(`+configJSON+`,'$.rules') ir`, `CAST(ir.key AS INTEGER)`
	if !db.isSQLite() {
		from, order = `jsonb_array_elements((`+configJSON+`)::jsonb->'rules') WITH ORDINALITY ir(value,idx)`, `ir.idx`
	}
	channel := strings.ReplaceAll(db.intelligenceChannelExpr(), "a.", account+".")
	group := db.intelligenceFieldSQL("ir.value", "group_id", "bigint")
	plan := `COALESCE(` + db.intelligenceFieldSQL("ir.value", "plan", "") + `,'')`
	member := `EXISTS(SELECT 1 FROM account_group_members rgm WHERE rgm.account_id=` + account + `.id AND rgm.group_id=` + group + `)`
	if state != "" {
		original := `EXISTS(SELECT 1 FROM json_each(` + state + `.state_json,'$.original_group_ids') orig WHERE orig.value=` + group + `)`
		if !db.isSQLite() {
			original = `EXISTS(SELECT 1 FROM jsonb_array_elements_text(` + state + `.state_json::jsonb->'original_group_ids') orig(value) WHERE orig.value::bigint=` + group + `)`
		}
		member = `(CASE WHEN ` + db.intelligenceStateActionExpr(state) + ` IN ('switch_group','` + ic.ActionPauseAndMove + `') THEN ` + original + ` ELSE ` + member + ` END)`
	}
	return `(SELECT ir.value FROM ` + from + ` WHERE ` + db.intelligenceFieldSQL("ir.value", "channel", "") + `=` + channel +
		` AND (` + plan + `='' OR ` + plan + `=` + db.intelligencePlanSQL(account) + `)` +
		` AND (COALESCE(` + group + `,0)=0 OR ` + member + `) ORDER BY ` + order + ` LIMIT 1)`
}

func (db *DB) intelligenceRuleAutomaticSQL(account, state, config string) string {
	rule := db.intelligenceRuleSQL(account, state, config)
	baseAllowed := `NOT COALESCE(` + db.intelligenceFieldSQL(config+`.config_json`, "rules_only", "boolean") + `,FALSE)`
	normalOnly := `COALESCE(` + db.intelligenceFieldSQL(rule, "normal_only", "boolean") + `,FALSE)`
	return `((COALESCE(` + db.intelligenceFieldSQL(rule, "automatic", "boolean") + `,` + baseAllowed + `) AND (NOT ` + normalOnly + ` OR ` + db.intelligenceNormalAccountSQL(account) + `)) OR COALESCE(` + db.intelligenceStateActionExpr(state) + `,'')<>'')`
}

func (db *DB) validateIntelligenceRules(ctx context.Context, q intelligenceQueryRower, s ic.Settings) error {
	for _, r := range s.Rules {
		for _, id := range []int64{r.GroupID, r.TargetGroupID} {
			if id <= 0 {
				continue
			}
			channel, err := intelligenceTargetGroupChannel(ctx, q, ic.Settings{TargetGroupID: id})
			if err != nil {
				return fmt.Errorf("规则 %q：%w", r.Name, err)
			}
			if channel != r.Channel {
				return fmt.Errorf("规则 %q：分组 #%d 不属于来源 %s", r.Name, id, r.Channel)
			}
		}
	}
	return nil
}

func (db *DB) intelligenceHistoryGroupSQL() string {
	if db.isSQLite() {
		return `EXISTS(SELECT 1 FROM json_each(run_json,'$.groups') hg WHERE json_extract(hg.value,'$.id')=%s)`
	}
	return `EXISTS(SELECT 1 FROM jsonb_array_elements(COALESCE(NULLIF(run_json::jsonb->'groups','null'::jsonb),'[]'::jsonb)) hg(value) WHERE (hg.value->>'id')::bigint=%s)`
}

func scanIntelligenceScope(raw string, scope *IntelligenceCheckScope) error {
	return json.Unmarshal([]byte(raw), &scope.Groups)
}
