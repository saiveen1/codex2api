package service

import "github.com/codex2api/internal/oauthgateway/upstream"

func confirmedOAuthFailure(item upstream.BatchItem) bool {
	if item.State != "failed" || item.ItemID == "" || item.ReceiptID != nil {
		return false
	}
	e := item.Error
	if e == nil || e.Uncertain || e.Source != "upstream_login" || (e.Action != "manual_review" && e.Action != "fix_request") {
		return false
	}
	if ev := e.Evidence; ev != nil && ev.UpstreamCode == "account_deactivated" {
		return false
	}
	// The public gateway uses manual_review even for a terminal invalid_state.
	// This permits a new, operator-confirmed login, never an automatic replay.
	switch e.Code {
	case "oauth_failed", "invalid_credentials", "mfa_material_invalid", "account_material_rejected":
		return true
	}
	return false
}

func retryReferences(s State) map[RetryReference]bool {
	refs := make(map[RetryReference]bool)
	for _, j := range s.Jobs {
		if j.RetryOf != nil {
			refs[*j.RetryOf] = true
		}
	}
	for _, j := range s.Archived {
		if j.RetryOf != nil {
			refs[*j.RetryOf] = true
		}
	}
	return refs
}

func jobViewWithRetries(j Job, refs map[RetryReference]bool) JobView {
	view := jobView(j)
	if len(refs) != 0 {
		for gi := range view.Groups {
			for ii := range view.Groups[gi].Items {
				item := &view.Groups[gi].Items[ii]
				if refs[RetryReference{JobID: j.ID, Line: item.Line}] {
					item.CanReauthorize = false
				}
			}
		}
	}
	return view
}

func checkRetry(s State, ref RetryReference, identifier string) error {
	if retryReferences(s)[ref] {
		return fault(409, "reauthorization_already_created", "该行已有重授权任务，请处理该任务，不能从原行重复提交。")
	}
	for _, j := range s.Jobs {
		if j.ID != ref.JobID {
			continue
		}
		for _, g := range jobView(j).Groups {
			for _, item := range g.Items {
				if item.Line != ref.Line || !item.CanReauthorize {
					continue
				}
				if item.Identifier != identifier {
					return fault(409, "reauthorization_identity_mismatch", "新材料标识与原任务该行不一致。")
				}
				return nil
			}
		}
		break
	}
	return fault(409, "cannot_reauthorize", "只能对未归档普通新增任务中已确认 OAuth 失败且无导入结果的行重授权。")
}
