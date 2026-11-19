package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestChatGPTWebChatExactCollisionAndStrictInput(t *testing.T) {
	models := chatGPTWebPublicChatModels([]string{"gpt-6", "gpt-6-pro", "GPT-5-6", "gpt-5.6"})
	want := []string{"auto", "gpt-6-web", "gpt-6-thinking", "gpt-6-pro-web", "gpt-5-6", "gpt-5-6-thinking", "gpt-5-6-pro"}
	if !slices.Equal(models, want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	body := `{"model":"gpt-6-pro-web","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"high","stream":true}`
	req, model, stream, err := parseChatGPTWebChat([]byte(body), models)
	if err != nil || req.Model != "gpt-6-pro" || req.ThinkingEffort != "extended" || model != "gpt-6-pro-web" || !stream {
		t.Fatalf("unexpected route/model/effort: %+v %q %t %v", req, model, stream, err)
	}
	for _, invalid := range []string{
		strings.Replace(body, "gpt-6-pro-web", "gpt-6-pro", 1),
		strings.Replace(body, "gpt-6-pro-web", "gpt-5-6-web", 1),
		strings.Replace(body, `"hello"`, `[{"type":"text","text":"hello"}]`, 1),
		strings.Replace(body, `"role":"user"`, `"role":"user","role":"assistant"`, 1),
		strings.Replace(body, `"reasoning_effort":"high"`, `"thinking_effort":"Extended"`, 1),
		strings.Replace(body, `"stream":true`, `"stream":true,"thinking_effort":"standard"`, 1),
		strings.Replace(body, `"stream":true`, `"tools":[]`, 1),
	} {
		if _, _, _, err := parseChatGPTWebChat([]byte(invalid), models); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
	t.Setenv("CHATGPT_WEB_ENABLED", "false")
	status := ChatGPTWebDeploymentStatus()
	if len(status["supported_chat_models"].([]string)) != len(want) || !slices.Equal(status["supported_models"].([]string), chatGPTWebImageModels) {
		t.Fatalf("deployment catalog: %v", status)
	}
}

func webChatRequest(t *testing.T, f *webFixture, path string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	r := gin.New()
	r.POST(path, f.handler.APIKeyAuthMiddleware(), f.handler.ChatCompletions)
	body := fmt.Sprintf(`{"model":"auto","messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer fixture-api-key")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(recorder, req)
	return recorder
}

const webChatCompleted = "{\"type\":\"completed\",\"model_slug\":\"other-worker-metadata\",\"usage_known\":false}\n"
const webChatDelta = "{\"type\":\"delta\",\"text\":\"hello\"}\n"
const webChatReady = `{"status":"chat_ready","capabilities":["chat"],"image_quota_remaining":null,"workspace_id":"fixture-upstream-account","stage":"complete","upstream_status":200}`

func TestChatGPTWebChatImageQuotaIndependence(t *testing.T) {
	for _, mode := range []string{"fresh-zero", "stale-zero", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			f := newWebFixture(t, nil)
			limits := f.key.Limits
			limits.ImageGenerationPolicy = database.ImageGenerationPolicyBlock
			if err := f.db.UpdateAPIKeyLimits(t.Context(), f.key.ID, limits); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			observed := now.Add(-time.Minute)
			if mode == "stale-zero" {
				observed = now.Add(-time.Hour)
			}
			id := f.account.ID()
			if mode == "unknown" {
				webAdmissionSQL(t, f, `UPDATE chatgpt_web_account_states SET status='unverified', capabilities='[]', image_quota_remaining=NULL, error_code='quota_unknown', last_check_error_code='quota_unknown', last_check_at=? WHERE account_id=?`, now.UnixNano(), id)
			} else {
				webAdmissionSQL(t, f, `UPDATE chatgpt_web_account_states SET status='quota_exhausted', image_quota_remaining=0, quota_reset_at=?, observed_at=?, error_code='quota_exhausted', last_check_error_code='quota_exhausted', last_check_at=? WHERE account_id=?`, now.Add(time.Hour).UnixNano(), observed.UnixNano(), now.UnixNano(), id)
			}
			var checks, executes atomic.Int32
			setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer synthetic-worker-secret" {
					t.Error("missing worker authentication or POST")
				}
				var req chatGPTWebChatRequest
				if json.NewDecoder(r.Body).Decode(&req) != nil || req.AccountID != id || req.ChatGPTAccountID != "fixture-upstream-account" || req.EnvironmentRevision <= 0 || req.CredentialRevision != chatGPTWebHash("fixture-access-token") {
					t.Error("wrong worker account binding")
				}
				switch r.URL.Path {
				case "/v1/accounts/check-chat":
					checks.Add(1)
					_, _ = io.WriteString(w, webChatReady)
				case "/v1/chat/execute":
					executes.Add(1)
					if req.Model != "auto" || !slices.Equal(req.Messages, []chatGPTWebChatMessage{{"user", "hello"}}) || req.ThinkingEffort != "" {
						t.Error("worker request changed text, slug or effort")
					}
					w.Header().Set("Content-Type", "application/x-ndjson")
					_, _ = io.WriteString(w, webChatDelta+webChatCompleted)
				default:
					t.Error("unexpected worker path", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			res := webChatRequest(t, f, "/chat/completions", false)
			body := res.Body.String()
			if res.Code != 200 || gjson.Get(body, "choices.0.message.content").String() != "hello" || gjson.Get(body, "model").String() != "auto" || gjson.Get(body, "usage").Exists() || gjson.Get(body, "usage_known").Raw != "false" || gjson.Get(body, "error").Exists() {
				t.Fatalf("unexpected chat response: %d %s", res.Code, body)
			}
			wantChecks := int32(1)
			if mode == "fresh-zero" {
				wantChecks = 0
			}
			if checks.Load() != wantChecks || executes.Load() != 1 {
				t.Fatalf("calls: check=%d execute=%d", checks.Load(), executes.Load())
			}
			state, err := f.db.GetChatGPTWebAccountState(t.Context(), id)
			if err != nil || !chatGPTWebStateReady(*state, "chat", now.Add(time.Minute)) || chatGPTWebStateReady(*state, "generate", now.Add(time.Minute)) {
				t.Fatalf("invalid resulting admission: %+v %v", state, err)
			}
			if mode == "unknown" {
				if state.Status != "unverified" || state.ImageQuotaRemaining != nil || !slices.Equal(state.Capabilities, []string{"chat"}) {
					t.Fatalf("chat check invented image readiness: %+v", state)
				}
			} else if state.Status != "quota_exhausted" || state.ImageQuotaRemaining == nil || *state.ImageQuotaRemaining != 0 || !state.ObservedAt.Equal(observed) {
				t.Fatalf("chat check altered image evidence: %+v", state)
			}
		})
	}
}

func TestChatGPTWebChatWorkerTerminalAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, wire, code     string
		workerHTTP, wantHTTP int
		stream, done         bool
	}{
		{"complete", webChatDelta + webChatCompleted, "", 200, 200, true, true},
		{"empty completion", webChatCompleted, "worker_protocol_error", 200, 502, false, false},
		{"text rate limit", "{\"type\":\"error\",\"code\":\"chat_rate_limited\",\"http_status\":429}\n", "chat_rate_limited", 200, 429, false, false},
		{"EOF after delta", webChatDelta, "chat_stream_interrupted", 200, 200, true, false},
		{"truncated terminal", webChatDelta + strings.TrimSuffix(webChatCompleted, "\n"), "chat_stream_interrupted", 200, 502, false, false},
		{"error after delta", webChatDelta + "{\"type\":\"error\",\"code\":\"content_policy_violation\",\"http_status\":403,\"message\":\"private-worker-data\"}\n", "content_policy_violation", 200, 200, true, false},
		{"old worker", "private-worker-data", "worker_chat_unsupported", 404, 503, true, false},
		{"auth fence", "{\"type\":\"error\",\"code\":\"auth_required\",\"http_status\":401}\n", "auth_required", 200, 401, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWebFixture(t, nil)
			var calls atomic.Int32
			setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/chat/execute" {
					t.Error("worker call was replayed or sent to another endpoint", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(tc.workerHTTP)
				_, _ = io.WriteString(w, tc.wire)
			})
			res := webChatRequest(t, f, "/v1/chat/completions", tc.stream)
			body := res.Body.String()
			if res.Code != tc.wantHTTP || strings.Contains(body, "data: [DONE]") != tc.done || strings.Contains(body, `"finish_reason":"stop"`) != tc.done || strings.Contains(body, `"usage":`) || strings.Contains(body, "private-worker-data") || (tc.code != "" && !strings.Contains(body, `"code":"`+tc.code+`"`)) || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", res.Code, calls.Load(), body)
			}
			if tc.code == "auth_required" {
				state, err := f.db.GetChatGPTWebAccountState(t.Context(), f.account.ID())
				if err != nil || state.RejectedCredentialRevision != chatGPTWebHash("fixture-access-token") {
					t.Fatalf("rejected credential not fenced: %v", err)
				}
				if next := webChatRequest(t, f, "/v1/chat/completions", false); next.Code != 503 || calls.Load() != 1 {
					t.Fatalf("same credential was reused: %d %s", next.Code, next.Body.String())
				}
			}
		})
	}
}
