package managedproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const testID = "11111111111111111111111111111111"

func testManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}
func saveTestSub(t *testing.T, m *Manager, name string) Subscription {
	t.Helper()
	s, err := m.SaveSubscription(context.Background(), Subscription{Name: name, URL: "https://feed.example/sub?token=PRIVATE_FEED_TOKEN", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFilteringDedupeAndNativeNamespace(t *testing.T) {
	s := Subscription{ID: testID, IncludeRegex: `HK|CN|香港|中国`, ExcludeRegex: `CN|中国`, DedupeRegex: `(HK-[0-9]+)`, PreferRegex: `直连|direct`}
	var proxies []providerProxy
	for _, n := range []string{"HK-1 relay", "HK-1 direct", "HK-2 relay", "CN-1 direct", "香港 independent", "JP-1"} {
		proxies = append(proxies, providerProxy{prefix(testID) + n, "Vmess"})
	}
	for _, typ := range []string{"Direct", "Reject", "Compatible", "Pass", "RejectDrop"} {
		proxies = append(proxies, providerProxy{prefix(testID) + "HK-builtin-" + typ, typ})
	}
	got, total, err := selectNames(s, proxies)
	if err != nil || total != 6 || !reflect.DeepEqual(got, []string{"HK-1 direct", "HK-2 relay", "香港 independent"}) {
		t.Fatalf("names=%v total=%d err=%v", got, total, err)
	}
	// Source order does not decide preference/ties.
	for i, j := 0, len(proxies)-1; i < j; i, j = i+1, j-1 {
		proxies[i], proxies[j] = proxies[j], proxies[i]
	}
	again, _, _ := selectNames(s, proxies)
	if !reflect.DeepEqual(got, again) {
		t.Fatal("unstable dedupe")
	}
	if _, _, err := selectNames(s, []providerProxy{{"HK-not-prefixed", "Vmess"}}); err == nil {
		t.Fatal("missing native prefix accepted")
	}
	if _, _, err := selectNames(s, []providerProxy{{prefix(testID) + "HK-1", "Vmess"}, {prefix(testID) + "HK-1", "Vmess"}}); err == nil {
		t.Fatal("ambiguous duplicate accepted")
	}
}

func TestStablePortsAcrossSubscriptionsPersistenceAndDelete(t *testing.T) {
	m := testManager(t)
	s1 := saveTestSub(t, m, "one")
	s2 := saveTestSub(t, m, "two")
	a, err := allocate(&m.state, s1.ID, "same full name")
	if err != nil {
		t.Fatal(err)
	}
	b, err := allocate(&m.state, s2.ID, "same full name")
	if err != nil {
		t.Fatal(err)
	}
	if a.Key == b.Key || a.URL == b.URL {
		t.Fatal("cross-subscription identity collision")
	}
	m.state.Nodes = []Node{a, b}
	if err := m.persist(m.state); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteSubscription(context.Background(), s1.ID); err != nil {
		t.Fatal(err)
	}
	if m.state.Nodes[0].Active || m.state.Nodes[0].URL != a.URL {
		t.Fatal("deleted URL/history was lost")
	}
	c, err := allocate(&m.state, s2.ID, "new name")
	if err != nil {
		t.Fatal(err)
	}
	if c.URL == a.URL {
		t.Fatal("historical port reused")
	}
	m.state.Nodes = append(m.state.Nodes, c)
	if err := m.persist(m.state); err != nil {
		t.Fatal(err)
	}
	m2, err := New(filepath.Dir(m.root))
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	again, err := allocate(&m2.state, s1.ID, "same full name")
	if err != nil || again.URL != a.URL {
		t.Fatal("stable port/credentials changed across restart")
	}
	for _, n := range m2.View().Nodes {
		if n.Active {
			t.Fatal("startup without core must fail closed")
		}
	}
	if err := m2.Configure(context.Background(), EngineConfig{ControllerPort: m2.state.Ports[a.Key]}); err == nil {
		t.Fatal("controller reused historical port")
	}
}

func TestPublicRedactionBlankEditAndPrivateModes(t *testing.T) {
	m := testManager(t)
	s := saveTestSub(t, m, "secret")
	if s.URL != "" || !s.URLSet {
		t.Fatal("Save leaked URL")
	}
	s.Name = "renamed"
	s.LastUpdated = "forged"
	s.TotalNodes = 123
	if _, err := m.SaveSubscription(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if m.state.Subscriptions[0].URL == "" || m.state.Subscriptions[0].LastUpdated != "" || m.state.Subscriptions[0].TotalNodes != 0 {
		t.Fatal("edit lost private URL or accepted client telemetry")
	}
	b, _ := json.Marshal(m.View())
	if strings.Contains(string(b), "PRIVATE_FEED_TOKEN") || strings.Contains(string(b), m.state.Secret) || !strings.Contains(string(b), `"url_set":true`) {
		t.Fatal("public view leaked private subscription/controller data")
	}
	copy := m.View()
	copy.Subscriptions[0].Name = "mutated"
	if m.View().Subscriptions[0].Name == "mutated" {
		t.Fatal("View aliases state")
	}
	if runtime.GOOS != "windows" {
		for _, item := range []struct {
			path string
			mode os.FileMode
		}{{m.root, 0700}, {filepath.Join(m.root, "feeds"), 0700}, {filepath.Join(m.root, "state.json"), 0600}} {
			st, err := os.Stat(item.path)
			if err != nil || st.Mode().Perm() != item.mode {
				t.Fatalf("private mode %s", item.path)
			}
		}
	}
}

func TestValidation(t *testing.T) {
	m := testManager(t)
	if err := m.Configure(context.Background(), EngineConfig{ControllerPort: 19090, PortStart: 10000}); err != nil {
		t.Fatal(err)
	}
	if m.state.NextPort != 10000 {
		t.Fatal("first configured port_start ignored")
	}
	for _, pattern := range []string{"[", "(?=not-supported)"} {
		_, err := m.SaveSubscription(context.Background(), Subscription{Name: "bad", URL: "https://feed.example", IncludeRegex: pattern})
		if err == nil {
			t.Fatal("invalid regex accepted")
		}
	}
	for _, minutes := range []int{-1, 1, 4} {
		_, err := m.SaveSubscription(context.Background(), Subscription{Name: "bad", URL: "https://feed.example", RefreshMinutes: minutes})
		if err == nil {
			t.Fatal("invalid schedule accepted")
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "https://user:secret@feed.example/", "http://127.0.0.1/", "http://10.1.2.3/", "http://[::1]/", "http://[::ffff:127.0.0.1]/", "http://169.254.169.254/", "http://100.64.0.1/", "http://localhost./", "http://[fc00::1]/", "http://[64:ff9b::a00:1]/"} {
		if _, err := validateRemoteURL(raw); err == nil {
			t.Fatalf("unsafe URL accepted: %s", raw)
		}
	}
	for _, raw := range []string{"https://feed.example/a?token=secret", "https://8.8.8.8/feed"} {
		if _, err := validateRemoteURL(raw); err != nil {
			t.Fatal(err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFeedFetchSSRFRedirectCapsAndRedactedErrors(t *testing.T) {
	if _, err := fetchRemote(context.Background(), "http://127.0.0.1:1/?token=SECRET", feedLimit); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("feed wrapper accepted private address or leaked URL")
	}
	client, transport := remoteClient()
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil {
		t.Fatal("subscription client must bypass global proxies")
	}
	for _, ips := range [][]netip.Addr{{netip.MustParseAddr("127.0.0.1")}, {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}} {
		_, err := publicAddresses(context.Background(), "feed.example", func(context.Context, string) ([]netip.Addr, error) { return ips, nil })
		if err == nil {
			t.Fatal("private/mixed DNS answer accepted")
		}
	}
	calls := 0
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://127.0.0.1/private?token=SECRET"}}, Body: io.NopCloser(strings.NewReader("PRIVATE_BODY")), Request: req}, nil
	})
	_, err := download(context.Background(), client, "https://feed.example/?token=SECRET", feedLimit)
	if err == nil || calls != 1 || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "PRIVATE_BODY") {
		t.Fatal("redirect not blocked/redacted")
	}
	for _, tc := range []struct {
		body   string
		status int
		limit  int64
		bad    bool
	}{{"plain MIME-free URI", 200, 64, false}, {"too long", 200, 3, true}, {"PRIVATE_BODY", 403, 64, true}, {"", 200, 64, true}} {
		client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Header: http.Header{}, ContentLength: -1, Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
		})
		_, err := download(context.Background(), client, "https://feed.example/?token=SECRET", tc.limit)
		if (err != nil) != tc.bad || err != nil && strings.Contains(err.Error(), "PRIVATE_BODY") {
			t.Fatalf("bounded response handling: %v", err)
		}
	}
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("SECRET transport diagnostic") })
	_, err = download(context.Background(), client, "https://feed.example/?token=SECRET", feedLimit)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("transport diagnostic leaked")
	}
}

func TestEngineConfigJSON(t *testing.T) {
	m := testManager(t)
	sub := saveTestSub(t, m, "one")
	s := cloneState(m.state)
	s.Feeds[sub.ID] = strings.Repeat("a", 64) + ".feed"
	n, _ := allocate(&s, sub.ID, "HK literal [name]")
	s.Nodes = []Node{n}
	for _, listeners := range []bool{false, true} {
		b, err := engineJSON(m.root, s, 19999, "test-controller-secret", listeners)
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if json.Unmarshal(b, &config) != nil {
			t.Fatal("invalid JSON")
		}
		if config["mode"] != "rule" || config["geodata-mode"] != false || config["geo-auto-update"] != false || config["mixed-port"] != float64(0) || config["dns"].(map[string]any)["enable"] != false {
			t.Fatal("unsafe engine defaults")
		}
		if !reflect.DeepEqual(config["rules"], []any{"MATCH,REJECT"}) {
			t.Fatal("default route is not fail closed")
		}
		provider := config["proxy-providers"].(map[string]any)[providerName(sub.ID)].(map[string]any)
		if provider["type"] != "file" || provider["url"] != nil || provider["override"].(map[string]any)["additional-prefix"] != prefix(sub.ID) {
			t.Fatal("provider must read immutable native file with a namespace")
		}
		ls := config["listeners"].([]any)
		if listeners {
			if len(ls) != 1 {
				t.Fatal("missing listener")
			}
			l := ls[0].(map[string]any)
			if l["proxy"] != routeGroupName(n) || l["listen"] != "127.0.0.1" || l["udp"] != false || l["type"] != "mixed" || len(l["users"].([]any)) != 1 {
				t.Fatal("unsafe or incorrectly targeted listener")
			}
			groups := config["proxy-groups"].([]any)
			if len(groups) != 1 || !reflect.DeepEqual(groups[0].(map[string]any)["proxies"], []any{"REJECT"}) {
				t.Fatal("route group must fail closed until selected")
			}
		} else if len(ls) != 0 {
			t.Fatal("staging core has listeners")
		}
		if strings.Contains(string(b), "PRIVATE_FEED_TOKEN") {
			t.Fatal("engine config includes subscription URL")
		}
	}
}

func TestOfficialArtifactSelectionChecksumsAndCorruption(t *testing.T) {
	assets := []releaseAsset{{Name: "mihomo-linux-amd64-v9.gz"}, {Name: "mihomo-linux-amd64-v3-v9.gz"}, {Name: "mihomo-linux-amd64-compatible-v9.gz"}, {Name: "mihomo-linux-arm64-v9.gz"}}
	a, err := chooseAsset(assets, "amd64")
	if err != nil || !strings.Contains(a.Name, "compatible") {
		t.Fatal("unsafe amd64 artifact selection")
	}
	if _, err := chooseAsset(assets, "arm64"); err != nil {
		t.Fatal(err)
	}
	if _, err := chooseAsset(assets, "unsupported"); err == nil {
		t.Fatal("unsupported architecture accepted")
	}
	sum := strings.Repeat("a", 64)
	if checksumFor([]byte(sum+"  "+a.Name+"\n"), a.Name) != sum || checksumFor([]byte(sum+"  other.gz"), a.Name) != "" {
		t.Fatal("incorrect checksum match")
	}
	if _, err := unpackBinary("bad.gz", []byte("invalid gzip")); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	if officialAssetURL("https://evil.example/MetaCubeX/mihomo/releases/download/v1/file.gz") {
		t.Fatal("unofficial binary source accepted")
	}
}

func TestQueuedOperationCancellation(t *testing.T) {
	m := testManager(t)
	m.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := m.Refresh(ctx, "")
	m.mu.Unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("queued refresh ignored cancellation: %v", err)
	}
}
