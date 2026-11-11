package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const diagnosticItemSamples = 24
const diagnosticToolSamples = 12

// Values are deliberately limited to protocol enums, booleans and numeric controls.
// Names, IDs, schema property names, instructions and content only leave as fingerprints/sizes.
type diagnosticValueShape struct {
	Path        string `json:"path"`
	Type        string `json:"type"`
	Bytes       int    `json:"bytes"`
	Items       int    `json:"items,omitempty"`
	Value       any    `json:"value,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type diagnosticItemShape struct {
	Index             int            `json:"index"`
	Path              string         `json:"path"`
	Type              string         `json:"type"`
	Role              string         `json:"role,omitempty"`
	Bytes             int            `json:"bytes"`
	CallIDFingerprint string         `json:"call_id_fingerprint,omitempty"`
	NameFingerprint   string         `json:"name_fingerprint,omitempty"`
	ContentTypes      map[string]int `json:"content_types,omitempty"`
	ArgumentsBytes    int            `json:"arguments_bytes,omitempty"`
	OutputBytes       int            `json:"output_bytes,omitempty"`
	EncryptedBytes    int            `json:"encrypted_bytes,omitempty"`
}

type diagnosticToolShape struct {
	Index           int    `json:"index"`
	Type            string `json:"type"`
	Bytes           int    `json:"bytes"`
	NameFingerprint string `json:"name_fingerprint,omitempty"`
	ParametersBytes int    `json:"parameters_bytes,omitempty"`
	Properties      int    `json:"properties,omitempty"`
	Required        int    `json:"required,omitempty"`
	Strict          *bool  `json:"strict,omitempty"`
}

func diagnosticFingerprint(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:12])
}

func diagnosticValueType(v gjson.Result) string {
	switch {
	case !v.Exists():
		return "missing"
	case v.IsObject():
		return "object"
	case v.IsArray():
		return "array"
	case v.Type == gjson.String:
		return "string"
	case v.Type == gjson.Number:
		return "number"
	case v.Type == gjson.True || v.Type == gjson.False:
		return "boolean"
	default:
		return "null"
	}
}

func diagnosticValueBytes(v gjson.Result) int {
	if v.Type == gjson.String {
		return len(v.Str)
	}
	return len(v.Raw)
}

func diagnosticValueCount(v gjson.Result) int {
	if !v.IsArray() && !v.IsObject() {
		return 0
	}
	count := 0
	v.ForEach(func(_, _ gjson.Result) bool { count++; return count < diagnosticItemLimit })
	return count
}

func diagnosticRole(value string) string {
	switch value {
	case "", "system", "developer", "user", "assistant", "tool", "model", "function":
		return strings.Clone(value)
	default:
		return "other"
	}
}

func diagnosticControlValue(path string, v gjson.Result) any {
	switch path {
	case "stream", "store", "parallel_tool_calls":
		if v.Type == gjson.True || v.Type == gjson.False {
			return v.Bool()
		}
	case "temperature", "top_p", "max_tokens", "max_output_tokens", "max_completion_tokens", "n":
		if v.Type == gjson.Number && v.Num >= -1e9 && v.Num <= 1e9 {
			return v.Num
		}
	case "reasoning.effort", "tool_choice", "tool_choice.type":
		if v.Type == gjson.String {
			return diagnosticEnum(v.Str)
		}
	case "text.format.type", "response_format.type":
		if v.Type == gjson.String {
			switch v.Str {
			case "text", "json_object", "json_schema":
				return strings.Clone(v.Str)
			default:
				return "other"
			}
		}
	}
	return nil
}

func diagnosticItemSample(path string, index int, item gjson.Result) diagnosticItemShape {
	kind := diagnosticEnum(item.Get("type").String())
	if kind == "" {
		kind = "message"
	}
	s := diagnosticItemShape{Index: index, Path: path, Type: kind, Role: diagnosticRole(item.Get("role").String()), Bytes: len(item.Raw),
		CallIDFingerprint: diagnosticFingerprint(item.Get("call_id").String()), NameFingerprint: diagnosticFingerprint(item.Get("name").String()),
		ArgumentsBytes: diagnosticValueBytes(item.Get("arguments")), OutputBytes: diagnosticValueBytes(item.Get("output")), EncryptedBytes: diagnosticValueBytes(item.Get("encrypted_content"))}
	if s.CallIDFingerprint == "" {
		s.CallIDFingerprint = diagnosticFingerprint(item.Get("tool_call_id").String())
	}
	content := item.Get("content")
	if !content.Exists() {
		content = item.Get("parts")
	}
	if content.Type == gjson.String {
		s.ContentTypes = map[string]int{"text": 1}
	} else if content.IsArray() {
		s.ContentTypes = map[string]int{}
		count := 0
		content.ForEach(func(_, part gjson.Result) bool {
			kind := diagnosticEnum(part.Get("type").String())
			if kind == "" {
				if part.Get("text").Exists() {
					kind = "text"
				} else {
					kind = "other"
				}
			}
			s.ContentTypes[kind]++
			count++
			return count < 128
		})
	}
	return s
}

func populateDiagnosticRequestDetails(s *diagnosticRequestShape, root gjson.Result, body []byte) {
	s.Fingerprint = diagnosticFingerprint(string(body))
	s.ModelFingerprint = diagnosticFingerprint(root.Get("model").String())
	s.InstructionsBytes = diagnosticValueBytes(root.Get("instructions")) + diagnosticValueBytes(root.Get("system"))
	for _, path := range []string{"model", "input", "messages", "contents", "instructions", "system", "tools", "tool_choice", "tool_choice.type", "stream", "store", "parallel_tool_calls", "reasoning.effort", "temperature", "top_p", "max_tokens", "max_output_tokens", "max_completion_tokens", "text.format.type", "response_format.type", "previous_response_id", "include", "n"} {
		v := root.Get(path)
		if !v.Exists() {
			continue
		}
		s.Parameters = append(s.Parameters, diagnosticValueShape{Path: path, Type: diagnosticValueType(v), Bytes: diagnosticValueBytes(v), Items: diagnosticValueCount(v), Value: diagnosticControlValue(path, v)})
	}
	budget, total := diagnosticItemLimit, 0
	for _, path := range []string{"input", "messages", "contents"} {
		items := root.Get(path)
		if !items.IsArray() {
			continue
		}
		index := 0
		items.ForEach(func(_, item gjson.Result) bool {
			if budget == 0 {
				s.SamplesTruncated = true
				return false
			}
			budget--
			sample := diagnosticItemSample(path, index, item)
			if len(s.ItemSamples) < diagnosticItemSamples {
				s.ItemSamples = append(s.ItemSamples, sample)
			} else {
				copy(s.ItemSamples[diagnosticItemSamples/2:], s.ItemSamples[diagnosticItemSamples/2+1:])
				s.ItemSamples[diagnosticItemSamples-1] = sample
				s.SamplesTruncated = true
			}
			index++
			total++
			return true
		})
	}
	index := 0
	tools := root.Get("tools")
	if tools.IsArray() {
		tools.ForEach(func(_, item gjson.Result) bool {
			if budget == 0 {
				s.SamplesTruncated = true
				return false
			}
			budget--
			function := item.Get("function")
			if !function.IsObject() {
				function = item
			}
			params := function.Get("parameters")
			if !params.Exists() {
				params = item.Get("input_schema")
			}
			kind := diagnosticEnum(item.Get("type").String())
			if kind == "" && params.Exists() {
				kind = "function"
			}
			sample := diagnosticToolShape{Index: index, Type: kind, Bytes: len(item.Raw), NameFingerprint: diagnosticFingerprint(function.Get("name").String()), ParametersBytes: diagnosticValueBytes(params), Properties: diagnosticValueCount(params.Get("properties")), Required: diagnosticValueCount(params.Get("required"))}
			strict := function.Get("strict")
			if strict.Type == gjson.True || strict.Type == gjson.False {
				value := strict.Bool()
				sample.Strict = &value
			}
			if len(s.ToolSamples) < diagnosticToolSamples {
				s.ToolSamples = append(s.ToolSamples, sample)
			} else {
				copy(s.ToolSamples[diagnosticToolSamples/2:], s.ToolSamples[diagnosticToolSamples/2+1:])
				s.ToolSamples[diagnosticToolSamples-1] = sample
				s.SamplesTruncated = true
			}
			index++
			return true
		})
	}
	sort.Strings(s.Fields)
	// A structure-only fingerprint complements exact-body fingerprints without retaining text.
	shape, _ := json.Marshal(struct {
		Fields []string       `json:"fields"`
		Items  map[string]int `json:"items"`
		Tools  map[string]int `json:"tools"`
		Counts string         `json:"counts"`
	}{s.Fields, s.ItemTypes, s.ToolTypes, strconv.Itoa(total) + ":" + strconv.Itoa(index) + ":" + strconv.Itoa(s.UnknownFields)})
	s.ShapeFingerprint = diagnosticFingerprint(string(shape))
}
