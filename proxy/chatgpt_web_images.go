package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

type chatGPTWebExecutionContextKey struct{}

// WithChatGPTWebExecutionID binds a durable job output to a stable execution.
// It is internal context, never a downstream account/credential override.
func WithChatGPTWebExecutionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, chatGPTWebExecutionContextKey{}, id)
}

func chatGPTWebExecutionBase(c *gin.Context) (string, error) {
	id, _ := c.Request.Context().Value(chatGPTWebExecutionContextKey{}).(string)
	if id == "" {
		id = strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	}
	if len(id) > 200 || strings.ContainsAny(id, "\r\n\x00") {
		return "", &chatGPTWebFailure{"invalid_request", 400, false}
	}
	if id == "" {
		id = NewUpstreamSessionUUID()
	}
	return chatGPTWebHash(fmt.Sprintf("%d\x00%s", requestAPIKeyID(c), id)), nil
}

func chatGPTWebStateReady(state database.ChatGPTWebAccountState, operation string, now time.Time) bool {
	if !state.Enabled || state.Status != "ready" || state.ObservedAt == nil || now.Sub(*state.ObservedAt) > 30*time.Minute || state.ObservedAt.After(now.Add(time.Minute)) {
		return false
	}
	if state.ImageQuotaRemaining != nil && *state.ImageQuotaRemaining <= 0 {
		return false
	}
	for _, capability := range state.Capabilities {
		if capability == operation {
			return true
		}
	}
	return false
}

func (h *Handler) chatGPTWebModelRecords(ctx context.Context, row *database.APIKeyRow) map[string]*scopedModelRecord {
	records := map[string]*scopedModelRecord{}
	if h.db == nil || h.store == nil || row == nil {
		return records
	}
	if _, err := loadChatGPTWebConfig(); err != nil {
		return records
	}
	if checkAPIKeyModel("gpt-image-2", row.Limits) != "" || row.Limits.ResolveImageGenerationPolicy() != database.ImageGenerationPolicyAllow {
		return records
	}
	states, err := h.db.ListChatGPTWebAccountStates(ctx)
	if err != nil {
		return records
	}
	for _, state := range states {
		if !chatGPTWebStateReady(state, "generate", time.Now()) && !chatGPTWebStateReady(state, "edit", time.Now()) {
			continue
		}
		account := h.store.FindByID(state.AccountID)
		if account == nil || account.IsRelayStyle() || !h.store.ChatGPTWebAccountVisibleToKey(row.ID, account) {
			continue
		}
		token, workspace := account.ChatGPTWebCredentials()
		proxyURL, usable := h.store.ResolveChatGPTWebProxy(account)
		if !usable || state.CredentialRevision != chatGPTWebHash(token) || state.WorkspaceFingerprint != chatGPTWebHash(workspace) || state.ProxyFingerprint != chatGPTWebHash(proxyURL) {
			continue
		}
		addScopedModel(records, "gpt-image-2", modelBackingCodex, time.Time{}, false)
		break
	}
	return records
}

func (h *Handler) takeChatGPTWebAccount(c *gin.Context, operation, owner string, until time.Time, pinnedID int64) (*auth.Account, *database.ChatGPTWebAccountState, string, error) {
	states, err := h.db.ListChatGPTWebAccountStates(c.Request.Context())
	if err != nil {
		return nil, nil, "", errors.New("web account state is unavailable")
	}
	identity := resolveRequestSessionIdentity(c.Request.Header, nil)
	filter := h.applyScopeBudgetFilter(c, applyAffinityGroupRouting(c, identity, func(a *auth.Account) bool { return a != nil && !a.IsRelayStyle() }))
	for _, state := range states {
		if pinnedID != 0 && state.AccountID != pinnedID {
			continue
		}
		if !chatGPTWebStateReady(state, operation, time.Now()) {
			continue
		}
		account := h.store.TakeChatGPTWebAccount(state.AccountID, requestAPIKeyID(c), filter)
		if account == nil {
			continue
		}
		proxyURL, usable := h.store.ResolveChatGPTWebProxy(account)
		token, workspace := account.ChatGPTWebCredentials()
		if !usable || state.CredentialRevision != chatGPTWebHash(token) || state.WorkspaceFingerprint != chatGPTWebHash(workspace) || state.ProxyFingerprint != chatGPTWebHash(proxyURL) {
			h.store.Release(account)
			continue
		}
		acquired, err := h.db.AcquireChatGPTWebAccountLease(c.Request.Context(), state.AccountID, owner, until)
		if err != nil {
			h.store.Release(account)
			return nil, nil, "", errors.New("web account lease is unavailable")
		}
		if !acquired {
			h.store.Release(account)
			continue
		}
		h.AcquireAPIKeyScopeConcurrency(c, account)
		return account, &state, proxyURL, nil
	}
	return nil, nil, "", errors.New("no verified, in-scope ChatGPT Web account is available; check web environment and quota")
}

func writeChatGPTWebError(c *gin.Context, err error, executionID string) {
	failure := &chatGPTWebFailure{code: "upstream_error", status: 502}
	if !errors.As(err, &failure) {
		failure = &chatGPTWebFailure{code: "upstream_error", status: 503}
	}
	payload := gin.H{"error": gin.H{"type": "chatgpt_web_error", "code": failure.code, "message": failure.Error(), "execution_id": executionID, "result_unknown": failure.unknown, "retryable": false}}
	if c.Writer.Written() {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", b)
		c.Writer.Flush()
		return
	}
	c.JSON(failure.status, payload)
}

func chatGPTWebProgress(c *gin.Context, stream bool, id, stage string) error {
	if !stream {
		return nil
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	b, _ := json.Marshal(gin.H{"type": "image_generation.progress", "execution_id": id, "stage": stage})
	if _, err := fmt.Fprintf(c.Writer, "event: image_generation.progress\ndata: %s\n\n", b); err != nil {
		return err
	}
	c.Writer.Flush()
	return nil
}

func (h *Handler) forwardChatGPTWebImages(c *gin.Context, raw []byte, model, prompt string, sourceImages []string, mask, responseFormat string, stream bool) {
	cfg, err := loadChatGPTWebConfig()
	if err != nil {
		c.JSON(503, gin.H{"error": gin.H{"code": "provider_disabled", "message": errChatGPTWebUnavailable.Error()}})
		return
	}
	attachUpstreamTrace(c, h.store)
	defer beginRequestDiagnostic(c)()
	if h.db == nil || h.store == nil || requestAPIKeyID(c) <= 0 {
		c.JSON(503, gin.H{"error": gin.H{"code": "provider_unavailable", "message": "ChatGPT Web requires a stored API key and durable database"}})
		return
	}
	row := apiKeyRowFromContext(c)
	if row == nil || row.Limits.ResolveImageGenerationPolicy() != database.ImageGenerationPolicyAllow {
		c.JSON(403, gin.H{"error": gin.H{"code": "image_generation_disabled", "message": "This API key does not permit image generation"}})
		return
	}
	if row.Limits.ExternalScheduler {
		c.JSON(400, gin.H{"error": gin.H{"code": "unsupported_parameter", "message": "ChatGPT Web does not support external scheduler account overrides"}})
		return
	}
	options, err := parseChatGPTWebImageOptions(raw, model, mask)
	if err != nil {
		writeChatGPTWebError(c, err, "")
		return
	}
	if responseFormat != "b64_json" && responseFormat != "url" {
		writeChatGPTWebError(c, &chatGPTWebFailure{"invalid_request", 400, false}, "")
		return
	}
	if len(prompt) > 32000 || len(sourceImages) > MaxImageEditInputCount {
		writeChatGPTWebError(c, &chatGPTWebFailure{"invalid_request", 400, false}, "")
		return
	}
	base, err := chatGPTWebExecutionBase(c)
	if err != nil {
		writeChatGPTWebError(c, err, "")
		return
	}
	operation := "generate"
	if len(sourceImages) > 0 {
		operation = "edit"
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), cfg.timeout)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	var images []chatGPTWebImage
	inputBytes := 0
	for _, source := range sourceImages {
		im, err := chatGPTWebFetchImage(ctx, source)
		if err != nil {
			writeChatGPTWebError(c, &chatGPTWebFailure{"invalid_request", 400, false}, "")
			return
		}
		inputBytes += len(im.B64JSON)
		if inputBytes > 40<<20 {
			writeChatGPTWebError(c, &chatGPTWebFailure{"invalid_request", 400, false}, "")
			return
		}
		// The worker accepts bytes and MIME only; output dimensions are not input fields.
		im.Width, im.Height = 0, 0
		images = append(images, im)
	}
	// A batch reserves one logical call per generated image, just as the studio
	// does when decomposing a durable job. Stop at first failure; retain successes.
	var results []imageCallResult
	var failed error
	var lastID string
	for i := 0; i < options.N; i++ {
		if i > 0 {
			h.attachAPIKeyModelRequestQuota(c, true)
			if status, _ := h.enforceAPIKeyLimits(c, model); status != 0 {
				failed = &chatGPTWebFailure{"quota_exhausted", status, false}
				break
			}
		}
		lastID = fmt.Sprintf("web-%s-%d", base, i)
		req := chatGPTWebRequest{ExecutionID: lastID, Operation: operation, Model: model, Prompt: prompt, Images: images, N: 1, Size: options.Size, Quality: options.Quality, OutputFormat: options.OutputFormat}
		result, err := h.runChatGPTWebImage(c, cfg, req, stream)
		if err != nil {
			failed = err
			break
		}
		results = append(results, result)
	}
	if len(results) == 0 {
		writeChatGPTWebError(c, failed, lastID)
		return
	}
	firstMeta := results[0]
	// Persist via the existing pipeline/gallery. Web usage is deliberately absent
	// rather than reporting estimated tokens as provider-measured usage.
	var urlFor imageURLBuilder
	syntheticBody := buildImagesResponsesRequest(prompt, nil, []byte(`{"type":"image_generation","model":"gpt-image-2"}`))
	if p := h.newImageGalleryPersister(c, responseFormat, model, syntheticBody); p != nil {
		urlFor = p.buildURL
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			p.finalize(cleanup)
		}()
	}
	if p := pipelineFromContext(ctx); p != nil {
		if err := p.Acquire(ctx); err != nil {
			writeChatGPTWebError(c, &chatGPTWebFailure{"image_download_failed", 502, true}, lastID)
			return
		}
	}
	out, err := buildImagesAPIResponse(ctx, results, time.Now().Unix(), nil, firstMeta, responseFormat, urlFor)
	if err != nil {
		writeChatGPTWebError(c, &chatGPTWebFailure{"image_download_failed", 502, true}, lastID)
		return
	}
	out, _ = sjson.SetBytes(out, "channel", database.UpstreamChannelChatGPTWeb)
	out, _ = sjson.SetBytes(out, "usage_known", false)
	if failed != nil {
		out, _ = sjson.SetBytes(out, "warning", chatGPTWebErrorMessage("upstream_error"))
		out, _ = sjson.SetBytes(out, "partial_success", true)
	}
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		event := "image_generation.completed"
		if operation == "edit" {
			event = "image_edit.completed"
		}
		for _, result := range results {
			payload := buildImagesStreamCompletedPayload(event, result, responseFormat, time.Now().Unix(), nil)
			payload, _ = sjson.SetBytes(payload, "channel", database.UpstreamChannelChatGPTWeb)
			payload, _ = sjson.SetBytes(payload, "usage_known", false)
			_, _ = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, payload)
			c.Writer.Flush()
		}
		if failed != nil {
			writeChatGPTWebError(c, failed, lastID)
		}
	} else {
		c.Data(http.StatusOK, "application/json", out)
	}
}

func (h *Handler) saveChatGPTWebStage(ctx context.Context, execution *database.ChatGPTWebExecution, stage string) error {
	expected := execution.Stage
	next := *execution
	next.Stage = stage
	ok, err := h.db.SaveChatGPTWebExecution(ctx, &next, expected)
	if err != nil || !ok {
		return errors.New("web execution state could not be persisted")
	}
	*execution = next
	return nil
}

func webOpaqueID(value string) string {
	if len(value) > 200 || strings.ContainsAny(value, "?&#\r\n\x00") || strings.Contains(value, "https://") || strings.Contains(value, "http://") {
		return ""
	}
	return value
}

func (h *Handler) runChatGPTWebImage(c *gin.Context, cfg chatGPTWebConfig, req chatGPTWebRequest, stream bool) (result imageCallResult, runErr error) {
	ctx := c.Request.Context()
	now := time.Now()
	owner := NewUpstreamSessionUUID()
	hashInput, _ := json.Marshal(req)
	requestHash := chatGPTWebHash(string(hashInput))
	// Only a previously submitted execution with recovery IDs and an expired
	// lease may resume; the worker will perform reads/downloads, never generation.
	existing, lookupErr := h.db.GetChatGPTWebExecution(ctx, req.ExecutionID, requestAPIKeyID(c))
	var pinnedID int64
	if lookupErr == nil && existing != nil {
		if !chatGPTWebCanResume(existing, requestHash, now) {
			return result, &chatGPTWebFailure{"execution_conflict", 409, existing.ResultUnknown}
		}
		pinnedID = existing.AccountID
	}
	until := now.Add(cfg.timeout + 30*time.Second)
	if deadline, ok := ctx.Deadline(); ok {
		until = deadline.Add(30 * time.Second)
	}
	account, state, proxyURL, err := h.takeChatGPTWebAccount(c, req.Operation, owner, until, pinnedID)
	if err != nil {
		return result, err
	}
	defer h.store.Release(account)
	defer h.ReleaseAPIKeyScopeConcurrency(c)
	uncertain := false
	defer func() {
		if !uncertain {
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = h.db.ReleaseChatGPTWebAccountLease(cleanup, account.ID(), owner)
		}
	}()
	token, workspace := account.ChatGPTWebCredentials()
	if chatGPTWebHash(token) != state.CredentialRevision || chatGPTWebHash(workspace) != state.WorkspaceFingerprint {
		return result, &chatGPTWebFailure{"auth_required", 409, false}
	}
	remaining := time.Until(until.Add(-30 * time.Second))
	req.chatGPTWebAccountRequest = chatGPTWebAccountRequest{AccountID: account.ID(), AccessToken: token, ChatGPTAccountID: workspace, CredentialRevision: state.CredentialRevision, EnvironmentRevision: state.EnvironmentRevision, ProxyURL: proxyURL, Environment: chatGPTWebEnvironment{state.DeviceID, state.SessionID}, TimeoutSeconds: int((remaining + time.Second - 1) / time.Second)}
	if req.TimeoutSeconds < 10 {
		return result, context.DeadlineExceeded
	}
	execution := &database.ChatGPTWebExecution{ID: req.ExecutionID, APIKeyID: requestAPIKeyID(c), AccountID: account.ID(), RequestHash: requestHash, CredentialRevision: state.CredentialRevision, EnvironmentRevision: state.EnvironmentRevision, ProxyFingerprint: state.ProxyFingerprint, Operation: req.Operation, Stage: "prepared", LeaseOwner: owner, LeaseUntil: until}
	execution.WorkspaceFingerprint = state.WorkspaceFingerprint
	if existing != nil {
		if existing.CredentialRevision != state.CredentialRevision || existing.EnvironmentRevision != state.EnvironmentRevision || existing.ProxyFingerprint != state.ProxyFingerprint || existing.WorkspaceFingerprint != state.WorkspaceFingerprint {
			return result, &chatGPTWebFailure{"execution_conflict", 409, true}
		}
		execution = existing
		execution.LeaseOwner, execution.LeaseUntil = owner, until
		execution.ErrorCode, execution.ResultUnknown = "", false
		stage := execution.Stage
		if stage == "unknown" || stage == "failed" {
			stage = "submitted"
			if len(execution.AssetIDs) > 0 {
				stage = "generated"
			}
		}
		if err := h.saveChatGPTWebStage(ctx, execution, stage); err != nil {
			return result, &chatGPTWebFailure{"execution_conflict", 409, true}
		}
	} else {
		created, err := h.db.CreateChatGPTWebExecution(ctx, execution)
		if err != nil || !created {
			return result, &chatGPTWebFailure{"execution_conflict", 409, false}
		}
	}
	started := time.Now()
	submitted := false
	defer func() {
		status := 200
		code := ""
		unknown := false
		if runErr != nil {
			failure := &chatGPTWebFailure{code: "upstream_error", status: 502, unknown: submitted}
			if !errors.As(runErr, &failure) {
				failure = &chatGPTWebFailure{code: "upstream_error", status: 502, unknown: submitted}
			}
			status, code, unknown = failure.status, failure.code, failure.unknown
			uncertain = unknown
			execution.ErrorCode = code
			execution.ResultUnknown = unknown
			stage := "failed"
			if unknown {
				stage = "unknown"
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = h.saveChatGPTWebStage(cleanup, execution, stage)
			if code == "auth_required" || code == "quota_exhausted" || code == "environment_challenge" {
				updated := *state
				updated.ErrorCode = code
				updated.Capabilities = nil
				updated.Status = map[string]string{"auth_required": "auth_required", "quota_exhausted": "quota_exhausted", "environment_challenge": "challenge"}[code]
				observed := time.Now().UTC()
				updated.ObservedAt = &observed
				_, _ = h.db.SaveChatGPTWebAccountObservation(cleanup, &updated, token)
			}
			cancel()
		}
		logInput := &database.UsageLogInput{Channel: database.UpstreamChannelChatGPTWeb, AccountID: account.ID(), Model: req.Model, Endpoint: c.Request.URL.Path, InboundEndpoint: c.Request.URL.Path, UpstreamEndpoint: "/backend-api/f/conversation", StatusCode: status, DurationMs: int(time.Since(started).Milliseconds()), Stream: stream}
		if runErr != nil {
			logInput.ErrorMessage = chatGPTWebErrorMessage(code)
			logInput.UpstreamErrorKind = "chatgpt_web"
		} else {
			applyImageUsageLogInfo(logInput, imageUsageLogInfoFromImages([]imageCallResult{result}))
		}
		if runErr == nil {
			contextJSON, _ := json.Marshal(map[string]any{"schema": 2, "request_id": snapshotUpstreamTrace(ctx).RequestID, "account_id": account.ID(), "provider": database.UpstreamChannelChatGPTWeb, "execution_id": execution.ID, "usage_known": false, "outcome": "success"})
			logInput.DiagnosticContext = string(contextJSON)
		}
		h.logUsageForRequest(c, logInput)
	}()
	if existing == nil {
		if err := ConsumeAPIKeyModelRequestQuota(ctx, req.Model); err != nil {
			return result, err
		}
		// An interrupted gateway response cannot prove the worker did not accept.
		if err := h.saveChatGPTWebStage(ctx, execution, "submitting"); err != nil {
			return result, err
		}
	}
	submitted = true
	if p := pipelineFromContext(ctx); p != nil {
		p.Release()
	}
	client := newChatGPTWebHTTPClient()
	defer client.CloseIdleConnections()
	resp, err := chatGPTWebPost(ctx, client, cfg, "/v1/images/execute", req)
	if err != nil {
		return result, &chatGPTWebFailure{"submission_unknown", 502, true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		status := resp.StatusCode
		if status < 400 || status > 599 {
			status = 502
		}
		// Only explicit local validation failures guarantee no worker submission.
		unknown := status != 400 && status != 401 && status != 403 && status != 422
		return result, &chatGPTWebFailure{"upstream_error", status, unknown}
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") {
		return result, &chatGPTWebFailure{"submission_unknown", 502, true}
	}
	limited := &io.LimitedReader{R: resp.Body, N: chatGPTWebMaxResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), chatGPTWebMaxResponseBytes)
	gotImage := false
	for scanner.Scan() {
		if ctx.Err() != nil {
			return result, &chatGPTWebFailure{"submission_unknown", 502, true}
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event chatGPTWebEvent
		if json.Unmarshal(line, &event) != nil {
			return result, &chatGPTWebFailure{"submission_unknown", 502, true}
		}
		switch event.Type {
		case "stage":
			stage := event.Stage
			if stage == "prepared" || stage == "submitting" {
				continue
			}
			switch stage {
			case "submitted", "generated", "downloading":
			default:
				return result, &chatGPTWebFailure{"submission_unknown", 502, true}
			}
			if id := webOpaqueID(event.ConversationID); id != "" {
				execution.ConversationID = id
			}
			if id := webOpaqueID(event.TaskID); id != "" {
				execution.TaskID = id
			}
			for _, id := range event.AssetIDs {
				if safe := webOpaqueID(id); safe != "" && len(execution.AssetIDs) < 8 {
					execution.AssetIDs = append(execution.AssetIDs, safe)
				}
			}
			// Recovery may replay earlier facts; never regress the durable stage.
			if chatGPTWebStageRank(stage) < chatGPTWebStageRank(execution.Stage) {
				continue
			}
			if stage != execution.Stage {
				if err := h.saveChatGPTWebStage(ctx, execution, stage); err != nil {
					return result, &chatGPTWebFailure{"submission_unknown", 502, true}
				}
			}
			if err := chatGPTWebProgress(c, stream, req.ExecutionID, stage); err != nil {
				return result, &chatGPTWebFailure{"submission_unknown", 502, true}
			}
		case "image":
			if gotImage || event.Image == nil {
				return result, &chatGPTWebFailure{"upstream_error", 502, true}
			}
			im, err := chatGPTWebDecodeImage(event.Image.B64JSON)
			if err != nil {
				return result, &chatGPTWebFailure{"image_download_failed", 502, true}
			}
			if im.MIMEType != event.Image.MIMEType || im.Width != event.Image.Width || im.Height != event.Image.Height {
				return result, &chatGPTWebFailure{"image_download_failed", 502, true}
			}
			assetID := webOpaqueID(event.Image.AssetID)
			if assetID == "" {
				return result, &chatGPTWebFailure{"image_download_failed", 502, true}
			}
			if len(execution.AssetIDs) == 0 {
				execution.AssetIDs = []string{assetID}
			} else {
				found := false
				for _, id := range execution.AssetIDs {
					if id == assetID {
						found = true
						break
					}
				}
				if !found {
					return result, &chatGPTWebFailure{"image_download_failed", 502, true}
				}
			}
			decoded, _ := decodeImageBase64(im.B64JSON)
			result = imageCallResult{Result: im.B64JSON, Model: req.Model, OutputFormat: strings.TrimPrefix(im.MIMEType, "image/"), Quality: req.Quality, Size: fmt.Sprintf("%dx%d", im.Width, im.Height), Width: im.Width, Height: im.Height, ByteSize: len(decoded)}
			gotImage = true
		case "completed":
			if !gotImage || limited.N <= 0 {
				return result, &chatGPTWebFailure{"image_download_failed", 502, true}
			}
			if err := h.completeChatGPTWebExecution(ctx, execution); err != nil {
				return result, &chatGPTWebFailure{"submission_unknown", 502, true}
			}
			return result, nil
		case "error":
			return result, chatGPTWebFailureFromEvent(event)
		default:
			return result, &chatGPTWebFailure{"submission_unknown", 502, true}
		}
	}
	return result, &chatGPTWebFailure{"submission_unknown", 502, true}
}

func chatGPTWebCanResume(e *database.ChatGPTWebExecution, requestHash string, now time.Time) bool {
	if e == nil || e.RequestHash != requestHash || e.LeaseUntil.After(now) {
		return false
	}
	if e.ConversationID == "" && e.TaskID == "" && len(e.AssetIDs) == 0 {
		return false
	}
	switch e.Stage {
	case "submitted", "generated", "downloading", "unknown":
		return true
	case "failed":
		return e.ErrorCode == "image_download_failed" || e.ErrorCode == "image_poll_timeout" || e.ErrorCode == "image_stream_interrupted"
	}
	return false
}

func chatGPTWebStageRank(stage string) int {
	return map[string]int{"prepared": 1, "submitting": 2, "submitted": 3, "generated": 4, "downloading": 5, "completed": 6}[stage]
}

// Cached worker replay can omit historical stage events. Only call after both
// validated bytes and a terminal completion have been received.
func (h *Handler) completeChatGPTWebExecution(ctx context.Context, execution *database.ChatGPTWebExecution) error {
	next := map[string]string{"submitting": "submitted", "submitted": "generated", "generated": "downloading", "downloading": "completed"}
	for execution.Stage != "completed" {
		stage, ok := next[execution.Stage]
		if !ok {
			return errors.New("invalid completed web execution stage")
		}
		if err := h.saveChatGPTWebStage(ctx, execution, stage); err != nil {
			return err
		}
	}
	return nil
}
