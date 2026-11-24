package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	ic "github.com/codex2api/internal/intelligencecheck"
	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
)

const intelligenceOutputLimit = 256 * 1024

type intelligenceExecution struct {
	CredentialRejected bool
	Output             string
	Error              string
	Complete           bool
	Interrupted        bool
	Skipped            bool
	DurationMS         int64
	RequestedModel     string
	ResponseModel      string
	InputTokens        int
	OutputTokens       int
	ReasoningTokens    int
	CachedTokens       int
	terminalPayload    []byte
	modelUnsupported   bool
	EffectiveContext   map[string]string
}

// intelligenceAccountChannel classifies the account source, not the requested model family.
func intelligenceAccountChannel(account *auth.Account) string {
	switch {
	case account.IsAntigravityAPI():
		return "antigravity"
	case account.IsGrokAPI():
		return "grok"
	case account.IsClaudeOAuth():
		return "claude"
	case account.IsKiro():
		return "kiro"
	default:
		return "codex"
	}
}

// intelligenceModelCandidates preserves configured priority, not whitelist order.
func intelligenceModelCandidates(account *auth.Account, configured string) []string {
	if account == nil {
		return nil
	}
	whitelist := account.CodexModels() // Snapshot of Account.Models, shared by all sources.
	restricted := len(whitelist) > 0
	if account.IsAntigravityAPI() {
		// The account stores wire IDs; compare against its projected detector IDs.
		whitelist = antigravityConnectionTestModels(account)
		restricted = true
	}
	allowed := make(map[string]string, len(whitelist))
	for _, model := range whitelist {
		model = strings.TrimSpace(model)
		allowed[strings.ToLower(model)] = model
	}
	var candidates []string
	for _, model := range ic.ModelCandidates(configured) {
		if restricted {
			model = allowed[strings.ToLower(model)]
			if model == "" {
				continue
			}
		}
		candidates = append(candidates, model)
	}
	return candidates
}

func (h *Handler) executeIntelligenceCheck(ctx context.Context, account *auth.Account, model, effort, prompt string, recordOnly ...bool) (result intelligenceExecution) {
	return h.executeIntelligenceCandidates(ctx, account, h.matchedIntelligenceModels(ctx, account, model), effort, prompt, len(recordOnly) == 0 || !recordOnly[0], false)
}

func (h *Handler) executeIntelligenceCandidates(ctx context.Context, account *auth.Account, candidates []string, effort, prompt string, applySignals, automatic bool) (result intelligenceExecution) {
	if account == nil {
		return intelligenceExecution{Interrupted: true, Error: "配置错误：该账号渠道不支持降智检测"}
	}
	if len(candidates) == 0 {
		return intelligenceExecution{Skipped: true, Error: "检测候选模型与账号可用模型无交集"}
	}
	for _, candidate := range candidates {
		if automatic {
			if _, held := h.intelligenceQuotaHold(account, candidate); held {
				return intelligenceExecution{Interrupted: true, RequestedModel: result.RequestedModel, DurationMS: result.DurationMS, Error: "账号限流或额度耗尽，已跳过本次自动检测"}
			}
		}
		attempt := h.executeIntelligenceModel(ctx, account, candidate, effort, prompt, applySignals)
		attempt.DurationMS += result.DurationMS
		if attempt.RequestedModel == "" {
			attempt.RequestedModel = result.RequestedModel
		}
		result = attempt
		if !result.modelUnsupported {
			break
		}
		if err := ctx.Err(); err != nil {
			result.Error = err.Error()
			result.modelUnsupported = false
			break
		}
	}
	if result.modelUnsupported {
		result.Skipped = true
		result.Error = "无匹配模型：上游不支持全部候选模型。" + result.Error
	}
	return result
}

// Uses the same outbound executors as generation. The detector context freezes
// the measurement prompt and model against user payload rules.
func (h *Handler) executeIntelligenceModel(ctx context.Context, account *auth.Account, model, effort, prompt string, applySignals bool) (result intelligenceExecution) {
	if err := ctx.Err(); err != nil {
		return intelligenceExecution{Error: err.Error()}
	}
	if h.store == nil || !h.store.ProxyAllowedForAccount(account.DBID, account.GetProxyURL()) {
		return intelligenceExecution{Interrupted: true, Error: "proxy access denied or unavailable"}
	}
	if _, usable := h.store.ResolveUsableProxyForAccount(account); !usable {
		return intelligenceExecution{Interrupted: true, Error: "proxy access denied or unavailable"}
	}
	claude := account.IsClaudeOAuth()
	if claude || account.IsGrokAPI() || account.IsAntigravityAPI() || account.IsKiro() {
		effort = ""
	}
	if err := h.validateQualityTestForAccount(ctx, account, qualityTestRequest{Model: model, ReasoningEffort: effort}); err != nil {
		return intelligenceExecution{Interrupted: true, Error: "配置错误：" + err.Error()}
	}
	// A trial must not rotate credentials or mark an account on refresh failure.
	if applySignals {
		if err := h.ensureKiroBearer(ctx, account); err != nil {
			// 只有上游明确拒绝凭据才封住本代凭据的自动检测；临时故障保持可重试。
			return intelligenceExecution{Error: "Kiro 账号令牌刷新失败: " + err.Error(), CredentialRejected: auth.IsPermanentRefreshFailure(err)}
		}
	}
	payload := map[string]any{"model": model, "stream": true}
	security := h.store.ClaudeSecurityConfig()
	if claude {
		maxTokens := int64(32768)
		if security.MaxOutputTokens > 0 && security.MaxOutputTokens < maxTokens {
			maxTokens = security.MaxOutputTokens
		}
		payload["messages"] = []map[string]any{{"role": "user", "content": prompt}}
		payload["max_tokens"] = maxTokens
	} else {
		payload["store"] = false
		payload["instructions"] = ""
		payload["input"] = []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": prompt}}}}
		if effort != "" {
			payload["reasoning"] = map[string]string{"effort": effort}
		}
	}
	body, _ := json.Marshal(payload)
	proxyURL := strings.TrimSpace(account.GetProxyURL())
	if !applySignals {
		ctx = proxy.WithRecordOnlyDetector(ctx)
	}
	ctx, cancel := context.WithCancel(proxy.WithAccountBoundEgress(proxy.WithUpstreamIdentityAudit(proxy.WithCodexDetectorRequest(ctx)), proxyURL))
	defer cancel()
	fingerprint := ""
	if claude {
		fingerprint = account.EffectiveClaudeFingerprintMode(h.store.ClaudeFingerprintModeDefault())
	} else if !account.IsRelayStyle() {
		fingerprint = account.EffectiveCodexFingerprintMode()
	}
	account.Mu().RLock()
	timezone := account.Timezone
	account.Mu().RUnlock()
	start := time.Now()
	statusCode := connectionTestTransportStatus
	var resp *http.Response
	var err error
	defer func() {
		result.DurationMS = time.Since(start).Milliseconds()
		secrets := codexTestSecrets(account)
		// Also mask configured header values if an upstream echoes them in an error.
		for key, value := range account.GetCustomHeaders() {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "auth") || strings.Contains(lower, "token") || strings.Contains(lower, "cookie") || strings.Contains(lower, "key") {
				secrets = append(secrets, value)
			}
		}
		result.Output = sanitizeCodexTestText(result.Output, secrets)
		result.Error = truncate(sanitizeCodexTestText(result.Error, secrets), 4096)
		result.ResponseModel = truncate(sanitizeCodexTestText(result.ResponseModel, secrets), 256)
		result.RequestedModel = model
		result.EffectiveContext = map[string]string{"fingerprint_mode": fingerprint, "timezone": timezone, "proxy": "direct", "transport": "http", "attempts": strconv.Itoa(proxy.UpstreamAttemptCount(ctx))}
		if proxyURL != "" {
			result.EffectiveContext["proxy"] = "configured"
		}
		if ua, ok := proxy.UpstreamIdentityAudit(ctx); ok {
			result.EffectiveContext["user_agent"] = sanitizeCodexTestText(ua, secrets)
		}
		if resp != nil && codexTestTransport(resp.Header) == "websocket" {
			result.EffectiveContext["transport"] = "websocket"
		}
		effectiveModel := result.ResponseModel
		if effectiveModel == "" {
			effectiveModel = model
		}
		h.logConnectionTestUsage(nil, account, connectionTestUsageInput{Reason: "intelligence_check", Endpoint: connectionTestEndpoint(account), Model: model, EffectiveModel: effectiveModel, ReasoningEffort: effort, StatusCode: statusCode, DurationMs: int(result.DurationMS), InputTokens: result.InputTokens, OutputTokens: result.OutputTokens, ReasoningTokens: result.ReasoningTokens, CachedTokens: result.CachedTokens, ErrorMessage: result.Error, ViaWebsocket: result.EffectiveContext["transport"] == "websocket"})
	}()
	switch {
	case claude:
		resp, err = proxy.ExecuteClaudeMessagesRequest(ctx, account, body, proxyURL, make(http.Header), fingerprint, security)
	case account.IsAntigravityAPI():
		resp, err = h.executeAntigravityConnectionTest(ctx, account, model, body, proxyURL, applySignals)
	case account.IsGrokAPI():
		resp, err = proxy.ExecuteGrokProtocolRequest(ctx, account, proxy.GrokProtocolResponses, nil, body, proxyURL, nil)
	case account.IsRelayStyle():
		resp, err = proxy.ExecuteRelayStyleRequest(ctx, account, body, proxyURL, nil)
	default:
		resp, err = proxy.ExecuteRequest(ctx, account, body, "", proxyURL, "", h.intelligenceDeviceConfig, nil)
	}
	if err != nil {
		result.Error = err.Error()
		return
	}
	defer resp.Body.Close()
	statusCode = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		result.Error = fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(raw), 4096))
		result.CredentialRejected = resp.StatusCode == http.StatusUnauthorized || intelligenceCredentialRejected(raw)
		result.modelUnsupported = readErr == nil && ctx.Err() == nil && !result.CredentialRejected && intelligenceModelUnsupported(resp.StatusCode, raw, model)
		if applySignals && !result.modelUnsupported {
			h.applyIntelligenceUpstreamFailure(account, resp, model, raw)
		}
		if !applySignals && resp.StatusCode == http.StatusTooManyRequests {
			result.Interrupted = true
		}
		return
	}
	if applySignals && claude {
		proxy.SyncClaudeUsageState(h.store, account, resp)
	} else if applySignals && !account.IsRelayStyle() {
		proxy.SyncCodexUsageState(h.store, account, resp)
	}
	result = readIntelligenceStream(resp.Body, claude)
	result.CredentialRejected = intelligenceCredentialRejected(result.terminalPayload) || proxy.IsResponsesOAuthRecoveryFailure(result.terminalPayload)
	result.modelUnsupported = !result.Complete && result.Output == "" && !result.CredentialRejected && intelligenceModelUnsupported(http.StatusOK, result.terminalPayload, model)
	if ctx.Err() != nil {
		result.Complete = false
		result.Error = ctx.Err().Error()
		result.modelUnsupported = false
	}
	if applySignals && !result.modelUnsupported && !claude && !account.IsAntigravityAPI() && result.Error != "" && len(result.terminalPayload) > 0 {
		if !h.applyResponsesUsageLimitFailure(account, resp, model, result.terminalPayload) {
			h.applyResponsesOAuthRecovery(account, result.terminalPayload)
		}
	}
	// Never mark an account successful/reset its authentication or quota state here.
	if applySignals && !result.modelUnsupported && claude && result.Error != "" {
		applyClaudeConnectionStreamFailure(h, account, model, "failed", result.Error, resp)
	}
	return
}

// applyIntelligenceUpstreamFailure preserves each source's native HTTP failure policy.
func (h *Handler) applyIntelligenceUpstreamFailure(account *auth.Account, resp *http.Response, model string, raw []byte) {
	detail := truncate(sanitizeCodexTestText(string(raw), codexTestSecrets(account)), 500)
	if account.IsAntigravityAPI() {
		// The dedicated executor already attempted OAuth refresh. Record the final
		// authentication failure without a Codex ban or account-wide quota cooldown.
		if resp.StatusCode == http.StatusUnauthorized {
			h.store.MarkError(account, detail)
		}
		return
	}
	if account.IsGrokAPI() {
		proxy.ApplyGrokCooldown(h.store, account, resp.StatusCode, raw, resp, model)
		return
	}
	if account.IsKiro() {
		proxy.ApplyKiroCooldown(h.store, account, resp.StatusCode, raw)
		return
	}
	if account.IsClaudeOAuth() {
		syncClaudeTestUsageState(h.store, account, model, resp, raw)
		if resp.StatusCode == http.StatusUnauthorized {
			h.store.MarkCooldownWithError(account, 5*time.Minute, "unauthorized", detail)
		}
		return
	}
	if !account.IsRelayStyle() {
		proxy.SyncCodexUsageState(h.store, account, resp)
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		h.store.MarkUpstreamUnauthorized(account, raw, detail)
	case http.StatusPaymentRequired:
		if proxy.IsDeactivatedWorkspaceError(raw) {
			h.store.MarkDeactivatedWorkspace(account, detail)
		} else {
			h.store.MarkError(account, detail)
		}
	case http.StatusForbidden:
		if proxy.IsAgentRuntimeDeletedError(raw) {
			h.store.MarkCooldownWithErrorExactDuration(account, 24*time.Hour, "unauthorized", detail)
		} else if proxy.IsDeactivatedWorkspaceError(raw) {
			h.store.MarkDeactivatedWorkspace(account, detail)
		}
	case http.StatusTooManyRequests:
		if account.IsRelayStyle() {
			h.store.MarkCooldown(account, time.Minute, "rate_limited")
		} else {
			proxy.Apply429Cooldown(h.store, account, raw, resp, model)
		}
	}
}

func readIntelligenceStream(body io.Reader, claude bool) (result intelligenceExecution) {
	var output strings.Builder
	claudeStop := ""
	// Responses may contain commentary and final_answer messages. Track individual
	// output/content indices so a terminal without output can still select only the
	// final message, and output_text.done replaces rather than duplicates deltas.
	type responseText struct {
		phase string
		parts map[int]string
	}
	messages := map[int]*responseText{}
	textBytes := 0
	message := func(index int) *responseText {
		if messages[index] == nil {
			messages[index] = &responseText{parts: map[int]string{}}
		}
		return messages[index]
	}
	setText := func(index, part int, text string, appendDelta bool) bool {
		m := message(index)
		if appendDelta {
			text = m.parts[part] + text
		}
		textBytes += len(text) - len(m.parts[part])
		if textBytes > intelligenceOutputLimit {
			result.Error = "最终答案超过 256 KiB 上限"
			return false
		}
		m.parts[part] = text
		return true
	}
	readItem := func(index int, item gjson.Result) bool {
		m := message(index)
		m.phase = item.Get("phase").String()
		if item.Get("type").String() != "message" || item.Get("role").String() != "assistant" {
			m.phase = "excluded"
			return true
		}
		for i, part := range item.Get("content").Array() {
			if part.Get("type").String() == "output_text" && !setText(index, i, part.Get("text").String(), false) {
				return false
			}
		}
		return true
	}
	appendText := func(text string) bool {
		if output.Len()+len(text) > intelligenceOutputLimit {
			result.Error = "最终答案超过 256 KiB 上限"
			return false
		}
		output.WriteString(text)
		return true
	}
	readUsage := func(usage gjson.Result) {
		for _, entry := range []struct {
			key   string
			value *int
		}{{"input_tokens", &result.InputTokens}, {"output_tokens", &result.OutputTokens}, {"output_tokens_details.reasoning_tokens", &result.ReasoningTokens}, {"input_tokens_details.cached_tokens", &result.CachedTokens}} {
			if value := usage.Get(entry.key); value.Exists() {
				*entry.value = int(value.Int())
			}
		}
		if claude {
			if value := usage.Get("cache_read_input_tokens"); value.Exists() {
				result.CachedTokens = int(value.Int())
			}
		}
	}
	err := proxy.ReadSSEStreamWithEvent(io.LimitReader(body, 16*1024*1024), func(event string, data []byte) bool {
		if !gjson.ValidBytes(data) {
			result.Error = "上游返回无效事件"
			return false
		}
		root := gjson.ParseBytes(data)
		kind := root.Get("type").String()
		if kind == "" {
			kind = event
		}
		if claude {
			switch kind {
			case "message_start":
				result.ResponseModel = root.Get("message.model").String()
				readUsage(root.Get("message.usage"))
			case "content_block_start":
				if root.Get("content_block.type").String() == "text" {
					return appendText(root.Get("content_block.text").String())
				}
			case "content_block_delta":
				if root.Get("delta.type").String() == "text_delta" {
					return appendText(root.Get("delta.text").String())
				}
			case "message_delta":
				claudeStop = root.Get("delta.stop_reason").String()
				readUsage(root.Get("usage"))
			case "message_stop":
				if claudeStop != "end_turn" && claudeStop != "stop_sequence" {
					result.Error = "Claude 未完整完成答案: " + claudeStop
				} else {
					result.Complete = true
				}
				return false
			case "error":
				result.terminalPayload = append([]byte(nil), data...)
				result.Error = formatUpstreamTestError(data, "Claude 上游错误")
				return false
			}
			return true
		}
		if model := root.Get("response.model").String(); model != "" {
			result.ResponseModel = model
		}
		readUsage(root.Get("response.usage"))
		switch kind {
		case "response.output_item.added", "response.output_item.done":
			return readItem(int(root.Get("output_index").Int()), root.Get("item"))
		case "response.output_text.delta":
			return setText(int(root.Get("output_index").Int()), int(root.Get("content_index").Int()), root.Get("delta").String(), true)
		case "response.output_text.done":
			return setText(int(root.Get("output_index").Int()), int(root.Get("content_index").Int()), root.Get("text").String(), false)
		case "response.completed":
			status := root.Get("response.status").String()
			if status != "" && status != "completed" {
				if status == "failed" {
					result.terminalPayload = append([]byte(nil), data...)
				}
				result.Error = formatUpstreamTestError(data, "上游未完整完成: "+status)
				return false
			}
			// The terminal response is authoritative if it contains output. Never grade
			// reasoning summaries, tool arguments, or an earlier partial answer.
			if items := root.Get("response.output"); items.Exists() {
				messages = map[int]*responseText{}
				textBytes = 0
				for index, item := range items.Array() {
					if !readItem(index, item) {
						return false
					}
				}
			}
			result.Complete = true
			return false
		case "response.failed", "response.incomplete", "error":
			result.terminalPayload = append([]byte(nil), data...)
			result.Error = formatUpstreamTestError(data, "上游未完整完成答案")
			return false
		}
		return true
	})
	if !claude {
		hasFinal := false
		indices := make([]int, 0, len(messages))
		for index, m := range messages {
			indices = append(indices, index)
			hasFinal = hasFinal || m.phase == "final_answer"
		}
		sort.Ints(indices)
		for _, index := range indices {
			m := messages[index]
			if m.phase != "" && m.phase != "final_answer" || hasFinal && m.phase != "final_answer" {
				continue
			}
			parts := make([]int, 0, len(m.parts))
			for part := range m.parts {
				parts = append(parts, part)
			}
			sort.Ints(parts)
			for _, part := range parts {
				if !appendText(m.parts[part]) {
					break
				}
			}
		}
	}
	result.Output = output.String()
	if err != nil && !result.Complete && result.Error == "" {
		result.Error = "读取上游响应失败: " + err.Error()
	}
	if !result.Complete && result.Error == "" {
		result.Error = "上游流已中断，未收到成功终态"
	}
	return
}

func intelligenceCredentialRejected(payload []byte) bool {
	for _, path := range []string{"error.code", "response.error.code"} {
		switch gjson.GetBytes(payload, path).String() {
		case "token_revoked", "account_deactivated", "workspace_deactivated", "organization_deactivated":
			return true
		}
	}
	return false
}

// Only provider-declared model rejections justify changing what we measure.
func intelligenceModelUnsupported(status int, payload []byte, model string) bool {
	if len(payload) == 0 || proxy.IsUsageLimitReachedError(payload) {
		return false
	}
	if status == http.StatusOK {
		status = int(gjson.GetBytes(payload, "response.status_code").Int())
		if status == 0 {
			status = int(gjson.GetBytes(payload, "status_code").Int())
		}
	}
	switch status {
	case 0, http.StatusOK, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
	default:
		return false
	}
	typ := firstNonEmptyGJSONString(payload, "response.status_details.error.type", "response.error.type", "error.type", "type")
	code := firstNonEmptyGJSONString(payload, "response.status_details.error.code", "response.error.code", "error.code", "code")
	for _, value := range []string{code, typ} {
		switch strings.ToLower(value) {
		case "model_not_found", "unsupported_model", "model_not_supported":
			return true
		}
	}
	message := firstNonEmptyGJSONString(payload, "response.status_details.error.message", "response.error.message", "error.message", "message", "detail")
	if message == "" {
		plain := strings.TrimSpace(string(payload))
		if plain == "" || strings.ContainsAny(plain[:1], "{[<") {
			return false
		}
		message = plain
	}
	message = strings.TrimRight(strings.ToLower(strings.TrimSpace(message)), ".!")
	message = strings.NewReplacer("'", "", "\"", "", "`", "").Replace(message)
	message = strings.TrimPrefix(message, "the ")
	model = strings.ToLower(model)
	if strings.HasPrefix(message, model+" model is not supported when using codex") ||
		strings.HasPrefix(message, "model is not supported when using codex") ||
		strings.HasPrefix(message, "unknown provider for model ") || strings.HasPrefix(message, "unknown provider for model:") {
		return true
	}
	switch message {
	case "model not found", "model not supported", "unsupported model", "model is not supported", "unknown model",
		"unknown model: " + model, "unsupported model: " + model:
		return true
	case "model: " + model:
		// Anthropic identifies a missing model as `not_found_error` + `model: ID`.
		return status == http.StatusNotFound || typ == "not_found_error"
	}
	for _, subject := range []string{"model " + model, model + " model", "models/" + model} {
		if rest, ok := strings.CutPrefix(message, subject); ok {
			switch rest {
			case " is not supported", " is not supported for this account", " is not supported by this account", " was not found", " is not found", " does not exist", " does not exist or you do not have access to it", " is no longer available":
				return true
			}
			if strings.HasPrefix(rest, " is not found for api version ") {
				return true
			}
		}
	}
	return false
}
