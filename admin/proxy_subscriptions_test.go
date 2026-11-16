package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/codex2api/managedproxy"
	"github.com/gin-gonic/gin"
)

type fakeSubscriptionManager struct{ view managedproxy.View }

func (f *fakeSubscriptionManager) View() managedproxy.View { return f.view }
func (f *fakeSubscriptionManager) Configure(context.Context, managedproxy.EngineConfig) error {
	return nil
}
func (f *fakeSubscriptionManager) SaveSubscription(_ context.Context, s managedproxy.Subscription) (managedproxy.Subscription, error) {
	return s, nil
}
func (f *fakeSubscriptionManager) DeleteSubscription(context.Context, string) error { return nil }
func (f *fakeSubscriptionManager) Refresh(context.Context, string) error            { return nil }
func (f *fakeSubscriptionManager) Ensure(context.Context) error                     { return nil }
func (f *fakeSubscriptionManager) Close() error                                     { return nil }
func (f *fakeSubscriptionManager) SetChangeHook(func())                             {}

func subscriptionTestRuntime(t *testing.T, manager subscriptionManager) *proxySubscriptionRuntime {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &proxySubscriptionRuntime{manager: manager, ctx: ctx, cancel: cancel}
	t.Cleanup(func() { cancel(); runtime.wg.Wait() })
	return runtime
}

func TestProxySubscriptionsListRedactsURLWithoutMutatingManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &fakeSubscriptionManager{view: managedproxy.View{Subscriptions: []managedproxy.Subscription{{ID: "s1", Name: "example", URL: "https://example.invalid/private-subscription-token"}}}}
	handler := &Handler{proxySubscriptions: subscriptionTestRuntime(t, manager)}
	router := gin.New()
	router.GET("/subscriptions", handler.ListProxySubscriptions)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest("GET", "/subscriptions", nil))
	if recorder.Code != 200 {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "private-subscription-token") {
		t.Fatal("subscription bearer URL leaked")
	}
	if manager.view.Subscriptions[0].URL == "" {
		t.Fatal("redaction mutated private manager state")
	}
}

func TestProxySubscriptionOperationRejectsConcurrentMutation(t *testing.T) {
	runtime := subscriptionTestRuntime(t, &fakeSubscriptionManager{})
	entered, release := make(chan struct{}), make(chan struct{})
	if !runtime.start("refreshing", func(context.Context) error { close(entered); <-release; return errors.New("synthetic refresh failure") }) {
		t.Fatal("first operation rejected")
	}
	<-entered
	if runtime.start("starting", func(context.Context) error { return nil }) {
		close(release)
		t.Fatal("concurrent operation accepted")
	}
	if !errors.Is(runtime.mutateSubscription(func() error { return nil }), errProxySubscriptionBusy) {
		close(release)
		t.Fatal("mutation bypassed operation gate")
	}
	close(release)
	runtime.wg.Wait()
	runtime.mu.Lock()
	operation := runtime.operation
	runtime.mu.Unlock()
	if operation.Running || operation.Stage != "failed" || operation.Error != "synthetic refresh failure" || operation.FinishedAt == "" {
		t.Fatalf("untruthful operation: %+v", operation)
	}
}

func TestProxySubscriptionSyncPreservesManualDisabledAndOtherProxies(t *testing.T) {
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	disabledURL := "http://user:secret@127.0.0.1:21001"
	expiredURL := "http://user:secret@127.0.0.1:21002"
	newURL := "http://user:secret@127.0.0.1:21003"
	handURL := "http://127.0.0.1:21004"
	disabledID, err := db.InsertProxy(ctx, disabledURL, "operator label")
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	if err := db.UpdateProxy(ctx, disabledID, nil, nil, &disabled); err != nil {
		t.Fatal(err)
	}
	expiredID, err := db.InsertProxy(ctx, expiredURL, "old node")
	if err != nil {
		t.Fatal(err)
	}
	handID, err := db.InsertProxy(ctx, handURL, "hand entered")
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeSubscriptionManager{view: managedproxy.View{Running: true, Subscriptions: []managedproxy.Subscription{{ID: "s1", Name: "Feed one"}}, Nodes: []managedproxy.Node{
		{Key: "a", SubscriptionID: "s1", Name: "disabled", URL: disabledURL, Active: true},
		{Key: "b", SubscriptionID: "s1", Name: "expired", URL: expiredURL, Active: false},
		{Key: "c", SubscriptionID: "s1", Name: "new", URL: newURL, Active: true},
	}}}
	handler := &Handler{db: db, store: newAdminProxyTestStore(t, db), proxySubscriptions: subscriptionTestRuntime(t, manager)}
	for range 2 {
		if err := handler.syncSubscriptionProxies(ctx); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.ListProxies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("duplicate import or unrelated removal: %d rows", len(rows))
	}
	for _, row := range rows {
		switch row.ID {
		case disabledID:
			if row.Enabled || row.Label != "operator label" {
				t.Fatal("operator decision overwritten")
			}
		case expiredID:
			if row.Enabled {
				t.Fatal("expired node stayed enabled")
			}
		case handID:
			if !row.Enabled || row.URL != handURL {
				t.Fatal("hand-entered proxy modified")
			}
		default:
			if row.URL != newURL || row.Label != "Feed one / new" {
				t.Fatalf("unexpected new node: %+v", row)
			}
		}
	}
}

func TestProxySubscriptionDoesNotPublishBeforeCoreIsRunning(t *testing.T) {
	db := newAdminProxyTestDB(t)
	manager := &fakeSubscriptionManager{view: managedproxy.View{Nodes: []managedproxy.Node{{Key: "new", URL: "http://127.0.0.1:21005", Active: true}}}}
	handler := &Handler{db: db, proxySubscriptions: subscriptionTestRuntime(t, manager)}
	if err := handler.syncSubscriptionProxies(context.Background()); err == nil {
		t.Fatal("stopped core published a node")
	}
	rows, err := db.ListProxies(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("unexpected proxies: %d err=%v", len(rows), err)
	}
}

func TestProxySubscriptionOwnedURLCannotBeChanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	url := "http://127.0.0.1:21006"
	id, err := db.InsertProxy(ctx, url, "owned")
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeSubscriptionManager{view: managedproxy.View{Nodes: []managedproxy.Node{{URL: url, Active: true}}}}
	handler := &Handler{db: db, proxySubscriptions: subscriptionTestRuntime(t, manager)}
	router := gin.New()
	router.PATCH("/proxies/:id", handler.UpdateProxy)
	request := httptest.NewRequest(http.MethodPatch, "/proxies/"+strconv.FormatInt(id, 10), strings.NewReader(`{"url":"http://127.0.0.1:29999"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != 400 {
		t.Fatalf("URL edit allowed: %d %s", recorder.Code, recorder.Body.String())
	}
	row, err := db.GetProxy(ctx, id)
	if err != nil || row.URL != url {
		t.Fatalf("owned URL changed: %+v %v", row, err)
	}
}

func TestProxySubscriptionLabelIsRuneBounded(t *testing.T) {
	got := subscriptionProxyLabel(strings.Repeat("订阅", 150), strings.Repeat("节点", 150))
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 255 {
		t.Fatalf("invalid label boundary: %d", utf8.RuneCountInString(got))
	}
}

type stubAutostartManager struct {
	fakeSubscriptionManager
	calls        int
	fail         int
	runningAfter int
}

func (m *stubAutostartManager) Ensure(context.Context) error {
	m.calls++
	if m.calls <= m.fail {
		return errors.New("core not ready")
	}
	return nil
}
func (m *stubAutostartManager) View() managedproxy.View {
	view := m.fakeSubscriptionManager.View()
	view.Running = m.calls > m.fail && m.calls >= m.runningAfter
	return view
}

func TestProxyEngineAutostartRestoresAfterContainerRecreate(t *testing.T) {
	manager := &stubAutostartManager{fail: 2, runningAfter: 3}
	manager.fakeSubscriptionManager.view = managedproxy.View{Subscriptions: []managedproxy.Subscription{{ID: "s1", Enabled: true}}}
	runtime := subscriptionTestRuntime(t, manager)
	handler := &Handler{proxySubscriptions: runtime}
	handler.autostartProxyEngine(runtime, manager, 0)
	if manager.calls != 3 {
		t.Fatalf("expected bounded retries until recovery, got calls=%d", manager.calls)
	}
}

func TestProxyEngineAutostartStopsAtBoundedLimit(t *testing.T) {
	manager := &stubAutostartManager{fail: backgroundRetryLimit + 5, runningAfter: backgroundRetryLimit + 1}
	manager.fakeSubscriptionManager.view = managedproxy.View{Subscriptions: []managedproxy.Subscription{{ID: "s1", Enabled: true}}}
	runtime := subscriptionTestRuntime(t, manager)
	handler := &Handler{proxySubscriptions: runtime}
	handler.autostartProxyEngine(runtime, manager, 0)
	if manager.calls != backgroundRetryLimit {
		t.Fatalf("retry loop not bounded: calls=%d limit=%d", manager.calls, backgroundRetryLimit)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.syncError == "" {
		t.Fatal("failed restore was not reported")
	}
}
