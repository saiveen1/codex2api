package proxy

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

type diagnosticError struct {
	Detail            string                 `json:"detail"`
	Code              string                 `json:"code,omitempty"`
	Type              string                 `json:"type,omitempty"`
	Param             string                 `json:"param,omitempty"`
	MessageClass      string                 `json:"message_class,omitempty"`
	Fingerprint       string                 `json:"fingerprint,omitempty"`
	Source            string                 `json:"source,omitempty"`
	BodyBytes         int                    `json:"body_bytes"`
	BodyFormat        string                 `json:"body_format,omitempty"`
	BodyFingerprint   string                 `json:"body_fingerprint,omitempty"`
	MessagePresent    bool                   `json:"message_present"`
	MessageBytes      int                    `json:"message_bytes,omitempty"`
	CodePresent       bool                   `json:"code_present"`
	TypePresent       bool                   `json:"type_present"`
	ParamPresent      bool                   `json:"param_present"`
	CodeRedacted      bool                   `json:"code_redacted,omitempty"`
	TypeRedacted      bool                   `json:"type_redacted,omitempty"`
	ParamRedacted     bool                   `json:"param_redacted,omitempty"`
	CodeFingerprint   string                 `json:"code_fingerprint,omitempty"`
	TypeFingerprint   string                 `json:"type_fingerprint,omitempty"`
	ParamFingerprint  string                 `json:"param_fingerprint,omitempty"`
	Evidence          []diagnosticValueShape `json:"evidence,omitempty"`
	EvidenceTruncated bool                   `json:"evidence_truncated,omitempty"`
	ReadError         string                 `json:"read_error,omitempty"`
}

func diagnosticErrorCode(s string) string {
	switch s {
	case "", "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "rate_limit_error", "server_error", "api_error", "overloaded_error", "upstream_error", "invalid_encrypted_content", "model_not_found", "unsupported_model", "cyber_policy", "context_length_exceeded", "invalid_request", "invalid_parameter", "missing_field", "invalid_field_type", "invalid_field_format", "unsupported_feature", "missing_api_key", "invalid_api_key", "invalid_auth", "insufficient_scope", "insufficient_quota", "rate_limit_exceeded", "rate_limit_reached", "service_unavailable", "upstream_timeout", "upstream_stream_break", "previous_response_not_found", "invalid_stream_id", "websocket_stream_limit_reached", "resource_not_found", "resource_conflict", "invalid-argument", "timeout", "transport", "client_closed", "client", "server", "unauthorized", "continue_thinking_error", "empty_response", "ws_busy_acquire", "message_too_big", "client_compatibility", "account_pool_concurrency_saturated", "invalid_json", "invalid_value", "missing", "extra_forbidden", "invalid_api_key_error", "permission_denied", "resource_exhausted", "UNAUTHENTICATED", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "INVALID_ARGUMENT", "NOT_FOUND", "UNAVAILABLE", "INTERNAL":
		return strings.Clone(s)
	}
	return "other"
}

func diagnosticParam(s string) string {
	if len(s) > 160 {
		return ""
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '[' || r == ']' })
	if len(parts) == 0 || len(parts) > 12 {
		return ""
	}
	for _, part := range parts {
		if diagnosticField(part) || part == "body" || part == "parts" || part == "input_schema" {
			continue
		}
		if len(part) > 6 {
			return ""
		}
		if _, err := strconv.ParseUint(part, 10, 20); err != nil {
			return ""
		}
	}
	return strings.Join(parts, ".")
}

func diagnosticEvidenceField(key string) string {
	if diagnosticField(key) {
		return strings.Clone(key)
	}
	switch key {
	case "error", "errors", "detail", "details", "code", "message", "param", "status", "status_code", "response", "status_details", "error_description", "errorCode", "errorMessage", "reason", "title", "loc", "msg", "violations", "request_id":
		return strings.Clone(key)
	}
	return "field#" + diagnosticFingerprint(key)
}

func collectDiagnosticErrorEvidence(d *diagnosticError, root gjson.Result) {
	var walk func(gjson.Result, string, int)
	walk = func(v gjson.Result, path string, depth int) {
		if len(d.Evidence) >= 32 {
			d.EvidenceTruncated = true
			return
		}
		d.Evidence = append(d.Evidence, diagnosticValueShape{Path: path, Type: diagnosticValueType(v), Bytes: diagnosticValueBytes(v), Items: diagnosticValueCount(v), Fingerprint: diagnosticFingerprint(v.Raw)})
		if !v.IsObject() && !v.IsArray() {
			return
		}
		if depth >= 4 {
			d.EvidenceTruncated = true
			return
		}
		index := 0
		v.ForEach(func(k, child gjson.Result) bool {
			if len(d.Evidence) >= 32 {
				d.EvidenceTruncated = true
				return false
			}
			name := diagnosticEvidenceField(k.String())
			if v.IsArray() {
				name = strconv.Itoa(index)
				index++
			}
			walk(child, path+"."+name, depth+1)
			return true
		})
	}
	walk(root, "$", 0)
}

func diagnosticFirstField(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if v := root.Get(path); v.Exists() {
			return v
		}
	}
	return gjson.Result{}
}

func diagnosticMessageClass(message string) string {
	lower := strings.ToLower(strings.TrimSpace(message))
	switch {
	case lower == "bad request":
		return "generic_bad_request"
	case strings.Contains(lower, "complexity") && strings.Contains(lower, "not supported for this image model"):
		return "image_complexity_unsupported"
	case strings.Contains(lower, "tool choice") && strings.Contains(lower, "not found in"):
		return "tool_choice_not_declared"
	case strings.Contains(lower, "could not be decrypted or decoded"):
		return "encrypted_content_rejected"
	case strings.Contains(lower, "context length") || strings.Contains(lower, "context window") || strings.Contains(lower, "maximum context"):
		return "context_length_exceeded"
	case strings.Contains(lower, "previous response") && strings.Contains(lower, "not found"):
		return "previous_response_not_found"
	case strings.Contains(lower, "unsupported parameter") || strings.Contains(lower, "unknown parameter"):
		return "unsupported_parameter"
	case strings.Contains(lower, "missing required") || lower == "field required":
		return "missing_required_parameter"
	case strings.Contains(lower, "invalid json") || strings.Contains(lower, "json parse"):
		return "invalid_json"
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests"):
		return "rate_limited"
	case strings.Contains(lower, "insufficient quota") || strings.Contains(lower, "exceeded your current quota"):
		return "insufficient_quota"
	case strings.Contains(lower, "invalid api key") || strings.Contains(lower, "incorrect api key"):
		return "invalid_api_key"
	case strings.Contains(lower, "overloaded"):
		return "overloaded"
	case strings.Contains(lower, "timed out") || strings.Contains(lower, "timeout"):
		return "timeout"
	case lower != "":
		return "message_omitted"
	default:
		return ""
	}
}

func summarizeDiagnosticError(body []byte) diagnosticError {
	d := diagnosticError{Detail: "unavailable", BodyBytes: len(body), BodyFormat: "empty"}
	if len(body) == 0 {
		return d
	}
	d.BodyFingerprint = diagnosticFingerprint(string(body))
	d.BodyFormat = "unknown"
	if len(body) > diagnosticErrorLimit {
		d.Detail = "size_limit"
		return d
	}
	if !gjson.ValidBytes(body) {
		d.Detail, d.BodyFormat = "unstructured_omitted", "text"
		if strings.HasPrefix(strings.TrimSpace(string(body)), "<") {
			d.BodyFormat = "html"
		}
		d.MessagePresent, d.MessageBytes = true, len(body)
		d.MessageClass = diagnosticMessageClass(string(body))
		d.Fingerprint = diagnosticFingerprint("\x00" + string(body))
		return d
	}
	d.Detail, d.BodyFormat = "structured", "json"
	root := gjson.ParseBytes(body)
	collectDiagnosticErrorEvidence(&d, root)
	for _, path := range []string{"response.error", "response.status_details.error", "error", "detail", "detail.0", "errors.0"} {
		if v := root.Get(path); v.IsObject() {
			root = v
			break
		}
	}
	code := diagnosticFirstField(root, "code", "errorCode", "status")
	errType := root.Get("type")
	param := root.Get("param")
	d.CodePresent, d.TypePresent, d.ParamPresent = code.Exists(), errType.Exists(), param.Exists()
	d.Code, d.Type, d.Param = diagnosticErrorCode(code.String()), diagnosticErrorCode(errType.String()), diagnosticParam(param.String())
	d.CodeRedacted = d.CodePresent && d.Code == "other"
	d.TypeRedacted = d.TypePresent && d.Type == "other"
	d.ParamRedacted = d.ParamPresent && d.Param == "" && param.String() != ""
	if d.CodeRedacted {
		d.CodeFingerprint = diagnosticFingerprint(code.Raw)
	}
	if d.TypeRedacted {
		d.TypeFingerprint = diagnosticFingerprint(errType.Raw)
	}
	if d.ParamRedacted {
		d.ParamFingerprint = diagnosticFingerprint(param.Raw)
	}
	message := ""
	for _, path := range []string{"message", "error_description", "errorMessage", "msg", "detail", "error", "title"} {
		v := root.Get(path)
		if !v.Exists() {
			continue
		}
		if path == "message" || path == "detail" || v.Type == gjson.String {
			d.MessagePresent = true
		}
		if v.Type == gjson.String && strings.TrimSpace(v.Str) != "" {
			message = v.Str
			break
		}
	}
	if root.Type == gjson.String {
		message, d.MessagePresent = root.Str, true
	}
	d.MessageBytes = len(message)
	d.Fingerprint = diagnosticFingerprint(code.String() + "\x00" + message)
	d.MessageClass = diagnosticMessageClass(message)
	if d.MessageClass == "generic_bad_request" {
		d.Detail = "generic"
	}
	return d
}

func diagnosticErrorSummary(status int, d diagnosticError) string {
	prefix := "No HTTP response"
	if status > 0 {
		prefix = fmt.Sprintf("HTTP %d", status)
	}
	parts := []string{prefix}
	for _, token := range []string{d.Code, d.Type, d.Param} {
		if token != "" && token != "other" && (len(parts) == 1 || parts[len(parts)-1] != token) {
			parts = append(parts, token)
		}
	}
	message := ""
	switch d.MessageClass {
	case "generic_bad_request":
		message = "Generic Bad Request; captured message does not identify the cause"
	case "image_complexity_unsupported":
		message = "Image model does not support the requested complexity"
	case "tool_choice_not_declared":
		message = "Selected tool was not found among declared tools"
	case "encrypted_content_rejected":
		message = "Encrypted content could not be decrypted or decoded"
	case "context_length_exceeded":
		message = "Request exceeds the context window"
	case "previous_response_not_found":
		message = "Referenced previous response was not found"
	case "unsupported_parameter":
		message = "Error message reports an unsupported parameter"
	case "missing_required_parameter":
		message = "Error message reports a missing required parameter"
	case "invalid_json":
		message = "Error message reports invalid JSON"
	case "rate_limited":
		message = "Upstream rate limit reached"
	case "insufficient_quota":
		message = "Upstream quota exhausted"
	case "invalid_api_key":
		message = "Upstream rejected API credentials"
	case "overloaded":
		message = "Upstream overloaded"
	case "timeout":
		message = "Upstream operation timed out"
	case "message_omitted":
		message = "Error message received; original text omitted for privacy (see evidence)"
	}
	if message == "" {
		switch d.Detail {
		case "empty":
			message = "Error response body was empty"
		case "size_limit":
			message = "Error evidence exceeded the inspection limit"
		case "unstructured_omitted":
			message = "Non-JSON error response received; original content omitted"
		case "structured", "generic":
			message = "Structured error received; inspect recorded fields for available evidence"
		default:
			message = "Error evidence unavailable; upstream cause cannot be determined"
		}
	}
	if d.ReadError != "" {
		message += "; error body read was incomplete (" + d.ReadError + ")"
	}
	parts = append(parts, message)
	return strings.Join(parts, " · ")
}
