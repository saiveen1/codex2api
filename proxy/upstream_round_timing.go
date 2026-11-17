package proxy

import (
	"context"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

// A dispatch and its raw Responses terminal belong to one upstream round. Never
// observe a replayed event or use the outer attempt's client-facing first byte.
type upstreamRoundTiming struct {
	started        time.Time
	firstContentMs int
	durationMs     int
	outputTokens   int
	actualTier     string
	terminal       bool
}

func startUpstreamRound(ctx context.Context) {
	if a := upstreamTraceFromContext(ctx); a != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current != nil {
			a.current.round = upstreamRoundTiming{started: time.Now()}
		}
	}
}

func observeUpstreamRound(ctx context.Context, kind string, data []byte) {
	a := upstreamTraceFromContext(ctx)
	if a == nil || !strings.HasPrefix(kind, "response.") {
		return
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if attempt := diagnosticAttemptForContext(ctx, a); attempt != nil {
		attempt.round.observe(kind, data, now)
	}
}

func (r *upstreamRoundTiming) observe(kind string, data []byte, at time.Time) {
	if r.started.IsZero() || r.terminal {
		return
	}
	if isResponsesTerminalEvent(kind) {
		r.terminal = true
		if isResponsesSuccessTerminalEvent(kind) {
			r.outputTokens = int(gjson.GetBytes(data, "response.usage.output_tokens").Int())
			r.durationMs = int(at.Sub(r.started).Milliseconds())
			r.actualTier = normalizeServiceTierValue(gjson.GetBytes(data, "response.service_tier").String())
		}
		return
	}
	if r.firstContentMs != 0 {
		return
	}
	// A structure, lifecycle or rate-limit event alone is not model content.
	var content bool
	switch {
	case strings.HasSuffix(kind, ".delta"):
		content = deltaEventHasContent(gjson.ParseBytes(data))
	case kind == "response.output_item.done":
		content = outputItemHasFirstTokenContent(gjson.GetBytes(data, "item"))
	case kind == "response.content_part.done", kind == "response.reasoning_summary_part.added":
		content = contentPartHasFirstTokenContent(gjson.GetBytes(data, "part"))
	case kind == "response.function_call_arguments.done":
		content = stringFieldHasValue(gjson.ParseBytes(data), "arguments")
	case kind == "response.output_text.done":
		content = stringFieldHasValue(gjson.ParseBytes(data), "text")
	}
	if content {
		r.firstContentMs = max(1, int(at.Sub(r.started).Milliseconds()))
	}
}

// Hidden rounds historically bill at the requested tier (base rate if unset).
// Recording an actual tier must not activate the database's actual-tier fallback.
func hiddenRoundServiceTiers(actual, requested string) usageServiceTiers {
	tiers := resolveUsageServiceTiers("", requested)
	if actual != "" {
		tiers.ActualServiceTier = actual
		tiers.ServiceTier = resolveServiceTier(actual, requested)
		if tiers.BillingServiceTier == "" {
			tiers.BillingServiceTier = "default"
		}
	}
	return tiers
}

func (r upstreamRoundTiming) apply(row *database.UsageLogInput) {
	if row.StatusCode < 200 || row.StatusCode >= 300 || !r.terminal || r.durationMs <= 0 || r.outputTokens <= 0 || r.outputTokens != row.OutputTokens {
		return
	}
	row.UpstreamRoundDurationMs = r.durationMs
	row.UpstreamFirstContentMs = r.firstContentMs
	if row.UpstreamRoundIndex == 0 {
		row.UpstreamRoundIndex = 1
	}
}
