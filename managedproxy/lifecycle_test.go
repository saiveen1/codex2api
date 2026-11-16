package managedproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The test binary doubles as an isolated synthetic native core. These tests do
// not establish compatibility with real mihomo; that needs a separate Linux check.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "-d" || os.Args[1] == "-t") {
		os.Exit(syntheticCore(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func syntheticCore(args []string) int {
	path := ""
	validate := false
	for i := 0; i < len(args); i++ {
		if args[i] == "-t" {
			validate = true
			continue
		}
		if (args[i] == "-d" || args[i] == "-f") && i+1 < len(args) {
			if args[i] == "-f" {
				path = args[i+1]
			}
			i++
		}
	}
	var config struct {
		Controller string `json:"external-controller"`
		Secret     string `json:"secret"`
		Providers  map[string]struct {
			Path     string `json:"path"`
			Override struct {
				Prefix string `json:"additional-prefix"`
			} `json:"override"`
		} `json:"proxy-providers"`
		Groups []struct {
			Name   string `json:"name"`
			Filter string `json:"filter"`
		} `json:"proxy-groups"`
		Listeners []struct {
			Listen string `json:"listen"`
			Port   int    `json:"port"`
			Proxy  string `json:"proxy"`
		} `json:"listeners"`
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &config) != nil {
		return 40
	}
	if validate {
		return 0
	}
	providers := map[string][]providerProxy{}
	for name, p := range config.Providers {
		b, err := os.ReadFile(p.Path)
		if err != nil {
			return 41
		}
		var feed struct {
			Proxies []providerProxy `json:"proxies"`
		}
		if json.Unmarshal(b, &feed) != nil {
			return 42
		}
		for i := range feed.Proxies {
			feed.Proxies[i].Name = p.Override.Prefix + feed.Proxies[i].Name
		}
		providers[name] = feed.Proxies
	}
	if len(config.Listeners) > 0 {
		for _, group := range config.Groups {
			if strings.Contains(group.Filter, "fail-main") {
				return 43
			}
		}
	}
	for _, l := range config.Listeners {
		if strings.Contains(l.Proxy, "fail-main") {
			return 43
		}
		ln, err := net.Listen("tcp4", net.JoinHostPort(l.Listen, strconv.Itoa(l.Port)))
		if err != nil {
			return 44
		}
		defer ln.Close()
		go func(l net.Listener) {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}(ln)
	}
	selections := map[string]string{}
	var selectionsMu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+config.Secret {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			_ = json.NewEncoder(w).Encode(map[string]string{"version": "synthetic-1"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/proxies/") {
			group := strings.TrimPrefix(r.URL.Path, "/proxies/")
			selectionsMu.Lock()
			defer selectionsMu.Unlock()
			if r.Method == http.MethodPut {
				var choice struct {
					Name string `json:"name"`
				}
				if json.NewDecoder(r.Body).Decode(&choice) != nil {
					w.WriteHeader(400)
					return
				}
				selections[group] = choice.Name
				w.WriteHeader(204)
				return
			}
			now := selections[group]
			if now == "" {
				now = "REJECT"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"now": now})
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/providers/proxies/")
		if proxies, ok := providers[name]; ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"proxies": proxies})
			return
		}
		w.WriteHeader(404)
	})
	server := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp4", config.Controller)
	if err != nil {
		return 45
	}
	if server.Serve(ln) != nil {
		return 46
	}
	return 0
}

func setupSynthetic(t *testing.T) *Manager {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("owned synthetic process tests are Linux-only")
	}
	m := testManager(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	controller, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	start := 0
	for i := 0; i < 20; i++ {
		p, err := freePort()
		if err != nil {
			t.Fatal(err)
		}
		if p == controller || p > 65000 {
			continue
		}
		good := true
		for n := p; n < p+4; n++ {
			if n == controller || portAvailable(n) != nil {
				good = false
				break
			}
		}
		if good {
			start = p
			break
		}
	}
	if start == 0 {
		t.Fatal("no synthetic listener range")
	}
	if err := m.Configure(context.Background(), EngineConfig{BinaryPath: binary, ControllerPort: controller, PortStart: start}); err != nil {
		t.Fatal(err)
	}
	return m
}
func feed(names ...string) []byte {
	proxies := []providerProxy{}
	for _, n := range names {
		proxies = append(proxies, providerProxy{Name: n, Type: "Vmess"})
	}
	b, _ := json.Marshal(map[string]any{"proxies": proxies})
	return b
}

func TestRefreshStagingFailureAndLastGoodPreservation(t *testing.T) {
	m := setupSynthetic(t)
	sub := saveTestSub(t, m, "one")
	content := feed("HK good")
	m.fetchFeed = func(context.Context, string, int64) ([]byte, error) { return content, nil }
	calls := 0
	m.SetChangeHook(func() { _ = m.View(); calls++ }) // Must be outside the manager mutex.
	if err := m.Refresh(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	oldProc := m.proc
	oldFeed := m.state.Feeds[sub.ID]
	oldNode := m.state.Nodes[0]
	oldUpdated := m.state.Subscriptions[0].LastUpdated
	if !m.View().Running || !m.View().Nodes[0].Active {
		t.Fatal("successful candidate not activated")
	}
	content = []byte("invalid native feed with SECRET credentials")
	if err := m.Refresh(context.Background(), sub.ID); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("bad stage was accepted/leaked content")
	}
	if m.proc != oldProc || !oldProc.alive() || m.state.Feeds[sub.ID] != oldFeed || m.state.Nodes[0] != oldNode || m.state.Subscriptions[0].LastUpdated != oldUpdated {
		t.Fatal("failed stage disturbed healthy main/state")
	}
	entries, err := os.ReadDir(filepath.Join(m.root, "feeds"))
	if err != nil || len(entries) != 1 {
		t.Fatal("failed candidate feed was not removed")
	}
	m.fetchFeed = func(context.Context, string, int64) ([]byte, error) {
		return nil, errors.New("managed proxy: synthetic public fetch failed")
	}
	if err := m.Refresh(context.Background(), sub.ID); err == nil {
		t.Fatal("fetch failure accepted")
	}
	if m.state.Nodes[0] != oldNode || m.state.Subscriptions[0].LastError == "" || m.state.Subscriptions[0].LastUpdated != oldUpdated || m.proc != oldProc {
		t.Fatal("fetch error lost last-good data or telemetry")
	}
	if calls != 3 {
		t.Fatalf("unexpected hook calls: %d", calls)
	}
}

func TestActivationRollbackDeletionAndEmptySelection(t *testing.T) {
	m := setupSynthetic(t)
	sub := saveTestSub(t, m, "one")
	other := saveTestSub(t, m, "two")
	content := feed("HK stable")
	m.fetchFeed = func(context.Context, string, int64) ([]byte, error) { return content, nil }
	if err := m.Refresh(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Nodes) != 1 {
		t.Fatal("pending subscription blocked targeted refresh")
	}
	if err := m.Refresh(context.Background(), other.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	old := append([]Node(nil), m.state.Nodes...)
	if len(old) != 2 || old[0].URL == old[1].URL {
		t.Fatal("native multi-subscription collision")
	}
	content = feed("HK fail-main")
	if err := m.Refresh(context.Background(), sub.ID); err == nil {
		t.Fatal("main startup failure accepted")
	}
	if !m.View().Running || !reflect.DeepEqual(old, m.state.Nodes) {
		t.Fatal("last-good core was not restored")
	}
	content = feed("HK stable")
	if err := m.DeleteSubscription(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	for _, n := range m.View().Nodes {
		if n.SubscriptionID == sub.ID && n.Active {
			t.Fatal("removed subscription listener remains active")
		}
	}
	for _, n := range m.state.Nodes {
		for _, previous := range old {
			if n.Key == previous.Key && n.URL != previous.URL {
				t.Fatal("delete changed stable URL")
			}
		}
	}
	other.ExcludeRegex = ".*"
	if _, err := m.SaveSubscription(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(context.Background(), other.ID); err != nil {
		t.Fatalf("empty kept selection must be valid: %v", err)
	}
	for _, n := range m.View().Nodes {
		if n.Active {
			t.Fatal("filtered listener remained active")
		}
	}
	if err := m.DeleteSubscription(context.Background(), other.ID); err != nil {
		t.Fatal(err)
	}
	if m.View().Running {
		t.Fatal("no enabled subscriptions must stop core")
	}
}

func TestEnsureNoFetchAndOccupiedControllerNotAdopted(t *testing.T) {
	m := setupSynthetic(t)
	sub := saveTestSub(t, m, "one")
	m.fetchFeed = func(context.Context, string, int64) ([]byte, error) { return feed("HK restored"), nil }
	if err := m.Refresh(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(m.root)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m2, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	m2.fetchFeed = func(context.Context, string, int64) ([]byte, error) {
		t.Error("Ensure fetched subscription")
		return nil, errors.New("unexpected fetch")
	}
	occupied, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(m2.state.Engine.ControllerPort)))
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.Ensure(context.Background()); err == nil {
		occupied.Close()
		t.Fatal("unknown preexisting controller was adopted")
	}
	if m2.View().Running {
		t.Fatal("occupied controller not fail closed")
	}
	_ = occupied.Close()
	if err := m2.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m2.View().Running {
		t.Fatal("saved feeds not restored")
	}
}
