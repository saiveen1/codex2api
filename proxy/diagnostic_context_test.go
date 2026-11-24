package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func decodeUsageDiagnostic(t *testing.T, input *database.UsageLogInput) diagnosticEvent {
	t.Helper()
	var e diagnosticEvent
	if err := json.Unmarshal([]byte(input.DiagnosticContext), &e); err != nil {
		t.Fatalf("decode diagnostic: %v", err)
	}
	if e.Schema != 2 || e.Summary == "" {
		t.Fatalf("incomplete event: %+v", e)
	}
	if strings.Contains(input.DiagnosticContext, "PRIVATE_SENTINEL") {
		t.Fatal("private content persisted")
	}
	if len(input.DiagnosticContext) > database.MaxUsageDiagnosticContextBytes {
		t.Fatal("diagnostic exceeds storage bound")
	}
	return e
}

func TestDiagnosticErrorEvidenceDistinguishesMissingFromRedacted(t *testing.T) {
	for _, tc := range []struct {
		name, body, detail, class string
		message                   bool
	}{
		{"generic detail", `{"detail":"Bad Request"}`, "generic", "generic_bad_request", true},
		{"detail object", `{"detail":{"code":"invalid_parameter","message":"PRIVATE_SENTINEL","param":"input[2].content"}}`, "structured", "message_omitted", true},
		{"validation list", `{"detail":[{"type":"missing","loc":["body","input"],"msg":"Field required","input":"PRIVATE_SENTINEL"}]}`, "structured", "missing_required_parameter", true},
		{"code only", `{"error":{"code":"insufficient_quota"}}`, "structured", "", false},
		{"html", `<html>PRIVATE_SENTINEL</html>`, "unstructured_omitted", "message_omitted", true},
		{"missing", ``, "unavailable", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := summarizeDiagnosticError([]byte(tc.body))
			if d.Detail != tc.detail || d.MessageClass != tc.class || d.MessagePresent != tc.message {
				t.Fatalf("evidence=%+v", d)
			}
			data, _ := json.Marshal(d)
			if bytes.Contains(data, []byte("PRIVATE_SENTINEL")) {
				t.Fatal("raw text leaked")
			}
			if tc.body != "" && (d.BodyBytes == 0 || d.BodyFingerprint == "") {
				t.Fatal("missing receipt")
			}
		})
	}
	d := summarizeDiagnosticError([]byte(`{"error":{"code":"PRIVATE_SENTINEL","type":"PRIVATE_SENTINEL","param":"PRIVATE_SENTINEL","message":"PRIVATE_SENTINEL"},"PRIVATE_SENTINEL":{"value":"PRIVATE_SENTINEL"}}`))
	if !d.CodeRedacted || !d.TypeRedacted || !d.ParamRedacted || d.CodeFingerprint == "" || len(d.Evidence) == 0 {
		t.Fatal("unknown fields silently disappeared")
	}
	data, _ := json.Marshal(d)
	if bytes.Contains(data, []byte("PRIVATE_SENTINEL")) {
		t.Fatal("unknown field names leaked")
	}
	if got := usageLogErrorMessage(400, []byte(`{"detail":"Bad Request"}`)); got == "HTTP 400" || !strings.Contains(got, "Bad Request") {
		t.Fatal(got)
	}
	if got := usageLogErrorMessage(429, []byte(`{"error":{"code":"insufficient_quota"}}`)); !strings.Contains(got, "insufficient_quota") {
		t.Fatal(got)
	}
}

func TestDiagnosticSubmittedHTTPContextAcrossFailures(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			read := installDiagnosticTestSink(t)
			plain := []byte(`{"model":"PRIVATE_SENTINEL","input":[{"type":"function_call_output","call_id":"PRIVATE_SENTINEL","output":"PRIVATE_SENTINEL"}],"tools":[{"type":"function","name":"PRIVATE_SENTINEL","parameters":{"type":"object","properties":{"PRIVATE_SENTINEL":{"type":"string"}}}}],"stream":true,"temperature":0.2}`)
			wire := plain
			if encoding == "gzip" {
				var b bytes.Buffer
				w := gzip.NewWriter(&b)
				_, _ = w.Write(plain)
				_ = w.Close()
				wire = b.Bytes()
			}
			if encoding == "zstd" {
				w, err := zstd.NewWriter(nil)
				if err != nil {
					t.Fatal(err)
				}
				wire = w.EncodeAll(plain, nil)
				w.Close()
			}
			for _, status := range []int{400, 401, 403, 404, 413, 429, 500, 503} {
				c := diagnosticTestContext()
				account := &auth.Account{DBID: 3}
				calls := 0
				client := &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					actual, _ := io.ReadAll(r.Body)
					if !bytes.Equal(actual, wire) {
						t.Fatal("diagnostic modified request bytes")
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"detail":"Bad Request"}`)), Request: r}, nil
				})}
				req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", "https://upstream.invalid/responses", bytes.NewReader(wire))
				req.Header.Set("Content-Encoding", encoding)
				resp, err := doTracedUpstreamRequest(client, req, account, "")
				if err != nil {
					t.Fatal(err)
				}
				_, err = readUpstreamErrorBody(c.Request.Context(), resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				input := &database.UsageLogInput{AccountID: 3, Endpoint: "/v1/responses", StatusCode: status, Stream: true, ErrorMessage: fmt.Sprintf("HTTP %d", status), UpstreamErrorKind: "client"}
				(&Handler{}).logUsageForRequest(c, input)
				e := decodeUsageDiagnostic(t, input)
				if calls != 1 || e.Attempt.Request == nil || e.Attempt.Request.Inspection != "complete" || e.Attempt.Request.Fingerprint != diagnosticFingerprint(string(plain)) || e.Attempt.RequestEncoding != encoding || e.Attempt.RequestBytes != int64(len(wire)) {
					t.Fatalf("bad submitted context: %+v", e.Attempt)
				}
				if e.Request.Fingerprint == e.Attempt.Request.Fingerprint || e.Error.Source != "upstream_http" || e.Error.MessageClass != "generic_bad_request" || input.ErrorMessage == fmt.Sprintf("HTTP %d", status) {
					t.Fatal("ingress/outbound or error context lost")
				}
			}
			if len(read()) != 16 {
				t.Fatal("unexpected extra attempts or missing diagnostics")
			}
		})
	}
}

func TestDiagnosticHTTP200StreamFailureRetainsSubmittedContext(t *testing.T) {
	installDiagnosticTestSink(t)
	c := diagnosticTestContext()
	request := []byte(`{"input":"PRIVATE_SENTINEL","stream":true}`)
	payload := `{"type":"response.failed","response":{"error":{"code":"invalid_encrypted_content","message":"could not be decrypted or decoded PRIVATE_SENTINEL"}}}`
	client := &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + payload + "\n\n")), Request: r}, nil
	})}
	req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", "https://upstream.invalid", bytes.NewReader(request))
	resp, err := doTracedUpstreamRequest(client, req, &auth.Account{DBID: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err = readSSEStreamWithContinuousRetryKeepalive(c.Request.Context(), resp.Body, func(string, []byte) bool { return true }); err != nil {
		t.Fatal(err)
	}
	input := &database.UsageLogInput{AccountID: 1, StatusCode: 400, Stream: true, UpstreamErrorKind: "invalid_encrypted_content"}
	(&Handler{}).logUsageForRequest(c, input)
	e := decodeUsageDiagnostic(t, input)
	if e.Attempt.HTTPStatus != 200 || e.Stage != "upstream_stream" || e.Error.Source != "upstream_stream" || e.Error.Code != "invalid_encrypted_content" || e.Attempt.Request.Fingerprint != diagnosticFingerprint(string(request)) {
		t.Fatalf("stream context: %+v", e)
	}
}

func TestDiagnosticOldAttemptSnapshotKeepsOwnSubmittedContext(t *testing.T) {
	installDiagnosticTestSink(t)
	c := diagnosticTestContext()
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 1}, "", true)(&http.Response{StatusCode: 200})
	first := []byte(`{"input":"first PRIVATE_SENTINEL"}`)
	RecordDiagnosticWebsocketRequest(c.Request.Context(), first)
	noteDiagnosticStreamEvent(c.Request.Context(), "error", []byte(`{"error":{"code":"invalid_parameter","message":"PRIVATE_SENTINEL"}}`))
	old := snapshotUpstreamTrace(c.Request.Context())
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 2}, "", true)(&http.Response{StatusCode: 200})
	RecordDiagnosticWebsocketRequest(c.Request.Context(), []byte(`{"input":"second PRIVATE_SENTINEL"}`))
	input := &database.UsageLogInput{AccountID: 1, StatusCode: 400, UpstreamErrorKind: "continue_thinking_error"}
	old.apply(input)
	(&Handler{}).logUsageForRequest(c, input)
	e := decodeUsageDiagnostic(t, input)
	if e.AccountID != 1 || e.Attempt.Index != 1 || e.Attempt.Request == nil || e.Attempt.Request.Fingerprint != diagnosticFingerprint(string(first)) || e.Error.Code != "invalid_parameter" {
		t.Fatal("later attempt replaced the held context")
	}
}

func TestDiagnosticLocalRejectPersistedOnceWithoutBilling(t *testing.T) {
	installDiagnosticTestSink(t)
	for _, tc := range []struct {
		name, path, remoteAddr, forwardedFor, wantIP string
		status                                       int
	}{
		{"body rejected", "/v1/responses", "203.0.113.10:1234", "", "203.0.113.10", http.StatusRequestEntityTooLarge},
		{"unmatched route", "/v1/unknown", "192.0.2.1:1234", "203.0.113.20", "203.0.113.20", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "diag.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			r := gin.New()
			if err := r.SetTrustedProxies([]string{"192.0.2.1"}); err != nil {
				t.Fatal(err)
			}
			r.Use(RequestDiagnosticMiddleware(nil, db), RequestDiagnosticMiddleware(nil, db))
			r.POST("/v1/responses", func(c *gin.Context) {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"code": "invalid_request_error", "message": "PRIVATE_SENTINEL"}})
			})
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.forwardedFor != "" {
				req.Header.Set("X-Forwarded-For", tc.forwardedFor)
			}
			r.ServeHTTP(w, req)
			db.FlushUsageLogs()
			rows, err := db.ListRecentUsageLogs(context.Background(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].StatusCode != tc.status || rows[0].TotalTokens != 0 || rows[0].AccountBilled != 0 || rows[0].UserBilled != 0 {
				t.Fatalf("unexpected local rows: %+v", rows)
			}
			if got := rows[0].ClientIP; got != tc.wantIP {
				t.Fatalf("client IP = %q, want %q", got, tc.wantIP)
			}
			var e diagnosticEvent
			if err = json.Unmarshal([]byte(rows[0].DiagnosticContext), &e); err != nil {
				t.Fatal(err)
			}
			if e.Attempt.Index != 0 || e.Error.Source != "downstream_response" || e.RequestID != w.Header().Get("X-Codex2API-Request-ID") {
				t.Fatal("local rejection lost origin")
			}
		})
	}
}

func TestDiagnosticCaptureFailuresAreExplicit(t *testing.T) {
	installDiagnosticTestSink(t)
	for _, tc := range []struct{ name, encoding, body, want string }{
		{"unknown encoding", "PRIVATE_SENTINEL", `{}`, "unsupported_encoding"},
		{"broken compressed body", "zstd", `PRIVATE_SENTINEL`, "decode_error"},
		{"invalid plain JSON", "identity", `{broken`, "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := diagnosticTestContext()
			beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 1}, "", false)(nil)
			req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", "https://upstream.invalid", strings.NewReader(tc.body))
			req.Header.Set("Content-Encoding", tc.encoding)
			noteDiagnosticHTTPRequest(req)
			s := snapshotUpstreamTrace(c.Request.Context())
			if s.Diagnostic.Request.Inspection != tc.want {
				t.Fatalf("capture=%+v", s.Diagnostic)
			}
		})
	}
	c := diagnosticTestContext()
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 1}, "", false)(nil)
	noteDiagnosticErrorEvidence(c.Request.Context(), []byte(`{"detail":"Bad Request"}`), "upstream_http", io.ErrUnexpectedEOF)
	if s := snapshotUpstreamTrace(c.Request.Context()); s.Diagnostic.Error.ReadError != "eof" {
		t.Fatal("read failure hidden")
	}
}

func TestDiagnosticFileDisabledStillPreservesUsageEvidence(t *testing.T) {
	read := installDiagnosticTestSink(t)
	t.Setenv("LOG_DISABLED", "true")
	c := diagnosticTestContext()
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 1}, "", false)(nil)
	noteDiagnosticDispatch(c.Request.Context(), nil, errors.New("PRIVATE_SENTINEL"), 0, nil)
	input := &database.UsageLogInput{AccountID: 1, StatusCode: 502, UpstreamErrorKind: "transport"}
	(&Handler{}).logUsageForRequest(c, input)
	decodeUsageDiagnostic(t, input)
	if len(read()) != 0 {
		t.Fatal("file logging was not disabled")
	}
}

func TestDiagnosticRequestSamplesAndEncodedContextAreBounded(t *testing.T) {
	var items []string
	for i := 0; i < 100; i++ {
		items = append(items, fmt.Sprintf(`{"type":"function_call_output","call_id":"PRIVATE_SENTINEL-%d","output":"PRIVATE_SENTINEL"}`, i))
	}
	body := []byte(`{"input":[` + strings.Join(items, ",") + `]}`)
	shape := summarizeDiagnosticRequest(body)
	if !shape.SamplesTruncated || len(shape.ItemSamples) != diagnosticItemSamples || shape.ItemSamples[0].Index != 0 || shape.ItemSamples[len(shape.ItemSamples)-1].Index != 99 {
		t.Fatal("samples lost order or bound")
	}
	e := diagnosticEvent{Request: shape, Attempt: diagnosticAttempt{Request: shape}, Error: summarizeDiagnosticError([]byte(`{"detail":"Bad Request"}`))}
	data := encodeDiagnosticEvent(e)
	if !json.Valid([]byte(data)) || len(data) > database.MaxUsageDiagnosticContextBytes || strings.Contains(data, "PRIVATE_SENTINEL") {
		t.Fatal("unsafe diagnostic serialization")
	}
}

func TestDiagnosticWebsocketLocalErrorPersistsWithoutHTTPError(t *testing.T) {
	installDiagnosticTestSink(t)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "ws-diag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c := diagnosticTestContext()
	upstreamTraceFromContext(c.Request.Context()).diagnosticDB = db
	noteDiagnosticWSError(c, api.NewAPIError(api.ErrCodeInvalidParameter, "PRIVATE_SENTINEL", api.ErrorTypeInvalidRequest))
	db.FlushUsageLogs()
	rows, err := db.ListRecentUsageLogs(context.Background(), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("WS diagnostic rows=%d err=%v", len(rows), err)
	}
	var e diagnosticEvent
	if err = json.Unmarshal([]byte(rows[0].DiagnosticContext), &e); err != nil {
		t.Fatal(err)
	}
	if !e.Websocket || e.OutcomeStatus != 400 || !e.Error.MessagePresent || e.Error.Source != "websocket_gateway" || strings.Contains(rows[0].DiagnosticContext, "PRIVATE_SENTINEL") {
		t.Fatal("WS diagnostic lost privacy/origin/evidence")
	}
}

func TestDiagnosticBoundStreamCannotPolluteNewAttempt(t *testing.T) {
	installDiagnosticTestSink(t)
	c := diagnosticTestContext()
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 1}, "", false)(&http.Response{StatusCode: 200})
	bound := bindDiagnosticAttempt(c.Request.Context())
	first := upstreamTraceFromContext(c.Request.Context()).current
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 2}, "", false)(&http.Response{StatusCode: 200})
	noteDiagnosticStreamEvent(bound, "error", []byte(`{"error":{"code":"invalid_parameter"}}`))
	noteDiagnosticStreamReadError(bound, io.ErrUnexpectedEOF)
	latest := snapshotUpstreamTrace(c.Request.Context())
	if latest.Diagnostic.Error.Code != "" || latest.Diagnostic.Transport != "" || first.diagnostic.Error.Code != "invalid_parameter" || first.diagnostic.Error.ReadError != "eof" {
		t.Fatal("stream evidence crossed attempts")
	}
}

func TestDiagnosticNullErrorPreludeDoesNotHideStreamFailure(t *testing.T) {
	installDiagnosticTestSink(t)
	c := diagnosticTestContext()
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 1}, "", false)(&http.Response{StatusCode: 200})
	noteDiagnosticStreamEvent(c.Request.Context(), "", []byte(`{"type":"response.created","error":null,"response":{"error":null}}`))
	if got := snapshotUpstreamTrace(c.Request.Context()).Diagnostic.Error; got.Source != "" {
		t.Fatal("null error captured as failure")
	}
	noteDiagnosticStreamEvent(c.Request.Context(), "", []byte(`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"PRIVATE_SENTINEL"}}}`))
	input := &database.UsageLogInput{AccountID: 1, StatusCode: 400, Stream: true, UpstreamErrorKind: "client"}
	(&Handler{}).logUsageForRequest(c, input)
	e := decodeUsageDiagnostic(t, input)
	if e.Error.Code != "context_length_exceeded" || e.Error.Source != "upstream_stream" || e.Attempt.HTTPStatus != 200 {
		t.Fatal("actual failure was hidden by a normal prelude")
	}
}
