package proxy

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/openaiidentity"
	"github.com/codex2api/internal/webasset"
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

func chatGPTWebStateMatches(state database.ChatGPTWebAccountState, token, requested, proxyURL string) bool {
	return openaiidentity.ValidWebWorkspaceID(state.WorkspaceID) && state.WorkspaceFingerprint == chatGPTWebHash(state.WorkspaceID) &&
		state.WorkspaceBindingFingerprint == chatGPTWebHash(requested) && (requested == "" || requested == state.WorkspaceID) &&
		state.CredentialRevision == chatGPTWebHash(token) && state.ProxyFingerprint == chatGPTWebHash(proxyURL)
}

func chatGPTWebStateReady(state database.ChatGPTWebAccountState, operation string, now time.Time) bool {
	if !state.Enabled || state.Status != "ready" || state.ObservedAt == nil || now.Sub(*state.ObservedAt) > 30*time.Minute || state.ObservedAt.After(now.Add(time.Minute)) {
		return false
	}
	if state.ImageQuotaRemaining == nil || *state.ImageQuotaRemaining <= 0 || state.ErrorCode != "" || !openaiidentity.ValidWebWorkspaceID(state.WorkspaceID) {
		return false
	}
	for _, capability := range state.Capabilities {
		if capability == operation {
			return true
		}
	}
	return false
}

func (h *Handler) chatGPTWebModelRecords(_ context.Context, row *database.APIKeyRow) map[string]*scopedModelRecord {
	records := map[string]*scopedModelRecord{}
	if row == nil || row.Limits.ResolveUpstreamChannel() != database.UpstreamChannelChatGPTWeb || row.Limits.ResolveImageGenerationPolicy() != database.ImageGenerationPolicyAllow {
		return records
	}
	// Discovery describes the supported channel/key contract. It must neither
	// probe upstream nor hide models because an account observation expired.
	for _, model := range chatGPTWebImageModels {
		if checkAPIKeyModel(model, row.Limits) == "" {
			addScopedModel(records, model, modelBackingCodex, time.Time{}, false)
		}
	}
	return records
}

// Called before the request count is installed, so this checks the original scope.
func (h *Handler) allScopedWebAccountsBelowCount(c *gin.Context, key *database.APIKeyRow, count int) bool {
	filter := h.chatGPTWebRequestFilter(c)
	found := false
	for _, a := range h.store.Accounts() {
		if !a.ChatGPTWebCheckEligible() || !auth.APIKeyRowAllowsChatGPTWebAccount(key, a) || !filter(a) {
			continue
		}
		found = true
		if a.WebReferenceCountAllowed(count) {
			return false
		}
	}
	return found
}

func (h *Handler) chatGPTWebRequestFilter(c *gin.Context) auth.AccountFilter {
	identity := resolveRequestSessionIdentity(c.Request.Header, nil)
	count := c.GetInt("chatGPTWebReferenceCount")
	return h.applyScopeBudgetFilter(c, applyAffinityGroupRouting(c, identity, func(a *auth.Account) bool {
		return a != nil && !a.IsRelayStyle() && (count == 0 || a.WebReferenceCountAllowed(count))
	}))
}

func (h *Handler) takeChatGPTWebAccount(c *gin.Context, operation, owner string, until time.Time, pinnedID int64) (*auth.Account, *database.ChatGPTWebAccountState, string, error) {
	states, err := h.db.ListChatGPTWebAccountStates(c.Request.Context())
	if err != nil {
		return nil, nil, "", errors.New("web account state is unavailable")
	}
	filter := h.chatGPTWebRequestFilter(c)
	keyID := requestAPIKeyID(c)
	key := apiKeyRowFromContext(c)
	roundRobin := pinnedID == 0 && key != nil && key.Limits.WebSchedulingMode == database.WebSchedulingRoundRobin
	start := 0
	if roundRobin {
		if last, ok := h.chatGPTWebLastAccount.Load(keyID); ok {
			// The DB returns ID order. Advance past the winner, not one raw
			// list entry, so gaps of unusable accounts don't bias rotation.
			if next := slices.IndexFunc(states, func(s database.ChatGPTWebAccountState) bool { return s.AccountID > last.(int64) }); next >= 0 {
				start = next
			}
		}
	}
	for i := range states {
		state := states[(start+i)%len(states)]
		if pinnedID != 0 && state.AccountID != pinnedID {
			continue
		}
		if !chatGPTWebStateReady(state, operation, time.Now()) {
			continue
		}
		account := h.store.TakeChatGPTWebAccount(state.AccountID, keyID, filter)
		if account == nil {
			continue
		}
		proxyURL, usable := h.store.ResolveChatGPTWebProxy(account)
		token, requested, _, expired, identityErr := account.ChatGPTWebIdentity()
		if !usable || expired || identityErr != nil || !chatGPTWebStateMatches(state, token, requested, proxyURL) {
			h.store.Release(account)
			continue
		}
		if guard, ok := c.Get(chatGPTWebAdmissionSelectionGuard); ok {
			if err := guard.(func(context.Context, *auth.Account) error)(c.Request.Context(), account); err != nil {
				h.store.Release(account)
				return nil, nil, "", err
			}
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
		// Re-read after acquiring the lease: a preceding owner may have just
		// recorded an authentication failure against our earlier ready snapshot.
		ctx := c.Request.Context()
		latest, readErr := h.db.GetChatGPTWebAccountState(ctx, account.ID())
		row, accountErr := h.db.GetAccountByID(ctx, account.ID())
		paused, pauseErr := h.db.IsIntelligenceCheckPaused(ctx, account.ID())
		proxyURL, usable = h.store.ResolveChatGPTWebProxy(account)
		token, requested, _, expired, identityErr = account.ChatGPTWebIdentity()
		if !usable || expired || identityErr != nil || readErr != nil || accountErr != nil || pauseErr != nil || paused || !auth.ChatGPTWebAutoAccountEligible(row, time.Now()) ||
			row.GetCredential("access_token") != token || !chatGPTWebStateReady(*latest, operation, time.Now()) || !chatGPTWebStateMatches(*latest, token, requested, proxyURL) {
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			releaseErr := h.db.ReleaseChatGPTWebAccountLease(cleanup, account.ID(), owner)
			cancel()
			h.store.Release(account)
			if readErr != nil || accountErr != nil || pauseErr != nil || releaseErr != nil {
				return nil, nil, "", errors.New("web account validation unavailable")
			}
			continue
		}
		h.AcquireAPIKeyScopeConcurrency(c, account)
		if roundRobin {
			h.chatGPTWebLastAccount.Store(keyID, state.AccountID)
		}
		return account, latest, proxyURL, nil
	}
	return nil, nil, "", &chatGPTWebFailure{"web_account_unavailable", http.StatusServiceUnavailable, false}
}

func chatGPTWebErrorFields(err error, executionID string) (gin.H, int) {
	failure := &chatGPTWebFailure{code: "upstream_error", status: 502}
	if !errors.As(err, &failure) {
		failure = &chatGPTWebFailure{code: "upstream_error", status: 503}
	}
	body := gin.H{"type": "chatgpt_web_error", "code": failure.code, "message": failure.Error(), "execution_id": executionID, "result_unknown": failure.unknown, "retryable": false}
	var check *ChatGPTWebCheckError
	if errors.As(err, &check) {
		stage, code := webCheckStage(check.Stage), webCheckCode(check.Code)
		origin := "未记录上游 HTTP 响应"
		if check.UpstreamStatus >= 100 && check.UpstreamStatus <= 599 {
			origin = fmt.Sprintf("上游 HTTP %d", check.UpstreamStatus)
			body["origin_http_status"] = check.UpstreamStatus
		}
		body["precheck_stage"] = stage
		label := stage
		if stage == "bootstrap" {
			label = "访问 ChatGPT 首页"
		}
		body["message"] = fmt.Sprintf("生图未提交：Web 账号预检失败（%s，%s；%s）", label, code, origin)
	}
	var detail *webExecutionFailure
	if errors.As(err, &detail) {
		body["last_confirmed_stage"] = detail.stage
		body["asset_count"] = detail.assets
		body["recovery_ids_known"] = detail.recoveryIDs
		body["validated_image"] = detail.imageValidated
	}
	return body, failure.status
}

func writeChatGPTWebError(c *gin.Context, err error, executionID string) {
	body, status := chatGPTWebErrorFields(err, executionID)
	payload := gin.H{"error": body}
	if c.Writer.Written() {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", b)
		c.Writer.Flush()
		return
	}
	c.JSON(status, payload)
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
	cfg, err := h.loadChatGPTWebConfig()
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
	if count := len(sourceImages); count > 0 {
		// Count only key-authorized candidates. Unknown limits must keep the normal
		// admission path (including bounded checks and its original errors).
		if h.allScopedWebAccountsBelowCount(c, row, count) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "chatgpt_web_error", "code": "web_reference_count_unavailable", "message": fmt.Sprintf("当前无可用账号支持 %d 张参考图", count), "retryable": false}})
			return
		}
		c.Set("chatGPTWebReferenceCount", count)
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
		lastID = "" // A Key limit may reject this item before an execution exists.
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
	tool, _ := json.Marshal(map[string]string{"type": "image_generation", "model": model})
	syntheticBody := buildImagesResponsesRequest(prompt, nil, tool)
	if p := h.newImageGalleryPersister(c, responseFormat, model, syntheticBody); p != nil {
		urlFor = p.buildURL
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			p.finalize(cleanup)
		}()
	}
	// Only successful executions reached response assembly. A failure here is
	// local delivery failure, not an ambiguous upstream generation.
	lastCompletedID := fmt.Sprintf("web-%s-%d", base, len(results)-1)
	if p := pipelineFromContext(ctx); p != nil {
		if err := p.Acquire(ctx); err != nil {
			writeChatGPTWebError(c, &chatGPTWebFailure{"image_delivery_failed", 502, false}, lastCompletedID)
			return
		}
	}
	out, err := buildImagesAPIResponse(ctx, results, time.Now().Unix(), nil, firstMeta, responseFormat, urlFor)
	if err != nil {
		writeChatGPTWebError(c, &chatGPTWebFailure{"image_delivery_failed", 502, false}, lastCompletedID)
		return
	}
	out, _ = sjson.SetBytes(out, "channel", database.UpstreamChannelChatGPTWeb)
	out, _ = sjson.SetBytes(out, "usage_known", false)
	if failed != nil {
		itemError, _ := chatGPTWebErrorFields(failed, lastID)
		out, _ = sjson.SetBytes(out, "warning", itemError["message"])
		out, _ = sjson.SetBytes(out, "partial_error", itemError)
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
		for i, result := range results {
			payload := buildImagesStreamCompletedPayload(event, result, responseFormat, time.Now().Unix(), nil)
			payload, _ = sjson.SetBytes(payload, "execution_id", fmt.Sprintf("web-%s-%d", base, i))
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
		log.Printf("[ChatGPTWebExecution] stage=%s save_failed=%t error=%v", stage, !ok, err)
		return errors.New("web execution state could not be persisted")
	}
	*execution = next
	return nil
}

func webOpaqueID(value string) string {
	if len(value) > 160 {
		return ""
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return ""
		}
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
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return result, &chatGPTWebFailure{"internal_error", http.StatusInternalServerError, false}
	}
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
	if err != nil && existing == nil {
		var unavailable *chatGPTWebFailure
		if errors.As(err, &unavailable) && unavailable.code == "web_account_unavailable" {
			if checkErr := h.prepareChatGPTWebAccounts(c, req.Operation, req.Model); checkErr != nil {
				if errors.Is(checkErr, context.DeadlineExceeded) {
					return result, &chatGPTWebFailure{"worker_timeout", http.StatusGatewayTimeout, false}
				}
				return result, checkErr
			}
			account, state, proxyURL, err = h.takeChatGPTWebAccount(c, req.Operation, owner, until, 0)
		}
	}
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
	token, requested, email, expired, identityErr := account.ChatGPTWebIdentity()
	if expired || identityErr != nil || !chatGPTWebStateMatches(*state, token, requested, proxyURL) {
		return result, &chatGPTWebFailure{"auth_required", 409, false}
	}
	remaining := time.Until(until.Add(-30 * time.Second))
	req.chatGPTWebAccountRequest = chatGPTWebAccountRequest{AccountID: account.ID(), AccessToken: token, ChatGPTAccountID: state.WorkspaceID, ExpectedEmail: email, CredentialRevision: state.CredentialRevision, EnvironmentRevision: state.EnvironmentRevision, ProxyURL: proxyURL, Environment: chatGPTWebEnvironment{state.DeviceID, state.SessionID}, TimeoutSeconds: int((remaining + time.Second - 1) / time.Second)}
	if req.TimeoutSeconds < 10 {
		return result, context.DeadlineExceeded
	}
	if err := checkImageJobAttempt(ctx); err != nil {
		return result, err
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
	imageValidated := false
	defer func() {
		status := 200
		code := ""
		unknown := false
		lastStage := execution.Stage
		if runErr != nil {
			failure := &chatGPTWebFailure{code: "upstream_error", status: 502, unknown: submitted}
			if !errors.As(runErr, &failure) {
				failure = &chatGPTWebFailure{code: "upstream_error", status: 502, unknown: submitted}
			}
			detail := describeWebFailure(failure, execution, imageValidated)
			runErr = detail
			status, code, unknown = detail.status, detail.code, detail.unknown
			uncertain = unknown
			execution.ErrorCode = code
			execution.ResultUnknown = unknown
			stage := "failed"
			if unknown {
				stage = "unknown"
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = h.saveChatGPTWebStage(cleanup, execution, stage)
			if code == "auth_required" || code == "identity_mismatch" || code == "workspace_unavailable" || code == "environment_challenge" || code == "quota_exhausted" {
				// Worker NDJSON errors refer to this account; HTTP worker-secret
				// failures do not. Preserve the last verified quota observation.
				if _, err := h.db.RecordChatGPTWebCheckFailure(cleanup, state, token, chatGPTWebHash(requested), started, code, "execute", 0); err != nil {
					log.Printf("[ChatGPTWeb] account_id=%d could not persist account failure code=%s", account.ID(), code)
				}
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
		outcome := "success"
		if runErr != nil {
			outcome = "failed"
			if unknown {
				outcome = "unconfirmed"
			}
		}
		contextJSON, _ := json.Marshal(map[string]any{"schema": 2, "request_id": snapshotUpstreamTrace(ctx).RequestID, "account_id": account.ID(), "provider": database.UpstreamChannelChatGPTWeb, "execution_id": execution.ID, "usage_known": false, "outcome": outcome, "last_confirmed_stage": lastStage, "asset_count": len(execution.AssetIDs), "error_code": code, "result_unknown": unknown, "validated_image": imageValidated})
		logInput.DiagnosticContext = string(contextJSON)
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
	// Keep individual events bounded while consuming legitimate extra assets.
	limited := &io.LimitedReader{R: resp.Body, N: 8*chatGPTWebMaxResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), chatGPTWebMaxResponseBytes)
	imageEvents := 0
	declaredAssets := len(execution.AssetIDs) > 0
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
			return result, &chatGPTWebFailure{"worker_protocol_error", 502, true}
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
				return result, &chatGPTWebFailure{"worker_protocol_error", 502, true}
			}
			// Validate before mutating the snapshot: an invalid event must not
			// poison the subsequent failure-record save.
			if (event.ConversationID != "" && webOpaqueID(event.ConversationID) == "") || (event.TaskID != "" && webOpaqueID(event.TaskID) == "") {
				return result, &chatGPTWebFailure{"worker_protocol_error", 502, true}
			}
			next := *execution
			next.AssetIDs = slices.Clone(execution.AssetIDs)
			if event.ConversationID != "" {
				next.ConversationID = event.ConversationID
			}
			if event.TaskID != "" {
				next.TaskID = event.TaskID
			}
			for _, id := range event.AssetIDs {
				if !webasset.Valid(id) {
					return result, &chatGPTWebFailure{"worker_protocol_error", 502, true}
				}
				if !slices.Contains(next.AssetIDs, id) {
					next.AssetIDs = append(next.AssetIDs, id)
				}
			}
			if len(next.AssetIDs) > 8 {
				return result, &chatGPTWebFailure{"worker_protocol_error", 502, true}
			}
			// Replay may add known IDs, but cannot regress the durable stage.
			if chatGPTWebStageRank(stage) < chatGPTWebStageRank(execution.Stage) {
				stage = execution.Stage
			}
			if err := h.saveChatGPTWebStage(ctx, &next, stage); err != nil {
				return result, &chatGPTWebFailure{"worker_protocol_error", 502, true}
			}
			*execution = next
			if len(event.AssetIDs) > 0 {
				declaredAssets = true
			}
			if err := chatGPTWebProgress(c, stream, req.ExecutionID, stage); err != nil {
				return result, &chatGPTWebFailure{"submission_unknown", 502, true}
			}
		case "image":
			imageEvents++
			if imageEvents > 8 || event.Image == nil {
				return result, &chatGPTWebFailure{"worker_protocol_error", 502, true}
			}
			im, err := chatGPTWebDecodeImage(event.Image.B64JSON)
			if err != nil {
				return result, &chatGPTWebFailure{"image_download_failed", 502, true}
			}
			if im.MIMEType != event.Image.MIMEType || im.Width != event.Image.Width || im.Height != event.Image.Height {
				return result, &chatGPTWebFailure{"image_download_failed", 502, true}
			}
			assetID := event.Image.AssetID
			if !webasset.Valid(assetID) {
				return result, &chatGPTWebFailure{"image_download_failed", 502, true}
			}
			if !slices.Contains(execution.AssetIDs, assetID) {
				if declaredAssets || len(execution.AssetIDs) >= 8 {
					return result, &chatGPTWebFailure{"worker_protocol_error", 502, true}
				}
				// Cached replay can emit images without repeating the stage list.
				execution.AssetIDs = append(execution.AssetIDs, assetID)
			}
			if imageValidated {
				continue
			} // n=1: extra valid assets are not additional outputs.
			decoded, _ := decodeImageBase64(im.B64JSON)
			result = imageCallResult{Result: im.B64JSON, Model: req.Model, OutputFormat: strings.TrimPrefix(im.MIMEType, "image/"), Quality: req.Quality, Size: fmt.Sprintf("%dx%d", im.Width, im.Height), Width: im.Width, Height: im.Height, ByteSize: len(decoded)}
			imageValidated = true
		case "completed":
			if !imageValidated || limited.N <= 0 {
				return result, &chatGPTWebFailure{"image_download_failed", 502, true}
			}
			if err := h.completeChatGPTWebExecution(ctx, execution); err != nil {
				return result, &chatGPTWebFailure{"worker_protocol_error", 502, true}
			}
			return result, nil
		case "error":
			if event.Code == "environment_challenge" {
				reason := event.Reason
				switch reason {
				case "homepage_interstitial", "prepare_arkose", "prepare_turnstile", "finalize_arkose", "finalize_turnstile", "prepare_incomplete", "finalize_incomplete", "origin_http", "local_verification":
				default:
					reason = "unspecified"
				}
				log.Printf("[ChatGPTWeb] account_id=%d verification_reason=%s origin_http=%d", req.AccountID, reason, event.OriginStatus)
			}
			return result, chatGPTWebFailureFromEvent(event)
		default:
			return result, &chatGPTWebFailure{"worker_protocol_error", 502, true}
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
