package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

func TestUpstreamIdempotencyConflictRequiresManualReview(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/oauth/batches" && r.Method == "POST" {
			readError(w, 409, "idempotency_conflict", "query_original")
			return
		}
		f.handle(w, r)
	}))
	in := exampleInput("client-chosen")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := a.store.View()
	remoteID := snapshot.Jobs[0].Groups[0].Request.RequestID
	if !strings.HasPrefix(remoteID, "bridge_") || remoteID == in.RequestID+".1" {
		t.Fatal("upstream request ID controlled by client")
	}
	v := drive(t, a, in.RequestID)
	if v.State != "needs_review" || f.lookups != 0 || f.imports != 0 {
		t.Fatal("conflicting original batch was attached")
	}
	if _, err := a.Action(in.RequestID, "refresh"); err == nil {
		t.Fatal("conflict can be attached through refresh")
	}
}
func TestConcurrentImportRetrySurvivesStatusCompletion(t *testing.T) {
	f := newPair(t)
	f.importMode = "failed"
	a, _, _ := configuredApp(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/oauth/batches/") && !strings.HasSuffix(r.URL.Path, "/result") && block.CompareAndSwap(true, false) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		f.handle(w, r)
	}))
	in := exampleInput("concurrent-retry")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		stepNow(t, a, in.RequestID)
	}
	f.mu.Lock()
	f.importMode = ""
	f.mu.Unlock()
	block.Store(true)
	done := make(chan error, 1)
	go func() { _, err := a.Step(context.Background()); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("status request did not reach checkpoint")
	}
	if _, err := a.Action(in.RequestID, "retry-import"); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("status request did not complete")
	}
	v := drive(t, a, in.RequestID)
	if v.Imported != 1 || f.imports != 2 || f.submits != 1 {
		t.Fatal("retry lost to stale completion")
	}
}
func TestResultMayAdvanceBeyondStatusSnapshot(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	var statusReads atomic.Int32
	writePartial := func(w http.ResponseWriter, b upstream.Batch, status int) {
		b.Items = append([]upstream.BatchItem(nil), b.Items...)
		b.Items[1].State = "running"
		b.Succeeded = 1
		b.Pending = 1
		b.State = "partial"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": b})
	}
	useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v1/oauth/batches" {
			rec := httptest.NewRecorder()
			f.handle(rec, r)
			var env struct {
				Data upstream.Batch `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Error(err)
				return
			}
			writePartial(w, env.Data, 202)
			return
		}
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/oauth/batches/") && !strings.HasSuffix(r.URL.Path, "/result") && statusReads.Add(1) == 1 {
			f.mu.Lock()
			var b upstream.Batch
			for _, batch := range f.batches {
				b = batch
			}
			f.mu.Unlock()
			writePartial(w, b, 200)
			return
		}
		f.handle(w, r)
	}))
	in := exampleInput("progress-race")
	in.Accounts = append(in.Accounts, InputAccount{Identifier: "two@example.invalid", Password: "other", TOTPSecret: "JBSWY3DPEHPK3PXP"})
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if v.State != "completed" || v.Imported != 2 || f.submits != 1 {
		t.Fatal("normal status-result race stopped delivery")
	}
	if statusReads.Load() > 4 {
		t.Fatal("unbounded status rereads")
	}
}
func TestAdvertisedUploadLimitStopsBeforeAuthorization(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/oauth/capabilities" {
			c := capabilities()
			c.MaxUploadBytes = 32
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": c})
			return
		}
		f.handle(w, r)
	}))
	in := exampleInput("size-limit")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if v.State != "failed" || f.submits != 0 {
		t.Fatal("advertised upload limit ignored")
	}
}
