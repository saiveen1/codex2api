package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestUpstreamRoundTimingBufferedReplay(t *testing.T) {
	start := time.Unix(0, 0)
	clock := start
	var timing = upstreamRoundTiming{started: start}
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
	row := &database.UsageLogInput{StatusCode: 200, OutputTokens: 217, FirstTokenMs: 8302, DurationMs: 8323}
	result.Rounds[0].Trace.round.apply(row)
	if row.UpstreamRoundDurationMs != 8323 || row.UpstreamFirstContentMs != 250 || row.UpstreamRoundIndex != 1 || row.FirstTokenMs != 8302 {
		t.Fatalf("wrong timing: %+v", row)
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
