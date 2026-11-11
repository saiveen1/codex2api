package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func installDiagnosticTestSink(t *testing.T) func() []diagnosticEvent {
	t.Helper()
	t.Setenv("LOG_DISABLED", "false")
	old := diagnosticLogger
	path := filepath.Join(t.TempDir(), "errors.jsonl")
	diagnosticLogger = &diagnosticFileLogger{path: path, maxBytes: diagnosticMaxFileSize, backups: 3}
	t.Cleanup(func() { closeDiagnosticLog(); diagnosticLogger = old })
	return func() []diagnosticEvent {
		t.Helper()
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("PRIVATE_SENTINEL")) {
			t.Fatalf("private content leaked: %s", data)
		}
		var events []diagnosticEvent
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var event diagnosticEvent
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		return events
	}
}

func diagnosticTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses?secret=PRIVATE_SENTINEL", nil)
	c.Request.Header.Set("Authorization", "Bearer PRIVATE_SENTINEL")
	attachUpstreamTrace(c, nil)
	setRawRequestBody(c, []byte(`{"model":"PRIVATE_SENTINEL","input":[{"role":"user","content":"PRIVATE_SENTINEL"}],"tools":[{"type":"function","name":"PRIVATE_SENTINEL","parameters":{"PRIVATE_SENTINEL":"PRIVATE_SENTINEL"}}]}`))
	return c
}

func TestDiagnosticRequestShapePrivacyAndBounds(t *testing.T) {
	body := []byte(`{"model":"PRIVATE_SENTINEL","PRIVATE_SENTINEL":"PRIVATE_SENTINEL","input":[{"type":"function_call_output","output":"PRIVATE_SENTINEL"},{"type":"reasoning","encrypted_content":"PRIVATE_SENTINEL"},{"type":"PRIVATE_SENTINEL","content":"PRIVATE_SENTINEL"}],"tools":[{"type":"function","name":"PRIVATE_SENTINEL"}],"tool_choice":{"type":"image_generation"},"reasoning":{"effort":"medium"},"previous_response_id":"PRIVATE_SENTINEL"}`)
	s := summarizeDiagnosticRequest(body)
	encoded, _ := json.Marshal(s)
	if bytes.Contains(encoded, []byte("PRIVATE_SENTINEL")) {
		t.Fatal(string(encoded))
	}
	if s.Inspection != "complete" || s.InputItems != 3 || s.Tools != 1 || s.MissingCallIDs != 1 || s.EncryptedItems != 1 || !s.ImageChoiceWithoutTool || s.UnknownFields != 1 {
		t.Fatalf("shape: %+v", s)
	}
	if got := summarizeDiagnosticRequest([]byte(`{"input":`)); got.Inspection != "invalid_json" {
		t.Fatal(got)
	}
	large := bytes.Repeat([]byte("x"), diagnosticBodyLimit+1)
	if got := summarizeDiagnosticRequest(large); got.Inspection != "size_limit" || got.Bytes != len(large) {
		t.Fatal(got)
	}
	items := []byte(`{"input":[` + strings.Repeat(`{},`, diagnosticItemLimit) + `{}]}`)
	if got := summarizeDiagnosticRequest(items); got.Inspection != "item_limit" || got.InputItems != diagnosticItemLimit {
		t.Fatal(got)
	}
}

func TestDiagnosticErrorPrivacy(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"PRIVATE_SENTINEL","code":"PRIVATE_SENTINEL","type":"PRIVATE_SENTINEL","param":"tools.PRIVATE_SENTINEL"},"request":{"input":"PRIVATE_SENTINEL"}}`,
		`<html>PRIVATE_SENTINEL</html>`, strings.Repeat("PRIVATE_SENTINEL", 1000),
	} {
		for _, result := range []any{summarizeDiagnosticError([]byte(body)), upstreamErrorLogBody([]byte(body)), upstreamErrorConsoleBody([]byte(body))} {
			data, _ := json.Marshal(result)
			if bytes.Contains(data, []byte("PRIVATE_SENTINEL")) {
				t.Fatal(string(data))
			}
		}
	}
	d := summarizeDiagnosticError([]byte(`{"detail":"Bad Request"}`))
	if d.Detail != "generic" || d.MessageClass != "generic_bad_request" || d.Code != "" {
		t.Fatal(d)
	}
	d = summarizeDiagnosticError([]byte(`{"response":{"error":{"code":"invalid_encrypted_content","type":"invalid_request_error","param":"input[3].encrypted_content","message":"PRIVATE_SENTINEL"}}}`))
	if d.Code != "invalid_encrypted_content" || d.Param != "input.3.encrypted_content" {
		t.Fatal(d)
	}
}

type diagnosticRoundTripper func(*http.Request) (*http.Response, error)

func (f diagnosticRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDiagnosticHTTP400CorrelationAndNoReplay(t *testing.T) {
	read := installDiagnosticTestSink(t)
	c := diagnosticTestContext()
	calls := 0
	body := []byte(`{"model":"PRIVATE_SENTINEL","tool_choice":{"type":"image_generation"},"tools":[]}`)
	client := &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, body) {
			t.Fatal("request changed")
		}
		return &http.Response{StatusCode: 400, Header: http.Header{"X-Request-Id": {"req_0123456789abcdef"}}, Body: io.NopCloser(strings.NewReader(`{"detail":"Bad Request"}`)), Request: r}, nil
	})}
	req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", "https://upstream.invalid", bytes.NewReader(body))
	resp, err := doTracedUpstreamRequest(client, req, &auth.Account{DBID: 3}, "http://PRIVATE_SENTINEL@proxy.invalid")
	if err != nil {
		t.Fatal(err)
	}
	got, err := readUpstreamErrorBody(c.Request.Context(), resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(got) != `{"detail":"Bad Request"}` || calls != 1 {
		t.Fatal("response changed or replayed", err, calls)
	}
	h := &Handler{}
	h.logUsageForRequest(c, &database.UsageLogInput{AccountID: 3, APIKeyID: 9, Endpoint: "/v1/responses", Model: "PRIVATE_SENTINEL", StatusCode: 400, AttemptIndex: 1})
	events := read()
	if len(events) != 2 {
		t.Fatalf("events: %+v", events)
	}
	e := events[1]
	if e.RequestID != c.Writer.Header().Get("X-Codex2API-Request-ID") || e.Attempt.Index != 1 || e.UpstreamRequestID != "req_0123456789abcdef" || e.Error.MessageClass != "generic_bad_request" || e.RetryScheduled || !e.Attempt.Request.ImageChoiceWithoutTool {
		t.Fatalf("event: %+v", e)
	}
}

func TestDiagnosticRetryStreamAndContinuationIsolation(t *testing.T) {
	read := installDiagnosticTestSink(t)
	c := diagnosticTestContext()
	h := &Handler{}
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 1}, "", false)(&http.Response{StatusCode: 200})
	noteDiagnosticErrorBody(c.Request.Context(), []byte(`{"response":{"error":{"code":"invalid_encrypted_content","message":"PRIVATE_SENTINEL"}}}`))
	first := snapshotUpstreamTrace(c.Request.Context())
	h.logUsageForRequest(c, &database.UsageLogInput{AccountID: 1, StatusCode: 400, Stream: true, IsRetryAttempt: true, AttemptIndex: 1, UpstreamErrorKind: "invalid_encrypted_content"})
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 2}, "", false)(&http.Response{StatusCode: 200})
	hidden := &database.UsageLogInput{AccountID: 1, StatusCode: 502, Stream: true, UpstreamErrorKind: "continue_thinking_error"}
	first.apply(hidden)
	h.logUsageForRequest(c, hidden)
	h.logUsageForRequest(c, &database.UsageLogInput{AccountID: 2, StatusCode: 200, Stream: true, AttemptIndex: 2})
	finishRequestDiagnostic(c, nil, false)
	events := read()
	if len(events) != 4 {
		t.Fatalf("events: %+v", events)
	}
	if events[0].Stage != "upstream_stream" || events[0].HTTPStatus != 200 || events[0].OutcomeStatus != 400 || !events[0].RetryScheduled {
		t.Fatal(events[0])
	}
	if events[1].Stage != "continuation" || events[1].AccountID != 1 || events[1].Attempt.Index != 1 {
		t.Fatal(events[1])
	}
	if events[2].Attempt.Index != 2 || events[2].Error.Code != "" || events[3].Outcome != "success" {
		t.Fatal(events[2:])
	}
}

func TestDiagnosticLocalRejectionsAndMiddlewareIdempotency(t *testing.T) {
	for _, status := range []int{400, 401, 413, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			read := installDiagnosticTestSink(t)
			r := gin.New()
			r.Use(RequestDiagnosticMiddleware(nil), RequestDiagnosticMiddleware(nil))
			r.POST("/v1/responses", func(c *gin.Context) {
				c.JSON(status, gin.H{"error": gin.H{"code": "invalid_request", "message": "PRIVATE_SENTINEL"}})
			})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader("PRIVATE_SENTINEL")))
			events := read()
			if w.Code != status || !strings.Contains(w.Body.String(), "PRIVATE_SENTINEL") || len(events) != 1 || events[0].OutcomeStatus != status || events[0].RequestID != w.Header().Get("X-Codex2API-Request-ID") {
				t.Fatalf("status=%d events=%+v", w.Code, events)
			}
		})
	}
}

func TestDiagnosticWebsocketIsolationAndParent(t *testing.T) {
	read := installDiagnosticTestSink(t)
	c := diagnosticTestContext()
	parent := snapshotUpstreamTrace(c.Request.Context()).RequestID
	other := c.Copy()
	other.Request = c.Request.Clone(c.Request.Context())
	resetUpstreamRequestTrace(c)
	resetUpstreamRequestTrace(other)
	first, second := snapshotUpstreamTrace(c.Request.Context()), snapshotUpstreamTrace(other.Request.Context())
	if first.RequestID == second.RequestID || first.ParentRequestID != parent || second.ParentRequestID != parent {
		t.Fatal(first, second)
	}
	noteDiagnosticWSError(c, api.NewAPIError(api.ErrCodeInvalidParameter, "PRIVATE_SENTINEL", api.ErrorTypeInvalidRequest))
	e := read()[0]
	if e.ParentRequestID != parent || e.RequestID != first.RequestID || e.Error.Code != "invalid_parameter" || !e.Websocket {
		t.Fatal(e)
	}
}

func TestDiagnosticTransportAndDisabled(t *testing.T) {
	read := installDiagnosticTestSink(t)
	for _, tc := range []struct {
		err  error
		want string
	}{{context.Canceled, "canceled"}, {context.DeadlineExceeded, "timeout"}, {&net.DNSError{Err: "PRIVATE_SENTINEL"}, "dns"}, {errors.New("PRIVATE_SENTINEL"), "transport"}} {
		if got := diagnosticTransportKind(tc.err); got != tc.want {
			t.Fatal(got)
		}
	}
	c := diagnosticTestContext()
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 1}, "", false)(nil)
	noteDiagnosticDispatch(c.Request.Context(), nil, context.DeadlineExceeded, 5, nil)
	if events := read(); len(events) != 1 || events[0].Kind != "timeout" {
		t.Fatal(events)
	}
	t.Setenv("LOG_DISABLED", "true")
	noteDiagnosticDispatch(c.Request.Context(), nil, errors.New("PRIVATE_SENTINEL"), 5, nil)
	if len(read()) != 1 {
		t.Fatal("disabled diagnostics wrote an event")
	}
}

func TestDiagnosticBusinessFailureAtHTTP200(t *testing.T) {
	read := installDiagnosticTestSink(t)
	c := diagnosticTestContext()
	(&Handler{}).logUsageForRequest(c, &database.UsageLogInput{StatusCode: 200, Stream: true, ErrorMessage: "PRIVATE_SENTINEL", UpstreamErrorKind: "empty_response"})
	finishRequestDiagnostic(c, nil, false)
	events := read()
	if len(events) != 2 || events[1].Outcome != "failure" || events[1].HTTPStatus != 200 {
		t.Fatal(events)
	}
}
