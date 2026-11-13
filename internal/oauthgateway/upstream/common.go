package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Error is a deliberately sanitized upstream failure.
type Error struct {
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	Source     string   `json:"source"`
	Action     string   `json:"action"`
	Retryable  bool     `json:"retryable"`
	RetryAfter *float64 `json:"retry_after"`
	Status     int      `json:"-"`
	Uncertain  bool     `json:"-"`
}

func (e *Error) Error() string { return "upstream request failed" }

var codes = makeSet(`unauthorized forbidden forbidden_origin insufficient_quota invalid_request invalid_input invalid_account invalid_identifier invalid_totp_secret invalid_options unsupported_country unsupported_options json_required invalid_json request_too_large one_material_source_required invalid_source_text invalid_material_format batch_too_large invalid_accounts duplicate_identity input_empty invalid_proxy proxy_required unsupported_proxy_scheme proxy_private_address invalid_proxy_host invalid_proxy_port invalid_proxy_credentials invalid_proxy_structure invalid_proxy_url ambiguous_proxy_use_structured proxy_public_ip_required proxy_requires_custom_mode https_proxy_hostname_pinning_unsupported unsupported_environment unsupported_user_agent invalid_language invalid_timezone unsupported_timezone invalid_custom_ua unsupported_custom_ua_exact_profile_required custom_ua_requires_custom_environment duplicate_user_agent custom_ua_transport_unavailable invalid_expected_identity rate_limited request_timeout capacity_exceeded idempotency_conflict code_already_created not_found job_not_found job_storage_limit job_store_unavailable job_material_invalid account_blocked account_busy account_cooldown account_requires_review oauth_failed invalid_credentials mfa_material_invalid account_material_rejected account_deactivated add_phone_required verification_required service_auth_required service_network_forbidden unsupported_contract worker_readonly_runtime_required unsupported_system_proxy oauth_client_configuration external_oauth_pinned_sentinel_runtime_required external_oauth_preinstalled_sentinel_runtime_required execution_service_unauthorized execution_service_forbidden execution_service_unavailable recovery_guard_unsupported account_attempt_limit owner_attempt_limit attempt_budget_exhausted execution_not_started execution_outcome_unknown job_running_cannot_cancel claim_lost queue_full session_capacity service_unavailable internal_error upstream_error upstream_unavailable transport_error invalid_response invalid_preview invalid_execution_status proxy_resolution_failed proxy_pin_failed result_not_ready result_unavailable result_expired invalid_result oauth_result_invalid receipt_conflict job_completion_conflict settlement_conflict operator_confirmed_no_result recovery_identity_mismatch recovery_identity_unproven unsupported_proxy_check_mode proxy_check_unsupported proxy_check_unavailable proxy_check_busy proxy_dns_failed proxy_check_timeout proxy_unreachable proxy_upstream_unavailable proxy_country_unverified proxy_country_mismatch proxy_check_failed`)
var sources = makeSet(`gateway_auth gateway_quota request task_storage upstream_login oauth_client execution delivery`)
var actions = makeSet(`authenticate check_quota fix_request retry_same_request query_original fix_configuration manual_review`)

func makeSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, v := range strings.Fields(s) {
		m[v] = true
	}
	return m
}
func safeCode(s string) string {
	if codes[s] {
		return s
	}
	return "upstream_error"
}
func (e *Error) UnmarshalJSON(b []byte) error {
	var r struct {
		Code       string   `json:"code"`
		Message    string   `json:"message"`
		Source     string   `json:"source"`
		Action     string   `json:"action"`
		Retryable  *bool    `json:"retryable"`
		RetryAfter *float64 `json:"retry_after"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &r) != nil || json.Unmarshal(b, &fields) != nil || !object(b, "code", "message", "source", "action", "retryable") || r.Code == "" || r.Source == "" || r.Action == "" || r.Retryable == nil {
		return errors.New("invalid upstream error")
	}
	if _, ok := fields["retry_after"]; !ok {
		return errors.New("invalid upstream error")
	}
	e.Code = safeCode(r.Code)
	e.Message = "Upstream request failed."
	e.Source = "execution"
	e.Action = "query_original"
	e.Retryable = false
	e.RetryAfter = nil
	if codes[r.Code] {
		if sources[r.Source] {
			e.Source = r.Source
		}
		if actions[r.Action] {
			e.Action = r.Action
		}
		e.Retryable = *r.Retryable && e.Action == "retry_same_request" && (e.Code == "rate_limited" || e.Code == "request_timeout" || e.Code == "capacity_exceeded")
		if r.RetryAfter != nil && *r.RetryAfter >= 0 && *r.RetryAfter <= 86400 {
			e.RetryAfter = r.RetryAfter
		}
	}
	return nil
}
func failure(code string, uncertain bool) *Error {
	return &Error{Code: code, Message: "Upstream request failed.", Source: "execution", Action: "query_original", Uncertain: uncertain}
}

// ValidateBaseURL permits administrator configured destinations and preserves path prefixes.
func ValidateBaseURL(raw string, allowHTTP bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.Contains(raw, "#") {
		return "", errors.New("invalid upstream base URL")
	}
	if u.Scheme == "http" && !allowHTTP {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("insecure upstream base URL")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

type client struct {
	base, token string
	http        *http.Client
}

func newClient(base, token string, allow bool) (*client, error) {
	base, err := ValidateBaseURL(base, allow)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true, ForceAttemptHTTP2: false, TLSHandshakeTimeout: 10 * time.Second}
	return &client{base: base, token: token, http: &http.Client{Transport: tr, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

const maxResponse = 4 << 20

func bounded(r io.Reader, n int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, n+1))
	if err != nil || int64(len(b)) > n {
		return nil, errors.New("invalid response")
	}
	return b, nil
}
func object(raw []byte, fields ...string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return false
	}
	for _, k := range fields {
		v, ok := m[k]
		if !ok || len(v) == 0 || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	return true
}
func validID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':') {
			return false
		}
	}
	return s != "." && s != ".."
}

// arrayObjects validates presence of nested mandatory fields before decoding zero values.
func arrayObjects(raw []byte, key string, fields ...string) bool {
	var m map[string]json.RawMessage
	var a []json.RawMessage
	if json.Unmarshal(raw, &m) != nil || json.Unmarshal(m[key], &a) != nil || a == nil {
		return false
	}
	for _, v := range a {
		if !object(v, fields...) {
			return false
		}
	}
	return true
}
