package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type diagnosticAttempt struct {
	Index           int                     `json:"index"`
	HTTPStatus      int                     `json:"http_status,omitempty"`
	Transport       string                  `json:"transport,omitempty"`
	Protocol        string                  `json:"protocol,omitempty"`
	RequestEncoding string                  `json:"request_encoding,omitempty"`
	RequestCapture  string                  `json:"request_capture,omitempty"`
	RequestBytes    int64                   `json:"request_bytes"`
	Request         *diagnosticRequestShape `json:"request,omitempty"`
	Error           diagnosticError         `json:"error"`
}

type diagnosticEvent struct {
	Schema            int                     `json:"schema"`
	Time              string                  `json:"time"`
	Event             string                  `json:"event"`
	Stage             string                  `json:"stage"`
	Summary           string                  `json:"summary,omitempty"`
	CaptureStatus     string                  `json:"capture_status,omitempty"`
	RequestID         string                  `json:"request_id"`
	ParentRequestID   string                  `json:"parent_request_id,omitempty"`
	UpstreamRequestID string                  `json:"upstream_request_id,omitempty"`
	AccountID         int64                   `json:"account_id,omitempty"`
	ProxyID           int64                   `json:"proxy_id,omitempty"`
	APIKeyID          int64                   `json:"api_key_id,omitempty"`
	Endpoint          string                  `json:"endpoint,omitempty"`
	ModelFingerprint  string                  `json:"model_fingerprint,omitempty"`
	Attempt           diagnosticAttempt       `json:"attempt"`
	UsageAttempt      int                     `json:"usage_attempt,omitempty"`
	RetryScheduled    bool                    `json:"retry_scheduled"`
	HTTPStatus        int                     `json:"downstream_http_status,omitempty"`
	OutcomeStatus     int                     `json:"outcome_status,omitempty"`
	Outcome           string                  `json:"outcome,omitempty"`
	Kind              string                  `json:"kind,omitempty"`
	Stream            bool                    `json:"stream"`
	Websocket         bool                    `json:"websocket"`
	DurationMs        int64                   `json:"duration_ms"`
	FirstTokenMs      int                     `json:"first_token_ms,omitempty"`
	Request           *diagnosticRequestShape `json:"request,omitempty"`
	Error             diagnosticError         `json:"error"`
	Decision          *dispatchDecision       `json:"decision,omitempty"`
}

var diagnosticIDPattern = regexp.MustCompile(`^(?:[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}|req_[a-fA-F0-9]{8,64})$`)

func diagnosticID(s string) string {
	if s == "" || diagnosticIDPattern.MatchString(s) {
		return s
	}
	return "sha256:" + diagnosticFingerprint(s)
}

func diagnosticEndpoint(s string) string {
	for _, known := range []string{"/responses/compact", "/responses/input_tokens", "/chat/completions", "/messages/count_tokens", "/images/generations", "/images/edits", "/videos/generations", "/videos/edits", "/videos/extensions", "/responses", "/messages", "/realtime", "/alpha/search", "/models", "/live"} {
		if s == known || s == "/v1"+known || s == "/backend-api/codex"+known {
			return known
		}
	}
	return "other"
}

func diagnosticBase(ctx context.Context, event, stage string) diagnosticEvent {
	s := snapshotUpstreamTrace(ctx)
	return diagnosticEvent{Schema: 2, Time: time.Now().UTC().Format(time.RFC3339Nano), Event: event, Stage: stage,
		RequestID: s.RequestID, ParentRequestID: diagnosticID(s.ParentRequestID), UpstreamRequestID: diagnosticID(s.UpstreamRequestID), AccountID: s.accountID, ProxyID: s.Proxy.ID,
		Attempt: s.Diagnostic, Error: diagnosticError{Detail: "unavailable"}}
}

func diagnosticTransportKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var cert *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &cert) || errors.As(err, &unknown) {
		return "tls"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "transport"
}

// Called after dispatch, never reads the live request/response streams.
func noteDiagnosticDispatch(ctx context.Context, resp *http.Response, err error, requestBytes int64, body []byte) {
	a := upstreamTraceFromContext(ctx)
	if a == nil {
		return
	}
	failed := err != nil || (resp != nil && resp.StatusCode >= 400)
	upstreamStatus, upstreamBody, structured := continuousRetryHTTPErrorDetails(err)
	var shape *diagnosticRequestShape
	if body != nil {
		shape = summarizeDiagnosticRequest(body)
	}
	a.mu.Lock()
	if a.current != nil {
		if a.current.diagnostic.Request == nil && shape != nil {
			a.current.diagnostic.RequestBytes = requestBytes
			a.current.diagnostic.Request = shape
			a.current.diagnostic.RequestEncoding = "identity"
			a.current.diagnostic.RequestCapture = "adapter_input"
		}
		if structured {
			a.current.diagnostic.HTTPStatus = upstreamStatus
			a.current.diagnostic.Error = summarizeDiagnosticError(upstreamBody)
			a.current.diagnostic.Error.Source = "upstream_handshake"
		} else if err != nil {
			a.current.diagnostic.Transport = diagnosticTransportKind(err)
		}
	}
	if failed {
		a.diagnosticFailed = true
	}
	a.mu.Unlock()
	if !failed {
		return
	}
	e := diagnosticBase(ctx, "upstream_attempt", "upstream_http")
	if err != nil && !structured {
		e.Stage = "upstream_transport"
		e.Kind = diagnosticTransportKind(err)
	}
	writeDiagnosticEvent(e)
}

func noteDiagnosticHTTPDispatch(req *http.Request, resp *http.Response, err error) {
	if snapshotUpstreamTrace(req.Context()).Diagnostic.Request == nil {
		noteDiagnosticHTTPRequest(req)
	}
	noteDiagnosticDispatch(req.Context(), resp, err, req.ContentLength, nil)
}

func noteDiagnosticErrorBody(ctx context.Context, body []byte) {
	noteDiagnosticErrorEvidence(ctx, body, "upstream_http", nil)
}

func diagnosticIngress(c *gin.Context) *diagnosticRequestShape {
	body, _ := rawRequestBodyFromContext(c)
	body = ingressRequestBody(c, body)
	if body != nil {
		return summarizeDiagnosticRequest(body)
	}
	return &diagnosticRequestShape{Bytes: int(c.Request.ContentLength), Inspection: "unavailable"}
}

func logUsageDiagnostic(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	a := upstreamTraceFromContext(c.Request.Context())
	if a == nil {
		return
	}
	failed := input.StatusCode >= 400 || input.ErrorMessage != "" || input.UpstreamErrorKind != ""
	a.mu.Lock()
	previousFailure := a.diagnosticFailed
	a.diagnosticUsageLogged = true
	if failed {
		a.diagnosticFailed = true
	}
	currentAttempt := 0
	if a.current != nil {
		currentAttempt = a.current.diagnostic.Index
	}
	if !input.IsRetryAttempt && input.TraceAttemptIndex == currentAttempt {
		a.diagnosticOutcome = input.StatusCode
		a.diagnosticFinal = "success"
		if failed {
			a.diagnosticFinal = "failure"
		}
	}
	a.mu.Unlock()
	if !failed && !previousFailure {
		return
	}
	e := diagnosticBase(c.Request.Context(), "attempt_outcome", "upstream_response")
	// Continuation snapshots and account switches must not inherit the latest attempt.
	var held diagnosticEvent
	if input.DiagnosticContext != "" && json.Unmarshal([]byte(input.DiagnosticContext), &held) == nil && held.Schema == 2 && held.RequestID == input.RequestID && held.AccountID == input.AccountID && held.Attempt.Index == input.TraceAttemptIndex {
		e.Attempt, e.AccountID = held.Attempt, held.AccountID
	}
	if input.TraceAttemptIndex != e.Attempt.Index || input.AccountID != e.AccountID {
		e.Attempt = diagnosticAttempt{Index: input.TraceAttemptIndex, HTTPStatus: input.TraceHTTPStatus, RequestBytes: -1}
	}
	e.RequestID = input.RequestID
	if input.ParentRequestID != "" {
		e.ParentRequestID = diagnosticID(input.ParentRequestID)
	}
	e.UpstreamRequestID = diagnosticID(input.UpstreamRequestID)
	e.AccountID, e.ProxyID, e.APIKeyID = input.AccountID, input.UpstreamProxyID, input.APIKeyID
	e.Endpoint = diagnosticEndpoint(input.Endpoint)
	e.ModelFingerprint = diagnosticFingerprint(input.Model)
	e.UsageAttempt, e.RetryScheduled = input.AttemptIndex, input.IsRetryAttempt
	e.HTTPStatus, e.OutcomeStatus = c.Writer.Status(), input.StatusCode
	e.Kind = diagnosticErrorCode(input.UpstreamErrorKind)
	e.Stream, e.Websocket = input.Stream, input.ViaWebsocket
	e.DurationMs, e.FirstTokenMs = int64(input.DurationMs), input.FirstTokenMs
	e.Request = diagnosticIngress(c)
	e.Error = e.Attempt.Error
	if e.Error.Detail == "" || e.Error.Detail == "unavailable" {
		e.Error = summarizeDiagnosticError([]byte(input.ErrorMessage))
		e.Error.Source = "usage_fallback"
		if code, _, ok := strings.Cut(input.ErrorMessage, " · "); ok && diagnosticErrorCode(code) != "other" {
			e.Error.Code = diagnosticErrorCode(code)
		}
	}
	switch {
	case input.UpstreamErrorKind == "continue_thinking_error":
		e.Stage = "continuation"
	case e.Attempt.Transport != "" && e.Attempt.HTTPStatus == 0:
		e.Stage = "upstream_transport"
	case e.Error.Source == "upstream_stream":
		e.Stage = "upstream_stream"
	case e.Attempt.HTTPStatus >= 400:
		e.Stage = "upstream_http"
	case input.Stream || input.ViaWebsocket:
		e.Stage = "upstream_stream"
	case input.AccountID == 0:
		e.Stage = "local"
	}
	e.Outcome = "success"
	if failed {
		e.Outcome = "failure"
	}
	saveDiagnosticUsageContext(input, e)
}

type diagnosticResponseWriter struct {
	gin.ResponseWriter
	errorBody []byte
	overflow  bool
}

func (w *diagnosticResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *diagnosticResponseWriter) capture(p []byte) {
	if w.Status() < 400 || w.overflow {
		return
	}
	if len(w.errorBody)+len(p) > diagnosticErrorLimit {
		w.errorBody = nil
		w.overflow = true
		return
	}
	w.errorBody = append(w.errorBody, p...)
}
func (w *diagnosticResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.capture(p[:n])
	return n, err
}
func (w *diagnosticResponseWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	w.capture([]byte(s[:n]))
	return n, err
}

// RequestDiagnosticMiddleware precedes body limits and rate limiting, but excludes admin/UI routes.
func RequestDiagnosticMiddleware(store *auth.Store, databases ...*database.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v1beta/") || strings.HasPrefix(path, "/backend-api/codex/") || diagnosticEndpoint(path) != "other" {
			attachUpstreamTrace(c, store)
			if len(databases) > 0 {
				a := upstreamTraceFromContext(c.Request.Context())
				a.diagnosticDB = databases[0]
			}
			defer beginRequestDiagnostic(c)()
		}
		c.Next()
	}
}

func beginRequestDiagnostic(c *gin.Context) func() {
	if c.GetBool("diagnostic_active") {
		return func() {}
	}
	c.Set("diagnostic_active", true)
	writer := &diagnosticResponseWriter{ResponseWriter: c.Writer}
	c.Writer = writer
	return func() {
		if recovered := recover(); recovered != nil {
			e := diagnosticBase(c.Request.Context(), "request_end", "panic")
			e.Outcome, e.OutcomeStatus = "failure", 500
			writeDiagnosticEvent(e)
			persistDiagnosticOnlyUsage(c, e)
			panic(recovered)
		}
		finishRequestDiagnostic(c, writer.errorBody, writer.overflow)
	}
}

func finishRequestDiagnostic(c *gin.Context, errorBody []byte, overflow bool) {
	if c == nil || c.Request == nil {
		return
	}
	a := upstreamTraceFromContext(c.Request.Context())
	if a == nil {
		return
	}
	a.mu.Lock()
	failed, outcome, started, final := a.diagnosticFailed, a.diagnosticOutcome, a.started, a.diagnosticFinal
	decision, dispatchStream := a.dispatchDecision, a.dispatchStream
	a.mu.Unlock()
	status := c.Writer.Status()
	if !failed && status < 400 && decision == nil {
		return
	}
	e := diagnosticBase(c.Request.Context(), "request_end", "completion")
	e.Endpoint = diagnosticEndpoint(c.FullPath())
	if e.Endpoint == "other" {
		e.Endpoint = diagnosticEndpoint(c.Request.URL.Path)
	}
	e.HTTPStatus, e.OutcomeStatus = status, outcome
	e.DurationMs = time.Since(started).Milliseconds()
	e.Request = diagnosticIngress(c)
	e.Error = summarizeDiagnosticError(errorBody)
	e.Error.Source = "downstream_response"
	if e.Request != nil {
		e.Stream = e.Request.Stream
	}
	e.Websocket = e.Attempt.Protocol == "websocket"
	if e.Attempt.Error.Source != "" {
		e.Error = e.Attempt.Error
	}
	if overflow && e.Error.Source == "downstream_response" {
		e.Error.Detail = "size_limit"
	}
	if status >= 400 {
		e.OutcomeStatus = status
		if e.Attempt.HTTPStatus >= 400 {
			e.Stage = "upstream_http"
		} else if e.Attempt.Transport != "" {
			e.Stage = "upstream_transport"
		} else if e.Error.Source == "upstream_stream" {
			e.Stage = "upstream_stream"
		} else {
			e.Stage = "local_or_gateway"
		}
	}
	e.Outcome = final
	if e.Outcome == "" {
		e.Outcome = "unknown"
	}
	if status >= 400 {
		e.Outcome = "failure"
	}
	if e.Error.Type == "authentication_error" {
		e.Stage = "authentication"
	}
	if decision != nil {
		e.Decision, e.Stream, e.APIKeyID = decision, dispatchStream, requestAPIKeyID(c)
		e.Stage, e.Outcome = "dispatch", "failure"
		if e.AccountID == 0 {
			e.AccountID = decision.SelectedAccountID
		}
		if status < 400 {
			e.OutcomeStatus = decision.status
		}
		if !decision.Attempted && e.Attempt.Error.Source == "" {
			e.Error.Source = "gateway_dispatch"
			e.Error.Code = decision.Reason
			e.Error.Detail = "classified"
		}
	}
	writeDiagnosticEvent(e)
	persistDiagnosticOnlyUsage(c, e)
}

func noteDiagnosticWSError(c *gin.Context, err *api.APIError) {
	if err == nil || c == nil || c.Request == nil {
		return
	}
	a := upstreamTraceFromContext(c.Request.Context())
	if a == nil {
		return
	}
	a.mu.Lock()
	a.diagnosticFailed = true
	a.diagnosticFinal = "failure"
	a.diagnosticOutcome = api.HTTPStatusCode(err.Code)
	a.mu.Unlock()
	e := diagnosticBase(c.Request.Context(), "websocket_error", "websocket_local_or_gateway")
	e.Websocket, e.Outcome, e.OutcomeStatus = true, "failure", api.HTTPStatusCode(err.Code)
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": string(err.Code), "type": string(err.Type)}})
	e.Error = summarizeDiagnosticError(body)
	e.Error.Source = "websocket_gateway"
	e.Error.BodyFormat, e.Error.BodyBytes, e.Error.BodyFingerprint = "api_error", 0, ""
	e.Error.MessagePresent, e.Error.MessageBytes = err.Message != "", len(err.Message)
	e.Error.MessageClass = diagnosticMessageClass(err.Message)
	e.Error.Fingerprint = diagnosticFingerprint(string(err.Code) + "\x00" + err.Message)
	e.Error.EvidenceTruncated = err.Details != nil
	e.Request = diagnosticIngress(c)
	writeDiagnosticEvent(e)
	persistDiagnosticOnlyUsage(c, e)
}

// Legacy logs keep their format but no longer dump arbitrary structured bodies.
func diagnosticLegacyBody(body []byte, limit int) string {
	if len(body) > limit {
		return `{"detail":"size_limit"} ... [truncated]`
	}
	if len(strings.TrimSpace(string(body))) > 0 && !json.Valid(body) {
		return "[non-JSON upstream body omitted]"
	}
	data, _ := json.Marshal(summarizeDiagnosticError(body))
	return string(data)
}
