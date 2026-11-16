package managedweb

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const fixtureSecret = "fixture-external-secret-12345"

func cleanEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CHATGPT_WEB_ENABLED", "CHATGPT_WEB_MODE", "CHATGPT_WEB_WORKER_URL", "CHATGPT_WEB_WORKER_SECRET", "CHATGPT_WEB_TIMEOUT_SECONDS"} {
		t.Setenv(name, "")
	}
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	cleanEnvironment(t)
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func externalConfig() Config {
	return Config{Enabled: true, Mode: "external", WorkerURL: "http://127.0.0.1:18273", WorkerSecret: fixtureSecret, TimeoutSeconds: 180}
}

func TestValidation(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:8080", "https://worker.example", "http://[::1]:80", "http://worker/"} {
		cfg := externalConfig()
		cfg.WorkerURL = origin
		if _, err := normalize(cfg); err != nil {
			t.Errorf("valid origin rejected: %v", err)
		}
	}
	for _, origin := range []string{"", "ftp://worker", "http://user:password@worker", "http://worker/x", "http://worker/%2f", "http://worker?", "http://worker?q=private", "http://worker#", "http://worker#private", "http://worker:", "http://worker:0", "http://worker:65536", "http://worker:abc", "http://127.0.0.1::80", "http://[bad]:80", "http://worker\\x", "http://worker\r\nprivate"} {
		cfg := externalConfig()
		cfg.WorkerURL = origin
		if _, err := normalize(cfg); err == nil {
			t.Errorf("invalid origin accepted: %q", origin)
		}
	}
	for _, secret := range []string{"", "short", strings.Repeat("s", 15), fixtureSecret + "\r", fixtureSecret + "\n", fixtureSecret + "\x00", strings.Repeat("s", 4097)} {
		cfg := externalConfig()
		cfg.WorkerSecret = secret
		if _, err := normalize(cfg); err == nil {
			t.Error("invalid secret accepted")
		} else if secret != "" && strings.Contains(err.Error(), secret) {
			t.Error("validation error exposed the secret")
		}
	}
	for _, timeout := range []int{-1, 9, 601} {
		cfg := externalConfig()
		cfg.TimeoutSeconds = timeout
		if _, err := normalize(cfg); err == nil {
			t.Errorf("invalid timeout accepted: %d", timeout)
		}
	}
	for _, timeout := range []int{10, 600} {
		cfg := externalConfig()
		cfg.TimeoutSeconds = timeout
		if _, err := normalize(cfg); err != nil {
			t.Error(err)
		}
	}
	if _, err := normalize(Config{Mode: "unknown"}); err == nil {
		t.Error("invalid mode accepted")
	}
	cfg, err := normalize(Config{})
	if err != nil || cfg.Mode != "managed" || cfg.Enabled || cfg.TimeoutSeconds != 180 {
		t.Fatal("incorrect defaults")
	}
}

func TestDefaultsPermissionsAndReload(t *testing.T) {
	m := newTestManager(t)
	v := m.View()
	if v.Config.Enabled || v.Config.Mode != "managed" || v.Config.TimeoutSeconds != 180 || v.Config.WorkerSecret != "" || !v.SecretSet || v.Running || v.Stage != "stopped" {
		t.Fatal("unexpected default view")
	}
	if _, err := m.Runtime(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("disabled runtime was exposed")
	}
	secret := m.state.ManagedSecret
	if decoded, err := hex.DecodeString(secret); err != nil || len(decoded) != 32 {
		t.Fatal("managed secret is not 32 random bytes")
	}
	if err := m.Ensure(context.Background()); err != nil || m.View().Running {
		t.Fatal("disabled ensure started a worker")
	}
	cfg := m.View().Config
	cfg.TimeoutSeconds = 600
	if err := m.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(filepath.Dir(m.root))
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if reloaded.state.ManagedSecret != secret || reloaded.View().Config.TimeoutSeconds != 600 {
		t.Fatal("private state was not stable across reload")
	}
	if runtime.GOOS != "windows" {
		for path, mode := range map[string]os.FileMode{m.root: 0700, filepath.Join(m.root, "worker-state"): 0700, filepath.Join(m.root, "state.json"): 0600} {
			st, err := os.Stat(path)
			if err != nil || st.Mode().Perm() != mode {
				t.Errorf("incorrect private permissions for %s", filepath.Base(path))
			}
		}
	}
}

func TestExternalRedactionRetentionAndDisable(t *testing.T) {
	m := newTestManager(t)
	managedSecret := m.state.ManagedSecret
	if err := m.Configure(context.Background(), externalConfig()); err != nil {
		t.Fatal(err)
	}
	v := m.View()
	b, _ := json.Marshal(v)
	if bytes.Contains(b, []byte(fixtureSecret)) || bytes.Contains(b, []byte(managedSecret)) || v.Config.WorkerSecret != "" || !v.SecretSet || !v.Available || v.Running || v.Stage != "external" {
		t.Fatal("external view leaked secrets or misreported state")
	}
	v.Config.TimeoutSeconds = 42
	if err := m.Configure(context.Background(), v.Config); err != nil {
		t.Fatal(err)
	}
	cfg, err := m.Runtime()
	if err != nil || cfg.WorkerSecret != fixtureSecret || cfg.TimeoutSeconds != 42 {
		t.Fatal("blank secret did not retain external credential")
	}
	cfg.WorkerSecret = "caller-mutated-copy"
	if again, _ := m.Runtime(); again.WorkerSecret != fixtureSecret {
		t.Fatal("runtime config exposed mutable internal state")
	}
	if err := m.Configure(context.Background(), Config{Mode: "managed"}); !errors.Is(err, ErrConflict) {
		t.Fatal("enabled mode switch did not require disable first")
	}
	cfg = m.View().Config
	cfg.Enabled = false
	if err := m.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Runtime(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("disable did not immediately gate runtime")
	}
	if err := m.Configure(context.Background(), Config{Mode: "managed"}); err != nil {
		t.Fatal(err)
	}
	cfg = externalConfig()
	cfg.WorkerSecret = ""
	if err := m.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if m.state.ManagedSecret != managedSecret || m.state.ExternalSecret != fixtureSecret {
		t.Fatal("managed and external credentials were not independent")
	}
	reloaded, err := New(filepath.Dir(m.root))
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if cfg, err := reloaded.Runtime(); err != nil || cfg.WorkerSecret != fixtureSecret {
		t.Fatal("external secret did not survive reload")
	}
}

func TestEnvironmentImportedOnlyOnce(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv("CHATGPT_WEB_ENABLED", "1")
	t.Setenv("CHATGPT_WEB_WORKER_URL", "http://127.0.0.1:4321")
	t.Setenv("CHATGPT_WEB_WORKER_SECRET", fixtureSecret)
	t.Setenv("CHATGPT_WEB_TIMEOUT_SECONDS", "210")
	dir := t.TempDir()
	m, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg, err := m.Runtime()
	if err != nil || cfg.Mode != "external" || cfg.WorkerURL != "http://127.0.0.1:4321" || cfg.WorkerSecret != fixtureSecret || cfg.TimeoutSeconds != 210 {
		t.Fatal("legacy external environment was not imported")
	}
	t.Setenv("CHATGPT_WEB_ENABLED", "invalid")
	t.Setenv("CHATGPT_WEB_WORKER_SECRET", "invalid")
	t.Setenv("CHATGPT_WEB_TIMEOUT_SECONDS", "invalid")
	reloaded, err := New(dir)
	if err != nil {
		t.Fatal("saved settings should ignore environment changes:", err)
	}
	defer reloaded.Close()
	if again, err := reloaded.Runtime(); err != nil || again != cfg {
		t.Fatal("environment overrode saved configuration")
	}
	if _, err := New(t.TempDir()); err == nil {
		t.Fatal("invalid initial environment accepted")
	}
}

func TestFailedPersistencePreservesSettings(t *testing.T) {
	m := newTestManager(t)
	if err := m.Configure(context.Background(), externalConfig()); err != nil {
		t.Fatal(err)
	}
	old, _ := m.Runtime()
	oldDisk, err := os.ReadFile(filepath.Join(m.root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A regular file where the parent directory should be forces a deterministic
	// save failure even when tests run as root. The original state stays untouched.
	realRoot := m.root
	m.root = filepath.Join(realRoot, "state.json")
	cfg := old
	cfg.Enabled = false
	if err := m.Configure(context.Background(), cfg); err == nil || strings.Contains(err.Error(), fixtureSecret) {
		t.Fatal("save failure was missing or unsafe")
	}
	m.root = realRoot
	if current, err := m.Runtime(); err != nil || current != old {
		t.Fatal("failed save changed runtime settings")
	}
	after, err := os.ReadFile(filepath.Join(realRoot, "state.json"))
	if err != nil || !bytes.Equal(oldDisk, after) {
		t.Fatal("failed save changed persisted settings")
	}
}

func TestRejectCorruptOrSymlinkState(t *testing.T) {
	m := newTestManager(t)
	path := filepath.Join(m.root, "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"managed_secret":"private-invalid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Dir(m.root)); err == nil || strings.Contains(err.Error(), "private-invalid") {
		t.Fatal("corrupt private state was accepted or echoed")
	}
	if runtime.GOOS == "windows" {
		return // Creating symlinks can require elevated permissions on Windows.
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("must-not-change"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Dir(m.root)); err == nil {
		t.Fatal("symlinked private state accepted")
	}
	st, _ := os.Stat(target)
	if st.Mode().Perm() != 0644 {
		t.Fatal("symlink target permissions were changed")
	}
}

func TestCancellationAndClosedManager(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Configure(ctx, externalConfig()); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled configuration accepted")
	}
	if err := m.Ensure(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled ensure accepted")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Configure(context.Background(), externalConfig()); !errors.Is(err, ErrClosed) {
		t.Fatal("closed manager accepted configuration")
	}
	if err := m.Ensure(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal("closed manager reconciled")
	}
}

func TestConcurrentViewsAndConfiguration(t *testing.T) {
	m := newTestManager(t)
	if err := m.Configure(context.Background(), externalConfig()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 12; i++ {
				v := m.View()
				if v.Config.WorkerSecret != "" {
					t.Error("view leaked a secret")
				}
				_, _ = m.Runtime()
				v.Config.TimeoutSeconds = 180 + i
				if err := m.Configure(context.Background(), v.Config); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestWorkerCommandAndEnvironment(t *testing.T) {
	env := workerEnvironment([]string{"KEEP=1", "CHATGPT_WEB_WORKER_SECRET=old", "CHATGPT_WEB_STATE_DIR=old", "CHATGPT_WEB_WORKER_SECRET=duplicate", "CHATGPT_WEB_STATE_DIR=duplicate"}, "/private/state", fixtureSecret)
	if strings.Join(env, "\n") != "KEEP=1\nCHATGPT_WEB_STATE_DIR=/private/state\nCHATGPT_WEB_WORKER_SECRET="+fixtureSecret {
		t.Fatal("worker environment did not scrub duplicate credentials and state paths")
	}
	cmd := workerCommand(54321, "/private/state", fixtureSecret)
	want := workerPython + " -m uvicorn app:create_app --factory --app-dir " + workerDir + " --host 127.0.0.1 --port 54321 --no-access-log --timeout-graceful-shutdown 650"
	if strings.Join(cmd.Args, " ") != want || cmd.Dir != workerDir || strings.Contains(strings.Join(cmd.Args, " "), fixtureSecret) {
		t.Fatal("worker command was changed or exposed credentials in arguments")
	}
}

func TestUnsupportedPlatformDoesNotSpawn(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("unsupported-platform guard is exercised on non-Linux hosts")
	}
	m := newTestManager(t)
	cfg := m.View().Config
	cfg.Enabled = true
	if err := m.Configure(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "Linux") || m.View().Running || m.View().Available || m.View().Stage != "error" {
		t.Fatal("managed mode did not fail safely on an unsupported platform")
	}
}
