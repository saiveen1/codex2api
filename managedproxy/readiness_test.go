package managedproxy

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderReadinessWaitsForNativeLoad(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic" {
			w.WriteHeader(401)
			return
		}
		proxies := []providerProxy{}
		if calls.Add(1) > 1 {
			proxies = append(proxies, providerProxy{Name: "loaded", Type: "Shadowsocks"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"proxies": proxies})
	}))
	defer server.Close()
	core := &coreProcess{done: make(chan struct{}), port: server.Listener.Addr().(*net.TCPAddr).Port, secret: "synthetic"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	proxies, err := core.provider(ctx, "synthetic")
	if err != nil || len(proxies) != 1 || calls.Load() != 2 {
		t.Fatalf("provider initialization not awaited: calls=%d nodes=%d err=%v", calls.Load(), len(proxies), err)
	}
}
