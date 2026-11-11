package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const diagnosticBodyLimit = 32 << 20
const diagnosticErrorLimit = 8 << 10
const diagnosticItemLimit = 20000

// Only schema names and fixed enums leave this parser, never arbitrary JSON values.
type diagnosticRequestShape struct {
	Bytes                  int            `json:"bytes"`
	Inspection             string         `json:"inspection"`
	Fields                 []string       `json:"fields,omitempty"`
	UnknownFields          int            `json:"unknown_fields,omitempty"`
	InputItems             int            `json:"input_items,omitempty"`
	Messages               int            `json:"messages,omitempty"`
	Tools                  int            `json:"tools,omitempty"`
	ItemTypes              map[string]int `json:"item_types,omitempty"`
	ToolTypes              map[string]int `json:"tool_types,omitempty"`
	MissingCallIDs         int            `json:"missing_call_ids,omitempty"`
	EncryptedItems         int            `json:"encrypted_items,omitempty"`
	PreviousResponse       bool           `json:"previous_response"`
	Stream                 bool           `json:"stream"`
	ReasoningEffort        string         `json:"reasoning_effort,omitempty"`
	ToolChoice             string         `json:"tool_choice,omitempty"`
	ImageChoiceWithoutTool bool           `json:"image_choice_without_tool,omitempty"`
}

func diagnosticField(s string) bool {
	switch s {
	case "model", "input", "messages", "contents", "instructions", "system", "tools", "tool_choice", "stream", "reasoning", "effort", "text", "format", "type", "name", "function", "parameters", "arguments", "output", "content", "role", "call_id", "id", "encrypted_content", "previous_response_id", "temperature", "top_p", "max_tokens", "max_output_tokens", "max_completion_tokens", "parallel_tool_calls", "store", "include", "metadata", "service_tier", "prompt_cache_key", "prompt_cache_retention", "safety_identifier", "user", "response_format", "complexity", "size", "quality", "n", "image", "images", "prompt", "stream_id":
		return true
	}
	return false
}

func diagnosticEnum(s string) string {
	switch s {
	case "", "auto", "none", "required", "low", "medium", "high", "minimal", "xhigh", "message", "reasoning", "function", "function_call", "function_call_output", "custom", "custom_tool_call", "custom_tool_call_output", "image_generation", "image_generation_call", "web_search", "web_search_preview", "web_search_call", "file_search", "computer", "computer_use_preview", "computer_call", "computer_call_output", "code_interpreter", "mcp", "compaction", "compaction_trigger", "item_reference", "input_text", "input_image", "input_file", "output_text":
		return strings.Clone(s)
	}
	return "other"
}

func summarizeDiagnosticRequest(body []byte) *diagnosticRequestShape {
	s := &diagnosticRequestShape{Bytes: len(body), Inspection: "unavailable"}
	if len(body) == 0 {
		return s
	}
	if len(body) > diagnosticBodyLimit {
		s.Inspection = "size_limit"
		return s
	}
	if !gjson.ValidBytes(body) {
		s.Inspection = "invalid_json"
		return s
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		s.Inspection = "not_object"
		return s
	}
	s.Inspection = "complete"
	seen := map[string]bool{}
	root.ForEach(func(k, v gjson.Result) bool {
		if diagnosticField(k.String()) {
			if !seen[k.String()] {
				s.Fields = append(s.Fields, strings.Clone(k.String()))
				seen[k.String()] = true
			}
		} else {
			s.UnknownFields++
		}
		return true
	})
	s.PreviousResponse = root.Get("previous_response_id").Exists()
	s.Stream = root.Get("stream").Bool()
	s.ReasoningEffort = diagnosticEnum(root.Get("reasoning.effort").String())
	s.ToolChoice = diagnosticEnum(root.Get("tool_choice.type").String())
	if s.ToolChoice == "" {
		s.ToolChoice = diagnosticEnum(root.Get("tool_choice").String())
	}
	s.ItemTypes = map[string]int{}
	s.ToolTypes = map[string]int{}
	budget := diagnosticItemLimit
	for _, path := range []string{"input", "messages", "tools"} {
		items := root.Get(path)
		if !items.IsArray() {
			continue
		}
		items.ForEach(func(_, item gjson.Result) bool {
			if budget == 0 {
				s.Inspection = "item_limit"
				return false
			}
			budget--
			kind := diagnosticEnum(item.Get("type").String())
			if kind == "" {
				kind = "message"
			}
			switch path {
			case "tools":
				s.Tools++
				s.ToolTypes[kind]++
			case "messages":
				s.Messages++
				s.ItemTypes[kind]++
			default:
				s.InputItems++
				s.ItemTypes[kind]++
			}
			if kind == "function_call" || kind == "function_call_output" || kind == "custom_tool_call" || kind == "custom_tool_call_output" {
				if item.Get("call_id").String() == "" {
					s.MissingCallIDs++
				}
			}
			if item.Get("encrypted_content").Exists() {
				s.EncryptedItems++
			}
			return true
		})
	}
	s.ImageChoiceWithoutTool = s.Inspection == "complete" && s.ToolChoice == "image_generation" && s.ToolTypes["image_generation"] == 0
	return s
}

type diagnosticError struct {
	Detail       string `json:"detail"`
	Code         string `json:"code,omitempty"`
	Type         string `json:"type,omitempty"`
	Param        string `json:"param,omitempty"`
	MessageClass string `json:"message_class,omitempty"`
	Fingerprint  string `json:"fingerprint,omitempty"`
}

func diagnosticFingerprint(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:12])
}

func diagnosticErrorCode(s string) string {
	switch s {
	case "", "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "rate_limit_error", "server_error", "api_error", "overloaded_error", "upstream_error", "invalid_encrypted_content", "model_not_found", "unsupported_model", "cyber_policy", "context_length_exceeded", "invalid_request", "invalid_parameter", "missing_field", "invalid_field_type", "invalid_field_format", "unsupported_feature", "missing_api_key", "invalid_api_key", "invalid_auth", "insufficient_scope", "insufficient_quota", "rate_limit_exceeded", "rate_limit_reached", "service_unavailable", "upstream_timeout", "upstream_stream_break", "previous_response_not_found", "invalid_stream_id", "websocket_stream_limit_reached", "resource_not_found", "resource_conflict", "invalid-argument", "timeout", "transport", "client_closed", "client", "server", "unauthorized", "continue_thinking_error", "empty_response", "ws_busy_acquire", "message_too_big", "client_compatibility", "account_pool_concurrency_saturated":
		return s
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
		if diagnosticField(part) {
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

func summarizeDiagnosticError(body []byte) diagnosticError {
	d := diagnosticError{Detail: "unavailable"}
	if len(body) == 0 {
		return d
	}
	if len(body) > diagnosticErrorLimit {
		d.Detail = "size_limit"
		return d
	}
	if !gjson.ValidBytes(body) {
		d.Detail = "unstructured_omitted"
		return d
	}
	d.Detail = "structured"
	root := gjson.ParseBytes(body)
	for _, path := range []string{"response.error", "response.status_details.error", "error", "detail"} {
		if v := root.Get(path); v.IsObject() {
			root = v
			break
		}
	}
	d.Code = diagnosticErrorCode(root.Get("code").String())
	d.Type = diagnosticErrorCode(root.Get("type").String())
	d.Param = diagnosticParam(root.Get("param").String())
	message := root.Get("message").String()
	if message == "" {
		message = root.Get("detail").String()
	}
	if message == "" && root.Get("error").Type == gjson.String {
		message = root.Get("error").String()
	}
	// Never echo an upstream message: even structured JSON may quote prompts or credentials.
	d.Fingerprint = diagnosticFingerprint(root.Get("code").String() + "\x00" + message)
	switch {
	case strings.EqualFold(strings.TrimSpace(message), "Bad Request"):
		d.MessageClass = "generic_bad_request"
		d.Detail = "generic"
	case strings.Contains(message, "Complexity") && strings.Contains(message, "not supported for this image model"):
		d.MessageClass = "image_complexity_unsupported"
	case strings.Contains(message, "Tool choice") && strings.Contains(message, "not found in"):
		d.MessageClass = "tool_choice_not_declared"
	case strings.Contains(message, "could not be decrypted or decoded"):
		d.MessageClass = "encrypted_content_rejected"
	default:
		if message != "" {
			d.MessageClass = "message_omitted"
		}
	}
	return d
}
