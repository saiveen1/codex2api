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
	codex          bool
	responseTiming database.UsageResponseTiming
}

func startUpstreamRound(ctx context.Context) {
	if a := upstreamTraceFromContext(ctx); a != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current != nil {
			a.current.round = upstreamRoundTiming{started: time.Now(), codex: a.current.round.codex}
		}
	}
}

func observeUpstreamRound(ctx context.Context, kind string, data []byte) {
	a := upstreamTraceFromContext(ctx)
	if a == nil || (!strings.HasPrefix(kind, "response.") && kind != "error") {
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
	if r.codex {
		r.observeResponseTiming(kind, data, at)
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

func (r *upstreamRoundTiming) observeResponseTiming(kind string, data []byte, at time.Time) {
	if r.responseTiming.FirstEffectiveMs != 0 {
		return
	}
	switch kind {
	case "response.metadata", "response.rate_limits", "response.usage", "response.ping", "response.pong", "response.keepalive", "response.heartbeat":
		return
	}
	parsed := gjson.ParseBytes(data)
	if !parsed.IsObject() {
		return
	}
	firstFrame := r.responseTiming.FirstFrameMs == 0
	effective := isEffectiveResponseOutput(kind, parsed)
	if (firstFrame || effective) && gjson.ValidBytes(data) {
		ms := max(1, int(at.Sub(r.started).Milliseconds()))
		if firstFrame {
			r.responseTiming.FirstFrameMs = ms
		}
		if effective {
			r.responseTiming.FirstEffectiveMs = ms
		}
	}
}

func isEffectiveResponseOutput(kind string, parsed gjson.Result) bool {
	switch kind {
	case "response.output_item.added", "response.output_item.done":
		item := parsed.Get("item")
		switch item.Get("type").String() {
		case "function_call", "tool_call", "tool_search_call", "local_shell_call", "custom_tool_call", "mcp_tool_call", "web_search_call", "image_generation_call":
			return true // A tool invocation is output even before its arguments arrive.
		}
		return kind == "response.output_item.done" && outputItemHasFirstTokenContent(item)
	case "response.content_part.done", "response.reasoning_summary_part.added":
		part := parsed.Get("part")
		if !part.Exists() {
			part = parsed.Get("content_part")
		}
		return contentPartHasFirstTokenContent(part)
	case "response.output_text.done", "response.reasoning_text.done", "response.reasoning_summary_text.done":
		return stringFieldHasValue(parsed, "text")
	case "response.function_call_arguments.done":
		return stringFieldHasValue(parsed, "arguments")
	case "response.refusal.done":
		return stringFieldHasValue(parsed, "refusal")
	case "response.custom_tool_call_input.done":
		return stringFieldHasValue(parsed, "input")
	case "response.image_generation_call.partial_image":
		return stringFieldHasValue(parsed, "partial_image_b64") || stringFieldHasValue(parsed, "partial_image")
	default:
		return strings.HasPrefix(kind, "response.") && strings.HasSuffix(kind, ".delta") && deltaEventHasContent(parsed)
	}
}

func (r upstreamRoundTiming) applyResponseTiming(row *database.UsageLogInput) {
	if r.responseTiming.FirstFrameMs == 0 || row.UpstreamEndpoint != "/v1/responses" {
		return
	}
	switch row.Endpoint {
	case "/v1/responses", "/v1/chat/completions":
		row.ResponseTiming = &r.responseTiming
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
