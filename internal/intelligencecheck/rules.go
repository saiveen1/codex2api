package intelligencecheck

import (
	"fmt"
	"slices"
	"strings"
)

// Rule overrides the base settings for the first matching source/group/plan.
// An empty override inherits the base value. Rule order is significant.
type Rule struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Channel          string   `json:"channel"`
	GroupID          int64    `json:"group_id,omitempty"`
	Plan             string   `json:"plan,omitempty"`
	Automatic        bool     `json:"automatic"`
	NormalOnly       bool     `json:"normal_only,omitempty"`
	Model            string   `json:"model,omitempty"`
	ReasoningEffort  string   `json:"reasoning_effort,omitempty"`
	JudgeMode        string   `json:"judge_mode,omitempty"`
	IntervalSeconds  int      `json:"interval_seconds,omitempty"`
	RetrySeconds     int      `json:"retry_seconds,omitempty"`
	TimeoutSeconds   int      `json:"timeout_seconds,omitempty"`
	DegradeThreshold int      `json:"degrade_threshold,omitempty"`
	RecoverThreshold int      `json:"recover_threshold,omitempty"`
	Action           string   `json:"action,omitempty"`
	TargetGroupID    int64    `json:"target_group_id,omitempty"`
	QuestionIDs      []string `json:"question_ids,omitempty"`
}

func NormalizePlan(plan string) string { return strings.ToLower(strings.TrimSpace(plan)) }

func (s Settings) Resolve(channel, plan string, groupIDs []int64) (Settings, Rule) {
	for _, r := range s.Rules {
		if r.Channel == channel && (r.Plan == "" || NormalizePlan(r.Plan) == plan) && (r.GroupID == 0 || slices.Contains(groupIDs, r.GroupID)) {
			return r.apply(s), r
		}
	}
	s.Rules = nil
	return s, Rule{}
}

func (r Rule) apply(s Settings) Settings {
	s.Rules = nil
	if r.Model != "" {
		switch r.Channel {
		case "codex":
			s.GPTModel = r.Model
		case "claude":
			s.ClaudeModel = r.Model
		case "grok":
			s.GrokModel = r.Model
		case "antigravity":
			s.AntigravityModel = r.Model
		case "kiro":
			s.KiroModel = r.Model
		}
	}
	if r.ReasoningEffort != "" {
		s.ReasoningEffort = r.ReasoningEffort
	}
	if r.JudgeMode != "" {
		s.JudgeMode = r.JudgeMode
	}
	if r.IntervalSeconds != 0 {
		s.IntervalSeconds = r.IntervalSeconds
	}
	if r.RetrySeconds != 0 {
		s.RetrySeconds = r.RetrySeconds
	}
	if r.TimeoutSeconds != 0 {
		s.TimeoutSeconds = r.TimeoutSeconds
	}
	if r.DegradeThreshold != 0 {
		s.DegradeThreshold = r.DegradeThreshold
	}
	if r.RecoverThreshold != 0 {
		s.RecoverThreshold = r.RecoverThreshold
	}
	if r.Action != "" {
		s.Action, s.TargetGroupID = r.Action, r.TargetGroupID
	}
	if len(r.QuestionIDs) > 0 {
		questions := make([]Question, 0, len(r.QuestionIDs))
		for _, q := range s.Questions {
			if slices.Contains(r.QuestionIDs, q.ID) {
				questions = append(questions, q)
			}
		}
		s.Questions = questions
	}
	return s
}

func validateRules(s Settings) error {
	if len(s.Rules) > 100 {
		return fmt.Errorf("最多配置 100 条范围规则")
	}
	seen := map[string]bool{}
	for _, r := range s.Rules {
		if strings.TrimSpace(r.ID) == "" || len(r.ID) > 100 || seen[r.ID] || strings.TrimSpace(r.Name) == "" || len(r.Name) > 255 {
			return fmt.Errorf("规则 ID 必须唯一，且必须填写规则名称")
		}
		seen[r.ID] = true
		if !IsChannel(r.Channel) || r.GroupID < 0 || len(r.Plan) > 100 || (r.GroupID == 0 && NormalizePlan(r.Plan) == "") {
			return fmt.Errorf("规则 %q：请选择来源，并指定分组或套餐", r.Name)
		}
		if r.Model != "" && strings.TrimSpace(r.Model) == "" || r.Channel != "codex" && r.ReasoningEffort != "" {
			return fmt.Errorf("规则 %q：模型不能全为空白，且推理强度只适用于 GPT/Codex", r.Name)
		}
		effective := r.apply(s)
		if len(r.QuestionIDs) != len(effective.Questions) && len(r.QuestionIDs) > 0 {
			return fmt.Errorf("规则 %q：题目不存在或重复", r.Name)
		}
		if len(r.QuestionIDs) > 0 && !slices.ContainsFunc(effective.Questions, func(q Question) bool { return q.Enabled }) {
			return fmt.Errorf("规则 %q：请选择至少一道启用题目", r.Name)
		}
		if err := ValidateSettings(effective); err != nil {
			return fmt.Errorf("规则 %q：%w", r.Name, err)
		}
	}
	return nil
}
