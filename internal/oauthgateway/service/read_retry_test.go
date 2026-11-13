package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

func readError(w http.ResponseWriter, status int, code, action string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]any{"code": code, "message": "synthetic", "source": "execution", "action": action, "retryable": false, "retry_after": nil}})
}
func TestTransientStatusReadRecoversWithoutNewOAuth(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	var reads atomic.Int32
	useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/oauth/batches/") && !strings.HasSuffix(r.URL.Path, "/result") && reads.Add(1) <= 2 {
			readError(w, 503, "service_unavailable", "query_original")
			return
		}
		f.handle(w, r)
	}))
	in := exampleInput("transient-read")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	stepNow(t, a, in.RequestID)
	stepNow(t, a, in.RequestID)
	v, _ := a.Job(in.RequestID)
	if v.Groups[0].ReadFailures != 1 || v.Groups[0].NextCheckAt == nil || v.State == "needs_review" {
		t.Fatal("transient read was not deferred")
	}
	before := reads.Load()
	if worked, err := a.Step(context.Background()); err != nil || worked || reads.Load() != before {
		t.Fatal("read ignored its backoff")
	}
	v = drive(t, a, in.RequestID)
	if v.Imported != 1 || f.submits != 1 || f.imports != 1 || v.Groups[0].ReadFailures != 0 {
		t.Fatal("read recovery replayed OAuth or retained failure state")
	}
}
func TestResultReadRetriesAreBoundedDespiteSuccessfulStatusReads(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	var reads atomic.Int32
	useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/result") {
			reads.Add(1)
			readError(w, 409, "result_not_ready", "query_original")
			return
		}
		f.handle(w, r)
	}))
	in := exampleInput("bounded-result")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if reads.Load() != maxReadFailures || v.State != "needs_review" || v.Groups[0].ReadFailures != maxReadFailures || f.submits != 1 || f.imports != 0 {
		t.Fatal("result retry budget was reset by status reads")
	}
	if _, err := a.Action(in.RequestID, "refresh"); err != nil {
		t.Fatal(err)
	}
	v, _ = a.Job(in.RequestID)
	if v.Groups[0].ReadFailures != 0 {
		t.Fatal("explicit refresh did not reset read budget")
	}
}
func TestLookupNotFoundQueriesSameRequestWithinBudget(t *testing.T) {
	f := newPair(t)
	f.lostSubmit = true
	a, _, _ := configuredApp(t, f)
	var lookups atomic.Int32
	useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/requests/") && lookups.Add(1) <= 2 {
			readError(w, 404, "not_found", "query_original")
			return
		}
		f.handle(w, r)
	}))
	in := exampleInput("lookup-lag")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if v.Imported != 1 || f.submits != 1 || lookups.Load() != 3 {
		t.Fatal("lookup lag caused a new authorization")
	}
}
func TestReadAuthenticationFailureStopsImmediately(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	var reads atomic.Int32
	useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/batches/") {
			reads.Add(1)
			readError(w, 401, "unauthorized", "authenticate")
			return
		}
		f.handle(w, r)
	}))
	in := exampleInput("auth-failure")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if v.State != "needs_review" || reads.Load() != 1 || f.submits != 1 {
		t.Fatal("authentication error entered polling retry")
	}
}
func TestReadFailureBudgetSurvivesRestart(t *testing.T) {
	f := newPair(t)
	a, store, target := configuredApp(t, f)
	in := exampleInput("persist-read-budget")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	stepNow(t, a, in.RequestID)
	if err := a.mutateGroup(in.RequestID, 0, func(g *Group) {
		g.ReadOperation = "result"
		g.ReadFailures = 3
		g.NextCheck = time.Now().Add(time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(store.reopen(t), target, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v, err := reopened.Job(in.RequestID)
	if err != nil || v.Groups[0].ReadFailures != 3 || v.Groups[0].NextCheckAt == nil {
		t.Fatal("restart lost retry budget")
	}
	if worked, err := reopened.Step(context.Background()); err != nil || worked {
		t.Fatal("restart ignored persisted backoff")
	}
}
func TestAlternatingStatusAndResultFailuresCannotResetBudget(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	var statusReads atomic.Int32
	useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/result") {
			readError(w, 409, "result_not_ready", "query_original")
			return
		}
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/batches/") && statusReads.Add(1)%2 == 0 {
			readError(w, 503, "service_unavailable", "query_original")
			return
		}
		f.handle(w, r)
	}))
	in := exampleInput("alternating-reads")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	v := drive(t, a, in.RequestID)
	if v.State != "needs_review" || v.Groups[0].ReadFailures != maxReadFailures || f.submits != 1 || f.imports != 0 {
		t.Fatal("alternating failed reads reset retry budget")
	}
}
func TestShutdownBeforeSubmitKeepsApprovedIntent(t *testing.T) {
	f := newPair(t)
	a, store, target := configuredApp(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	useWrapper(t, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/oauth/capabilities" {
			cancel()
		}
		f.handle(w, r)
	}))
	in := exampleInput("shutdown-before-submit")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := a.store.View()
	if err != nil || state.Jobs[0].Groups[0].Stage != "checking" || state.Jobs[0].Groups[0].Request.Accounts[0].Password == "" || f.submits != 0 {
		t.Fatal("shutdown discarded an unsubmitted approved intent")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(store.reopen(t), target, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v := drive(t, reopened, in.RequestID)
	if v.Imported != 1 || f.submits != 1 {
		t.Fatal("unsubmitted intent did not resume once")
	}
}
func TestReadBackoffAndFifthFailureStop(t *testing.T) {
	f := newPair(t)
	a, _, _ := configuredApp(t, f)
	in := exampleInput("backoff")
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	stepNow(t, a, in.RequestID)
	err := &upstream.Error{Code: "service_unavailable", Source: "execution", Action: "query_original", Status: 503}
	for i, delay := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 0} {
		before := time.Now()
		if e := a.readFailure(context.Background(), in.RequestID, 0, "status", err); e != nil {
			t.Fatal(e)
		}
		state, _ := a.store.View()
		g := state.Jobs[0].Groups[0]
		if g.ReadFailures != i+1 {
			t.Fatal("failure counter changed")
		}
		if delay == 0 {
			if g.Stage != "needs_review" || !g.NextCheck.IsZero() {
				t.Fatal("fifth failure scheduled another read")
			}
		} else if g.NextCheck.Before(before.Add(delay)) || g.NextCheck.After(time.Now().Add(delay)) {
			t.Fatal("wrong read backoff")
		}
	}
	if worked, err := a.Step(context.Background()); err != nil || worked {
		t.Fatal("exhausted read budget still active")
	}
}
