package managedproxy

import (
	"context"
	"testing"
	"time"
)

func TestViewRemainsReadableDuringOperation(t *testing.T) {
	manager, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	_, err = manager.SaveSubscription(context.Background(), Subscription{Name: "example", URL: "https://example.invalid/subscription", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = manager.View() // Publish the last confirmed, redacted snapshot.
	manager.mu.Lock()
	defer manager.mu.Unlock()
	result := make(chan View, 1)
	go func() { result <- manager.View() }()
	select {
	case view := <-result:
		if len(view.Subscriptions) != 1 || view.Subscriptions[0].URL != "" {
			t.Fatal("invalid cached view")
		}
		view.Subscriptions[0].Name = "mutated caller copy"
		if manager.View().Subscriptions[0].Name != "example" {
			t.Fatal("cached slices were exposed")
		}
	case <-time.After(time.Second):
		t.Fatal("status read blocked behind a long operation")
	}
}
