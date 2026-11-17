package upstream

import "encoding/json"

// These are display facts, not permission to replay an upstream operation.
// Keep the public gateway's wire keys so encrypted state round-trips through
// the same allowlist. Never retain free-text messages, URLs or response bodies.
type FailureEvidence struct {
	UpstreamCode string `json:"response_error_code,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	Endpoint     string `json:"page_type,omitempty"`
}

type ItemProgress struct {
	Stage string `json:"stage,omitempty"`
}

var reasonCodes = makeSet(`invalid_state invalid_username_or_password invalid_password incorrect_password invalid_credentials invalid_totp_secret incorrect_totp_secret account_deactivated invalid_client unauthorized_client unsupported_grant_type invalid_scope`)
var failureEndpoints = makeSet(`api_accounts_mfa_verify api_accounts_password_verify`)
var progressStages = makeSet(`preparing proxy_check oauth_login password_verify totp_verify email_bind_submit email_otp_wait email_otp_verify workspace_select token_exchange finalizing completed`)

func allowedValue(raw json.RawMessage, allowed map[string]bool) string {
	var value string
	if json.Unmarshal(raw, &value) == nil && allowed[value] {
		return value
	}
	return ""
}

func failureEvidence(raw json.RawMessage) *FailureEvidence {
	var fields struct {
		Code     json.RawMessage `json:"response_error_code"`
		Error    json.RawMessage `json:"error"`
		Status   json.RawMessage `json:"http_status"`
		Endpoint json.RawMessage `json:"page_type"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	ev := FailureEvidence{UpstreamCode: allowedValue(fields.Code, reasonCodes), Endpoint: allowedValue(fields.Endpoint, failureEndpoints)}
	if ev.UpstreamCode == "" {
		ev.UpstreamCode = allowedValue(fields.Error, reasonCodes)
	}
	var status int
	if json.Unmarshal(fields.Status, &status) == nil && status >= 100 && status <= 599 {
		ev.HTTPStatus = status
	}
	if ev == (FailureEvidence{}) {
		return nil
	}
	return &ev
}

func (p *ItemProgress) UnmarshalJSON(raw []byte) error {
	var fields struct {
		Stage json.RawMessage `json:"stage"`
	}
	p.Stage = ""
	if json.Unmarshal(raw, &fields) == nil {
		p.Stage = allowedValue(fields.Stage, progressStages)
	}
	return nil
}
