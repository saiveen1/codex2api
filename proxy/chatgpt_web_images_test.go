package proxy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type webFixture struct {
	handler *Handler
	key     *database.APIKeyRow
	account *auth.Account
	db      *database.DB
	dbPath  string
	calls   atomic.Int32
	checks  atomic.Int32
}

func newWebFixture(t *testing.T, respond func(http.ResponseWriter, *http.Request, chatGPTWebRequest)) *webFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := &webFixture{}
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-worker-secret-123" {
			t.Error("worker secret missing")
		}
		if r.URL.Path == "/v1/accounts/check" {
			f.checks.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"ready","capabilities":["generate","edit"],"image_quota_remaining":10,"workspace_id":"fixture-upstream-account","stage":"complete","upstream_status":200}`)
			return
		}
		f.calls.Add(1)
		var body chatGPTWebRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("bad worker request")
			w.WriteHeader(400)
			return
		}
		if body.AccessToken != "fixture-access-token" || body.AccountID != f.account.ID() || body.ChatGPTAccountID != "fixture-upstream-account" {
			t.Error("account snapshot changed")
		}
		if body.CredentialRevision != chatGPTWebHash(body.AccessToken) {
			t.Error("missing credential version")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		if respond != nil {
			respond(w, r, body)
			return
		}
		writeWebFixtureSuccess(w)
	}))
	t.Cleanup(worker.Close)
	t.Setenv("CHATGPT_WEB_ENABLED", "true")
	t.Setenv("CHATGPT_WEB_WORKER_URL", worker.URL)
	t.Setenv("CHATGPT_WEB_WORKER_SECRET", "fixture-worker-secret-123")
	t.Setenv("CHATGPT_WEB_TIMEOUT_SECONDS", "10")
	f.dbPath = filepath.Join(t.TempDir(), "web.db")
	db, err := database.New("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "fixture", map[string]any{"access_token": "fixture-access-token", "account_id": "fixture-upstream-account", "plan_type": "free"}, "")
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "web", Key: "fixture-api-key", Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb}})
	if err != nil {
		t.Fatal(err)
	}
	f.key, err = db.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 9})
	t.Cleanup(store.Stop)
	f.account = &auth.Account{DBID: id, AccessToken: "fixture-access-token", AccountID: "fixture-upstream-account", PlanType: "free", Status: auth.StatusReady}
	store.AddAccount(f.account)
	f.handler = NewHandler(store, db, nil, nil)
	f.handler.syncAPIKeyAllowedGroups(f.key)
	_, err = db.SetChatGPTWebAccountEnabled(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.handler.CheckChatGPTWebAccount(ctx, id); err != nil {
		t.Fatal(err)
	}
	return f
}
func writeWebFixtureSuccess(w http.ResponseWriter) {
	enc := json.NewEncoder(w)
	for _, stage := range []string{"submitted", "generated", "downloading"} {
		_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: stage, ConversationID: "conversation-fixture", AssetIDs: []string{"file_output"}})
	}
	_ = enc.Encode(chatGPTWebEvent{Type: "image", Image: &chatGPTWebImage{B64JSON: tinyPNGBase64, MIMEType: "image/png", Width: 1, Height: 1, AssetID: "file_output"}})
	_ = enc.Encode(chatGPTWebEvent{Type: "completed"})
}
func (f *webFixture) request(t *testing.T, path, body, id string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if id != "" {
		c.Request.Header.Set("Idempotency-Key", id)
	}
	c.Set(contextAPIKeyRow, f.key)
	c.Set(contextAPIKeyID, f.key.ID)
	c.Set(contextAPIKeyName, f.key.Name)
	if strings.HasSuffix(path, "edits") {
		f.handler.ImagesEdits(c)
	} else {
		f.handler.ImagesGenerations(c)
	}
	return r
}
func TestChatGPTWebGenerateAndIdempotency(t *testing.T) {
	f := newWebFixture(t, nil)
	r := f.request(t, "/v1/images/generations", `{"model":"gpt-image-2","prompt":"white square"}`, "same-generation")
	if r.Code != 200 || gjson.Get(r.Body.String(), "data.0.b64_json").String() != tinyPNGBase64 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	if gjson.Get(r.Body.String(), "usage_known").Bool() || gjson.Get(r.Body.String(), "usage.input_tokens").Exists() {
		t.Fatal("invented provider tokens")
	}
	entries, err := f.db.ListChatGPTWebExecutions(context.Background(), f.account.ID(), 10)
	if err != nil || len(entries) != 1 || entries[0].Stage != "completed" {
		t.Fatalf("ledger=%+v err=%v", entries, err)
	}
	r = f.request(t, "/v1/images/generations", `{"model":"gpt-image-2","prompt":"white square"}`, "same-generation")
	if r.Code != 409 || f.calls.Load() != 1 {
		t.Fatalf("duplicate status=%d calls=%d", r.Code, f.calls.Load())
	}
	if f.account.ActiveRequests.Load() != 0 {
		t.Fatal("native slot leaked")
	}
}
func TestChatGPTWebJSONEdit(t *testing.T) {
	f := newWebFixture(t, func(w http.ResponseWriter, r *http.Request, req chatGPTWebRequest) {
		if req.Operation != "edit" || len(req.Images) != 1 || req.Images[0].B64JSON != tinyPNGBase64 {
			t.Errorf("bad edit payload: operation=%s images=%d", req.Operation, len(req.Images))
		}
		writeWebFixtureSuccess(w)
	})
	body := fmt.Sprintf(`{"model":"gpt-image-2","prompt":"make the square red","images":[{"image_url":"data:image/png;base64,%s"}]}`, tinyPNGBase64)
	r := f.request(t, "/v1/images/edits", body, "")
	if r.Code != 200 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}
func TestChatGPTWebMultipartEdit(t *testing.T) {
	f := newWebFixture(t, nil)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "gpt-image-2")
	_ = mw.WriteField("prompt", "red square")
	part, _ := mw.CreateFormFile("image", "input.png")
	im, _ := decodeImageBase64(tinyPNGBase64)
	_, _ = part.Write(im)
	_ = mw.Close()
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	c.Request.Header.Set("Content-Type", mw.FormDataContentType())
	c.Set(contextAPIKeyRow, f.key)
	c.Set(contextAPIKeyID, f.key.ID)
	f.handler.ImagesEdits(c)
	if r.Code != 200 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}
func TestChatGPTWebDisconnectDoesNotRetry(t *testing.T) {
	f := newWebFixture(t, func(w http.ResponseWriter, r *http.Request, req chatGPTWebRequest) {
		_ = json.NewEncoder(w).Encode(chatGPTWebEvent{Type: "stage", Stage: "submitted", ConversationID: "conversation-fixture"})
	})
	r := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "disconnect")
	if r.Code != 502 || f.calls.Load() != 1 || !gjson.Get(r.Body.String(), "error.result_unknown").Bool() {
		t.Fatalf("status=%d calls=%d body=%s", r.Code, f.calls.Load(), r.Body.String())
	}
	entries, _ := f.db.ListChatGPTWebExecutions(context.Background(), f.account.ID(), 10)
	if len(entries) != 1 || entries[0].Stage != "unknown" || entries[0].ConversationID != "conversation-fixture" {
		t.Fatalf("lost recovery state: %+v", entries)
	}
	r = f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "disconnect")
	if r.Code != 409 || f.calls.Load() != 1 {
		t.Fatal("ambiguous request was regenerated")
	}
	if f.account.ModelCooldownRemaining("gpt-image-2") > 0 {
		t.Fatal("web failure polluted Codex cooldown")
	}
}
func TestChatGPTWebRejectsUnverifiedDisabledAndWrongScope(t *testing.T) {
	for _, kind := range []string{"disabled", "unverified", "key-denied"} {
		t.Run(kind, func(t *testing.T) {
			f := newWebFixture(t, nil)
			switch kind {
			case "disabled":
				t.Setenv("CHATGPT_WEB_ENABLED", "false")
			case "unverified":
				_, _ = f.db.SetChatGPTWebAccountEnabled(context.Background(), f.account.ID(), false)
				_, _ = f.db.SetChatGPTWebAccountEnabled(context.Background(), f.account.ID(), true)
				setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/accounts/check" {
						t.Error("generated without a verified observation")
					}
					w.WriteHeader(http.StatusUnauthorized)
				})
			case "key-denied":
				f.account.SetAllowedAPIKeyIDs([]int64{f.key.ID + 100})
			}
			r := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "")
			if r.Code < 400 || f.calls.Load() != 0 {
				t.Fatalf("gate bypass: status=%d calls=%d", r.Code, f.calls.Load())
			}
		})
	}
}
func TestChatGPTWebUnsupportedOptionsBeforeSubmission(t *testing.T) {
	f := newWebFixture(t, nil)
	for _, body := range []string{`{"prompt":"x","size":"1024x1024"}`, `{"prompt":"x","quality":"low"}`, `{"prompt":"x","mask":{"image_url":"x"}}`, `{"prompt":"x","background":"transparent"}`, `{"prompt":"x","model":"gpt-image-2-4k"}`, `{"prompt":"x","n":1.5}`} {
		r := f.request(t, "/v1/images/generations", body, "")
		if r.Code != 400 {
			t.Fatalf("body=%s status=%d result=%s", body, r.Code, r.Body.String())
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("unsupported parameter was silently submitted")
	}
}
func TestChatGPTWebEnvironmentCredentialRotation(t *testing.T) {
	f := newWebFixture(t, nil)
	f.account.AccessToken = "rotated-access-token"
	r := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "")
	if r.Code < 400 || f.calls.Load() != 0 {
		t.Fatal("stale environment used after token rotation")
	}
}
func TestChatGPTWebEndpointsFailClosed(t *testing.T) {
	for _, path := range []string{"/v1/chat/execute", "/v1/accounts/check-chat", "/v1/chat/completions/other", "/v1/responses", "/v1/responses/compact", "/v1/messages", "/v1/live", "/backend-api/codex/realtime/calls"} {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		c.Set(contextAPIKeyRow, &database.APIKeyRow{Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb}})
		if !rejectChatGPTWebEndpoint(c) || r.Code != 400 {
			t.Fatalf("not fenced: %s", path)
		}
	}
}
func TestChatGPTWebInputURLSafety(t *testing.T) {
	var called atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Add(1) }))
	defer s.Close()
	if _, err := chatGPTWebFetchImage(context.Background(), s.URL+"/image"); err == nil {
		t.Fatal("private URL accepted")
	}
	if called.Load() != 0 {
		t.Fatal("private URL was contacted")
	}
	for _, address := range []string{"127.0.0.1", "10.1.1.1", "169.254.169.254", "100.64.0.1", "192.0.2.1", "198.18.0.1", "203.0.113.1", "::1", "fc00::1", "64:ff9b::a00:1", "2001:db8::1", "2002:a00:1::"} {
		if chatGPTWebPublicIP(net.ParseIP(address)) {
			t.Errorf("accepted nonpublic IP %s", address)
		}
	}
}
func TestChatGPTWebWorkerRedirectDoesNotLeakSecret(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	cfg := chatGPTWebConfig{endpoint: source.URL, secret: "private-fixture-secret", timeout: time.Second}
	client := newChatGPTWebHTTPClient()
	defer client.CloseIdleConnections()
	resp, err := chatGPTWebPost(context.Background(), client, cfg, "/v1/accounts/check", map[string]string{"access_token": "private-AT"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 307 || received.Load() != 0 {
		t.Fatal("worker redirect leaked credentials")
	}
}
func TestChatGPTWebErrorRedactionAndQuotaIsolation(t *testing.T) {
	f := newWebFixture(t, func(w http.ResponseWriter, r *http.Request, req chatGPTWebRequest) {
		fmt.Fprintln(w, `{"type":"error","code":"quota_exhausted","http_status":429,"message":"fixture-access-token socks5://password@private"}`)
	})
	r := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "")
	if r.Code != 429 || strings.Contains(r.Body.String(), "fixture-access-token") || strings.Contains(r.Body.String(), "password") {
		t.Fatalf("unsafe error: %s", r.Body.String())
	}
	state, _ := f.db.GetChatGPTWebAccountState(context.Background(), f.account.ID())
	if state.Status != "quota_exhausted" {
		t.Fatalf("web status=%s", state.Status)
	}
	if f.account.ModelCooldownRemaining("gpt-image-2") != 0 || f.account.GetAccessToken() != "fixture-access-token" {
		t.Fatal("native state was changed")
	}
}
func TestChatGPTWebModelsOnlyShowImageChannel(t *testing.T) {
	f := newWebFixture(t, nil)
	records := f.handler.scopedModelRecords(context.Background(), f.key)
	if len(records) != len(chatGPTWebImageModels) || records["gpt-image-2"] == nil {
		t.Fatalf("wrong web models: %+v", records)
	}
	t.Setenv("CHATGPT_WEB_ENABLED", "false")
	records = f.handler.scopedModelRecords(context.Background(), f.key)
	if len(records) != len(chatGPTWebImageModels) {
		t.Fatal("runtime service state emptied the supported model catalog")
	}
}

func TestChatGPTWebPartialBatchPreservesSuccess(t *testing.T) {
	var attempts atomic.Int32
	f := newWebFixture(t, func(w http.ResponseWriter, r *http.Request, req chatGPTWebRequest) {
		if attempts.Add(1) == 1 {
			writeWebFixtureSuccess(w)
			return
		}
		fmt.Fprintln(w, `{"type":"error","code":"quota_exhausted","http_status":429}`)
	})
	r := f.request(t, "/v1/images/generations", `{"prompt":"square","n":3}`, "batch")
	if r.Code != 200 || len(gjson.Get(r.Body.String(), "data").Array()) != 1 || !gjson.Get(r.Body.String(), "partial_success").Bool() || f.calls.Load() != 2 {
		t.Fatalf("partial result lost: status=%d calls=%d body=%s", r.Code, f.calls.Load(), r.Body.String())
	}
}

func TestChatGPTWebStreamingUsesImagesCompletionShape(t *testing.T) {
	f := newWebFixture(t, nil)
	r := f.request(t, "/v1/images/generations", `{"prompt":"square","stream":true}`, "stream")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "event: image_generation.completed") || !strings.Contains(r.Body.String(), `"b64_json":"`+tinyPNGBase64+`"`) {
		t.Fatalf("incompatible stream: %s", r.Body.String())
	}
}

func TestChatGPTWebResumeGate(t *testing.T) {
	now := time.Now()
	e := &database.ChatGPTWebExecution{Stage: "unknown", RequestHash: "binding", LeaseUntil: now.Add(-time.Second)}
	if chatGPTWebCanResume(e, "binding", now) {
		t.Fatal("unknown submission without ID resumed")
	}
	e.ConversationID = "original-conversation"
	if !chatGPTWebCanResume(e, "binding", now) {
		t.Fatal("known conversation cannot be recovered")
	}
	if chatGPTWebCanResume(e, "different", now) {
		t.Fatal("payload binding ignored")
	}
	e.LeaseUntil = now.Add(time.Second)
	if chatGPTWebCanResume(e, "binding", now) {
		t.Fatal("active lease stolen")
	}
	e.LeaseUntil = now.Add(-time.Second)
	e.Stage = "completed"
	if chatGPTWebCanResume(e, "binding", now) {
		t.Fatal("completed execution resubmitted")
	}
	e.Stage = "failed"
	e.ErrorCode = "content_policy_violation"
	if chatGPTWebCanResume(e, "binding", now) {
		t.Fatal("refused request resumed")
	}
	e.ErrorCode = "image_download_failed"
	e.AssetIDs = []string{"file_output"}
	if !chatGPTWebCanResume(e, "binding", now) {
		t.Fatal("known image download cannot be recovered")
	}
}

func TestChatGPTWebInvalidOutputNotSuccess(t *testing.T) {
	f := newWebFixture(t, func(w http.ResponseWriter, r *http.Request, req chatGPTWebRequest) {
		fmt.Fprintln(w, `{"type":"stage","stage":"submitted","conversation_id":"known"}`)
		_ = json.NewEncoder(w).Encode(chatGPTWebEvent{Type: "image", Image: &chatGPTWebImage{B64JSON: tinyPNGBase64, MIMEType: "image/png", Width: 999, Height: 999}})
		fmt.Fprintln(w, `{"type":"completed"}`)
	})
	r := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "invalid-result")
	if r.Code != 502 || gjson.Get(r.Body.String(), "data").Exists() || f.calls.Load() != 1 {
		t.Fatalf("invalid image accepted: %s", r.Body.String())
	}
}

func TestChatGPTWebCachedCompletionMayOmitHistoricalStages(t *testing.T) {
	var attempts atomic.Int32
	f := newWebFixture(t, func(w http.ResponseWriter, r *http.Request, req chatGPTWebRequest) {
		if attempts.Add(1) == 1 {
			fmt.Fprintln(w, `{"type":"stage","stage":"submitted","conversation_id":"original-conversation"}`)
			fmt.Fprintln(w, `{"type":"error","code":"image_stream_interrupted","http_status":502}`)
			return
		}
		_ = json.NewEncoder(w).Encode(chatGPTWebEvent{Type: "image", Image: &chatGPTWebImage{B64JSON: tinyPNGBase64, MIMEType: "image/png", Width: 1, Height: 1, AssetID: "file_output"}})
		_ = json.NewEncoder(w).Encode(chatGPTWebEvent{Type: "image", Image: &chatGPTWebImage{B64JSON: tinyPNGBase64, MIMEType: "image/png", Width: 1, Height: 1, AssetID: "file_extra_cached"}})
		fmt.Fprintln(w, `{"type":"completed","usage_known":false}`)
	})
	first := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "cached")
	if first.Code != 502 {
		t.Fatalf("expected interrupted request: %s", first.Body.String())
	}
	// Advance only the isolated fixture's lease timestamp, not the production clock.
	raw, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.Exec(`UPDATE chatgpt_web_executions SET lease_until=? WHERE api_key_id=?`, time.Now().Add(-time.Minute).UnixNano(), f.key.ID); err != nil {
		t.Fatal(err)
	}
	r := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "cached")
	if r.Code != 200 || gjson.Get(r.Body.String(), "data.0.b64_json").String() != tinyPNGBase64 {
		t.Fatalf("cache replay failed: %s", r.Body.String())
	}
	rows, _ := f.db.ListChatGPTWebExecutions(context.Background(), f.account.ID(), 10)
	if len(rows) != 1 || rows[0].Stage != "completed" || len(rows[0].AssetIDs) != 2 || gjson.Get(r.Body.String(), "data.#").Int() != 1 || rows[0].ConversationID != "original-conversation" || attempts.Load() != 2 {
		t.Fatalf("bad completed state: %+v", rows)
	}
}

func TestChatGPTWebWorkspaceSwitchInvalidatesReadiness(t *testing.T) {
	f := newWebFixture(t, nil)
	f.handler.store.ApplyAccountCustomHeaders(f.account.ID(), map[string]string{"Chatgpt-Account-Id": "other-workspace"})
	r := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "workspace")
	if r.Code != 503 || f.calls.Load() != 0 {
		t.Fatalf("unchecked workspace used: %s", r.Body.String())
	}
}

func TestChatGPTWebFailedEgressPreventsChecksAndGeneration(t *testing.T) {
	for _, resin := range []bool{false, true} {
		t.Run(fmt.Sprint(resin), func(t *testing.T) {
			f := newWebFixture(t, nil)
			old := auth.ResinEgressEnabled()
			t.Cleanup(func() { auth.SetResinEgressEnabled(old) })
			auth.SetResinEgressEnabled(resin)
			f.handler.store.SetProxyPoolEnabled(true)
			if _, err := f.handler.CheckChatGPTWebAccount(context.Background(), f.account.ID()); err == nil {
				t.Fatal("empty managed pool authorized direct web check")
			}
			r := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "egress")
			if r.Code != 503 || f.calls.Load() != 0 {
				t.Fatalf("unusable egress was forwarded: %s", r.Body.String())
			}
		})
	}
}
