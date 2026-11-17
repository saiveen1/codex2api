package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestChatGPTWebRejectedCredentialAcrossImageRequests(t *testing.T) {
	for _, code := range []string{"auth_required", "identity_mismatch"} {
		t.Run(code, func(t *testing.T) {
			f := newWebFixture(t, func(w http.ResponseWriter, _ *http.Request, _ chatGPTWebRequest) {
				fmt.Fprintf(w, `{"type":"error","code":%q,"http_status":403}`, code)
			})
			body := `{"prompt":"square"}`
			if r := f.request(t, "/v1/images/generations", body, "first"); r.Code == 200 {
				t.Fatal("account rejection ignored")
			}
			state, err := f.db.GetChatGPTWebAccountState(t.Context(), f.account.ID())
			if err != nil {
				t.Fatal(err)
			}
			if state.Status == "ready" || state.RejectedCredentialRevision != chatGPTWebHash("fixture-access-token") || state.LastCheckStage != "execute" {
				t.Fatal("rejection not persisted against account")
			}
			// Even after the transient-error delay has expired, do not probe or submit.
			webAdmissionSQL(t, f, `UPDATE chatgpt_web_account_states SET last_check_at=? WHERE account_id=?`, time.Now().Add(-time.Hour).UnixNano(), f.account.ID())
			if r := f.request(t, "/v1/images/generations", body, "new"); r.Code != 503 || f.calls.Load() != 1 || f.checks.Load() != 1 {
				t.Fatalf("retried rejected credential: code=%d generates=%d checks=%d", r.Code, f.calls.Load(), f.checks.Load())
			}
			if !chatGPTWebAutoCheckNeeded(state, "generate", "new-token", "fixture-upstream-account", "", false, time.Now()) {
				t.Fatal("rotation could not trigger a check")
			}
			if _, err = f.handler.CheckChatGPTWebAccount(t.Context(), f.account.ID()); err != nil {
				t.Fatal(err)
			}
			state, err = f.db.GetChatGPTWebAccountState(t.Context(), f.account.ID())
			if err != nil || state.RejectedCredentialRevision != "" || state.Status != "ready" {
				t.Fatal("verified recovery did not clear rejection")
			}
		})
	}
}

func TestChatGPTWebWorkerSecretUnauthorizedDoesNotPoisonAccount(t *testing.T) {
	f := newWebFixture(t, nil)
	setWebCheckFixture(t, f, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error_code":"auth_required","stage":"admission","upstream_status":0}`)
	})
	_, err := f.handler.CheckChatGPTWebAccount(t.Context(), f.account.ID())
	var failure *ChatGPTWebCheckError
	if !errors.As(err, &failure) || failure.Code != "worker_unavailable" || failure.Recorded {
		t.Fatalf("wrong failure: %v", err)
	}
	state, err := f.db.GetChatGPTWebAccountState(t.Context(), f.account.ID())
	if err != nil || state.Status != "ready" || state.RejectedCredentialRevision != "" {
		t.Fatal("worker secret fenced account")
	}
}

func TestChatGPTWebNewRequestSelectsOtherAccountAfterRejection(t *testing.T) {
	f := newWebFixture(t, nil)
	good := webAdmissionAddAccount(t, f, webAdmissionGroup(t, f, "healthy"), nil)
	if _, err := f.db.SetChatGPTWebAccountEnabled(t.Context(), good.ID(), true); err != nil {
		t.Fatal(err)
	}
	var generated, checked []int64
	setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
		var req chatGPTWebRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if r.URL.Path == "/v1/accounts/check" {
			checked = append(checked, req.AccountID)
			fmt.Fprint(w, webAdmissionReady)
			return
		}
		generated = append(generated, req.AccountID)
		w.Header().Set("Content-Type", "application/x-ndjson")
		if req.AccountID == f.account.ID() {
			fmt.Fprint(w, `{"type":"error","code":"auth_required","http_status":401}`)
			return
		}
		writeWebFixtureSuccess(w)
	})
	body := `{"prompt":"square"}`
	if r := f.request(t, "/v1/images/generations", body, "reject-once"); r.Code == 200 || len(generated) != 1 {
		t.Fatal("failed execution was retried")
	}
	if r := f.request(t, "/v1/images/generations", body, "new-id"); r.Code != 200 {
		t.Fatalf("new request failed: %d %s", r.Code, r.Body.String())
	}
	if len(generated) != 2 || generated[0] != f.account.ID() || generated[1] != good.ID() || len(checked) != 1 || checked[0] != good.ID() {
		t.Fatalf("wrong routing: checks=%v executions=%v", checked, generated)
	}
}

func TestChatGPTWebPreviouslyReadyAccountNowDisabled(t *testing.T) {
	f := newWebFixture(t, nil)
	if err := f.db.SetAccountEnabled(t.Context(), f.account.ID(), false); err != nil {
		t.Fatal(err)
	}
	r := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "disabled-in-database")
	if r.Code != 503 || f.calls.Load() != 0 || f.checks.Load() != 1 {
		t.Fatalf("used stale ready account: %d checks=%d generates=%d", r.Code, f.checks.Load(), f.calls.Load())
	}
}
