package service

import "github.com/codex2api/internal/oauthgateway/upstream"

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
	"invalid_gateway_account":          "网关结果不是完整的 Codex OAuth 账号；未写入账号。",
	"gateway_identity_mismatch":        "网关结果缺少主体/工作区，或与凭据身份不一致；未写入账号。",
	"invalid_import_destination":       "目标分组或运行代理不可用；修正后仅重试入库，不重新授权。",
	"local_lookup_failed":              "查询本地账号失败，尚未写入；可稍后仅重试入库。",
	"existing_account_requires_review": "已有相同账号或凭据路由；原账号保持不变。本功能不会替换其凭据，可核对后丢弃本次结果并归档。",
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
