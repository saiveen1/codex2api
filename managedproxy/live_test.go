//go:build linux

package managedproxy

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"testing"
	"time"
)

// Explicit remote acceptance only. The subscription URL is supplied in memory,
// never through fixtures, source, build arguments or diagnostic messages.
func TestManagedProxyLiveAcceptance(t *testing.T) {
	feed := os.Getenv("CODEX2API_PROXY_LIVE_SUBSCRIPTION_URL")
	if feed == "" {
		t.Skip("explicit live subscription acceptance not requested")
	}
	manager, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	sub, err := manager.SaveSubscription(ctx, Subscription{Name: "live-primary", URL: feed, Enabled: true, ExcludeRegex: `(?i)香港|hong\s*kong|中国|大陆|\bchina\b|剩余|到期|套餐`, RefreshMinutes: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("LIVE_PHASE native-install-and-parse")
	if err := manager.Refresh(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	view := manager.View()
	if !view.Running || len(view.Nodes) == 0 {
		t.Fatal("real core did not publish active nodes")
	}
	var selected Node
	for _, node := range view.Nodes {
		if node.Active {
			selected = node
			break
		}
	}
	if selected.Key == "" {
		t.Fatal("no node survived configured filtering")
	}
	originalURL := selected.URL
	second, err := manager.SaveSubscription(ctx, Subscription{Name: "live-secondary", URL: feed, Enabled: true, IncludeRegex: "^" + regexp.QuoteMeta(selected.Name) + "$", RefreshMinutes: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Refresh(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	after := manager.View()
	firstActive, secondActive := 0, 0
	for _, node := range after.Nodes {
		if !node.Active {
			continue
		}
		if node.SubscriptionID == sub.ID {
			firstActive++
			if node.Key == selected.Key && node.URL != originalURL {
				t.Fatal("refresh changed a stable node URL")
			}
		}
		if node.SubscriptionID == second.ID {
			secondActive++
		}
	}
	if len(after.Subscriptions) != 2 || firstActive == 0 || secondActive != 1 {
		t.Fatalf("native multi-subscription isolation failed: first=%d second=%d", firstActive, secondActive)
	}
	// A tiny connectivity check, not an upstream model or account request.
	connected := false
	attempts := 0
	for _, node := range after.Nodes {
		if !node.Active || node.SubscriptionID != sub.ID {
			continue
		}
		if attempts >= 5 {
			break
		}
		attempts++
		proxyURL, err := url.Parse(node.URL)
		if err != nil {
			t.Fatal("invalid generated local proxy URL")
		}
		transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 10 * time.Second, DisableKeepAlives: true}
		client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.gstatic.com/generate_204", nil)
		if err != nil {
			t.Fatal("cannot construct synthetic connectivity request")
		}
		response, requestErr := client.Do(req)
		if response != nil {
			response.Body.Close()
			connected = response.StatusCode == 204 || response.StatusCode == 200
		}
		transport.CloseIdleConnections()
		if requestErr == nil && connected {
			break
		}
		if requestErr != nil {
			t.Logf("HTTPS_CHECK index=%d result=%s", attempts, connectivityErrorClass(requestErr))
		}
	}
	if !connected {
		t.Fatalf("real node HTTPS connectivity not confirmed after %d bounded checks", attempts)
	}
	if err := manager.DeleteSubscription(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	final := manager.View()
	for _, node := range final.Nodes {
		if node.SubscriptionID == second.ID && node.Active {
			t.Fatal("deleted subscription kept an active listener")
		}
	}
	t.Logf("LIVE_ACCEPTANCE_OK core=%s primary_nodes=%d secondary_nodes=%d https_checks=%d", final.Version, firstActive, secondActive, attempts)
}
