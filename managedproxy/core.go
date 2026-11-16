package managedproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"time"
)

type coreProcess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	binary  string
	dir     string
	config  []byte
	port    int
	secret  string
	ports   map[int]bool
	version string
	healthy bool
}

func (p *coreProcess) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// No rules reference geodata, DNS/TUN are disabled, and the default route rejects.
// Only explicitly targeted authenticated loopback mixed listeners can carry traffic.
func engineJSON(root string, s diskState, controller int, secret string, listeners bool) ([]byte, error) {
	providers := map[string]any{}
	for _, sub := range s.Subscriptions {
		if !sub.Enabled || s.Feeds[sub.ID] == "" {
			continue
		}
		providers[providerName(sub.ID)] = map[string]any{
			"type": "file", "path": filepath.ToSlash(filepath.Join(root, "feeds", s.Feeds[sub.ID])),
			"health-check": map[string]any{"enable": false},
			"override":     map[string]any{"additional-prefix": prefix(sub.ID)},
		}
	}
	inbounds := []any{}
	groups := []any{}
	if listeners {
		for _, n := range s.Nodes {
			if !n.Active {
				continue
			}
			if _, ok := providers[providerName(n.SubscriptionID)]; !ok {
				return nil, errors.New("managed proxy: active listener has no provider")
			}
			port := s.Ports[n.Key]
			if port < 1024 || port > 65535 {
				return nil, errors.New("managed proxy: invalid listener port")
			}
			groups = append(groups, routeGroup(n))
			inbounds = append(inbounds, map[string]any{
				"name": "mp_" + n.Key, "type": "mixed", "listen": "127.0.0.1", "port": port,
				"udp": false, "proxy": routeGroupName(n),
				"users": []any{map[string]string{"username": s.Username, "password": s.Password}},
			})
		}
	}
	return json.Marshal(map[string]any{
		"port": 0, "socks-port": 0, "mixed-port": 0, "redir-port": 0, "tproxy-port": 0,
		"allow-lan": false, "bind-address": "127.0.0.1", "ipv6": false,
		"skip-auth-prefixes": []string{},
		"mode":               "rule", "log-level": "silent", "find-process-mode": "off",
		"external-controller": net.JoinHostPort("127.0.0.1", strconv.Itoa(controller)), "secret": secret,
		"external-ui": "", "geodata-mode": false, "geo-auto-update": false,
		"dns": map[string]any{"enable": false}, "tun": map[string]any{"enable": false},
		"profile":         map[string]any{"store-selected": false, "store-fake-ip": false},
		"proxy-providers": providers, "proxy-groups": groups, "listeners": inbounds,
		"rules": []string{"MATCH,REJECT"},
	})
}

func freePort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, errors.New("managed proxy: no free staging controller port")
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
func portAvailable(port int) error {
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("managed proxy: loopback port %d is occupied; refusing to adopt an existing service", port)
	}
	return l.Close()
}

func validateConfig(ctx context.Context, binary, root string, config []byte) error {
	if runtime.GOOS != "linux" {
		return errors.New("managed proxy: native launch is Linux-only (Windows needs WMI)")
	}
	dir, err := os.MkdirTemp(filepath.Join(root, "runs"), "validate-")
	if err != nil {
		return errors.New("managed proxy: cannot create validation directory")
	}
	defer os.RemoveAll(dir)
	if err := privateDir(dir); err != nil {
		return err
	}
	payload, err := localizeProviders(root, dir, config)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "validate.json")
	if err := atomicFile(path, payload, 0600); err != nil {
		return err
	}
	defer os.Remove(path)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-t", "-d", dir, "-f", path)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Run() != nil {
		return errors.New("managed proxy: native configuration validation failed (output suppressed to protect feed credentials)")
	}
	return nil
}

func (m *Manager) launchCore(ctx context.Context, binary string, config []byte, port int, secret string) (*coreProcess, error) {
	root := m.root
	if runtime.GOOS != "linux" {
		return nil, errors.New("managed proxy: native launch is Linux-only (Windows needs WMI)")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := portAvailable(port); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Join(root, "runs"), "core-")
	if err != nil {
		return nil, errors.New("managed proxy: cannot create private runtime directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		os.RemoveAll(dir)
		return nil, errors.New("managed proxy: cannot protect runtime directory")
	}
	payload, err := localizeProviders(root, dir, config)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	path := filepath.Join(dir, "config.json")
	if err := atomicFile(path, payload, 0600); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	p := &coreProcess{binary: binary, dir: dir, config: append([]byte(nil), config...), port: port, secret: secret, done: make(chan struct{}), ports: map[int]bool{}}
	p.cmd = exec.Command(binary, "-d", dir, "-f", path)
	p.cmd.Stdout, p.cmd.Stderr = io.Discard, io.Discard
	if p.cmd.Start() != nil {
		os.RemoveAll(dir)
		return nil, errors.New("managed proxy: owned mihomo process could not start")
	}
	tracked := m.children[:0]
	for _, child := range m.children {
		if child.alive() {
			tracked = append(tracked, child)
		}
	}
	m.children = append(tracked, p)
	go func() { _ = p.cmd.Wait(); close(p.done) }()
	readyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		var v struct {
			Version string `json:"version"`
		}
		if err := p.request(readyCtx, "/version", &v); err == nil {
			if v.Version == "" || len(v.Version) > 128 || strings.ContainsAny(v.Version, "\r\n\x00/:@") {
				_ = p.stop()
				return nil, errors.New("managed proxy: invalid native version response")
			}
			p.version = v.Version
			return p, nil
		}
		select {
		case <-p.done:
			_ = p.stop()
			return nil, errors.New("managed proxy: owned mihomo exited before controller readiness")
		case <-readyCtx.Done():
			_ = p.stop()
			return nil, errors.New("managed proxy: owned controller readiness timed out")
		case <-ticker.C:
		}
	}
}

// request never follows redirects or uses an environment/global proxy. It is only
// called after launching the recorded child, never to discover/adopt another core.
func (p *coreProcess) request(ctx context.Context, path string, out any) error {
	if !p.alive() {
		return errors.New("managed proxy: owned core is not running")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 64 << 10, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(p.port)+path, nil)
	if err != nil {
		return errors.New("managed proxy: invalid controller request")
	}
	req.Header.Set("Authorization", "Bearer "+p.secret)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("managed proxy: owned controller request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("managed proxy: owned controller rejected request")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(b) > 16<<20 || json.Unmarshal(b, out) != nil {
		return errors.New("managed proxy: invalid or oversized native controller response")
	}
	return nil
}

func (p *coreProcess) provider(ctx context.Context, id string) ([]providerProxy, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var result struct {
			Proxies []providerProxy `json:"proxies"`
		}
		if err := p.request(ctx, "/providers/proxies/"+url.PathEscape(providerName(id)), &result); err != nil {
			return nil, err
		}
		if len(result.Proxies) > 0 {
			return result.Proxies, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("managed proxy: native provider did not become ready or contains no usable nodes")
		case <-p.done:
			return nil, errors.New("managed proxy: owned core exited during provider initialization")
		case <-ticker.C:
		}
	}
}

func (p *coreProcess) stop() error {
	if p == nil {
		return nil
	}
	if p.alive() {
		_ = p.cmd.Process.Signal(os.Interrupt)
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-p.done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			_ = p.cmd.Process.Kill()
			killTimer := time.NewTimer(2 * time.Second)
			defer killTimer.Stop()
			select {
			case <-p.done:
			case <-killTimer.C:
				return errors.New("managed proxy: owned subprocess could not be reaped")
			}
		}
	}
	_ = os.RemoveAll(p.dir)
	return nil
}

func (m *Manager) monitor(p *coreProcess) {
	go func() {
		<-p.done
		m.mu.Lock()
		if m.proc != p || m.closed {
			m.mu.Unlock()
			return
		}
		m.state.LastError = "managed proxy: owned mihomo exited; local nodes are inactive until Ensure succeeds"
		_ = m.persist(m.state)
		hook := m.hook
		m.mu.Unlock()
		if hook != nil {
			hook()
		}
	}()
}

// activate deliberately uses a fully validated stop/restart, avoiding cores whose
// forced reload can silently retain a removed inbound or partially apply a config.
func (m *Manager) activate(ctx context.Context, binary string, config []byte, s diskState) (func(), error) {
	old := m.proc
	for _, n := range s.Nodes {
		if !n.Active {
			continue
		}
		p := s.Ports[n.Key]
		if old != nil && old.alive() && old.ports[p] {
			continue
		}
		if err := portAvailable(p); err != nil {
			return nil, err
		}
	}
	if old == nil || !old.alive() || old.port != s.Engine.ControllerPort {
		if err := portAvailable(s.Engine.ControllerPort); err != nil {
			return nil, err
		}
	}
	restore := func() {
		if m.proc != nil && m.proc != old {
			m.proc.healthy = false
			if m.proc.stop() != nil {
				return
			}
		}
		m.proc = nil
		if old == nil || m.ctx.Err() != nil {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(m.ctx, 25*time.Second)
		defer cancel()
		p, err := m.launchCore(rollbackCtx, old.binary, old.config, old.port, old.secret)
		if err == nil {
			if selectErr := p.selectNodes(rollbackCtx, m.state.Nodes); selectErr != nil {
				_ = p.stop()
				return
			}
			p.ports = old.ports
			p.healthy = old.healthy
			m.proc = p
			m.version = p.version
			m.monitor(p)
		}
	}
	if old != nil {
		if err := old.stop(); err != nil {
			old.healthy = false
			return nil, err
		}
	}
	m.proc = nil
	p, err := m.launchCore(ctx, binary, config, s.Engine.ControllerPort, s.Secret)
	if err != nil {
		restore()
		return nil, err
	}
	m.proc = p
	// Re-read the real main providers, not just the stage; immutable feed files
	// must produce exactly the same original selected names before DB activation.
	for _, sub := range s.Subscriptions {
		if !sub.Enabled || s.Feeds[sub.ID] == "" {
			continue
		}
		proxies, err := p.provider(ctx, sub.ID)
		if err != nil {
			restore()
			return nil, err
		}
		names, _, err := selectNames(sub, proxies)
		if err != nil {
			restore()
			return nil, err
		}
		expected := make([]string, 0)
		for _, n := range s.Nodes {
			if n.Active && n.SubscriptionID == sub.ID {
				expected = append(expected, n.Name)
			}
		}
		a, _ := json.Marshal(names)
		b, _ := json.Marshal(expected)
		if !bytes.Equal(a, b) {
			restore()
			return nil, errors.New("managed proxy: provider changed between staging and activation")
		}
	}
	for _, n := range s.Nodes {
		if !n.Active {
			continue
		}
		port := s.Ports[n.Key]
		if selectErr := p.selectNode(ctx, n); selectErr != nil {
			restore()
			return nil, selectErr
		}
		conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			restore()
			return nil, fmt.Errorf("managed proxy: owned listener on port %d did not bind", port)
		}
		_ = conn.Close()
		p.ports[port] = true
	}
	p.healthy = true
	m.version = p.version
	m.monitor(p)
	return restore, nil
}

// Every core gets a separate home/cache and provider copies inside that home.
// This avoids both native safe-path restrictions and concurrent cache-file locks.
// Keep the original config for rollback; previous runtime directories are removed.
func localizeProviders(root, dir string, config []byte) ([]byte, error) {
	var decoded map[string]any
	if json.Unmarshal(config, &decoded) != nil {
		return nil, errors.New("managed proxy: invalid internal config")
	}
	providers, ok := decoded["proxy-providers"].(map[string]any)
	if !ok {
		return nil, errors.New("managed proxy: missing internal providers")
	}
	feedDir := filepath.Join(dir, "feeds")
	if err := privateDir(feedDir); err != nil {
		return nil, err
	}
	for _, entry := range providers {
		provider, ok := entry.(map[string]any)
		if !ok {
			return nil, errors.New("managed proxy: invalid internal provider")
		}
		source, ok := provider["path"].(string)
		if !ok || filepath.Clean(filepath.FromSlash(source)) != filepath.Join(root, "feeds", filepath.Base(source)) {
			return nil, errors.New("managed proxy: provider source escapes private feed directory")
		}
		body, err := readPrivate(filepath.FromSlash(source), feedLimit)
		if err != nil {
			return nil, errors.New("managed proxy: cannot read immutable provider source")
		}
		dest := filepath.Join(feedDir, filepath.Base(source))
		old, err := readPrivate(dest, feedLimit)
		if os.IsNotExist(err) {
			if err := atomicFile(dest, body, 0600); err != nil {
				return nil, err
			}
		} else if err != nil || !bytes.Equal(old, body) {
			return nil, errors.New("managed proxy: immutable runtime provider copy changed")
		}
		provider["path"] = filepath.ToSlash(dest)
	}
	return json.Marshal(decoded)
}
