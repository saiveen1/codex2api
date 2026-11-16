package admin

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/managedweb"
	"github.com/gin-gonic/gin"
)

func TestChatGPTWebControlValidationPersistenceAndRedaction(t *testing.T) {
	for _, key := range []string{"CHATGPT_WEB_ENABLED", "CHATGPT_WEB_WORKER_URL", "CHATGPT_WEB_WORKER_SECRET", "CHATGPT_WEB_TIMEOUT_SECONDS"} {
		t.Setenv(key, "")
	}
	dir := t.TempDir()
	manager, err := managedweb.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	h := &Handler{chatGPTWebManager: manager}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.PUT("/config", h.ConfigureChatGPTWeb)
	r.GET("/status", h.GetChatGPTWebStatus)
	const secret = "synthetic-external-secret-1234"
	call := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("PUT", "/config", strings.NewReader(body)))
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("secret exposed")
		}
		return w
	}
	for _, body := range []string{
		`{}`, `{"enabled":null}`, `{"enabled":false,"unknown":true}`,
		`{"enabled":false} {}`, `{"enabled":true,"mode":"shell","timeout_seconds":180}`,
		`{"enabled":true,"mode":"external","worker_url":"http://user:pass@127.0.0.1:8091","worker_secret":"` + secret + `","timeout_seconds":180}`,
		`{"enabled":true,"mode":"external","worker_url":"http://127.0.0.1:8091","worker_secret":"` + secret + `","timeout_seconds":601}`,
		`{"enabled":false,"worker_secret":"` + strings.Repeat("x", 9000) + `"}`,
	} {
		if w := call(body); w.Code != 400 {
			t.Fatalf("invalid config accepted: %d", w.Code)
		}
	}
	if manager.View().Config.Enabled {
		t.Fatal("invalid requests enabled provider")
	}
	w := call(`{"enabled":true,"mode":"external","worker_url":"http://127.0.0.1:8091","worker_secret":"` + secret + `","timeout_seconds":45}`)
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	var status struct {
		Enabled    bool
		Configured bool
		Control    managedweb.View
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || !status.Configured || !status.Control.SecretSet || status.Control.Config.WorkerSecret != "" || status.Control.Running {
		t.Fatalf("invalid status: %+v", status)
	}
	if w := call(`{"enabled":false,"mode":"external","worker_url":"http://127.0.0.1:8091","worker_secret":"","timeout_seconds":45}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, err := manager.Runtime(); err == nil {
		t.Fatal("disabled runtime remains usable")
	}
	reloaded, err := managedweb.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reloaded.Close() })
	if reloaded.View().Config.Enabled || !reloaded.View().SecretSet || reloaded.View().Config.TimeoutSeconds != 45 {
		t.Fatal("saved state not restored")
	}
	cfg := reloaded.View().Config
	cfg.Enabled = true
	if err := reloaded.Configure(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := reloaded.Runtime()
	if err != nil || runtime.WorkerSecret != secret {
		t.Fatal("blank secret did not preserve existing credential")
	}
}

func TestChatGPTWebControlUnavailable(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("PUT", "/config", strings.NewReader(`{"enabled":true}`))
	(&Handler{}).ConfigureChatGPTWeb(c)
	if c.Writer.Status() != 503 {
		t.Fatal(c.Writer.Status())
	}
}
