package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestIntelligenceExecutorPinsPromptAndFinishesBeforeEOF(t *testing.T) {
	const prompt = "题面\n最终只恢复数字结果。"
	h, a, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "input.0.content.0.text").String() != prompt || gjson.GetBytes(body, "instructions").String() != "" || gjson.GetBytes(body, "model").String() != "gpt-4o-mini" {
			t.Errorf("payload changed: %s", body)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test-secret" || r.Header.Get("Cookie") != "" {
			t.Error("wrong identity")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"wrong reasoning\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"29\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"actual\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}],\"usage\":{\"input_tokens\":12,\"output_tokens\":8}}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	h.store.AddAccount(&auth.Account{DBID: 43, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "wrong", BaseURL: "http://127.0.0.1:1"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := h.executeIntelligenceCheck(ctx, a, "gpt-4o-mini", "high", prompt)
	if !result.Complete || result.Error != "" || result.Output != "21" || result.ResponseModel != "actual" || result.InputTokens != 12 {
		t.Fatalf("result=%+v", result)
	}
	if result.DurationMS > 1500 {
		t.Fatal("waited for EOF")
	}
}

func TestIntelligenceStreamErrorsOverridePartialAnswers(t *testing.T) {
	for _, wire := range []string{
		`data: {"type":"response.output_text.delta","delta":"21"}` + "\n\n",
		`data: {"type":"response.output_text.delta","delta":"21"}` + "\n\n" + `data: {"type":"response.incomplete","response":{"status":"incomplete"}}` + "\n\n",
		`data: {"type":"response.completed","response":{"status":"failed"}}` + "\n\n",
	} {
		result := readIntelligenceStream(strings.NewReader(wire), false)
		if result.Complete || result.Error == "" {
			t.Fatalf("must fail incomplete stream: %+v", result)
		}
	}
	result := readIntelligenceStream(strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude\",\"usage\":{\"input_tokens\":4}}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"29\"}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"21\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\ndata: {\"type\":\"message_stop\"}\n\n"), true)
	if !result.Complete || result.Output != "21" || result.InputTokens != 4 || result.OutputTokens != 6 {
		t.Fatalf("claude=%+v", result)
	}
}

func TestIntelligenceTimeoutKeepsPartialAnswerAsError(t *testing.T) {
	var requests atomic.Int32
	h, a, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	a.Models = []string{"gpt-4o-mini", "gpt-4o"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := h.executeIntelligenceCheck(ctx, a, "gpt-4o-mini, gpt-4o", "high", "answer")
	if result.Complete || result.Error == "" || result.Output != "21" || result.RequestedModel != "gpt-4o-mini" || requests.Load() != 1 {
		t.Fatalf("timeout result=%+v calls=%d", result, requests.Load())
	}
}

func TestIntelligenceSuccessDoesNotClearUnrelatedAccountState(t *testing.T) {
	h, a, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}]}}\n\n")
	})
	a.Status = auth.StatusCooldown
	a.CooldownReason = "unauthorized"
	a.CooldownUtil = time.Now().Add(time.Hour)
	a.DispatchPaused = 1
	result := h.executeIntelligenceCheck(context.Background(), a, "gpt-4o-mini", "high", "answer")
	if !result.Complete || a.Status != auth.StatusCooldown || a.CooldownReason != "unauthorized" || a.DispatchPaused != 1 {
		t.Fatalf("unrelated state was reset: %+v", result)
	}
	if result.EffectiveContext["attempts"] != "1" {
		t.Fatalf("missing actual attempt audit: %+v", result.EffectiveContext)
	}
}

func TestIntelligenceFinalAnswerExcludesCommentary(t *testing.T) {
	for name, wire := range map[string]string{
		"terminal": `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"让我计算一下"}]},{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"21"}]}]}}` + "\n\n",
		"streamed": `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","phase":"commentary"}}` + "\n\n" + `data: {"type":"response.output_text.delta","output_index":0,"delta":"让我计算一下"}` + "\n\n" + `data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant","phase":"final_answer"}}` + "\n\n" + `data: {"type":"response.output_text.delta","output_index":1,"delta":"21"}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			result := readIntelligenceStream(strings.NewReader(wire), false)
			if !result.Complete || result.Output != "21" {
				t.Fatalf("final answer polluted: %+v", result)
			}
		})
	}
}

func TestIntelligenceUnboundAccountIgnoresGlobalProxyPool(t *testing.T) {
	requests := 0
	h, a, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		fmt.Fprintf(w, "data: %s\n\n", intelligenceSuccess)
	})
	h.store.SetProxyPoolEnabled(true)
	h.store.SetProxyURL("http://127.0.0.1:1")
	result := h.executeIntelligenceCheck(context.Background(), a, "gpt-4o-mini", "high", "answer")
	if !result.Complete || result.Error != "" || requests != 1 {
		t.Fatalf("unbound account did not go direct: %+v calls=%d", result, requests)
	}
}

func TestIntelligenceBoundProxyFailureNeverFallsBack(t *testing.T) {
	h, a, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not fall back to direct") })
	a.ProxyURL = "http://127.0.0.1:1"
	result := h.executeIntelligenceCheck(context.Background(), a, "gpt-4o-mini", "high", "answer")
	if result.Interrupted || result.Complete || result.Error == "" {
		t.Fatalf("proxy network failure result=%+v", result)
	}
}

func TestIntelligenceCredentialRejected(t *testing.T) {
	for _, raw := range []string{`{"error":{"code":"token_revoked"}}`, `{"response":{"error":{"code":"account_deactivated"}}}`} {
		if !intelligenceCredentialRejected([]byte(raw)) {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{`{"error":{"code":"server_error"}}`, `{"error":{"code":"token_expired"}}`, `{"output":"token_revoked"}`, `invalid JSON`} {
		if intelligenceCredentialRejected([]byte(raw)) {
			t.Fatal("transient or generated text blocked credentials", raw)
		}
	}
}

func TestIntelligenceModelCandidates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		account    *auth.Account
		configured string
		want       []string
	}{
		{"priority", &auth.Account{Models: []string{"first-in-whitelist", "Second", "third"}}, " ignored, SECOND， \r\n third, second，first-in-whitelist ", []string{"Second", "third", "first-in-whitelist"}},
		{"single", &auth.Account{}, " gpt-4o-mini ", []string{"gpt-4o-mini"}},
		{"empty", &auth.Account{}, " , ，\r\n ", nil},
		{"no intersection", &auth.Account{Models: []string{"first"}}, "second, third", nil},
		{"projected Antigravity tier", &auth.Account{UpstreamType: auth.UpstreamAntigravity, Models: []string{"gemini-3.8-flash-tiered"}}, "not-allowed, gemini-3.8-flash-medium", []string{"gemini-3.8-flash-medium"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := intelligenceModelCandidates(tc.account, tc.configured); !slices.Equal(got, tc.want) {
				t.Fatalf("candidates = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIntelligenceExecutorSkipsWithNoMatchingModel(t *testing.T) {
	h, account, _ := newCodexDiagnosticsTestHandler(t, func(http.ResponseWriter, *http.Request) {
		t.Error("no intersection must not send a request")
	})
	result := h.executeIntelligenceCheck(t.Context(), account, "gpt-4o， gpt-5", "high", "answer")
	if !result.Skipped || result.Complete || result.Interrupted || result.Error == "" || result.RequestedModel != "" {
		t.Fatalf("expected skipped before any request: %+v", result)
	}
}

func TestIntelligenceExecutorFallsBackOnlyForExplicitModelRejection(t *testing.T) {
	const first, second = "gpt-4o-mini", "gpt-4o"
	const unsupported = `{"error":{"message":"The 'gpt-4o-mini' model is not supported when using Codex with a ChatGPT account."}}`
	const wrong = `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"wrong"}]}]}}`
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		fallback bool
		output   string
	}{
		{"HTTP unsupported", http.StatusBadRequest, unsupported, true, "21"},
		{"SSE unsupported", http.StatusOK, `{"type":"response.failed","response":{"error":{"code":"model_not_found"}}}`, true, "21"},
		{"Anthropic model missing", http.StatusNotFound, `{"type":"error","error":{"type":"not_found_error","message":"model: gpt-4o-mini"}}`, true, "21"},
		{"all unsupported", http.StatusNotFound, `{"error":{"code":"model_not_found"}}`, true, ""},
		{"bad request", http.StatusBadRequest, `{"error":{"message":"JSON mode is not supported for this model"}}`, false, ""},
		{"missing route", http.StatusNotFound, `{"error":{"type":"not_found_error","message":"Not Found"}}`, false, ""},
		{"limited", http.StatusTooManyRequests, unsupported, false, ""},
		{"server error", http.StatusInternalServerError, unsupported, false, ""},
		{"stream error", http.StatusOK, `{"type":"error","error":{"code":"server_error"}}`, false, ""},
		{"stream rate limit", http.StatusOK, `{"type":"response.failed","response":{"status_code":429,"error":{"code":"model_not_found"}}}`, false, ""},
		{"wrong answer", http.StatusOK, wrong, false, "wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan string, 8)
			h, account, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				model := gjson.GetBytes(body, "model").String()
				requests <- model
				if model == second && tc.output != "" {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", intelligenceSuccess)
				} else if tc.status == http.StatusOK {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", tc.body)
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				}
			})
			if tc.name == "HTTP unsupported" {
				h.db = newTestAdminDB(t)
			}
			account.Models = []string{second, first}
			result := h.executeIntelligenceCheck(t.Context(), account, "filtered-out，"+first+"\n"+second+","+first, "high", "answer", true)
			want := []string{first}
			if tc.fallback {
				want = append(want, second)
			}
			if len(requests) != len(want) {
				t.Fatalf("requests = %d, want models %v; result: %+v", len(requests), want, result)
			}
			for _, model := range want {
				if got := <-requests; got != model {
					t.Fatalf("request model = %s, want %s", got, model)
				}
			}
			complete := tc.output != ""
			if result.RequestedModel != want[len(want)-1] || result.Complete != complete || result.Output != tc.output || (result.Error == "") != complete || result.Skipped != (tc.fallback && !complete) || result.modelUnsupported != result.Skipped {
				t.Fatalf("unexpected result: %+v", result)
			}
			if h.db != nil {
				h.db.FlushUsageLogs()
				logs, err := h.db.ListRecentUsageLogs(t.Context(), 10)
				if err != nil || len(logs) != 2 {
					t.Fatalf("expected one usage row per attempted model: logs=%v err=%v", logs, err)
				}
				if logs[0].Model != second || logs[0].EffectiveModel != result.ResponseModel || logs[1].Model != first || logs[1].EffectiveModel != first {
					t.Fatalf("incorrect usage models: first=%+v second=%+v", logs[1], logs[0])
				}
			}
		})
	}
}
