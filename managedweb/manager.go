// Package managedweb owns the bundled ChatGPT Web worker, never an existing service.
package managedweb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	workerDir        = "/opt/chatgpt-web-worker"
	workerPython     = workerDir + "/.venv/bin/python"
	readinessTimeout = 10 * time.Second
	closeTimeout     = time.Second
)

var (
	ErrConflict    = errors.New("managed web: worker is starting or draining; disable and wait before changing worker settings")
	ErrUnavailable = errors.New("managed web: worker is not enabled and ready")
	ErrClosed      = errors.New("managed web: manager is closed")
)

type Config struct {
	Enabled        bool   `json:"enabled"`
	Mode           string `json:"mode"`
	WorkerURL      string `json:"worker_url"`
	WorkerSecret   string `json:"worker_secret,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type View struct {
	Config    Config `json:"config"`
	SecretSet bool   `json:"secret_set"`
	Running   bool   `json:"running"`
	Stage     string `json:"stage"`
	LastError string `json:"last_error"`
	Available bool   `json:"available"`
}

type diskState struct {
	Version        int    `json:"version"`
	Config         Config `json:"config"`
	ManagedSecret  string `json:"managed_secret"`
	ExternalSecret string `json:"external_secret,omitempty"`
}

type child struct {
	cmd      *exec.Cmd
	url      string
	done     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	ready    bool
	stopping bool
	failure  string
}

// Manager is safe for concurrent use. One application owner must hold each dataDir;
// it deliberately does not adopt children or use PID files across application runs.
// The unexported command seam is only used by synthetic, offline lifecycle tests.
type Manager struct {
	mu          sync.Mutex
	root        string
	state       diskState
	proc        *child
	stage       string
	lastError   string
	closed      bool
	command     func(port int, stateDir, secret string) *exec.Cmd
	readyWithin time.Duration
}

// New imports environment defaults only when state.json is absent. It persists
// defaults and a stable random managed secret but does not start the worker.
func New(dataDir string) (*Manager, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("managed web: data directory is required")
	}
	root, err := filepath.Abs(filepath.Join(dataDir, "chatgpt-web"))
	if err != nil {
		return nil, errors.New("managed web: invalid data directory")
	}
	for _, dir := range []string{root, filepath.Join(root, "worker-state")} {
		if err := privateDir(dir); err != nil {
			return nil, err
		}
	}
	m := &Manager{root: root, stage: "stopped", readyWithin: readinessTimeout}
	path := filepath.Join(root, "state.json")
	st, err := os.Lstat(path)
	if err == nil {
		if !st.Mode().IsRegular() || st.Size() > 64<<10 || os.Chmod(path, 0600) != nil {
			return nil, errors.New("managed web: invalid private state file")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, errors.New("managed web: cannot read private state")
		}
		b, readErr := io.ReadAll(io.LimitReader(f, (64<<10)+1))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || len(b) > 64<<10 || json.Unmarshal(b, &m.state) != nil {
			return nil, errors.New("managed web: invalid private state")
		}
		secret, err := hex.DecodeString(m.state.ManagedSecret)
		if err != nil || len(secret) != 32 || m.state.Version != 1 || m.state.Config.WorkerSecret != "" {
			return nil, errors.New("managed web: invalid private state")
		}
		cfg := m.privateConfigLocked()
		if normalized, err := normalize(cfg); err != nil || normalized != cfg || (m.state.ExternalSecret != "" && !validSecret(m.state.ExternalSecret)) {
			return nil, errors.New("managed web: invalid private configuration")
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("managed web: cannot inspect private state")
	} else {
		cfg, err := environmentConfig()
		if err != nil {
			return nil, err
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, errors.New("managed web: cannot generate worker secret")
		}
		m.state = diskState{Version: 1, Config: cfg, ManagedSecret: hex.EncodeToString(secret)}
		if cfg.Mode == "external" {
			m.state.ExternalSecret = cfg.WorkerSecret
		}
		m.state.Config.WorkerSecret = ""
		if err := m.persist(m.state); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return errors.New("managed web: cannot create private directory")
	}
	st, err := os.Lstat(path)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || os.Chmod(path, 0700) != nil {
		return errors.New("managed web: cannot protect private directory")
	}
	return nil
}

func validSecret(secret string) bool {
	if len(secret) < 16 || len(secret) > 4096 {
		return false
	}
	for _, c := range secret {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func normalize(cfg Config) (Config, error) {
	if cfg.Mode == "" {
		cfg.Mode = "managed"
	}
	if cfg.Mode != "managed" && cfg.Mode != "external" {
		return Config{}, errors.New("managed web: mode must be managed or external")
	}
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = 180
	}
	if cfg.TimeoutSeconds < 10 || cfg.TimeoutSeconds > 600 {
		return Config{}, errors.New("managed web: timeout must be between 10 and 600 seconds")
	}
	if cfg.Mode == "managed" {
		// Managed origins and credentials are generated, never supplied by callers.
		cfg.WorkerURL, cfg.WorkerSecret = "", ""
		return cfg, nil
	}
	u, err := url.Parse(cfg.WorkerURL)
	if err == nil && u.Path == "/" && u.RawPath == "" {
		u.Path = ""
		cfg.WorkerURL = u.String()
	}
	if err != nil || len(cfg.WorkerURL) > 2048 || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(cfg.WorkerURL, "?#\\\r\n\t ") {
		return Config{}, errors.New("managed web: worker URL must be an http(s) origin without credentials, path, query or fragment")
	}
	// Reject empty ports, malformed IPv6, and port zero as well as out-of-range ports.
	if strings.HasSuffix(u.Host, ":") || (strings.Contains(u.Hostname(), ":") && net.ParseIP(u.Hostname()) == nil) {
		return Config{}, errors.New("managed web: invalid worker origin host or port")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Config{}, errors.New("managed web: invalid worker origin port")
		}
	}
	if (cfg.Enabled || cfg.WorkerSecret != "") && !validSecret(cfg.WorkerSecret) {
		return Config{}, errors.New("managed web: external worker secret must contain 16 to 4096 characters without control characters")
	}
	return cfg, nil
}

func environmentConfig() (Config, error) {
	cfg := Config{Mode: strings.TrimSpace(os.Getenv("CHATGPT_WEB_MODE")), WorkerURL: strings.TrimSpace(os.Getenv("CHATGPT_WEB_WORKER_URL")), WorkerSecret: os.Getenv("CHATGPT_WEB_WORKER_SECRET")}
	if cfg.Mode == "" {
		cfg.Mode = "managed"
		if cfg.WorkerURL != "" {
			cfg.Mode = "external"
		}
	}
	if raw := strings.TrimSpace(os.Getenv("CHATGPT_WEB_ENABLED")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, errors.New("managed web: invalid CHATGPT_WEB_ENABLED")
		}
		cfg.Enabled = v
	}
	if raw := strings.TrimSpace(os.Getenv("CHATGPT_WEB_TIMEOUT_SECONDS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, errors.New("managed web: invalid CHATGPT_WEB_TIMEOUT_SECONDS")
		}
		cfg.TimeoutSeconds = n
	}
	return normalize(cfg)
}

func (m *Manager) persist(state diskState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return errors.New("managed web: cannot encode private configuration")
	}
	f, err := os.CreateTemp(m.root, ".state-*")
	if err != nil {
		return errors.New("managed web: cannot save private configuration")
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return errors.New("managed web: cannot protect private configuration")
	}
	_, writeErr := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || os.Rename(f.Name(), filepath.Join(m.root, "state.json")) != nil {
		return errors.New("managed web: cannot save private configuration")
	}
	// Rename is the commit point. A directory sync is best effort: reporting a
	// failure after this point would falsely imply the old configuration survived.
	if dir, err := os.Open(m.root); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (m *Manager) privateConfigLocked() Config {
	cfg := m.state.Config
	if cfg.Mode == "external" {
		cfg.WorkerSecret = m.state.ExternalSecret
	}
	return cfg
}

// Configure commits private configuration before applying runtime changes. A
// runtime error leaves the new desired configuration saved and Stage set to error.
// Blank external secrets retain the previously saved external secret.
func (m *Manager) Configure(ctx context.Context, cfg Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.Mode == "external" && cfg.WorkerSecret == "" {
		cfg.WorkerSecret = m.state.ExternalSecret
	}
	cfg, err := normalize(cfg)
	if err != nil {
		return err
	}
	old := m.privateConfigLocked()
	if old.Enabled && cfg.Mode != old.Mode {
		return ErrConflict
	}
	if m.proc != nil {
		if m.proc.stopping && cfg != old {
			return ErrConflict
		}
		if m.stage == "starting" && cfg != old {
			disabled := old
			disabled.Enabled = false
			if cfg != disabled {
				return ErrConflict
			}
		}
		// Disabling cannot also switch modes/settings on a live child.
		if cfg.Mode != old.Mode || cfg.WorkerURL != old.WorkerURL || cfg.WorkerSecret != old.WorkerSecret {
			return ErrConflict
		}
	}
	next := m.state
	next.Config = cfg
	if cfg.Mode == "external" {
		next.ExternalSecret = cfg.WorkerSecret
	}
	next.Config.WorkerSecret = ""
	if err := m.persist(next); err != nil {
		return err
	}
	m.state = next
	if cfg.Enabled && old.Enabled && cfg.Mode == old.Mode && cfg.WorkerURL == old.WorkerURL && cfg.WorkerSecret == old.WorkerSecret && cfg.TimeoutSeconds != old.TimeoutSeconds {
		return nil // Timeout-only edits never start or restart a worker.
	}
	return m.applyLocked()
}

// Ensure reconciles startup state; readiness continues asynchronously for at most
// ten seconds. The request context never owns the worker's lifetime.
func (m *Manager) Ensure(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.applyLocked()
}

func bundledAvailable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	st, err := os.Stat(workerPython)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0
}

func (m *Manager) applyLocked() error {
	if !m.state.Config.Enabled {
		if m.proc != nil {
			return m.drainLocked(m.proc)
		}
		m.stage, m.lastError = "stopped", ""
		return nil
	}
	if m.proc != nil {
		if m.proc.stopping {
			return ErrConflict
		}
		return nil
	}
	if m.state.Config.Mode == "external" {
		m.stage, m.lastError = "external", ""
		return nil
	}
	if runtime.GOOS != "linux" {
		return m.failLocked("managed web: managed workers require Linux; use the bundled Linux Docker image or external mode")
	}
	if m.command == nil && !bundledAvailable() {
		return m.failLocked("managed web: bundled Python worker is unavailable; use an image containing /opt/chatgpt-web-worker/.venv/bin/python")
	}
	// Reserve a fresh loopback port instead of adopting anything already listening.
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return m.failLocked("managed web: cannot reserve a private worker port")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	stateDir := filepath.Join(m.root, "worker-state")
	if err := privateDir(stateDir); err != nil {
		listener.Close()
		return m.failLocked("managed web: cannot protect worker state directory")
	}
	command := m.command
	if command == nil {
		command = workerCommand
	}
	cmd := command(port, stateDir, m.state.ManagedSecret)
	// Never expose worker diagnostics, account material, or credentials in logs.
	cmd.Stdout, cmd.Stderr, cmd.Stdin = io.Discard, io.Discard, nil
	// Uvicorn binds its own socket; release immediately before starting our child.
	// Authenticated readiness (not /health alone) protects this small bind gap.
	if err := listener.Close(); err != nil {
		return m.failLocked("managed web: cannot release private worker port")
	}
	if err := cmd.Start(); err != nil {
		return m.failLocked("managed web: cannot start bundled worker; verify its bundled dependencies and executable permissions")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &child{cmd: cmd, url: "http://127.0.0.1:" + strconv.Itoa(port), done: make(chan struct{}), ctx: ctx, cancel: cancel}
	m.proc = p
	m.stage, m.lastError = "starting", ""
	go m.wait(p)
	go m.awaitReady(p, m.state.ManagedSecret)
	return nil
}

func workerCommand(port int, stateDir, secret string) *exec.Cmd {
	cmd := exec.Command(workerPython, "-m", "uvicorn", "app:create_app", "--factory", "--app-dir", workerDir, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--no-access-log", "--timeout-graceful-shutdown", "650")
	cmd.Env = workerEnvironment(os.Environ(), stateDir, secret)
	cmd.Dir = workerDir
	return cmd
}

func workerEnvironment(env []string, stateDir, secret string) []string {
	out := make([]string, 0, len(env)+2)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if name != "CHATGPT_WEB_STATE_DIR" && name != "CHATGPT_WEB_WORKER_SECRET" {
			out = append(out, entry)
		}
	}
	return append(out, "CHATGPT_WEB_STATE_DIR="+stateDir, "CHATGPT_WEB_WORKER_SECRET="+secret)
}

func (m *Manager) failLocked(message string) error {
	m.stage, m.lastError = "error", message
	return errors.New(message)
}

func (m *Manager) drainLocked(p *child) error {
	p.ready = false
	p.cancel()
	m.stage = "draining"
	if p.stopping {
		return nil
	}
	p.stopping = true
	if err := p.cmd.Process.Signal(syscall.Signal(15)); err != nil && !errors.Is(err, os.ErrProcessDone) {
		p.failure = "managed web: cannot request graceful worker shutdown; owned process has not been killed"
		m.lastError = p.failure
		return errors.New(p.failure)
	}
	return nil
}

func (m *Manager) wait(p *child) {
	_ = p.cmd.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	p.cancel()
	p.ready = false
	if m.proc == p {
		m.proc = nil
		switch {
		case p.failure != "":
			m.stage, m.lastError = "error", p.failure
		case p.stopping:
			m.stage, m.lastError = "stopped", ""
		default:
			m.stage, m.lastError = "error", "managed web: owned worker exited unexpectedly; explicitly ensure or re-enable to retry"
		}
	}
	close(p.done)
}

func (m *Manager) awaitReady(p *child, secret string) {
	ctx, cancel := context.WithTimeout(p.ctx, m.readyWithin)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 500 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if probe(ctx, client, p.url, secret) {
			m.mu.Lock()
			accepted := ctx.Err() == nil && m.proc == p && !p.stopping && m.state.Config.Enabled && !m.closed
			if accepted {
				p.ready = true
				m.stage, m.lastError = "running", ""
			}
			m.mu.Unlock()
			if accepted {
				return
			}
		}
		select {
		case <-p.ctx.Done():
			return
		case <-ctx.Done():
			m.mu.Lock()
			if m.proc == p && !p.stopping {
				p.failure = "managed web: worker readiness timed out after 10 seconds; graceful shutdown requested"
				m.lastError = p.failure
				_ = m.drainLocked(p)
			}
			m.mu.Unlock()
			return
		case <-ticker.C:
		}
	}
}

func probe(ctx context.Context, client *http.Client, origin, secret string) bool {
	request := func(path, token string, status int, key, want string) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+path, nil)
		if err != nil {
			return false
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
		if err != nil || len(body) > 1024 || resp.StatusCode != status {
			return false
		}
		var data map[string]any
		if json.Unmarshal(body, &data) != nil || data[key] != want {
			return false
		}
		return path != "/health" || data["protocol_version"] == "1"
	}
	// Invalid IDs are rejected before any ledger lookup or upstream work. Check
	// rejection with missing and wrong credentials too, not just public health.
	return request("/health", "", 200, "status", "ok") &&
		request("/v1/executions/!", "", 401, "error_code", "auth_required") &&
		request("/v1/executions/!", "managedweb-readiness-invalid", 401, "error_code", "auth_required") &&
		request("/v1/executions/!", secret, 400, "code", "invalid_request")
}

func (m *Manager) View() View {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.state.Config
	cfg.WorkerSecret = ""
	secretSet := m.state.ManagedSecret != ""
	available := runtime.GOOS == "linux" && (m.command != nil || bundledAvailable())
	if cfg.Mode == "external" {
		secretSet = m.state.ExternalSecret != ""
		available = cfg.WorkerURL != "" && secretSet
	} else if m.proc != nil {
		cfg.WorkerURL = m.proc.url
	}
	return View{Config: cfg, SecretSet: secretSet, Running: m.proc != nil, Stage: m.stage, LastError: m.lastError, Available: available}
}

// Runtime is the only secret-bearing accessor. External mode is not health-probed;
// managed mode requires authenticated readiness of this manager's live child.
func (m *Manager) Runtime() (Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.state.Config.Enabled {
		return Config{}, ErrUnavailable
	}
	cfg := m.privateConfigLocked()
	if cfg.Mode == "managed" {
		if m.proc == nil || !m.proc.ready || m.proc.stopping {
			return Config{}, ErrUnavailable
		}
		cfg.WorkerURL, cfg.WorkerSecret = m.proc.url, m.state.ManagedSecret
	}
	return cfg, nil
}

// Close gates Runtime immediately and sends SIGTERM only to its owned child. It
// waits at most one second, then returns an error if still draining; Wait continues
// asynchronously. It never kills requests, deletes state, or disables saved config.
// The host must allow up to the worker's 650-second graceful deadline before exit.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	p := m.proc
	if p == nil {
		m.stage = "stopped"
		m.mu.Unlock()
		return nil
	}
	err := m.drainLocked(p)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	timer := time.NewTimer(closeTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
		return errors.New("managed web: owned worker is still draining; no requests were forcibly terminated")
	}
}
