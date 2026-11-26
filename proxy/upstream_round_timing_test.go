package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestCodexResponseTimingSeparatesBusinessFramesFromEffectiveOutput(t *testing.T) {
	start := time.Unix(0, 0)
	for _, tc := range []struct {
		name, kind, payload string
		want                int
	}{
		{"text", "response.output_text.delta", `{"delta":"hello"}`, 35},
		{"reasoning", "response.reasoning_summary_text.delta", `{"delta":"reasoning"}`, 35},
		{"tool", "response.output_item.added", `{"item":{"type":"function_call","arguments":""}}`, 35},
		{"image", "response.image_generation_call.partial_image", `{"partial_image_b64":"sample"}`, 35},
		{"terminal usage only", "response.completed", `{"response":{"usage":{"output_tokens":42}}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			round := upstreamRoundTiming{started: start, codex: true}
			round.observe("response.metadata", []byte(`{}`), start.Add(time.Millisecond))
			round.observe("response.created", []byte(`{"response":{}}`), start.Add(5*time.Millisecond))
			round.observe("response.output_item.added", []byte(`{"item":{"type":"message","content":[]}}`), start.Add(10*time.Millisecond))
			round.observe("response.output_text.delta", []byte(`{"delta":""}`), start.Add(15*time.Millisecond))
			round.observe(tc.kind, []byte(tc.payload), start.Add(35*time.Millisecond))
			round.observe("response.output_text.delta", []byte(`{"delta":"later"}`), start.Add(100*time.Millisecond))
			want := database.UsageResponseTiming{FirstFrameMs: 5, FirstEffectiveMs: tc.want}
			if round.responseTiming != want {
				t.Fatalf("timing = %+v, want %+v", round.responseTiming, want)
			}
		})
	}
	first := upstreamRoundTiming{started: start, codex: true}
	first.observe("response.output_text.delta", []byte(`{"delta":"instant"}`), start)
	if first.responseTiming != (database.UsageResponseTiming{FirstFrameMs: 1, FirstEffectiveMs: 1}) {
		t.Fatalf("a sub-millisecond measurement must not be lost: %+v", first.responseTiming)
	}
}

func TestCodexResponseTimingRetryDoesNotInheritSamples(t *testing.T) {
	t.Setenv("LOG_DISABLED", "true")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	attachUpstreamTrace(c, nil)
	ctx := c.Request.Context()
	account := &auth.Account{DBID: 11}
	beginUpstreamTrace(ctx, account, "", false)(nil)
	startUpstreamRound(ctx)
	old := bindDiagnosticAttempt(ctx)
	body := sseBody(`{"type":"codex.rate_limits"}`, `{"type":"response.metadata"}`, evCreated())
	defer body.Close()
	if err := readSSEStreamWithContinuousRetryKeepalive(ctx, body, func(string, []byte) bool { return true }); err != nil {
		t.Fatal(err)
	}
	retry := &database.UsageLogInput{AccountID: 11, Endpoint: "/v1/responses", UpstreamEndpoint: "/v1/responses", StatusCode: 502, IsRetryAttempt: true}
	populateUpstreamTrace(c, retry)
	logUsageDiagnostic(c, retry)
	if got := gjson.Get(retry.DiagnosticContext, "response_timing"); got.Get("first_frame_ms").Int() < 1 || got.Get("first_effective_ms").Exists() {
		t.Fatalf("retry row fabricated effective output: %s", got.Raw)
	}
	beginUpstreamTrace(ctx, account, "", true)(nil)
	observeUpstreamRound(old, "response.output_text.delta", []byte(`{"delta":"late"}`))
	keepalive := strings.NewReader(": keepalive\n\nevent: ping\ndata: {}\n\ndata: {\"type\":\"response.metadata\"}\n\ndata: [DONE]\n\n")
	if err := readSSEStreamWithContinuousRetryKeepalive(ctx, keepalive, func(string, []byte) bool { return true }); err != nil {
		t.Fatal(err)
	}
	final := &database.UsageLogInput{AccountID: 11, Endpoint: "/v1/responses", UpstreamEndpoint: "/v1/responses", StatusCode: 200}
	populateUpstreamTrace(c, final)
	logUsageDiagnostic(c, final)
	if final.RequestID != retry.RequestID || final.TraceAttemptIndex <= retry.TraceAttemptIndex || gjson.Get(final.DiagnosticContext, "response_timing").Exists() {
		t.Fatalf("new attempt reused previous measurements: %s", final.DiagnosticContext)
	}
}

func TestUpstreamRoundTimingBufferedReplay(t *testing.T) {
	start := time.Unix(0, 0)
	clock := start
	timing := upstreamRoundTiming{started: start, codex: true}
	var firstForward time.Time
	fold := &continueFold{
		baseBody: []byte(testBaseBody), maxRounds: 1, clientGone: func() bool { return false },
		observe: func([]byte) {},
		observeRaw: func(kind string, data []byte) {
			switch kind {
			case "response.output_item.added":
				clock = start.Add(50 * time.Millisecond)
			case "response.output_text.delta":
				clock = start.Add(250 * time.Millisecond)
			case "response.completed":
				clock = start.Add(8323 * time.Millisecond)
			}
			timing.observe(kind, data, clock)
		},
		trace: func() upstreamTraceSnapshot { return upstreamTraceSnapshot{round: timing} },
		forward: func(data []byte) bool {
			if firstForward.IsZero() && gjson.GetBytes(data, "type").String() == "response.output_text.delta" {
				firstForward = clock
			}
			return true
		},
	}
	result := runContinueThinkingFold(sseResponse(evCreated(), evMessageAdded(1, 0), evMessageDelta(2, 0, "text"), evCompleted(3, 100, 217, 0)), fold)
	if len(result.Rounds) != 1 || result.FinalUsage.OutputTokens != 217 || !firstForward.Equal(start.Add(8323*time.Millisecond)) {
		t.Fatalf("fold or replay time changed: %+v / %v", result, firstForward)
	}
	row := &database.UsageLogInput{StatusCode: 200, OutputTokens: 217, FirstTokenMs: 8302, DurationMs: 8323, Endpoint: "/v1/responses", UpstreamEndpoint: "/v1/responses"}
	result.Rounds[0].Trace.round.apply(row)
	if row.UpstreamRoundDurationMs != 8323 || row.UpstreamFirstContentMs != 250 || row.UpstreamRoundIndex != 1 || row.FirstTokenMs != 8302 {
		t.Fatalf("wrong timing: %+v", row)
	}
	result.Rounds[0].Trace.round.applyResponseTiming(row)
	if row.ResponseTiming == nil || row.ResponseTiming.FirstFrameMs != 1 || row.ResponseTiming.FirstEffectiveMs != 250 {
		t.Fatalf("measured client replay instead of raw upstream frames: %+v", row.ResponseTiming)
	}
	for _, tc := range []struct{ code, tokens int }{{200, 216}, {500, 217}, {200, 0}} {
		other := &database.UsageLogInput{StatusCode: tc.code, OutputTokens: tc.tokens}
		timing.apply(other)
		if other.UpstreamRoundDurationMs != 0 {
			t.Fatalf("unpaired metric: %+v", other)
		}
	}
}

func TestUpstreamRoundTimingFoldRounds(t *testing.T) {
	start := time.Unix(0, 0)
	current := upstreamRoundTiming{started: start}
	var round int
	fold := &continueFold{
		baseBody: []byte(testBaseBody), maxRounds: 2, clientGone: func() bool { return false }, observe: func([]byte) {}, forward: func([]byte) bool { return true },
		trace: func() upstreamTraceSnapshot { return upstreamTraceSnapshot{round: current} },
		observeRaw: func(kind string, data []byte) {
			ms := 20
			if kind == "response.completed" {
				ms = 2000 + round*3000
			}
			current.observe(kind, data, current.started.Add(time.Duration(ms)*time.Millisecond))
		},
		openRound: func([]byte) (*http.Response, error) {
			round++
			current = upstreamRoundTiming{started: start.Add(10 * time.Second)}
			return cleanSecondRound(), nil
		},
	}
	result := runContinueThinkingFold(fingerprintFirstRound(), fold)
	if len(result.Rounds) != 2 || result.FinalUsage.OutputTokens != 900 {
		t.Fatalf("rounds/usage: %+v", result)
	}
	for i, want := range []struct{ tokens, ms int }{{516, 2000}, {900, 5000}} {
		s := result.Rounds[i]
		row := &database.UsageLogInput{StatusCode: 200, OutputTokens: s.Usage.OutputTokens, UpstreamRoundIndex: i + 1}
		s.Trace.round.apply(row)
		if row.OutputTokens != want.tokens || row.UpstreamRoundDurationMs != want.ms || row.UpstreamFirstContentMs != 20 || row.UpstreamRoundIndex != i+1 {
			t.Fatalf("round %d: %+v", i+1, row)
		}
	}
}

func TestUpstreamRoundTimingUnbufferedAndAttemptIsolation(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	attachUpstreamTrace(c, nil)
	ctx := c.Request.Context()
	a := upstreamTraceFromContext(ctx)
	account := &auth.Account{DBID: 11}
	beginUpstreamTrace(ctx, account, "", false)
	startUpstreamRound(ctx)
	// Backdate dispatch in this isolated fixture; no wall-clock waits or real network.
	a.current.round.started = a.current.round.started.Add(-8 * time.Second)
	old := bindDiagnosticAttempt(ctx)
	body := sseBody(evCreated(), evMessageAdded(1, 0), evMessageDelta(2, 0, "answer"), evCompleted(3, 100, 217, 0))
	defer body.Close()
	if err := readSSEStreamWithContinuousRetryKeepalive(ctx, body, func(string, []byte) bool { return true }); err != nil {
		t.Fatal(err)
	}
	row := &database.UsageLogInput{StatusCode: 200, OutputTokens: 217, AccountID: 11}
	populateUpstreamTrace(c, row)
	if row.UpstreamRoundDurationMs < 8000 || row.UpstreamFirstContentMs < 8000 || row.UpstreamRoundIndex != 1 {
		t.Fatalf("raw stream: %+v", row)
	}
	beginUpstreamTrace(ctx, account, "", false)
	startUpstreamRound(ctx)
	observeUpstreamRound(old, "response.completed", []byte(evCompleted(4, 100, 217, 0)))
	row = &database.UsageLogInput{StatusCode: 200, OutputTokens: 217, AccountID: 11}
	populateUpstreamTrace(c, row)
	if row.UpstreamRoundDurationMs != 0 || row.UpstreamRoundIndex != 0 {
		t.Fatalf("late event inherited by next attempt: %+v", row)
	}
	observeUpstreamRound(context.Background(), "response.completed", nil)
}

func TestHiddenRoundServiceTierPreservesBilling(t *testing.T) {
	for _, tc := range []struct{ actual, requested, bill string }{{"priority", "", "default"}, {"default", "priority", "priority"}, {"", "", ""}} {
		tiers := hiddenRoundServiceTiers(tc.actual, tc.requested)
		if tiers.ActualServiceTier != tc.actual || tiers.BillingServiceTier != tc.bill {
			t.Fatalf("%+v -> %+v", tc, tiers)
		}
	}
}
