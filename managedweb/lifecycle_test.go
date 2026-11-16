package managedweb

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// This helper is the already-built test binary, not Python or a real worker. It
// binds only the port reserved by the manager and has a hard orphan deadline.
func TestManagedWorkerHelper(t *testing.T) {
	if os.Getenv("MANAGEDWEB_TEST_HELPER") != "1" {
		return
	}
	deadline := time.AfterFunc(12*time.Second, func() { os.Exit(3) })
	defer deadline.Stop()
	mode := os.Getenv("MANAGEDWEB_TEST_MODE")
	secret := os.Getenv("CHATGPT_WEB_WORKER_SECRET")
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.Signal(15))
	defer signal.Stop(term)
	listener, err := net.Listen("tcp4", "127.0.0.1:"+os.Getenv("MANAGEDWEB_TEST_PORT"))
	if err != nil {
		os.Exit(4)
	}
	release, draining := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	server := &http.Server{ReadHeaderTimeout: time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			if mode == "unready" {
				w.WriteHeader(503)
			}
			io.WriteString(w, `{"status":"ok","protocol_version":"1"}`)
		case "/test/release":
			w.WriteHeader(204)
			w.(http.Flusher).Flush()
			releaseOnce.Do(func() { close(release) })
		case "/test/draining":
			select {
			case <-draining:
				w.WriteHeader(204)
			case <-r.Context().Done():
			}
		case "/test/exit":
			os.Exit(7)
		default:
			if r.Header.Get("Authorization") != "Bearer "+secret {
				w.WriteHeader(401)
				io.WriteString(w, `{"error_code":"auth_required"}`)
				return
			}
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","code":"invalid_request","http_status":400,"retryable":false,"result_unknown":false}`)
		}
	})
	go func() {
		<-term
		close(draining)
		if mode == "hold" {
			<-release
		}
		_ = server.Close()
	}()
	_ = server.Serve(listener)
	os.Exit(0)
}

func syntheticManager(t *testing.T, mode string) *Manager {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("owned process lifecycle is Linux-only; no non-WMI Windows spawn")
	}
	m := newTestManager(t)
	m.command = func(port int, stateDir, secret string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestManagedWorkerHelper$", "-test.timeout=10s")
		cmd.Env = append(workerEnvironment(os.Environ(), stateDir, secret), "MANAGEDWEB_TEST_HELPER=1", "MANAGEDWEB_TEST_MODE="+mode, "MANAGEDWEB_TEST_PORT="+strconv.Itoa(port))
		return cmd
	}
	t.Cleanup(func() {
		m.mu.Lock()
		p := m.proc
		m.mu.Unlock()
		if p != nil {
			_ = m.Close()
			_, _ = helperRequest(p.url, "/test/release")
			select {
			case <-p.done:
			case <-time.After(3 * time.Second):
				t.Error("synthetic child did not gracefully exit; its hard deadline remains active")
			}
		}
	})
	return m
}

func helperRequest(origin, path string) (int, error) {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: time.Second, Transport: transport}
	resp, err := client.Get(origin + path)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, nil
}

func waitFor(t *testing.T, m *Manager, predicate func(View) bool) View {
	t.Helper()
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		v := m.View()
		if predicate(v) {
			return v
		}
		select {
		case <-timer.C:
			t.Fatalf("state deadline expired: stage=%s running=%v error=%s", v.Stage, v.Running, v.LastError)
		case <-ticker.C:
		}
	}
}

func startSynthetic(t *testing.T, m *Manager) {
	t.Helper()
	cfg := m.View().Config
	cfg.Enabled = true
	if err := m.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedLifecycleDisableAndConflicts(t *testing.T) {
	m := syntheticManager(t, "hold")
	startSynthetic(t, m)
	v := waitFor(t, m, func(v View) bool { return v.Stage == "running" })
	cfg, err := m.Runtime()
	if err != nil || !strings.HasPrefix(cfg.WorkerURL, "http://127.0.0.1:") || cfg.WorkerSecret == "" || !v.Running {
		t.Fatal("owned worker did not become ready")
	}
	m.mu.Lock()
	owned := m.proc
	m.mu.Unlock()
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	v.Config.TimeoutSeconds = 240
	if err := m.Configure(context.Background(), v.Config); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	unchanged := m.proc == owned
	m.mu.Unlock()
	if again, err := m.Runtime(); err != nil || !unchanged || again.WorkerURL != cfg.WorkerURL || again.TimeoutSeconds != 240 {
		t.Fatal("timeout-only update restarted or changed the worker")
	}
	if err := m.Configure(context.Background(), externalConfig()); !errors.Is(err, ErrConflict) {
		t.Fatal("live mode switch was not rejected")
	}
	marker := filepath.Join(m.root, "worker-state", "preserved")
	if err := os.WriteFile(marker, []byte("persistent"), 0600); err != nil {
		t.Fatal(err)
	}
	disabled := m.View().Config
	disabled.Enabled = false
	if err := m.Configure(context.Background(), disabled); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Runtime(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("disabled runtime remained usable while draining")
	}
	if v := m.View(); v.Stage != "draining" || !v.Running || v.Config.Enabled {
		t.Fatal("owned child was not kept alive for graceful drain")
	}
	if status, err := helperRequest(cfg.WorkerURL, "/test/draining"); err != nil || status != 204 {
		t.Fatal("owned child did not receive SIGTERM")
	}
	disabled.TimeoutSeconds++
	if err := m.Configure(context.Background(), disabled); !errors.Is(err, ErrConflict) {
		t.Fatal("settings changed during drain")
	}
	if err := m.Configure(context.Background(), externalConfig()); !errors.Is(err, ErrConflict) {
		t.Fatal("mode changed before graceful exit")
	}
	if status, err := helperRequest(cfg.WorkerURL, "/test/release"); err != nil || status != 204 {
		t.Fatal("synthetic drain release failed")
	}
	waitFor(t, m, func(v View) bool { return v.Stage == "stopped" && !v.Running })
	if b, err := os.ReadFile(marker); err != nil || string(b) != "persistent" {
		t.Fatal("disable deleted persisted worker state")
	}
	if err := m.Configure(context.Background(), externalConfig()); err != nil {
		t.Fatal("mode switch after graceful exit was rejected:", err)
	}
}

func TestStartingConflictAndDisable(t *testing.T) {
	m := syntheticManager(t, "unready")
	startSynthetic(t, m)
	if m.View().Stage != "starting" {
		t.Fatal("starting stage was not observable")
	}
	if _, err := m.Runtime(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unready child was usable")
	}
	cfg := m.View().Config
	cfg.TimeoutSeconds++
	if err := m.Configure(context.Background(), cfg); !errors.Is(err, ErrConflict) {
		t.Fatal("settings changed while starting")
	}
	cfg = m.View().Config
	cfg.Enabled = false
	if err := m.Configure(context.Background(), cfg); err != nil {
		t.Fatal("could not disable a starting worker:", err)
	}
	waitFor(t, m, func(v View) bool { return v.Stage == "stopped" && !v.Running })
}

func TestReadinessTimeoutAndUnexpectedExit(t *testing.T) {
	t.Run("readiness timeout", func(t *testing.T) {
		m := syntheticManager(t, "unready")
		m.readyWithin = 1500 * time.Millisecond
		startSynthetic(t, m)
		v := waitFor(t, m, func(v View) bool { return v.Stage == "error" && !v.Running })
		if !strings.Contains(v.LastError, "readiness timed out") {
			t.Fatal("readiness timeout was not retained after drain")
		}
		if _, err := m.Runtime(); !errors.Is(err, ErrUnavailable) {
			t.Fatal("timed-out worker remained available")
		}
	})
	t.Run("unexpected exit", func(t *testing.T) {
		m := syntheticManager(t, "normal")
		startSynthetic(t, m)
		waitFor(t, m, func(v View) bool { return v.Stage == "running" })
		cfg, _ := m.Runtime()
		_, _ = helperRequest(cfg.WorkerURL, "/test/exit")
		v := waitFor(t, m, func(v View) bool { return v.Stage == "error" && !v.Running })
		if !strings.Contains(v.LastError, "exited unexpectedly") {
			t.Fatal("unexpected exit was not observable")
		}
		if _, err := m.Runtime(); !errors.Is(err, ErrUnavailable) {
			t.Fatal("exited worker remained ready")
		}
		cfg = v.Config
		cfg.TimeoutSeconds++
		if err := m.Configure(context.Background(), cfg); err != nil || m.View().Running || m.View().Stage != "error" {
			t.Fatal("timeout-only update restarted an exited worker")
		}
	})
}

func TestCloseIsBoundedAndDoesNotKill(t *testing.T) {
	m := syntheticManager(t, "hold")
	startSynthetic(t, m)
	waitFor(t, m, func(v View) bool { return v.Stage == "running" })
	cfg, _ := m.Runtime()
	started := time.Now()
	if err := m.Close(); err == nil || !strings.Contains(err.Error(), "still draining") {
		t.Fatal("close did not report the outstanding graceful drain")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("close exceeded its bounded wait")
	}
	if _, err := m.Runtime(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("closed manager still provided runtime credentials")
	}
	if v := m.View(); v.Stage != "draining" || !v.Running || !v.Config.Enabled {
		t.Fatal("close killed the child or rewrote desired configuration")
	}
	if status, err := helperRequest(cfg.WorkerURL, "/test/release"); err != nil || status != 204 {
		t.Fatal("close forcibly terminated the draining child")
	}
	waitFor(t, m, func(v View) bool { return v.Stage == "stopped" && !v.Running })
	reloaded, err := New(filepath.Dir(m.root))
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if !reloaded.View().Config.Enabled || reloaded.View().Running {
		t.Fatal("close changed persisted enablement or New spawned a process")
	}
}

func TestProbeRequiresHealthProtocolAndSecret(t *testing.T) {
	for _, mode := range []string{"health-only", "wrong-protocol", "wrong-secret", "accept-any-token", "valid"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					version := "1"
					if mode == "wrong-protocol" {
						version = "2"
					}
					io.WriteString(w, `{"status":"ok","protocol_version":"`+version+`"}`)
					return
				}
				if mode == "health-only" {
					io.WriteString(w, `{"status":"ok"}`)
					return
				}
				if mode == "accept-any-token" && r.Header.Get("Authorization") != "" {
					w.WriteHeader(400)
					io.WriteString(w, `{"type":"error","code":"invalid_request","http_status":400,"retryable":false,"result_unknown":false}`)
					return
				}
				if mode == "wrong-secret" || r.Header.Get("Authorization") != "Bearer "+fixtureSecret {
					w.WriteHeader(401)
					io.WriteString(w, `{"error_code":"auth_required"}`)
					return
				}
				w.WriteHeader(400)
				io.WriteString(w, `{"type":"error","code":"invalid_request","http_status":400,"retryable":false,"result_unknown":false}`)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if got := probe(ctx, server.Client(), server.URL, fixtureSecret); got != (mode == "valid") {
				t.Fatal("probe accepted an unrelated service or rejected the authenticated worker")
			}
		})
	}
}

func TestStartFailureIsSafeAndDesiredStatePersists(t *testing.T) {
	m := syntheticManager(t, "normal")
	m.command = func(int, string, string) *exec.Cmd {
		return exec.Command(filepath.Join(t.TempDir(), "missing-private-worker"))
	}
	cfg := m.View().Config
	cfg.Enabled = true
	if err := m.Configure(context.Background(), cfg); err == nil || strings.Contains(err.Error(), "missing-private-worker") || strings.Contains(err.Error(), m.state.ManagedSecret) {
		t.Fatal("launch failure was missing or exposed diagnostics")
	}
	if v := m.View(); v.Stage != "error" || v.Running || !v.Config.Enabled {
		t.Fatal("launch failure lost desired configuration or retained readiness")
	}
	reloaded, err := New(filepath.Dir(m.root))
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if !reloaded.View().Config.Enabled {
		t.Fatal("desired configuration was not persisted before launch")
	}
}
