package admin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestChatGPTWebBatchDecode(t *testing.T) {
	for _, raw := range []string{
		``, `null`, `{}`, `[]`,
		`{"scope":"accounts","account_ids":[1]}`,
		`{"scope":"accounts","enabled":null,"account_ids":[1]}`,
		`{"scope":"accounts","enabled":"true","account_ids":[1]}`,
		`{"scope":"accounts","enabled":true,"account_ids":[]}`,
		`{"scope":"accounts","enabled":true,"account_ids":null}`,
		`{"scope":"accounts","enabled":true,"account_ids":[0]}`,
		`{"scope":"accounts","enabled":true,"account_ids":[-1]}`,
		`{"scope":"accounts","enabled":true,"account_ids":[1.5]}`,
		`{"scope":"accounts","enabled":true,"account_ids":["1"]}`,
		`{"scope":"accounts","enabled":true,"account_ids":[1],"group_id":null}`,
		`{"scope":"accounts","enabled":true,"account_ids":[1],"api_key_id":2}`,
		`{"scope":"group","enabled":true,"group_id":0}`,
		`{"scope":"group","enabled":true,"group_id":null}`,
		`{"scope":"group","enabled":true,"group_id":2,"account_ids":[]}`,
		`{"scope":"group","enabled":true,"group_id":2,"api_key_id":null}`,
		`{"scope":"api_key","enabled":true,"api_key_id":2,"group_id":2}`,
		`{"scope":"api_key","enabled":true,"api_key_id":2,"account_ids":null}`,
		`{"scope":"api_key","enabled":true,"api_key_id":9223372036854775808}`,
		`{"scope":"all","enabled":true}`,
		`{"scope":"group","enabled":true,"group_id":2,"unknown":true}`,
		`{"scope":"group","enabled":true,"group_id":2} {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, failure := decodeChatGPTWebBatch(strings.NewReader(raw)); failure == nil || failure.status != 400 || failure.code == "" {
				t.Fatalf("accepted malformed selection: %#v", failure)
			}
		})
	}
	for _, raw := range []string{
		`{"scope":"accounts","enabled":false,"account_ids":[2,1,2]}`,
		`{"scope":"group","enabled":false,"group_id":2}`,
		`{"scope":"api_key","enabled":false,"api_key_id":2}`,
	} {
		selection, failure := decodeChatGPTWebBatch(strings.NewReader(raw))
		if failure != nil || selection.enabled {
			t.Fatalf("valid selection rejected: %#v %#v", selection, failure)
		}
		if selection.scope == "accounts" && !reflect.DeepEqual(selection.accountIDs, []int64{2, 1}) {
			t.Fatalf("did not dedupe IDs: %v", selection.accountIDs)
		}
	}
	for _, count := range []int{1000, 1001} {
		raw := `{"scope":"accounts","enabled":true,"account_ids":[` + strings.Repeat("1,", count-1) + "1]}"
		_, failure := decodeChatGPTWebBatch(strings.NewReader(raw))
		if (failure == nil) != (count == 1000) {
			t.Fatalf("count=%d failure=%#v", count, failure)
		}
	}
}

func TestChatGPTWebBatchOAuthEligibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*database.AccountRow)
		want   bool
	}{
		{"oauth without runtime readiness", func(r *database.AccountRow) { r.Enabled = false; r.Status = "error" }, true},
		{"explicit codex", func(r *database.AccountRow) { r.Credentials["upstream_type"] = " CoDeX " }, true},
		{"non oauth", func(r *database.AccountRow) { r.Type = "responses_api" }, false},
		{"foreign platform", func(r *database.AccountRow) { r.Platform = "xai" }, false},
		{"foreign upstream", func(r *database.AccountRow) { r.Credentials["upstream_type"] = "grok" }, false},
		{"agent identity", func(r *database.AccountRow) { r.Credentials["auth_mode"] = "agentIdentity" }, false},
		{"relay url", func(r *database.AccountRow) { r.Credentials["base_url"] = "https://fixture.invalid" }, false},
		{"relay key", func(r *database.AccountRow) { r.Credentials["api_key"] = "fixture-secret" }, false},
		{"deleted status", func(r *database.AccountRow) { r.Status = "deleted" }, false},
		{"deleted marker", func(r *database.AccountRow) { r.ErrorMessage = "deleted" }, false},
		{"deleted timestamp", func(r *database.AccountRow) { r.DeletedAt.Valid = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := &database.AccountRow{ID: 1, Platform: "openai", Type: "oauth", Credentials: map[string]any{"expires_at": "2000-01-01T00:00:00Z"}}
			tc.change(row)
			if chatGPTWebBatchEligible(row) != tc.want {
				t.Fatalf("eligibility want=%v", tc.want)
			}
		})
	}
	if chatGPTWebBatchEligible(nil) {
		t.Fatal("nil row eligible")
	}
}

func batchWebFixtureAccount(t *testing.T, db *database.DB, plan string, extra map[string]any) int64 {
	t.Helper()
	creds := map[string]any{"access_token": "batch-fixture-access-secret", "refresh_token": "batch-fixture-refresh-secret", "plan_type": plan, "account_id": "batch-workspace", "expires_at": "2000-01-01T00:00:00Z"}
	for key, value := range extra {
		creds[key] = value
	}
	id, err := db.InsertAccountWithCredentials(context.Background(), "batch-fixture", creds, "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func batchWebRequest(t *testing.T, h *Handler, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/admin/chatgpt-web/accounts/batch", h.BatchSetChatGPTWebAccounts)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/chatgpt-web/accounts/batch", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != status {
		t.Fatalf("status=%d want=%d body=%s", recorder.Code, status, recorder.Body.String())
	}
	for _, secret := range []string{"batch-fixture-access-secret", "batch-fixture-refresh-secret", "batch-fixture-rawkey", "credentials", "allowed_api_key_ids"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("response exposed %q", secret)
		}
	}
	return recorder
}

func batchWebResponse(t *testing.T, recorder *httptest.ResponseRecorder) chatGPTWebBatchResponse {
	t.Helper()
	var response chatGPTWebBatchResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Matched != len(response.Results) || response.Matched != response.Updated+response.Unchanged+response.Failed {
		t.Fatalf("inconsistent counts: %+v", response)
	}
	return response
}

func TestChatGPTWebBatchExplicitValidationAndNoop(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	good := batchWebFixtureAccount(t, db, "free", nil)
	relay := batchWebFixtureAccount(t, db, "free", map[string]any{"base_url": "https://fixture.invalid"})
	deleted := batchWebFixtureAccount(t, db, "free", nil)
	if err := db.SoftDeleteAccount(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db} // No store, proxy or worker: selection must be DB-only.
	for _, tc := range []struct {
		id     int64
		status int
		code   string
	}{{999999, 404, "account_not_found"}, {relay, 400, "account_ineligible"}, {deleted, 404, "account_not_found"}} {
		recorder := batchWebRequest(t, h, fmt.Sprintf(`{"scope":"accounts","enabled":true,"account_ids":[%d,%d]}`, good, tc.id), tc.status)
		if !strings.Contains(recorder.Body.String(), tc.code) {
			t.Fatalf("missing typed error: %s", recorder.Body.String())
		}
		if _, err := db.GetChatGPTWebAccountState(ctx, good); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("invalid explicit selection partially wrote: %v", err)
		}
	}
	disabled := batchWebResponse(t, batchWebRequest(t, h, fmt.Sprintf(`{"scope":"accounts","enabled":false,"account_ids":[%d]}`, good), 200))
	if disabled.Unchanged != 1 || disabled.Results[0].Changed {
		t.Fatalf("absent disabled state was changed: %+v", disabled)
	}
	if _, err := db.GetChatGPTWebAccountState(ctx, good); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("no-op disable inserted state: %v", err)
	}
	body := fmt.Sprintf(`{"scope":"accounts","enabled":true,"account_ids":[%d,%d]}`, good, good)
	first := batchWebResponse(t, batchWebRequest(t, h, body, 200))
	if first.Matched != 1 || first.Updated != 1 || !first.Results[0].Changed || first.Results[0].State.Status != "unverified" {
		t.Fatalf("incorrect enable: %+v", first)
	}
	state := first.Results[0].State
	hash := func(value string) string { sum := sha256.Sum256([]byte(value)); return hex.EncodeToString(sum[:]) }
	quota, observed := 3, time.Now().UTC().Add(-time.Second)
	state.Status, state.Capabilities, state.ImageQuotaRemaining, state.ObservedAt = "ready", []string{"generate"}, &quota, &observed
	state.WorkspaceID, state.WorkspaceFingerprint = "batch-workspace", hash("batch-workspace")
	state.WorkspaceBindingFingerprint, state.ProxyFingerprint = hash("batch-workspace"), hash("direct")
	if applied, err := db.SaveChatGPTWebAccountObservation(ctx, state, "batch-fixture-access-secret"); err != nil || !applied {
		t.Fatalf("save fixture observation: applied=%v err=%v", applied, err)
	}
	before, err := db.GetChatGPTWebAccountState(ctx, good)
	if err != nil {
		t.Fatal(err)
	}
	second := batchWebResponse(t, batchWebRequest(t, h, body, 200))
	if second.Unchanged != 1 || second.Results[0].Changed || !reflect.DeepEqual(before, second.Results[0].State) {
		t.Fatalf("no-op enable reset observation: before=%+v after=%+v", before, second)
	}
	disabled = batchWebResponse(t, batchWebRequest(t, h, fmt.Sprintf(`{"scope":"accounts","enabled":false,"account_ids":[%d]}`, good), 200))
	if disabled.Updated != 1 || !disabled.Results[0].Changed || disabled.Results[0].State.Enabled {
		t.Fatalf("disable failed: %+v", disabled)
	}
	batchWebRequest(t, &Handler{}, `{"scope":"group","enabled":true,"group_id":1}`+strings.Repeat(" ", 64<<10), 400)
}

func TestChatGPTWebBatchFreshGroupAndKeyScopes(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	group := func(name string) int64 {
		id, err := db.CreateAccountGroup(ctx, name, "", "", 0, 0, sql.NullInt64{})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b, noAffinity, empty := group("batch-a"), group("batch-b"), group("batch-no-affinity"), group("batch-empty")
	limits := database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb, PlanAllow: []string{"free"}, NoAffinityGroupIDs: []int64{noAffinity}}
	key, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "batch-key", Key: "batch-fixture-rawkey", AllowedGroupIDs: []int64{a}, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	nativeKey, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "batch-native", Key: "batch-fixture-native-key"})
	if err != nil {
		t.Fatal(err)
	}
	freeA := batchWebFixtureAccount(t, db, "free", map[string]any{"allowed_api_key_ids": []int64{key}})
	plusA := batchWebFixtureAccount(t, db, "plus", nil)
	blockedA := batchWebFixtureAccount(t, db, "free", map[string]any{"allowed_api_key_ids": []int64{nativeKey}})
	relayA := batchWebFixtureAccount(t, db, "free", map[string]any{"base_url": "https://fixture.invalid"})
	deletedA := batchWebFixtureAccount(t, db, "free", nil)
	freeB := batchWebFixtureAccount(t, db, "free", nil)
	plusB := batchWebFixtureAccount(t, db, "plus", nil)
	freeNoAffinity := batchWebFixtureAccount(t, db, "free", nil)
	for _, pair := range [][2]int64{{freeA, a}, {plusA, a}, {blockedA, a}, {relayA, a}, {deletedA, a}, {freeB, b}, {plusB, b}, {freeNoAffinity, noAffinity}} {
		if err := db.SetAccountGroups(ctx, pair[0], []int64{pair[1]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SoftDeleteAccount(ctx, deletedA); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	assertIDs := func(body string, want []int64) {
		t.Helper()
		response := batchWebResponse(t, batchWebRequest(t, h, body, 200))
		ids := make([]int64, 0, len(response.Results))
		for _, result := range response.Results {
			ids = append(ids, result.AccountID)
		}
		slices.Sort(ids)
		slices.Sort(want)
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("selected=%v want=%v", ids, want)
		}
	}
	groupBody := fmt.Sprintf(`{"scope":"group","enabled":false,"group_id":%d}`, a)
	keyBody := fmt.Sprintf(`{"scope":"api_key","enabled":false,"api_key_id":%d}`, key)
	assertIDs(groupBody, []int64{freeA, plusA, blockedA})
	assertIDs(keyBody, []int64{freeA, freeNoAffinity})
	if err := db.SetAccountGroups(ctx, freeA, []int64{b}); err != nil {
		t.Fatal(err)
	}
	assertIDs(groupBody, []int64{plusA, blockedA})
	assertIDs(keyBody, []int64{freeNoAffinity})
	if err := db.UpdateAPIKeyAllowedGroups(ctx, key, []int64{b}); err != nil {
		t.Fatal(err)
	}
	limits.NoAffinityGroupIDs = nil
	if err := db.UpdateAPIKeyLimits(ctx, key, limits); err != nil {
		t.Fatal(err)
	}
	assertIDs(keyBody, []int64{freeA, freeB})
	limits.PlanAllow = []string{"plus"}
	if err := db.UpdateAPIKeyLimits(ctx, key, limits); err != nil {
		t.Fatal(err)
	}
	assertIDs(keyBody, []int64{plusB})
	assertIDs(fmt.Sprintf(`{"scope":"group","enabled":true,"group_id":%d}`, empty), []int64{})
	batchWebRequest(t, h, `{"scope":"group","enabled":true,"group_id":999999}`, 404)
	batchWebRequest(t, h, `{"scope":"api_key","enabled":true,"api_key_id":999999}`, 404)
	batchWebRequest(t, h, fmt.Sprintf(`{"scope":"api_key","enabled":true,"api_key_id":%d}`, nativeKey), 400)
}

func TestChatGPTWebBatchResolvedLimit(t *testing.T) {
	rows := make([]*database.AccountRow, 1001)
	memberships := make(map[int64][]int64, len(rows))
	for i := range rows {
		id := int64(i + 1)
		rows[i] = &database.AccountRow{ID: id, Type: "oauth"}
		memberships[id] = []int64{7}
	}
	for _, scope := range []string{"group", "api_key"} {
		selection := chatGPTWebBatchSelection{scope: scope, groupID: 7}
		key := &database.APIKeyRow{ID: 9, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb}}
		ids, failure := selectChatGPTWebBatchRows(selection, rows[:1000], memberships, key)
		if failure != nil || len(ids) != 1000 {
			t.Fatalf("scope=%s rejected 1000: %#v", scope, failure)
		}
		ids, failure = selectChatGPTWebBatchRows(selection, rows, memberships, key)
		if failure == nil || failure.code != "scope_too_large" || ids != nil {
			t.Fatalf("scope=%s silently truncated: ids=%d failure=%#v", scope, len(ids), failure)
		}
	}
}

type batchWebFakeStateStore struct {
	states   map[int64]*database.ChatGPTWebAccountState
	readErr  map[int64]error
	writeErr map[int64]error
	writes   []int64
}

func (s *batchWebFakeStateStore) GetChatGPTWebAccountState(_ context.Context, id int64) (*database.ChatGPTWebAccountState, error) {
	if err := s.readErr[id]; err != nil {
		return nil, err
	}
	if state := s.states[id]; state != nil {
		return state, nil
	}
	return nil, sql.ErrNoRows
}

func (s *batchWebFakeStateStore) SetChatGPTWebAccountEnabled(_ context.Context, id int64, enabled bool) (*database.ChatGPTWebAccountState, error) {
	s.writes = append(s.writes, id)
	if err := s.writeErr[id]; err != nil {
		return nil, err
	}
	state := &database.ChatGPTWebAccountState{AccountID: id, Enabled: enabled}
	s.states[id] = state
	return state, nil
}

func TestChatGPTWebBatchBestEffortSafeCounts(t *testing.T) {
	store := &batchWebFakeStateStore{
		states:   map[int64]*database.ChatGPTWebAccountState{1: {AccountID: 1, Enabled: true, Status: "ready"}},
		readErr:  map[int64]error{3: errors.New("batch-fixture-access-secret")},
		writeErr: map[int64]error{4: errors.New("batch-fixture-rawkey"), 5: sql.ErrNoRows},
	}
	response := applyChatGPTWebBatch(context.Background(), store, []int64{1, 2, 3, 4, 5, 6}, true)
	if response.Matched != 6 || response.Updated != 2 || response.Unchanged != 1 || response.Failed != 3 || !reflect.DeepEqual(store.writes, []int64{2, 4, 5, 6}) {
		t.Fatalf("best effort accounting: %+v writes=%v", response, store.writes)
	}
	if response.Results[0].Changed || !response.Results[1].Changed || response.Results[2].Changed || response.Results[4].Code != "account_not_found" || !response.Results[5].Changed {
		t.Fatalf("incorrect per-account result: %+v", response.Results)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "batch-fixture") {
		t.Fatalf("raw failure leaked: %s", encoded)
	}
}
