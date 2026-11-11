package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestDiagnosticLogDirResolvedAtFirstWrite(t *testing.T) {
	dir := t.TempDir()
	old := diagnosticLogger
	diagnosticLogger = &diagnosticFileLogger{maxBytes: diagnosticMaxFileSize, backups: diagnosticBackupCount}
	t.Cleanup(func() { closeDiagnosticLog(); diagnosticLogger = old })
	t.Setenv("LOG_DIR", dir)
	t.Setenv("LOG_DISABLED", "false")
	writeDiagnostic(map[string]int{"schema": 1})
	if _, err := os.Stat(filepath.Join(dir, diagnosticLogFileName)); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticStructuredHandshakePreservesHTTPStatus(t *testing.T) {
	read := installDiagnosticTestSink(t)
	c := diagnosticTestContext()
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 2}, "", true)(nil)
	err := continuousRetryTestHTTPError{status: 403, body: []byte(`{"error":{"code":"invalid_api_key","message":"PRIVATE_SENTINEL"}}`)}
	noteDiagnosticDispatch(c.Request.Context(), nil, err, 0, nil)
	events := read()
	if len(events) != 1 || events[0].Stage != "upstream_http" || events[0].Attempt.HTTPStatus != 403 || events[0].Attempt.Transport != "" || events[0].Attempt.Error.Code != "invalid_api_key" {
		t.Fatal(events)
	}
}

func TestDiagnosticPanicStillReachesRecovery(t *testing.T) {
	read := installDiagnosticTestSink(t)
	r := gin.New()
	r.Use(api.RecoveryMiddleware(), RequestDiagnosticMiddleware(nil))
	r.POST("/v1/responses", func(c *gin.Context) { panic("synthetic diagnostic panic") })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	events := read()
	if w.Code != 500 || len(events) != 1 || events[0].Stage != "panic" {
		t.Fatal(w.Code, events)
	}
}

func TestDiagnosticNoTraceNoEffects(t *testing.T) {
	read := installDiagnosticTestSink(t)
	noteDiagnosticDispatch(context.Background(), nil, context.Canceled, 0, nil)
	if len(read()) != 0 {
		t.Fatal("unattributed record")
	}
}

type diagnosticDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlineSet bool
}

func (w *diagnosticDeadlineRecorder) SetWriteDeadline(time.Time) error {
	w.deadlineSet = true
	return nil
}

func TestDiagnosticWriterUnwrapPreservesDeadlineAndKeepalive(t *testing.T) {
	installDiagnosticTestSink(t)
	w := &diagnosticDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	attachUpstreamTrace(c, nil)
	defer beginRequestDiagnostic(c)()
	if raw, unwrapped := unwrapHTTPResponseWriter(c.Writer); !unwrapped || raw != w {
		t.Fatal("writer traversal blocked")
	}
	if err := http.NewResponseController(c.Writer).SetWriteDeadline(time.Now()); err != nil || !w.deadlineSet {
		t.Fatal("write deadline blocked", err)
	}
}
