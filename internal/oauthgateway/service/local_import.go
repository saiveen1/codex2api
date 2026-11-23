package service

import (
	"context"
	"strings"

	"github.com/codex2api/internal/oauthgateway/upstream"
	"github.com/codex2api/internal/openaiidentity"
)

// LocalImportError uses fixed messages. The wire decoder never accepts source
// "codex", so an external gateway cannot forge this local adapter classification.
func LocalImportError(code string) *upstream.Error {
	message, ok := localImportMessages[code]
	if !ok {
		return &upstream.Error{Code: "import_outcome_unknown", Message: localImportMessages["import_outcome_unknown"], Source: "codex", Action: "manual_review", Uncertain: true}
	}
	action := "manual_review"
	switch code {
	case "invalid_import_destination", "runtime_load_pending":
		action = "fix_configuration"
	case "local_lookup_failed":
		action = "retry_same_request"
	}
	return &upstream.Error{Code: code, Message: message, Source: "codex", Action: action, Uncertain: code == "import_outcome_unknown"}
}

var localImportMessages = map[string]string{
	"recovery_identity_unverified":     "无法证明原主体或工作区，需人工核验；未替换凭据。",
	"recovery_identity_mismatch":       "恢复结果与原主体或工作区不符；未替换凭据。",
	"recovery_credentials_changed":     "原账号凭据版本已变更；未覆盖并发刷新或手动修改。",
	"recovery_account_unavailable":     "原账号已删除、禁用或不允许恢复；不会自动复活。",
	"recovery_cancelled":               "恢复已取消或保管已撤销；仅核对原任务，不写回结果。",
	"recovery_attempt_limit":           "自动恢复已达到三次上限，请人工修复或重新明确授权保管。",
	"recovery_refresh_requires_review": "正常刷新未确认可通过重登解决；请检查封禁、验证、网络或刷新未知结果。",
	"recovery_retention_failed":        "授权已入库，但恢复材料未能绑定可验证身份；未启用自动恢复。",
	"invalid_gateway_account":          "网关结果不是完整的 Codex OAuth 账号；未写入账号。",
	"gateway_identity_mismatch":        "网关结果缺少主体/工作区，或与凭据身份不一致；未写入账号。",
	"invalid_import_destination":       "目标分组或运行代理不可用；修正后仅重试入库，不重新授权。",
	"local_lookup_failed":              "查询本地账号失败，尚未写入；可稍后仅重试入库。",
	"existing_account_requires_review": "已有相同账号或凭据路由；手动新增不会覆盖原账号。请明确选择原账号修复并核验身份，本次结果可核对后归档。",
	"import_outcome_unknown":           "账号事务结果未确认，请核对账号库；禁止自动重试写入。",
	"runtime_load_pending":             "账号和分组已入库，但运行时加载待核对；不要重复导入。",
}

func localImportProblem(remote *upstream.Error) *Problem {
	if remote == nil || remote.Source != "codex" {
		return nil
	}
	if _, ok := localImportMessages[remote.Code]; !ok {
		return nil
	}
	clean := LocalImportError(remote.Code)
	clean.Uncertain = clean.Uncertain || remote.Uncertain
	problem := Problem(*clean)
	return &problem
}

func committedAccountResult(ctx context.Context, target Target, job Job, g Group, line int, result upstream.ImportResult) *AccountResult {
	if line < 1 || line > len(g.Lines) {
		return nil
	}
	r := &AccountResult{Line: g.Lines[line-1], Outcome: "imported"}
	if line <= len(g.Request.Accounts) && g.Request.Accounts[line-1].Identifier != "" {
		r.IdentifierMasked = mask(g.Request.Accounts[line-1].Identifier)
	}
	switch {
	case job.Recovery != nil && result.Updated == 1:
		r.AccountID, r.Outcome = job.Recovery.AccountID, reauthorizationSucceeded
	case result.Success == 1:
		if len(result.CreatedIDs) == 1 {
			r.AccountID = result.CreatedIDs[0]
		}
	default:
		return nil // No confirmed account write with a known identity to snapshot.
	}
	if c, ok := target.(AccountResultTarget); ok && r.AccountID > 0 {
		row := c.AccountResult(ctx, r.AccountID)
		// Only known public plan labels are safe to persist from a credential row.
		plan := strings.ToLower(strings.TrimSpace(row.PlanType))
		switch plan {
		case "free", "go", "plus", "pro", "prolite", "pro_lite", "pro-lite", "promax", "pro_max", "pro-max", "team", "teamplus", "business", "self_serve_business_prolite", "enterprise", "ent26", "enterprise_cbp_usage_based", "enterprise_cbp_automation", "edu", "edu_plus", "edu_pro", "education", "k12":
			r.PlanType = plan
		}
		if openaiidentity.ValidWebWorkspaceID(row.WorkspaceID) {
			r.WorkspaceID = row.WorkspaceID
			r.WorkspaceName = row.WorkspaceName
		}
		if row.GroupIDs != nil {
			ids := sortedGroups(*row.GroupIDs)
			r.GroupIDs = &ids
		}
		if row.RuntimeProxyID != nil && *row.RuntimeProxyID >= 0 {
			id := *row.RuntimeProxyID
			r.RuntimeProxyID = &id
		}
	}
	return r
}
