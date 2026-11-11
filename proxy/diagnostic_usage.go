package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func compactDiagnosticRequest(s *diagnosticRequestShape) *diagnosticRequestShape {
	if s == nil {
		return nil
	}
	copy := *s
	copy.ItemSamples, copy.ToolSamples, copy.Parameters = nil, nil, nil
	copy.SamplesTruncated = true
	return &copy
}

func encodeDiagnosticEvent(e diagnosticEvent) string {
	e.Schema = 2
	status := e.OutcomeStatus
	if status == 0 {
		status = e.Attempt.HTTPStatus
	}
	if e.Summary == "" {
		if e.Outcome == "success" {
			e.Summary = "Request recovered after an earlier failed attempt"
		} else if e.Attempt.Transport != "" {
			e.Summary = e.Stage + " · " + e.Attempt.Transport + "; no provider explanation is inferred from a transport failure"
		} else {
			e.Summary = e.Stage + " · " + diagnosticErrorSummary(status, e.Error)
		}
	}
	if e.CaptureStatus == "" {
		e.CaptureStatus = "complete"
		if e.Error.Detail == "unavailable" && e.Attempt.Transport == "" {
			e.CaptureStatus = "unavailable"
		}
		if e.Error.Detail == "size_limit" || e.Error.ReadError != "" || e.Error.EvidenceTruncated {
			e.CaptureStatus = "partial"
		}
		for _, shape := range []*diagnosticRequestShape{e.Request, e.Attempt.Request} {
			if shape != nil && (shape.Inspection != "complete" || shape.SamplesTruncated) {
				e.CaptureStatus = "partial"
			}
		}
		if e.Attempt.Index > 0 && (e.Attempt.Request == nil || e.Attempt.RequestCapture == "adapter_input") {
			e.CaptureStatus = "partial"
		}
	}
	data, err := json.Marshal(e)
	if err != nil {
		return database.NormalizeUsageDiagnosticContext("invalid")
	}
	if len(data) > database.MaxUsageDiagnosticContextBytes {
		e.CaptureStatus = "size_limit"
		e.Request = compactDiagnosticRequest(e.Request)
		e.Attempt.Request = compactDiagnosticRequest(e.Attempt.Request)
		data, _ = json.Marshal(e)
	}
	if len(data) > database.MaxUsageDiagnosticContextBytes {
		e.Error.Evidence, e.Attempt.Error.Evidence = nil, nil
		e.Error.EvidenceTruncated, e.Attempt.Error.EvidenceTruncated = true, true
		data, _ = json.Marshal(e)
	}
	return database.NormalizeUsageDiagnosticContext(string(data))
}

func writeDiagnosticEvent(e diagnosticEvent) {
	writeDiagnostic(json.RawMessage(encodeDiagnosticEvent(e)))
}

func applyDiagnosticTraceSnapshot(s upstreamTraceSnapshot, input *database.UsageLogInput) {
	if input.StatusCode < 400 && input.ErrorMessage == "" && input.UpstreamErrorKind == "" {
		return
	}
	input.DiagnosticContext = encodeDiagnosticEvent(diagnosticEvent{Schema: 2, RequestID: s.RequestID, ParentRequestID: diagnosticID(s.ParentRequestID), AccountID: s.accountID, UpstreamRequestID: diagnosticID(s.UpstreamRequestID), ProxyID: s.Proxy.ID, Attempt: s.Diagnostic, Error: s.Diagnostic.Error})
}

func saveDiagnosticUsageContext(input *database.UsageLogInput, e diagnosticEvent) {
	input.DiagnosticContext = encodeDiagnosticEvent(e)
	message := strings.TrimSpace(input.ErrorMessage)
	if e.Outcome == "failure" && (message == "" || message == fmt.Sprintf("HTTP %d", input.StatusCode)) {
		input.ErrorMessage = diagnosticErrorSummary(input.StatusCode, e.Error)
		if e.Attempt.Transport != "" {
			input.ErrorMessage = e.Stage + " · " + e.Attempt.Transport
		}
	}
	writeDiagnostic(json.RawMessage(input.DiagnosticContext))
}

// Early rejects have no inference usage call. Store a zero-token, zero-billing row once,
// using the existing bounded usage buffer/retention rather than a second unbounded log store.
func persistDiagnosticOnlyUsage(c *gin.Context, e diagnosticEvent) {
	a := upstreamTraceFromContext(c.Request.Context())
	if a == nil {
		return
	}
	a.mu.Lock()
	db, logged := a.diagnosticDB, a.diagnosticUsageLogged
	if db != nil && !logged {
		a.diagnosticUsageLogged = true
	}
	a.mu.Unlock()
	if db == nil || logged || e.Outcome != "failure" || e.OutcomeStatus < 400 {
		return
	}
	input := &database.UsageLogInput{RequestID: e.RequestID, ParentRequestID: e.ParentRequestID, Endpoint: e.Endpoint,
		StatusCode: e.OutcomeStatus, DurationMs: int(e.DurationMs), Stream: e.Stream, ViaWebsocket: e.Websocket,
		UpstreamErrorKind: e.Stage, ErrorMessage: diagnosticErrorSummary(e.OutcomeStatus, e.Error), DiagnosticContext: encodeDiagnosticEvent(e)}
	populateAPIKeyMetaFromContext(c, input)
	if err := db.InsertUsageLog(context.Background(), input); err != nil {
		log.Print("failed to enqueue local request diagnostic")
	}
}
