package proxy

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// Hold the selected account until release, just like an in-flight Web request.
func takeWebSchedulingAccount(t *testing.T, f *webFixture, key *database.APIKeyRow, pinned, want int64) func() {
	t.Helper()
	c := webAdmissionContext(f, t.Context())
	c.Set(contextAPIKeyID, key.ID)
	c.Set(contextAPIKeyRow, key)
	owner := NewUpstreamSessionUUID()
	account, _, _, err := f.handler.takeChatGPTWebAccount(c, "generate", owner, time.Now().Add(time.Minute), pinned)
	if want == 0 {
		var failure *chatGPTWebFailure
		if account != nil || !errors.As(err, &failure) || failure.code != "web_account_unavailable" {
			t.Fatalf("pinned=%d: expected unavailable account, got %v", pinned, err)
		}
		return nil
	}
	if err != nil || account == nil {
		t.Fatalf("key=%d pinned=%d: want account %d, got no selection (err=%v)", key.ID, pinned, want, err)
	}
	if account.ID() != want {
		t.Fatalf("key=%d pinned=%d: want account %d, got %d", key.ID, pinned, want, account.ID())
	}
	return func() {
		if err := f.db.ReleaseChatGPTWebAccountLease(t.Context(), want, owner); err != nil {
			t.Fatal(err)
		}
		f.handler.ReleaseAPIKeyScopeConcurrency(c)
		f.handler.store.Release(account)
	}
}

func readyWebSchedulingAccount(t *testing.T, f *webFixture, group int64, allowed []int64) *auth.Account {
	t.Helper()
	account := webAdmissionAddAccount(t, f, group, allowed)
	account.SetAllowedAPIKeyIDs(allowed)
	if _, err := f.db.SetChatGPTWebAccountEnabled(t.Context(), account.ID(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handler.CheckChatGPTWebAccount(t.Context(), account.ID()); err != nil {
		t.Fatal(err)
	}
	return account
}

func TestChatGPTWebSchedulingSparsePerKeyRotation(t *testing.T) {
	f := newWebFixture(t, nil)
	group := webAdmissionGroup(t, f, "sparse pool")
	limits := f.key.Limits
	limits.WebSchedulingMode = database.WebSchedulingRoundRobin
	webAdmissionKey(t, f, nil, limits)
	secondID, err := f.db.InsertAPIKeyWithOptions(t.Context(), database.APIKeyInput{Name: "second", Key: "second-web-key", Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.db.GetAPIKeyByID(t.Context(), secondID)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.syncAPIKeyAllowedGroups(second)
	first := f.account.ID()
	for range 35 {
		a := webAdmissionAddAccount(t, f, group, nil)
		if _, err := f.db.SetChatGPTWebAccountEnabled(t.Context(), a.ID(), false); err != nil {
			t.Fatal(err)
		}
	}
	middle := readyWebSchedulingAccount(t, f, group, []int64{secondID}).ID()
	last := readyWebSchedulingAccount(t, f, group, nil).ID()
	checks := f.checks.Load()
	for range 3 {
		takeWebSchedulingAccount(t, f, f.key, 0, first)()
		takeWebSchedulingAccount(t, f, second, 0, first)()
		takeWebSchedulingAccount(t, f, second, 0, middle)()
		takeWebSchedulingAccount(t, f, f.key, 0, last)()
		takeWebSchedulingAccount(t, f, second, 0, last)()
	}
	if f.checks.Load() != checks || f.calls.Load() != 0 {
		t.Fatal("selection triggered a worker call")
	}
}

func TestChatGPTWebSchedulingBusyAndPinned(t *testing.T) {
	f := newWebFixture(t, nil)
	first := f.account.ID()
	second := readyWebSchedulingAccount(t, f, webAdmissionGroup(t, f, "overflow"), nil).ID()
	for _, mode := range []string{"", database.WebSchedulingFillFirst, database.WebSchedulingRoundRobin} {
		limits := f.key.Limits
		limits.WebSchedulingMode = mode
		webAdmissionKey(t, f, nil, limits)
		releaseFirst := takeWebSchedulingAccount(t, f, f.key, 0, first)
		takeWebSchedulingAccount(t, f, f.key, first, 0) // pinned recovery cannot use the free second account
		releaseSecond := takeWebSchedulingAccount(t, f, f.key, 0, second)
		takeWebSchedulingAccount(t, f, f.key, 0, 0) // both leases held
		releaseSecond()
		releaseFirst()
		takeWebSchedulingAccount(t, f, f.key, first, first)() // must not move the round-robin cursor
		takeWebSchedulingAccount(t, f, f.key, 0, first)()
		if mode != database.WebSchedulingRoundRobin {
			takeWebSchedulingAccount(t, f, f.key, 0, first)()
		}
	}
	// A fully occupied account with NO Web lease must still be skipped.
	f.handler.store.SetMaxConcurrency(1)
	occupied := f.handler.store.TakeChatGPTWebAccount(first, f.key.ID, nil)
	if occupied == nil {
		t.Fatal("could not reserve shared account capacity")
	}
	defer f.handler.store.Release(occupied)
	takeWebSchedulingAccount(t, f, f.key, first, 0)
	takeWebSchedulingAccount(t, f, f.key, 0, second)()
}

func TestChatGPTWebSchedulingConcurrentKeyCapacity(t *testing.T) {
	entered, resume := make(chan struct{}), make(chan struct{})
	f := newWebFixture(t, func(w http.ResponseWriter, r *http.Request, req chatGPTWebRequest) {
		if req.Prompt == "first" {
			close(entered)
			select {
			case <-resume:
			case <-r.Context().Done():
				return
			}
		}
		writeWebFixtureSuccess(w)
	})
	limits := f.key.Limits
	limits.MaxConcurrency = 1
	limits.WebSchedulingMode = database.WebSchedulingRoundRobin
	webAdmissionKey(t, f, nil, limits)
	readyWebSchedulingAccount(t, f, webAdmissionGroup(t, f, "spare"), nil)
	const path = "/v1/images/generations"
	done := make(chan int, 1)
	go func() { done <- f.request(t, path, `{"model":"gpt-image-2","prompt":"first"}`, "first").Code }()
	select {
	case <-entered:
	case status := <-done:
		t.Fatalf("first request failed before reaching worker: %d", status)
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("first request did not reach worker")
	}
	blocked := f.request(t, path, `{"model":"gpt-image-2","prompt":"second"}`, "second")
	close(resume)
	if first := <-done; first != http.StatusOK || blocked.Code != http.StatusTooManyRequests || f.calls.Load() != 1 {
		t.Fatalf("first=%d overlapping=%d worker calls=%d", first, blocked.Code, f.calls.Load())
	}
}
