package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/managedweb"
)

func TestChatGPTWebManagedConfigOverridesEnvironment(t *testing.T) {
	t.Setenv("CHATGPT_WEB_ENABLED", "false")
	t.Setenv("CHATGPT_WEB_WORKER_URL", "")
	t.Setenv("CHATGPT_WEB_WORKER_SECRET", "")
	t.Setenv("CHATGPT_WEB_TIMEOUT_SECONDS", "")
	manager, err := managedweb.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	h := &Handler{}
	h.SetChatGPTWebManager(manager)
	cfg := managedweb.Config{Enabled: true, Mode: "external", WorkerURL: "http://127.0.0.1:8091", WorkerSecret: "synthetic-secret-12345", TimeoutSeconds: 45}
	if err := manager.Configure(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	actual, err := h.loadChatGPTWebConfig()
	if err != nil || !actual.enabled || actual.endpoint != cfg.WorkerURL || actual.timeout != 45*time.Second || actual.secret != cfg.WorkerSecret {
		t.Fatal("runtime settings were not used")
	}
	cfg.Enabled = false
	if err := manager.Configure(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHATGPT_WEB_ENABLED", "true")
	t.Setenv("CHATGPT_WEB_WORKER_URL", cfg.WorkerURL)
	t.Setenv("CHATGPT_WEB_WORKER_SECRET", cfg.WorkerSecret)
	if _, err := h.loadChatGPTWebConfig(); err == nil {
		t.Fatal("disabled manager fell back to environment")
	}
	h.SetChatGPTWebManager(nil)
	if _, err := h.loadChatGPTWebConfig(); err == nil {
		t.Fatal("failed manager initialization fell back to environment")
	}
}
