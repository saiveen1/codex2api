package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/openaiidentity"
)

// ChatGPTWebCheckError contains only classified diagnostics, never raw upstream
// bodies, URLs, tokens, cookies, or provider exception messages.
type ChatGPTWebCheckError struct {
	Code           string
	Stage          string
	HTTPStatus     int
	UpstreamStatus int
	Recorded       bool
}

func (e *ChatGPTWebCheckError) Error() string {
	switch e.Code {
	case "account_disabled":
		return "Enable this account for ChatGPT Web before checking"
	case "account_busy":
		return "This account is already checking or generating; wait for that operation to finish"
	case "account_ineligible":
		return "This account is not an available OpenAI OAuth account"
	case "egress_unavailable":
		return "This account has no permitted usable Web proxy route"
	case "token_expired":
		return "The access token expired and the existing credential lifecycle could not refresh it"
	case "workspace_unavailable":
		return "The requested workspace is unavailable or no verified default workspace was returned"
	case "identity_mismatch":
		return "The authenticated Web identity does not match this account"
	case "quota_unknown":
		return "Authentication succeeded but no verifiable image quota was returned"
	case "worker_unavailable":
		return "The Web worker is disabled, not ready, or unreachable"
	case "worker_timeout":
		return "The Web environment check exceeded its time budget"
	case "worker_protocol_error":
		return "The Web worker returned an invalid environment observation"
	case "observation_stale":
		return "Credentials, workspace, environment, or proxy changed during this check; check the current account again"
	case "internal_error":
		return "The Web check record could not be read or saved"
	default:
		return chatGPTWebErrorMessage(e.Code)
	}
}

func webCheckCode(code string) string {
	switch code {
	case "auth_required", "environment_challenge", "quota_exhausted", "upstream_error", "invalid_request", "workspace_unavailable", "identity_mismatch", "quota_unknown", "token_expired", "worker_unavailable", "worker_timeout", "worker_protocol_error", "account_disabled", "account_busy", "account_ineligible", "egress_unavailable", "observation_stale", "internal_error":
		return code
	}
	return "worker_protocol_error"
}

func webCheckStage(stage string) string {
	switch stage {
	case "admission", "bootstrap", "identity", "workspace", "quota", "persist", "complete":
		return stage
	}
	return "admission"
}

func webCheckHTTPStatus(code string) int {
	switch code {
	case "auth_required", "identity_mismatch", "token_expired":
		return 422
	case "environment_challenge":
		return 403
	case "quota_exhausted":
		return 429
	case "account_disabled", "account_busy", "observation_stale":
		return 409
	case "account_ineligible", "invalid_request":
		return 400
	case "workspace_unavailable", "quota_unknown":
		return 422
	case "worker_unavailable", "egress_unavailable":
		return 503
	case "worker_timeout":
		return 504
	case "internal_error":
		return 500
	default:
		return 502
	}
}

func (h *Handler) CheckChatGPTWebAccount(ctx context.Context, accountID int64) (*database.ChatGPTWebAccountState, error) {
	return h.checkChatGPTWebAccount(ctx, accountID, true)
}

type chatGPTWebCheckAdmissionKey struct{}

type chatGPTWebCheckAdmission struct {
	validate  func(context.Context) error
	operation string
}

func (h *Handler) checkChatGPTWebAccount(ctx context.Context, accountID int64, initializeDefaults bool) (*database.ChatGPTWebAccountState, error) {
	admission, _ := ctx.Value(chatGPTWebCheckAdmissionKey{}).(*chatGPTWebCheckAdmission)
	admit := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if admission != nil {
			return admission.validate(ctx)
		}
		return nil
	}
	started := time.Now().UTC()
	var state *database.ChatGPTWebAccountState
	var token, binding string
	// Failure diagnostics and lease release must survive cancellation, but share
	// one bounded cleanup budget. This context is never used for upstream work.
	var cleanupCtx context.Context
	var cleanupCancel context.CancelFunc
	defer func() {
		if cleanupCancel != nil {
			cleanupCancel()
		}
	}()
	cleanupContext := func() context.Context {
		if cleanupCtx == nil {
			cleanupCtx, cleanupCancel = context.WithTimeout(context.Background(), 3*time.Second)
		}
		return cleanupCtx
	}
	fail := func(code, stage string, upstream int) (*database.ChatGPTWebAccountState, error) {
		code, stage = webCheckCode(code), webCheckStage(stage)
		if upstream < 100 || upstream > 599 {
			upstream = 0
		}
		failure := &ChatGPTWebCheckError{Code: code, Stage: stage, HTTPStatus: webCheckHTTPStatus(code), UpstreamStatus: upstream}
		if state != nil && h.db != nil && code != "account_busy" && code != "observation_stale" {
			cleanup := cleanupContext()
			applied, err := h.db.RecordChatGPTWebCheckFailure(cleanup, state, token, binding, started, code, stage, upstream)
			if err == nil && applied {
				failure.Recorded = true
				if latest, readErr := h.db.GetChatGPTWebAccountState(cleanup, accountID); readErr == nil {
					state = latest
				}
			}
		}
		log.Printf("[ChatGPTWebCheck] account_id=%d code=%s stage=%s upstream_status=%d recorded=%t", accountID, code, stage, upstream, failure.Recorded)
		return state, failure
	}
	if h.db == nil || h.store == nil {
		return fail("internal_error", "admission", 0)
	}
	if initializeDefaults {
		if _, err := auth.ListChatGPTWebAccountStates(ctx, h.db); err != nil {
			return fail("internal_error", "admission", 0)
		}
	}
	var err error
	state, err = h.db.GetChatGPTWebAccountState(ctx, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return fail("account_disabled", "admission", 0)
	}
	if err != nil {
		return fail("internal_error", "admission", 0)
	}
	if !state.Enabled {
		return fail("account_disabled", "admission", 0)
	}
	cfg, err := h.loadChatGPTWebConfig()
	if err != nil {
		return fail("worker_unavailable", "admission", 0)
	}
	account := h.store.FindByID(accountID)
	if account == nil || account.IsRelayStyle() {
		return fail("account_ineligible", "admission", 0)
	}
	token, requested, email, expired, identityErr := account.ChatGPTWebIdentity()
	if identityErr != nil {
		return fail("workspace_unavailable", "workspace", 0)
	}
	binding = chatGPTWebHash(requested)
	if token == "" {
		return fail("auth_required", "identity", 0)
	}
	proxyURL, usable := h.store.ResolveChatGPTWebProxy(account)
	if !usable {
		return fail("egress_unavailable", "admission", 0)
	}
	if err := admit(); err != nil {
		return state, err
	}
	owner := "check-" + NewUpstreamSessionUUID()
	lease, err := h.db.AcquireChatGPTWebAccountLease(ctx, accountID, owner, started.Add(40*time.Second))
	if err != nil {
		return fail("internal_error", "admission", 0)
	}
	if !lease {
		return fail("account_busy", "admission", 0)
	}
	defer func() {
		_ = h.db.ReleaseChatGPTWebAccountLease(cleanupContext(), accountID, owner)
	}()
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := admit(); err != nil {
		return state, err
	}
	if !initializeDefaults {
		// A preceding check may have finished between selection and the lease.
		state, err = h.db.GetChatGPTWebAccountState(ctx, accountID)
		if err != nil {
			return fail("internal_error", "admission", 0)
		}
		if !state.Enabled {
			return fail("account_disabled", "admission", 0)
		}
		if admission != nil && !chatGPTWebAutoCheckNeeded(state, admission.operation, token, requested, proxyURL, expired, time.Now()) {
			return state, nil
		}
	}
	if expired {
		// Reuse the existing single-owner refresh/rotation lifecycle, never a
		// worker refresh or a second refresh-token loop.
		if err := h.store.RefreshSingle(checkCtx, accountID); err != nil {
			return fail("token_expired", "identity", 0)
		}
		token, requested, email, expired, identityErr = account.ChatGPTWebIdentity()
		if identityErr != nil {
			return fail("workspace_unavailable", "workspace", 0)
		}
		binding = chatGPTWebHash(requested)
		if token == "" || expired {
			return fail("token_expired", "identity", 0)
		}
		proxyURL, usable = h.store.ResolveChatGPTWebProxy(account)
		if !usable {
			return fail("egress_unavailable", "admission", 0)
		}
	}
	if err := admit(); err != nil {
		return state, err
	}
	request := chatGPTWebAccountRequest{AccountID: accountID, AccessToken: token, ChatGPTAccountID: requested, ExpectedEmail: email, CredentialRevision: chatGPTWebHash(token), EnvironmentRevision: state.EnvironmentRevision, ProxyURL: proxyURL, Environment: chatGPTWebEnvironment{state.DeviceID, state.SessionID}, TimeoutSeconds: 30}
	if !initializeDefaults {
		if deadline, ok := checkCtx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return state, context.DeadlineExceeded
			}
			if seconds := int((remaining + time.Second - 1) / time.Second); seconds < request.TimeoutSeconds {
				request.TimeoutSeconds = seconds
			}
		}
	}
	client := newChatGPTWebHTTPClient()
	defer client.CloseIdleConnections()
	resp, err := chatGPTWebPost(checkCtx, client, cfg, "/v1/accounts/check", request)
	if err != nil {
		var networkError net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(checkCtx.Err(), context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
			return fail("worker_timeout", "admission", 0)
		}
		return fail("worker_unavailable", "admission", 0)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(data) > 65536 {
		return fail("worker_protocol_error", "admission", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Non-2xx authentication is between the gateway and worker. Account
		// authentication failures arrive as observations inside an HTTP 200.
		return state, &ChatGPTWebCheckError{Code: "worker_unavailable", Stage: "admission", HTTPStatus: webCheckHTTPStatus("worker_unavailable")}
	}
	var observation struct {
		Status              string     `json:"status"`
		Capabilities        []string   `json:"capabilities"`
		ImageQuotaRemaining *int       `json:"image_quota_remaining"`
		QuotaResetAt        *time.Time `json:"quota_reset_at"`
		ErrorCode           string     `json:"error_code"`
		Code                string     `json:"code"`
		WorkspaceID         string     `json:"workspace_id"`
		Stage               string     `json:"stage"`
		UpstreamStatus      int        `json:"upstream_status"`
	}
	if json.Unmarshal(data, &observation) != nil {
		return fail("worker_protocol_error", "admission", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		code := observation.ErrorCode
		if code == "" {
			code = observation.Code
		}
		if code == "" {
			code = "worker_protocol_error"
		}
		return fail(code, observation.Stage, observation.UpstreamStatus)
	}
	if observation.UpstreamStatus < 0 || observation.UpstreamStatus > 599 {
		return fail("worker_protocol_error", "admission", 0)
	}
	switch observation.Status {
	case "ready", "quota_exhausted":
	case "unverified", "auth_required", "challenge":
		code := observation.ErrorCode
		if code == "" {
			code = map[string]string{"unverified": "quota_unknown", "auth_required": "auth_required", "challenge": "environment_challenge"}[observation.Status]
		}
		return fail(code, observation.Stage, observation.UpstreamStatus)
	default:
		return fail("worker_protocol_error", "admission", 0)
	}
	if !openaiidentity.ValidWebWorkspaceID(observation.WorkspaceID) || (requested != "" && observation.WorkspaceID != requested) {
		return fail("workspace_unavailable", "workspace", observation.UpstreamStatus)
	}
	if observation.ImageQuotaRemaining == nil || *observation.ImageQuotaRemaining < 0 {
		return fail("quota_unknown", "quota", observation.UpstreamStatus)
	}
	caps := []string{}
	for _, capability := range observation.Capabilities {
		if capability == "generate" || capability == "edit" {
			caps = append(caps, capability)
		}
	}
	if observation.Status == "ready" && (len(caps) == 0 || *observation.ImageQuotaRemaining <= 0 || observation.ErrorCode != "") {
		return fail("worker_protocol_error", "quota", observation.UpstreamStatus)
	}
	if observation.Status == "quota_exhausted" && *observation.ImageQuotaRemaining != 0 {
		return fail("worker_protocol_error", "quota", observation.UpstreamStatus)
	}
	if err := admit(); err != nil {
		return state, err
	}
	currentToken, currentRequested, _, _, currentErr := account.ChatGPTWebIdentity()
	currentProxy, currentUsable := h.store.ResolveChatGPTWebProxy(account)
	if currentErr != nil || currentToken != token || currentRequested != requested || !currentUsable || currentProxy != proxyURL {
		return fail("observation_stale", "persist", 0)
	}
	now := time.Now().UTC()
	state.Status, state.Capabilities = observation.Status, caps
	state.ImageQuotaRemaining, state.QuotaResetAt, state.ObservedAt = observation.ImageQuotaRemaining, observation.QuotaResetAt, &now
	state.CredentialRevision, state.ProxyFingerprint = request.CredentialRevision, chatGPTWebHash(proxyURL)
	state.WorkspaceID, state.WorkspaceFingerprint, state.WorkspaceBindingFingerprint = observation.WorkspaceID, chatGPTWebHash(observation.WorkspaceID), binding
	state.ErrorCode = ""
	if observation.Status == "quota_exhausted" {
		state.ErrorCode = "quota_exhausted"
	}
	state.LastCheckAt, state.LastCheckStage, state.LastCheckErrorCode, state.LastCheckHTTPStatus = &started, "complete", state.ErrorCode, observation.UpstreamStatus
	saved, err := h.db.SaveChatGPTWebAccountObservation(ctx, state, token)
	if err != nil {
		return fail("internal_error", "persist", 0)
	}
	if !saved {
		return fail("observation_stale", "persist", 0)
	}
	return state, nil
}
