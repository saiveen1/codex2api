package database

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// MaxUsageDiagnosticContextBytes bounds the serialized diagnostic context stored
// with a usage log. The payload is opaque to the database layer.
const MaxUsageDiagnosticContextBytes = 24 * 1024

// NormalizeUsageDiagnosticContext preserves valid JSON byte-for-byte, without
// retaining the caller's backing storage. Rejected payloads become small JSON
// status objects; raw diagnostic data is never included or partially truncated.
func NormalizeUsageDiagnosticContext(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) > MaxUsageDiagnosticContextBytes {
		return `{"schema":2,"capture_status":"size_limit"}`
	}
	if !utf8.ValidString(raw) || !json.Valid([]byte(raw)) {
		return `{"schema":2,"capture_status":"invalid"}`
	}
	return strings.Clone(raw)
}
