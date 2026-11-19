package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/codex2api/security/outputguard"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const chatGPTWebChatEndpoint = "/v1/chat/execute"

const (
	chatGPTWebMaxChatMessages = 128
	chatGPTWebMaxChatText     = 512 << 10
	chatGPTWebMaxChatOutput   = 1 << 20
)

var chatGPTWebChatSlugs = [...]string{"auto", "gpt-6", "gpt-6-thinking", "gpt-6-pro", "gpt-5-6", "gpt-5-6-thinking", "gpt-5-6-pro"}

func chatGPTWebPublicChatModels(existing []string) []string {
	models := make([]string, 0, len(chatGPTWebChatSlugs))
	for _, slug := range chatGPTWebChatSlugs {
		if slices.Contains(existing, slug) {
			models = append(models, slug+"-web")
		} else {
			models = append(models, slug)
		}
	}
	return models
}

func (h *Handler) ChatGPTWebChatModels(ctx context.Context) []string {
	return chatGPTWebPublicChatModels(h.supportedModelIDs(ctx))
}

type chatGPTWebChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatGPTWebChatRequest struct {
	chatGPTWebAccountRequest
	Model          string                  `json:"model"`
	Messages       []chatGPTWebChatMessage `json:"messages"`
	ThinkingEffort string                  `json:"thinking_effort,omitempty"`
}

func parseChatGPTWebChat(raw []byte, models []string) (req chatGPTWebChatRequest, publicModel string, stream bool, err error) {
	invalid := &chatGPTWebFailure{"invalid_request", http.StatusBadRequest, false}
	unsupported := &chatGPTWebFailure{"unsupported_parameter", http.StatusBadRequest, false}
	if !utf8.Valid(raw) || !gjson.ValidBytes(raw) {
		return req, "", false, invalid
	}
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return req, "", false, invalid
	}
	seen := make(map[string]bool, 5)
	totalText := 0
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if seen[name] {
			err = invalid
			return false
		}
		seen[name] = true
		switch name {
		case "model":
			if value.Type != gjson.String {
				err = invalid
			}
			publicModel = value.String()
		case "stream":
			if value.Type != gjson.True && value.Type != gjson.False {
				err = invalid
			}
			stream = value.Bool()
		case "thinking_effort":
			if value.Type != gjson.String || (value.Str != "standard" && value.Str != "extended" && value.Str != "max") || seen["reasoning_effort"] {
				err = unsupported
			}
			req.ThinkingEffort = value.Str
		case "reasoning_effort":
			if value.Type != gjson.String || seen["thinking_effort"] {
				err = unsupported
				break
			}
			switch value.Str {
			case "low", "medium":
				req.ThinkingEffort = "standard"
			case "high":
				req.ThinkingEffort = "extended"
			case "xhigh":
				req.ThinkingEffort = "max"
			default:
				err = unsupported
			}
		case "messages":
			if !value.IsArray() {
				err = invalid
				break
			}
			value.ForEach(func(_, item gjson.Result) bool {
				if len(req.Messages) >= chatGPTWebMaxChatMessages || !item.IsObject() {
					err = invalid
					return false
				}
				var msg chatGPTWebChatMessage
				var role, content bool
				item.ForEach(func(k, v gjson.Result) bool {
					switch {
					case k.Str == "role" && !role && v.Type == gjson.String:
						msg.Role, role = v.Str, true
					case k.Str == "content" && !content && v.Type == gjson.String:
						msg.Content, content = v.Str, true
					default:
						err = unsupported
					}
					return err == nil
				})
				if !role || !content {
					if err == nil {
						err = invalid
					}
					return false
				}
				totalText += len(msg.Content)
				if strings.TrimSpace(msg.Content) == "" || totalText > chatGPTWebMaxChatText {
					err = invalid
					return false
				}
				switch msg.Role {
				case "system", "developer", "user", "assistant":
					req.Messages = append(req.Messages, msg)
				default:
					err = unsupported
				}
				return err == nil
			})
		default:
			err = unsupported
		}
		return err == nil
	})
	if err != nil {
		return
	}
	if publicModel == "" || len(req.Messages) == 0 || req.Messages[len(req.Messages)-1].Role != "user" || strings.TrimSpace(req.Messages[len(req.Messages)-1].Content) == "" {
		return req, publicModel, stream, invalid
	}
	if !slices.Contains(models, publicModel) {
		return req, publicModel, stream, &chatGPTWebFailure{"unsupported_model", http.StatusBadRequest, false}
	}
	req.Model = strings.TrimSuffix(publicModel, "-web")
	return
}

func chatGPTWebEndpointUnsupported(status int) bool {
	return status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented
}

func chatGPTWebAccountReadinessError(code string) bool {
	switch code {
	case "auth_required", "identity_mismatch", "workspace_unavailable", "environment_challenge":
		return true
	}
	return false
}

func chatGPTWebChatFailure(err error) (string, int, string) {
	if quota := apiKeyModelRequestError(err); quota != nil {
		return string(quota.apiErr.Code), quota.status, quota.apiErr.Message
	}
	var check *ChatGPTWebCheckError
	if errors.As(err, &check) {
		return check.Code, check.HTTPStatus, "ChatGPT Web account precheck failed: " + chatGPTWebChatMessageForCode(check.Code)
	}
	var failure *chatGPTWebFailure
	if errors.As(err, &failure) {
		return failure.code, failure.status, chatGPTWebChatMessageForCode(failure.code)
	}
	switch {
	case errors.Is(err, errModelOutputGuardTriggered):
		return GuardTriggeredPublicCode, http.StatusBadGateway, GuardTriggeredPublicMessage
	case errors.Is(err, context.DeadlineExceeded):
		return "worker_timeout", http.StatusGatewayTimeout, chatGPTWebChatMessageForCode("worker_timeout")
	case errors.Is(err, context.Canceled):
		return "cancelled", logStatusClientClosed, "The client cancelled the request"
	default:
		return "upstream_error", http.StatusBadGateway, chatGPTWebChatMessageForCode("upstream_error")
	}
}

func chatGPTWebChatMessageForCode(code string) string {
	switch code {
	case "invalid_request":
		return "Expected a model and text-only messages ending with a non-empty user message"
	case "unsupported_parameter":
		return "ChatGPT Web supports only model, text messages, stream, and one supported effort parameter"
	case "unsupported_model":
		return "Requested model is not supported for ChatGPT Web text chat; use this key's model list"
	case "quota_exhausted", "chat_rate_limited":
		return "ChatGPT Web reported a limit for this chat request"
	case "content_policy_violation":
		return "ChatGPT Web refused this request"
	case "worker_chat_unsupported":
		return "The configured ChatGPT Web worker does not support text chat; upgrade the worker"
	case "worker_protocol_error", "chat_stream_interrupted", "submission_unknown":
		return "The ChatGPT Web chat stream was not confirmed complete; the gateway did not replay it"
	case "worker_timeout":
		return "The ChatGPT Web request timed out"
	case "worker_unavailable":
		return "The ChatGPT Web worker is disabled, not ready, or unavailable"
	case "web_account_unavailable":
		return "No eligible in-scope ChatGPT Web text account is available"
	case "auth_required", "identity_mismatch", "workspace_unavailable", "environment_challenge", "token_expired", "egress_unavailable":
		return (&ChatGPTWebCheckError{Code: code}).Error()
	default:
		return "ChatGPT Web text chat failed"
	}
}

func writeChatGPTWebChatError(c *gin.Context, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, errModelOutputGuardTriggered) || sendAPIKeyModelRequestQuotaError(c, err) {
		return
	}
	code, status, message := chatGPTWebChatFailure(err)
	if !c.Writer.Written() {
		writeOpenAICodedError(c, status, code, message)
		return
	}
	_ = writeChatGPTWebChatSSE(c, gin.H{"error": gin.H{"message": message, "type": "upstream_error", "code": code}})
}

func (h *Handler) forwardChatGPTWebChat(c *gin.Context, raw []byte) {
	if len(raw) > security.MaxRequestBodySize {
		writeOpenAICodedError(c, http.StatusRequestEntityTooLarge, "invalid_request", "Request body is too large")
		return
	}
	row := apiKeyRowFromContext(c)
	if row == nil || row.ID <= 0 || h.db == nil || h.store == nil {
		writeChatGPTWebChatError(c, chatGPTWebAdmissionUnavailable())
		return
	}
	if row.Limits.ExternalScheduler {
		writeOpenAICodedError(c, http.StatusBadRequest, "unsupported_parameter", "ChatGPT Web does not support external scheduler account overrides")
		return
	}
	guard := outputguard.NewRequestGuard(nativeGuardCoreConfig())
	h.capturePromptRequestIngress(c, raw)
	req, model, stream, err := parseChatGPTWebChat(raw, h.ChatGPTWebChatModels(c.Request.Context()))
	if err != nil {
		writeChatGPTWebChatError(c, err)
		return
	}
	if h.inspectPromptFilterOpenAI(c, raw, "/v1/chat/completions", model) || h.enforceAPIKeyLimitsAndReply(c, model) {
		return
	}
	release, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if release != nil {
		defer release()
	}
	cfg, err := h.loadChatGPTWebConfig()
	if err != nil {
		writeChatGPTWebChatError(c, &chatGPTWebFailure{"worker_unavailable", http.StatusServiceUnavailable, false})
		return
	}
	h.attachAPIKeyModelRequestQuota(c, false)
	ctx, cancel := context.WithTimeout(c.Request.Context(), cfg.timeout)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	writeChatGPTWebChatError(c, h.runChatGPTWebChat(c, cfg, row, req, model, stream, guard))
}

func (h *Handler) runChatGPTWebChat(c *gin.Context, cfg chatGPTWebConfig, row *database.APIKeyRow, req chatGPTWebChatRequest, model string, stream bool, guard *outputguard.RequestGuard) (runErr error) {
	ctx := c.Request.Context()
	deadline, _ := ctx.Deadline()
	owner := "chat-" + NewUpstreamSessionUUID()
	key, err := h.readChatGPTWebAdmissionKey(ctx, row, requestAPIKeyID(c), "chat", model)
	if err != nil {
		return err
	}
	h.syncAPIKeyAllowedGroups(key)
	filter := h.chatGPTWebRequestFilter(c)
	c.Set(chatGPTWebAdmissionSelectionGuard, func(ctx context.Context, account *auth.Account) error {
		freshKey, err := h.readChatGPTWebAdmissionKey(ctx, row, requestAPIKeyID(c), "chat", model)
		if err != nil {
			return err
		}
		_, err = h.validateChatGPTWebAdmissionAccount(ctx, freshKey, account, filter)
		return err
	})
	until := deadline.Add(30 * time.Second)
	account, state, proxyURL, err := h.takeChatGPTWebAccount(c, "chat", owner, until, 0)
	var unavailable *chatGPTWebFailure
	if errors.As(err, &unavailable) && unavailable.code == "web_account_unavailable" {
		if err = h.prepareChatGPTWebAccounts(c, "chat", model); err != nil {
			return err
		}
		account, state, proxyURL, err = h.takeChatGPTWebAccount(c, "chat", owner, until, 0)
	}
	if err != nil {
		return err
	}
	defer h.store.Release(account)
	defer h.ReleaseAPIKeyScopeConcurrency(c)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.db.ReleaseChatGPTWebAccountLease(cleanup, account.ID(), owner)
	}()
	token, requested, email, expired, identityErr := account.ChatGPTWebIdentity()
	if expired || identityErr != nil || !chatGPTWebStateMatches(*state, token, requested, proxyURL) {
		return &chatGPTWebFailure{"auth_required", http.StatusConflict, false}
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	req.chatGPTWebAccountRequest = chatGPTWebAccountRequest{AccountID: account.ID(), AccessToken: token, ChatGPTAccountID: state.WorkspaceID, ExpectedEmail: email, CredentialRevision: state.CredentialRevision, EnvironmentRevision: state.EnvironmentRevision, ProxyURL: proxyURL, Environment: chatGPTWebEnvironment{state.DeviceID, state.SessionID}, TimeoutSeconds: int((remaining + time.Second - 1) / time.Second)}
	if err := ConsumeAPIKeyModelRequestQuota(ctx, model); err != nil {
		return err
	}
	if p := pipelineFromContext(ctx); p != nil {
		p.Release()
	}
	started := time.Now()
	firstTokenMs := 0
	reportedModel := ""
	defer func() {
		input := &database.UsageLogInput{Channel: database.UpstreamChannelChatGPTWeb, AccountID: account.ID(), Model: model, EffectiveModel: req.Model, Endpoint: c.Request.URL.Path, InboundEndpoint: c.Request.URL.Path, UpstreamEndpoint: chatGPTWebChatEndpoint, StatusCode: http.StatusOK, DurationMs: int(time.Since(started).Milliseconds()), FirstTokenMs: firstTokenMs, Stream: stream, ReasoningEffort: req.ThinkingEffort}
		code := ""
		if runErr != nil {
			code, input.StatusCode, input.ErrorMessage = chatGPTWebChatFailure(runErr)
			input.UpstreamErrorKind = "chatgpt_web"
		}
		diagnostic, _ := json.Marshal(gin.H{"usage_known": false, "error_code": code, "upstream_reported_model": reportedModel})
		input.DiagnosticContext = string(diagnostic)
		h.logUsageForRequest(c, input)
	}()
	client := newChatGPTWebHTTPClient()
	defer client.CloseIdleConnections()
	resp, err := chatGPTWebPost(ctx, client, cfg, chatGPTWebChatEndpoint, req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return &chatGPTWebFailure{"worker_timeout", http.StatusGatewayTimeout, true}
		}
		return &chatGPTWebFailure{"submission_unknown", http.StatusBadGateway, true}
	}
	defer resp.Body.Close()
	if chatGPTWebEndpointUnsupported(resp.StatusCode) {
		return &chatGPTWebFailure{"worker_chat_unsupported", http.StatusServiceUnavailable, false}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &chatGPTWebFailure{"worker_unavailable", http.StatusServiceUnavailable, false}
	}
	if resp.StatusCode != http.StatusOK {
		return &chatGPTWebFailure{"upstream_error", http.StatusBadGateway, true}
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "application/x-ndjson" {
		return &chatGPTWebFailure{"worker_protocol_error", http.StatusBadGateway, true}
	}
	projector := NewChatProjector()
	coordinator := NewGuardTerminationCoordinator(guard, nil, resp.Body)
	observe := func(body []byte) error {
		if err := ObserveEvent(guard, projector, "", body); err != nil || guard.IsTripped() {
			reason := guard.TrippedReason()
			coordinator.HandleTripped(reason)
			_ = SendDownstreamGuardTerminalHTTP(c, nil, nil, "chat", reason)
			return errModelOutputGuardTriggered
		}
		return nil
	}
	var text strings.Builder
	id := "chatcmpl-" + NewUpstreamSessionUUID()
	chunk := func(kind string, data gin.H, reason any) gin.H {
		object := "chat.completion.chunk"
		if kind == "message" {
			object = "chat.completion"
		}
		return gin.H{"id": id, "created": started.Unix(), "model": model, "object": object, "choices": []gin.H{{"index": 0, kind: data, "finish_reason": reason}}, "usage_known": false}
	}
	seenDelta := false
	workerErr, err := readChatGPTWebChat(ctx, resp.Body, &reportedModel, func(delta string) error {
		if !seenDelta {
			firstTokenMs = int(time.Since(started).Milliseconds())
		}
		data := gin.H{"content": delta}
		if !seenDelta {
			data["role"] = "assistant"
			seenDelta = true
		}
		if !stream {
			_, _ = text.WriteString(delta)
			return nil
		}
		payload := chunk("delta", data, nil)
		encoded, _ := json.Marshal(payload)
		if err := observe(encoded); err != nil {
			return err
		}
		return writeChatGPTWebChatSSEData(c, encoded)
	})
	if workerErr != nil {
		if chatGPTWebAccountReadinessError(workerErr.code) {
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if _, err := h.db.RecordChatGPTWebCheckFailure(cleanup, state, token, chatGPTWebHash(requested), started, workerErr.code, "execute", 0); err != nil {
				log.Printf("[ChatGPTWeb] account_id=%d could not persist chat account failure code=%s", account.ID(), workerErr.code)
			}
			cancel()
		}
		return workerErr
	}
	if err != nil {
		return err
	}
	if !stream {
		payload, _ := json.Marshal(chunk("message", gin.H{"role": "assistant", "content": text.String()}, "stop"))
		if err := observe(payload); err != nil {
			return err
		}
		if p := pipelineFromContext(ctx); p != nil {
			if err := p.Acquire(ctx); err != nil {
				return err
			}
		}
		c.Data(http.StatusOK, "application/json", payload)
		return nil
	}
	if err := writeChatGPTWebChatSSE(c, chunk("delta", gin.H{}, "stop")); err != nil {
		return err
	}
	if _, err := io.WriteString(c.Writer, "data: [DONE]\n\n"); err != nil {
		return context.Canceled
	}
	c.Writer.Flush()
	return nil
}

func writeChatGPTWebChatSSE(c *gin.Context, payload gin.H) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writeChatGPTWebChatSSEData(c, encoded)
}

func writeChatGPTWebChatSSEData(c *gin.Context, encoded []byte) error {
	if !c.Writer.Written() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
	}
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", encoded); err != nil {
		return context.Canceled
	}
	c.Writer.Flush()
	return nil
}

// Only a complete NDJSON record can certify completion. EOF and malformed
// framing never synthesize an OpenAI stop or [DONE].
func readChatGPTWebChat(ctx context.Context, src io.Reader, reportedModel *string, delta func(string) error) (*chatGPTWebFailure, error) {
	scanner := bufio.NewScanner(io.LimitReader(src, chatGPTWebMaxResponseBytes))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) != 0 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, nil
	})
	protocolError := &chatGPTWebFailure{"worker_protocol_error", http.StatusBadGateway, true}
	var totalText int
	var hasText bool
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event struct {
			chatGPTWebEvent
			Text       *string `json:"text"`
			UsageKnown *bool   `json:"usage_known"`
			ModelSlug  string  `json:"model_slug"`
		}
		if json.Unmarshal(line, &event) != nil {
			return nil, protocolError
		}
		switch event.Type {
		case "delta":
			if event.Text == nil {
				return nil, protocolError
			}
			if *event.Text != "" {
				totalText += len(*event.Text)
				if totalText > chatGPTWebMaxChatOutput {
					return nil, protocolError
				}
				hasText = hasText || strings.TrimSpace(*event.Text) != ""
				if err := delta(*event.Text); err != nil {
					return nil, err
				}
			}
		case "completed":
			if !hasText || event.UsageKnown == nil || *event.UsageKnown {
				return nil, protocolError
			}
			if len(event.ModelSlug) <= 128 && strings.IndexFunc(event.ModelSlug, func(r rune) bool {
				return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_')
			}) < 0 {
				*reportedModel = event.ModelSlug
			}
			return nil, nil
		case "error":
			failure := chatGPTWebFailureFromEvent(event.chatGPTWebEvent)
			switch event.Code {
			case "chat_stream_interrupted", "chat_rate_limited", "worker_timeout", "worker_unavailable", "worker_chat_unsupported":
				failure.code = event.Code
			}
			return failure, nil
		default:
			return nil, protocolError
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, &chatGPTWebFailure{"chat_stream_interrupted", http.StatusBadGateway, true}
}
