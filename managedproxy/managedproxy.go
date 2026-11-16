// Package managedproxy owns an isolated mihomo process and its loopback listeners.
// Subscription URLs and downloaded feeds are private; View redacts subscription URLs.
package managedproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

type EngineConfig struct {
	BinaryPath     string `json:"binary_path"`
	AutoInstall    bool   `json:"auto_install"`
	ControllerPort int    `json:"controller_port"`
	PortStart      int    `json:"port_start"`
}

type Subscription struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	URLSet         bool   `json:"url_set"`
	Enabled        bool   `json:"enabled"`
	IncludeRegex   string `json:"include_regex"`
	ExcludeRegex   string `json:"exclude_regex"`
	DedupeRegex    string `json:"dedupe_regex"`
	PreferRegex    string `json:"prefer_regex"`
	RefreshMinutes int    `json:"refresh_minutes"`
	LastUpdated    string `json:"last_updated"`
	LastError      string `json:"last_error"`
	TotalNodes     int    `json:"total_nodes"`
	KeptNodes      int    `json:"kept_nodes"`
}

type Node struct {
	Key            string `json:"key"`
	SubscriptionID string `json:"subscription_id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	Active         bool   `json:"active"`
}

type View struct {
	Engine        EngineConfig   `json:"engine"`
	Subscriptions []Subscription `json:"subscriptions"`
	Nodes         []Node         `json:"nodes"`
	Running       bool           `json:"running"`
	Version       string         `json:"version"`
	LastError     string         `json:"last_error"`
}

type diskState struct {
	Format        int               `json:"format"`
	Engine        EngineConfig      `json:"engine"`
	Subscriptions []Subscription    `json:"subscriptions"`
	Nodes         []Node            `json:"nodes"`
	Ports         map[string]int    `json:"ports"`
	Feeds         map[string]string `json:"feeds"`
	NextPort      int               `json:"next_port"`
	Username      string            `json:"username"`
	Password      string            `json:"password"`
	Secret        string            `json:"secret"`
	LastError     string            `json:"last_error"`
}

// Manager serializes mutations, including periodic refreshes. It never adopts an
// existing controller or changes the application's HTTP transport or host network.
type Manager struct {
	mu           contextMutex
	viewSnapshot atomic.Value
	root         string
	state        diskState
	proc         *coreProcess
	children     []*coreProcess
	version      string
	hook         func()
	closed       bool
	cancel       context.CancelFunc
	ctx          context.Context
	scheduler    bool
	fetchFeed    func(context.Context, string, int64) ([]byte, error)
}

// New stores all private files beneath dataDir/proxy-subscriptions. It does not
// start a process or fetch subscriptions; call Ensure after application startup.
func New(dataDir string) (*Manager, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("managed proxy: data directory is required")
	}
	root, err := filepath.Abs(filepath.Join(dataDir, "proxy-subscriptions"))
	if err != nil {
		return nil, errors.New("managed proxy: invalid data directory")
	}
	for _, dir := range []string{root, filepath.Join(root, "feeds"), filepath.Join(root, "bin"), filepath.Join(root, "runs")} {
		if err := privateDir(dir); err != nil {
			return nil, err
		}
	}
	m := &Manager{root: root, fetchFeed: fetchRemote, mu: contextMutex{gate: make(chan struct{}, 1)}}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	b, err := readPrivate(filepath.Join(root, "state.json"), 16<<20)
	if err == nil {
		if len(b) > 16<<20 || json.Unmarshal(b, &m.state) != nil || m.state.Format != 1 {
			m.cancel()
			return nil, errors.New("managed proxy: invalid private state")
		}
		if err := m.validateState(); err != nil {
			m.cancel()
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		m.cancel()
		return nil, errors.New("managed proxy: cannot read private state")
	} else {
		m.state = diskState{Format: 1, Engine: EngineConfig{AutoInstall: true, ControllerPort: 19090, PortStart: 20000}, Ports: map[string]int{}, Feeds: map[string]string{}, NextPort: 20000}
		for _, p := range []*string{&m.state.Username, &m.state.Password, &m.state.Secret} {
			*p, err = randomToken(24)
			if err != nil {
				m.cancel()
				return nil, err
			}
		}
		if err := m.persist(m.state); err != nil {
			m.cancel()
			return nil, err
		}
	}
	m.viewSnapshot.Store(m.viewLocked())
	return m, nil
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return errors.New("managed proxy: cannot create private directory")
	}
	st, err := os.Lstat(path)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed proxy: private directory must not be a symlink")
	}
	if os.Chmod(path, 0700) != nil {
		return errors.New("managed proxy: cannot protect private directory")
	}
	return nil
}

func (m *Manager) validateState() error {
	s := &m.state
	c, err := normalizeEngine(s.Engine)
	if err != nil {
		return errors.New("managed proxy: invalid saved engine settings")
	}
	s.Engine = c
	if s.Ports == nil || s.Feeds == nil || len(s.Secret) < 32 || len(s.Password) < 32 || len(s.Username) < 16 {
		return errors.New("managed proxy: incomplete private state")
	}
	used := map[int]bool{}
	for key, port := range s.Ports {
		if !hexID(key, 64) || port < 1024 || port > 65535 || used[port] || port == s.Engine.ControllerPort {
			return errors.New("managed proxy: invalid saved port allocation")
		}
		used[port] = true
	}
	seen := map[string]bool{}
	for _, sub := range s.Subscriptions {
		if !hexID(sub.ID, 32) || seen[sub.ID] {
			return errors.New("managed proxy: invalid saved subscription ID")
		}
		seen[sub.ID] = true
		if err := validateSubscription(sub); err != nil {
			return errors.New("managed proxy: invalid saved subscription")
		}
	}
	for id, name := range s.Feeds {
		if !hexID(id, 32) || filepath.Base(name) != name || !strings.HasSuffix(name, ".feed") || !hexID(strings.TrimSuffix(name, ".feed"), 64) {
			return errors.New("managed proxy: invalid saved feed reference")
		}
	}
	for _, node := range s.Nodes {
		if !hexID(node.SubscriptionID, 32) || node.Key != nodeKey(node.SubscriptionID, node.Name) || s.Ports[node.Key] == 0 || node.URL != localURL(*s, s.Ports[node.Key]) {
			return errors.New("managed proxy: invalid saved node")
		}
	}
	return nil
}

func normalizeEngine(c EngineConfig) (EngineConfig, error) {
	if c.ControllerPort == 0 {
		c.ControllerPort = 19090
	}
	if c.PortStart == 0 {
		c.PortStart = 20000
	}
	if c.ControllerPort < 1024 || c.ControllerPort > 65535 || c.PortStart < 1024 || c.PortStart > 65535 || c.ControllerPort == c.PortStart {
		return c, errors.New("managed proxy: ports must be distinct and between 1024 and 65535")
	}
	c.BinaryPath = strings.TrimSpace(c.BinaryPath)
	return c, nil
}

// View returns copies. Subscription.URL is always empty and URLSet reports whether
// it exists. Node.URL deliberately contains the authenticated *local* proxy URL for
// the caller's DB reconciliation; do not publish it to unauthenticated clients.
func (m *Manager) View() View {
	if !m.mu.tryLock() {
		if cached, ok := m.viewSnapshot.Load().(View); ok {
			return copyView(cached)
		}
		return View{Subscriptions: []Subscription{}, Nodes: []Node{}}
	}
	defer m.mu.Unlock()
	v := m.viewLocked()
	m.viewSnapshot.Store(v)
	return copyView(v)
}

func copyView(v View) View {
	v.Subscriptions = append([]Subscription{}, v.Subscriptions...)
	v.Nodes = append([]Node{}, v.Nodes...)
	return v
}

func (m *Manager) viewLocked() View { return m.viewForState(m.state) }

func (m *Manager) viewForState(s diskState) View {
	running := !m.closed && m.proc != nil && m.proc.healthy && m.proc.alive()
	v := View{Engine: s.Engine, Subscriptions: []Subscription{}, Nodes: []Node{}, Running: running, Version: m.version, LastError: s.LastError}
	for _, sub := range s.Subscriptions {
		v.Subscriptions = append(v.Subscriptions, publicSubscription(sub))
	}
	for _, n := range s.Nodes {
		n.Active = n.Active && running
		v.Nodes = append(v.Nodes, n)
	}
	return v
}

func publicSubscription(s Subscription) Subscription { s.URLSet = s.URL != ""; s.URL = ""; return s }

func (m *Manager) SetChangeHook(hook func()) { m.mu.Lock(); m.hook = hook; m.mu.Unlock() }

// Configure persists settings for the next Ensure/Refresh without interrupting
// healthy listeners. A controller port can never reuse a historical node port.
func (m *Manager) Configure(ctx context.Context, config EngineConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := normalizeEngine(config)
	if err != nil {
		return err
	}
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("managed proxy: manager is closed")
	}
	for _, p := range m.state.Ports {
		if p == c.ControllerPort {
			return errors.New("managed proxy: controller conflicts with a reserved node port")
		}
	}
	s := cloneState(m.state)
	s.Engine = c
	if len(s.Ports) == 0 {
		s.NextPort = c.PortStart
	}
	if err := m.persist(s); err != nil {
		return err
	}
	m.state = s
	return nil
}

// SaveSubscription does not fetch or reconfigure the core. Blank URL on an edit
// preserves the existing private URL; IDs and telemetry cannot be supplied by clients.
func (m *Manager) SaveSubscription(ctx context.Context, sub Subscription) (Subscription, error) {
	if err := ctx.Err(); err != nil {
		return Subscription{}, err
	}
	if err := m.mu.lock(ctx); err != nil {
		return Subscription{}, err
	}
	defer m.mu.Unlock()
	if m.closed {
		return Subscription{}, errors.New("managed proxy: manager is closed")
	}
	s := cloneState(m.state)
	idx := -1
	if sub.ID != "" {
		for i := range s.Subscriptions {
			if s.Subscriptions[i].ID == sub.ID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return Subscription{}, errors.New("managed proxy: subscription not found")
		}
		old := s.Subscriptions[idx]
		if sub.URL == "" {
			sub.URL = old.URL
		}
		sub.LastUpdated, sub.LastError, sub.TotalNodes, sub.KeptNodes = old.LastUpdated, old.LastError, old.TotalNodes, old.KeptNodes
	} else {
		var err error
		sub.ID, err = randomToken(16)
		if err != nil {
			return Subscription{}, err
		}
		sub.LastUpdated, sub.LastError, sub.TotalNodes, sub.KeptNodes = "", "", 0, 0
	}
	sub.URLSet = sub.URL != ""
	if err := validateSubscription(sub); err != nil {
		return Subscription{}, err
	}
	if idx < 0 {
		s.Subscriptions = append(s.Subscriptions, sub)
	} else {
		s.Subscriptions[idx] = sub
	}
	if err := m.persist(s); err != nil {
		return Subscription{}, err
	}
	m.state = s
	return publicSubscription(sub), nil
}

func validateSubscription(s Subscription) error {
	if strings.TrimSpace(s.Name) == "" || len(s.Name) > 256 || strings.ContainsAny(s.Name, "\r\n\x00") {
		return errors.New("managed proxy: name is required (maximum 256 bytes, no control characters)")
	}
	if s.RefreshMinutes != 0 && (s.RefreshMinutes < 5 || s.RefreshMinutes > 525600) {
		return errors.New("managed proxy: refresh_minutes must be 0 or between 5 and 525600")
	}
	if _, err := validateRemoteURL(s.URL); err != nil {
		return err
	}
	for _, pattern := range []string{s.IncludeRegex, s.ExcludeRegex, s.DedupeRegex, s.PreferRegex} {
		if len(pattern) > 4096 {
			return errors.New("managed proxy: regular expression is too long")
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return errors.New("managed proxy: invalid regular expression")
		}
	}
	return nil
}

func (m *Manager) persist(s diskState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return errors.New("managed proxy: cannot encode private state")
	}
	if len(b) > 16<<20 {
		return errors.New("managed proxy: private state exceeds safe size limit")
	}
	if err := atomicFile(filepath.Join(m.root, "state.json"), b, 0600); err != nil {
		return err
	}
	m.viewSnapshot.Store(m.viewForState(s))
	return nil
}

func atomicFile(path string, content []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return errors.New("managed proxy: cannot create private file")
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(content)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return errors.New("managed proxy: cannot atomically save private file")
	}
	return nil
}

func cloneState(s diskState) diskState {
	b, _ := json.Marshal(s)
	var c diskState
	_ = json.Unmarshal(b, &c)
	return c
}
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("managed proxy: secure randomness unavailable")
	}
	return hex.EncodeToString(b), nil
}
func hexID(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}
func nodeKey(id, name string) string {
	h := sha256.Sum256([]byte(id + "\x00" + name))
	return hex.EncodeToString(h[:])
}
func providerName(id string) string { return "mp_" + id }
func prefix(id string) string       { return providerName(id) + "__" }
func localURL(s diskState, port int) string {
	return (&url.URL{Scheme: "http", User: url.UserPassword(s.Username, s.Password), Host: fmt.Sprintf("127.0.0.1:%d", port)}).String()
}

// allocate never reuses historical ports, even after a subscription is deleted.
func allocate(s *diskState, id, name string) (Node, error) {
	key := nodeKey(id, name)
	port, exists := s.Ports[key]
	if !exists {
		if s.NextPort < s.Engine.PortStart {
			s.NextPort = s.Engine.PortStart
		}
		used := map[int]bool{s.Engine.ControllerPort: true}
		for _, p := range s.Ports {
			used[p] = true
		}
		for s.NextPort <= 65535 && used[s.NextPort] {
			s.NextPort++
		}
		if s.NextPort > 65535 {
			return Node{}, errors.New("managed proxy: stable listener port range exhausted")
		}
		port = s.NextPort
		s.Ports[key] = port
		s.NextPort++
	}
	return Node{Key: key, SubscriptionID: id, Name: name, URL: localURL(*s, port), Active: true}, nil
}

func sortedNodes(nodes map[string]Node) []Node {
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SubscriptionID == out[j].SubscriptionID {
			return out[i].Name < out[j].Name
		}
		return out[i].SubscriptionID < out[j].SubscriptionID
	})
	return out
}

// operationContext bounds manual work and cancels it when Close is called.
func (m *Manager) operationContext(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	stop := context.AfterFunc(m.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

// A channel mutex lets queued administrative operations honor cancellation
// without launching lock-wait goroutines that outlive their requests.
type contextMutex struct{ gate chan struct{} }

func (m *contextMutex) Lock()   { m.gate <- struct{}{} }
func (m *contextMutex) Unlock() { <-m.gate }
func (m *contextMutex) tryLock() bool {
	select {
	case m.gate <- struct{}{}:
		return true
	default:
		return false
	}
}
func (m *contextMutex) lock(ctx context.Context) error {
	select {
	case m.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func readPrivate(path string, limit int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("managed proxy: private file is invalid or oversized")
	}
	if os.Chmod(path, 0600) != nil {
		return nil, errors.New("managed proxy: cannot protect private file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("managed proxy: cannot open private file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("managed proxy: cannot read bounded private file")
	}
	return b, nil
}
