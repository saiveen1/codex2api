package proxy

import "github.com/codex2api/database"

// Only gateway-confirmed facts, safe to expose to the calling Key. Do not send
// upstream conversation IDs, account IDs, prompts or raw worker errors here.
type webExecutionFailure struct {
	*chatGPTWebFailure
	stage          string
	assets         int
	recoveryIDs    bool
	imageValidated bool
}

func (e *webExecutionFailure) Unwrap() error { return e.chatGPTWebFailure }

func describeWebFailure(f *chatGPTWebFailure, e *database.ChatGPTWebExecution, validated bool) *webExecutionFailure {
	detail := &webExecutionFailure{chatGPTWebFailure: &chatGPTWebFailure{f.code, f.status, f.unknown}, stage: e.Stage, assets: len(e.AssetIDs), recoveryIDs: e.TaskID != "" || e.ConversationID != "" || len(e.AssetIDs) > 0, imageValidated: validated}
	if f.unknown && (detail.assets > 0 || validated) {
		// The worker has already reported an asset. A failure to validate, drain or
		// deliver it is not evidence that submission itself was ambiguous.
		detail.unknown = false
		if f.code == "submission_unknown" || f.code == "upstream_error" {
			detail.code = "image_stream_interrupted"
		}
	} else if f.code == "submission_unknown" && detail.recoveryIDs {
		detail.code = "image_stream_interrupted"
	}
	return detail
}
