package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
)

const diagnosticBodyLimit = 32 << 20
const diagnosticErrorLimit = 8 << 10
const diagnosticItemLimit = 20000

// Only schema names and fixed enums leave this parser, never arbitrary JSON values.
type diagnosticRequestShape struct {
	Bytes                  int                    `json:"bytes"`
	Inspection             string                 `json:"inspection"`
	Fingerprint            string                 `json:"fingerprint,omitempty"`
	ShapeFingerprint       string                 `json:"shape_fingerprint,omitempty"`
	ModelFingerprint       string                 `json:"model_fingerprint,omitempty"`
	InstructionsBytes      int                    `json:"instructions_bytes,omitempty"`
	Parameters             []diagnosticValueShape `json:"parameters,omitempty"`
	ItemSamples            []diagnosticItemShape  `json:"item_samples,omitempty"`
	ToolSamples            []diagnosticToolShape  `json:"tool_samples,omitempty"`
	SamplesTruncated       bool                   `json:"samples_truncated,omitempty"`
	Fields                 []string               `json:"fields,omitempty"`
	UnknownFields          int                    `json:"unknown_fields,omitempty"`
	InputItems             int                    `json:"input_items,omitempty"`
	Messages               int                    `json:"messages,omitempty"`
	Tools                  int                    `json:"tools,omitempty"`
	ItemTypes              map[string]int         `json:"item_types,omitempty"`
	ToolTypes              map[string]int         `json:"tool_types,omitempty"`
	MissingCallIDs         int                    `json:"missing_call_ids,omitempty"`
	EncryptedItems         int                    `json:"encrypted_items,omitempty"`
	PreviousResponse       bool                   `json:"previous_response"`
	Stream                 bool                   `json:"stream"`
	ReasoningEffort        string                 `json:"reasoning_effort,omitempty"`
	ToolChoice             string                 `json:"tool_choice,omitempty"`
	ImageChoiceWithoutTool bool                   `json:"image_choice_without_tool,omitempty"`
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
	populateDiagnosticRequestDetails(s, root, body)
	s.ImageChoiceWithoutTool = s.Inspection == "complete" && s.ToolChoice == "image_generation" && s.ToolTypes["image_generation"] == 0
	return s
}
