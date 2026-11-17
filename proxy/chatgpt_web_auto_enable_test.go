package proxy

import (
	"database/sql"
	"errors"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
)

func TestChatGPTWebAutoEnableEntrypointsKeepReadinessGate(t *testing.T) {
	for _, entry := range []string{"check", "models", "generation"} {
		t.Run(entry, func(t *testing.T) {
			f := newWebFixture(t, nil)
			ctx := t.Context()
			group, err := f.db.CreateAccountGroup(ctx, "auto-scope", "", "", 0, 0, sql.NullInt64{})
			if err != nil {
				t.Fatal(err)
			}
			id, err := f.db.InsertAccountWithCredentials(ctx, "new-group-member", map[string]any{"access_token": "fixture-access-token", "account_id": "fixture-upstream-account", "plan_type": "free"}, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.SetAccountGroups(ctx, id, []int64{group}); err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateAPIKeyAllowedGroups(ctx, f.key.ID, []int64{group}); err != nil {
				t.Fatal(err)
			}
			f.key, err = f.db.GetAPIKeyByID(ctx, f.key.ID)
			if err != nil {
				t.Fatal(err)
			}
			f.account = &auth.Account{DBID: id, AccessToken: "fixture-access-token", AccountID: "fixture-upstream-account", PlanType: "free", Status: auth.StatusReady, GroupIDs: []int64{group}}
			f.handler.store.AddAccount(f.account)
			f.handler.syncAPIKeyAllowedGroups(f.key)
			if _, err := f.db.GetChatGPTWebAccountState(ctx, id); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("fixture is not unconfigured", err)
			}
			switch entry {
			case "models":
				if got := f.handler.chatGPTWebModelRecords(ctx, f.key); len(got) != 0 {
					t.Fatal("unverified default advertised ready model")
				}
			case "generation":
				r := f.request(t, "/v1/images/generations", `{"prompt":"synthetic square"}`, "not-checked")
				if r.Code != http.StatusServiceUnavailable || f.calls.Load() != 0 {
					t.Fatalf("unverified default generated: %d %s", r.Code, r.Body.String())
				}
			}
			if entry != "check" {
				state, err := f.db.GetChatGPTWebAccountState(ctx, id)
				if err != nil || !state.Enabled || state.Status != "unverified" || state.ObservedAt != nil {
					t.Fatalf("missing safe default: %+v %v", state, err)
				}
			}
			// No explicit enable call. The normal check must initialize group defaults.
			state, err := f.handler.CheckChatGPTWebAccount(ctx, id)
			if err != nil || !state.Enabled || state.Status != "ready" {
				t.Fatalf("check could not use automatic enablement: %+v %v", state, err)
			}
			r := f.request(t, "/v1/images/generations", `{"prompt":"synthetic square"}`, "checked")
			if r.Code != http.StatusOK || f.calls.Load() != 1 {
				t.Fatalf("checked default unavailable: %d %s", r.Code, r.Body.String())
			}
			if _, err := f.db.SetChatGPTWebAccountEnabled(ctx, id, false); err != nil {
				t.Fatal(err)
			}
			_, err = f.handler.CheckChatGPTWebAccount(ctx, id)
			var detail *ChatGPTWebCheckError
			if !errors.As(err, &detail) || detail.Code != "account_disabled" {
				t.Fatalf("manual disable ignored: %v", err)
			}
			r = f.request(t, "/v1/images/generations", `{"prompt":"synthetic square"}`, "disabled")
			if r.Code != http.StatusServiceUnavailable || f.calls.Load() != 1 {
				t.Fatal("manual disable allowed another generation")
			}
		})
	}
}
