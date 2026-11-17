package proxy

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const (
	chatGPTWebAdmissionPrepared       = "chatGPTWebAdmissionPrepared"
	chatGPTWebAdmissionSelectionGuard = "chatGPTWebAdmissionSelectionGuard"
	chatGPTWebAdmissionAttempts       = 3
	chatGPTWebAdmissionCandidates     = 32
	chatGPTWebAdmissionBudget         = 20 * time.Second
)

func chatGPTWebAdmissionUnavailable() error {
	return &chatGPTWebFailure{"web_account_unavailable", http.StatusServiceUnavailable, false}
}

func chatGPTWebAdmissionKeyValid(row *database.APIKeyRow, model string) bool {
	return row != nil && row.ID > 0 && row.Enabled && (!row.ExpiresAt.Valid || row.ExpiresAt.Time.After(time.Now())) &&
		row.Limits.ResolveUpstreamChannel() == database.UpstreamChannelChatGPTWeb &&
		row.Limits.ResolveImageGenerationPolicy() == database.ImageGenerationPolicyAllow &&
		!row.Limits.ExternalScheduler && checkAPIKeyModel(model, row.Limits) == ""
}

// A configuration edit requires a new HTTP authorization pass. In particular,
// never replace an in-flight request's original scope with a broader DB scope.
func chatGPTWebAdmissionSameScope(a, b *database.APIKeyRow) bool {
	return a != nil && b != nil && a.ID == b.ID && a.Enabled == b.Enabled && a.ExpiresAt.Valid == b.ExpiresAt.Valid &&
		(!a.ExpiresAt.Valid || a.ExpiresAt.Time.Equal(b.ExpiresAt.Time)) &&
		reflect.DeepEqual(a.AllowedGroupIDs, b.AllowedGroupIDs) && reflect.DeepEqual(a.Limits, b.Limits)
}

func chatGPTWebAdmissionGroupBound(row *database.APIKeyRow, groups []int64) bool {
	return row != nil && slices.ContainsFunc(row.AllowedGroupIDs, func(id int64) bool {
		return id > 0 && slices.Contains(groups, id)
	})
}

func chatGPTWebAutoCheckNeeded(state *database.ChatGPTWebAccountState, operation, token, requested, proxyURL string, expired bool, now time.Time) bool {
	if state == nil {
		return true
	}
	if !state.Enabled {
		return false
	}
	if state.RejectedCredentialRevision != "" {
		if state.RejectedCredentialRevision == chatGPTWebHash(token) {
			return false
		}
		// Rotated credentials need a real check, not the old credential's cooldown.
		if state.LastCheckErrorCode == "auth_required" || state.LastCheckErrorCode == "identity_mismatch" {
			return true
		}
	}
	if state.LastCheckErrorCode != "" && state.LastCheckAt != nil && now.Sub(*state.LastCheckAt) < 2*time.Minute {
		return false
	}
	matches := !expired && chatGPTWebStateMatches(*state, token, requested, proxyURL)
	if matches && state.Status == "quota_exhausted" && state.QuotaResetAt != nil && state.QuotaResetAt.After(now) {
		return false
	}
	if matches && chatGPTWebStateReady(*state, operation, now) {
		return false // Busy ready accounts must not be probed again.
	}
	fresh := state.ObservedAt != nil && now.Sub(*state.ObservedAt) <= 30*time.Minute && !state.ObservedAt.After(now.Add(time.Minute))
	if matches && fresh && (state.Status == "ready" || state.Status == "quota_exhausted") {
		if state.ImageQuotaRemaining != nil && *state.ImageQuotaRemaining == 0 && (state.QuotaResetAt == nil || state.QuotaResetAt.After(now)) {
			return false
		}
		if operation == "edit" && !slices.Contains(state.Capabilities, "edit") {
			return false
		}
	}
	return true
}

// prepareChatGPTWebAccounts only prepares local readiness; it never submits an
// image. Its caller may retry local selection once, only for a new execution.
func (h *Handler) prepareChatGPTWebAccounts(c *gin.Context, operation, model string) (resultErr error) {
	parent := c.Request.Context()
	if err := parent.Err(); err != nil {
		return err
	}
	if _, done := c.Get(chatGPTWebAdmissionPrepared); done {
		return chatGPTWebAdmissionUnavailable()
	}
	c.Set(chatGPTWebAdmissionPrepared, true)
	ctx, cancel := context.WithTimeout(parent, chatGPTWebAdmissionBudget)
	defer func() {
		if err := ctx.Err(); err != nil {
			resultErr = err
		}
		cancel()
	}()
	if h.db == nil || h.store == nil || requestAPIKeyID(c) <= 0 || (operation != "generate" && operation != "edit") {
		return chatGPTWebAdmissionUnavailable()
	}
	original := apiKeyRowFromContext(c)
	readKey := func(ctx context.Context) (*database.APIKeyRow, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, err := h.db.GetAPIKeyByID(ctx, requestAPIKeyID(c))
		if err != nil || !chatGPTWebAdmissionKeyValid(row, model) || !chatGPTWebAdmissionSameScope(original, row) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, chatGPTWebAdmissionUnavailable()
		}
		return row, nil
	}
	row, err := readKey(ctx)
	if err != nil {
		return err
	}
	h.syncAPIKeyAllowedGroups(row)
	filter := h.chatGPTWebRequestFilter(c)
	states, err := h.db.ListChatGPTWebAccountStates(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return chatGPTWebAdmissionUnavailable()
	}
	byID := make(map[int64]*database.ChatGPTWebAccountState, len(states))
	for i := range states {
		byID[states[i].AccountID] = &states[i]
	}
	type candidate struct {
		account   *auth.Account
		expired   bool
		lastCheck time.Time
	}
	var candidates []candidate
	for _, account := range h.store.Accounts() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !account.ChatGPTWebCheckEligible() || !auth.APIKeyRowAllowsChatGPTWebAccount(row, account) || (filter != nil && !filter(account)) {
			continue
		}
		token, requested, _, expired, identityErr := account.ChatGPTWebIdentity()
		proxyURL, usable := h.store.ResolveChatGPTWebProxy(account)
		state := byID[account.ID()]
		if identityErr != nil || !usable || (state == nil && !chatGPTWebAdmissionGroupBound(row, account.GroupIDSnapshot())) ||
			!chatGPTWebAutoCheckNeeded(state, operation, token, requested, proxyURL, expired, time.Now()) {
			continue
		}
		item := candidate{account: account, expired: expired}
		if state != nil && state.LastCheckAt != nil {
			item.lastCheck = *state.LastCheckAt
		}
		candidates = append(candidates, item)
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		if a.expired != b.expired {
			if !a.expired {
				return -1
			}
			return 1
		}
		if order := a.lastCheck.Compare(b.lastCheck); order != 0 {
			return order
		}
		if a.account.ID() < b.account.ID() {
			return -1
		}
		if a.account.ID() > b.account.ID() {
			return 1
		}
		return 0
	})

	// Count every fresh account validation, including checks around lease/refresh
	// and before returning, not just distinct IDs. Never scan the DB account pool.
	validated := 0
	validate := func(ctx context.Context, account *auth.Account) (*auth.Account, error) {
		key, err := readKey(ctx)
		if err != nil {
			return nil, err
		}
		if validated >= chatGPTWebAdmissionCandidates {
			return nil, chatGPTWebAdmissionUnavailable()
		}
		validated++
		current, err := h.db.GetAccountByID(ctx, account.ID())
		if err != nil {
			return nil, chatGPTWebAdmissionUnavailable()
		}
		if !auth.ChatGPTWebAutoAccountEligible(current, time.Now()) {
			return nil, chatGPTWebAdmissionUnavailable()
		}
		groups, err := h.db.GetAccountGroupIDs(ctx, account.ID())
		if err != nil {
			return nil, chatGPTWebAdmissionUnavailable()
		}
		paused, err := h.db.IsIntelligenceCheckPaused(ctx, account.ID())
		if err != nil || paused {
			return nil, chatGPTWebAdmissionUnavailable()
		}
		fresh := &auth.Account{
			DBID: current.ID, AccessToken: current.GetCredential("access_token"),
			AccountID: current.GetCredential("account_id"), CustomHeaders: current.GetCredentialStringMap("custom_headers"),
			PlanType: current.GetCredential("plan_type"), GroupIDs: groups, ProxyURL: current.ProxyURL,
			CooldownReason: current.CooldownReason, Status: auth.StatusReady,
		}
		if current.Status == "cooldown" {
			fresh.Status = auth.StatusCooldown
		}
		if raw := current.GetCredential("expires_at"); raw != "" {
			fresh.ExpiresAt, err = time.Parse(time.RFC3339, raw)
			if err != nil {
				return nil, chatGPTWebAdmissionUnavailable()
			}
		}
		fresh.SetAllowedAPIKeyIDs(current.GetCredentialInt64Slice("allowed_api_key_ids"))
		if !auth.APIKeyRowAllowsChatGPTWebAccount(key, fresh) || (filter != nil && (!filter(fresh) || !filter(account))) ||
			!h.store.ChatGPTWebAccountCanCheckContext(ctx, key.ID, fresh) || !h.store.ChatGPTWebAccountCanCheckContext(ctx, key.ID, account) {
			return nil, chatGPTWebAdmissionUnavailable()
		}
		token, requested, _, expired, identityErr := account.ChatGPTWebIdentity()
		freshToken, freshRequested, _, freshExpired, freshIdentityErr := fresh.ChatGPTWebIdentity()
		proxyURL, usable := h.store.ResolveChatGPTWebProxy(account)
		freshProxy, freshUsable := h.store.ResolveChatGPTWebProxy(fresh)
		if identityErr != nil || freshIdentityErr != nil || token != freshToken || requested != freshRequested ||
			(current.GetCredential("expires_at") != "" && expired != freshExpired) || !usable || !freshUsable || proxyURL != freshProxy {
			return nil, chatGPTWebAdmissionUnavailable()
		}
		return fresh, ctx.Err()
	}

	h.chatGPTWebAdmissionOnce.Do(func() { h.chatGPTWebAdmissionSlots = make(chan struct{}, 7) })
	attempts := 0
	var lastCheckError *ChatGPTWebCheckError
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempts >= chatGPTWebAdmissionAttempts || validated >= chatGPTWebAdmissionCandidates {
			break
		}
		account := candidate.account
		fresh, err := validate(ctx, account)
		if err != nil {
			continue
		}
		state, err := h.db.GetChatGPTWebAccountState(ctx, account.ID())
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return chatGPTWebAdmissionUnavailable()
		}
		if missing && !chatGPTWebAdmissionGroupBound(row, fresh.GroupIDSnapshot()) {
			continue
		}
		token, requested, _, expired, identityErr := account.ChatGPTWebIdentity()
		proxyURL, usable := h.store.ResolveChatGPTWebProxy(account)
		if identityErr != nil || !usable || !chatGPTWebAutoCheckNeeded(state, operation, token, requested, proxyURL, expired, time.Now()) {
			continue
		}
		select {
		case h.chatGPTWebAdmissionSlots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		attempts++
		fresh, err = validate(ctx, account)
		if err != nil || (missing && !chatGPTWebAdmissionGroupBound(row, fresh.GroupIDSnapshot())) {
			<-h.chatGPTWebAdmissionSlots
			continue
		}
		// Only selected candidates may receive defaults, after authorization and
		// after obtaining a process check slot. Insert-only preserves disables.
		if missing {
			state, err = h.db.InitChatGPTWebAccountEnabled(ctx, account.ID())
		}
		if err != nil || state == nil || !state.Enabled {
			<-h.chatGPTWebAdmissionSlots
			continue
		}
		admission := &chatGPTWebCheckAdmission{operation: operation, validate: func(checkCtx context.Context) error {
			_, err := validate(checkCtx, account)
			return err
		}}
		checked, checkErr := h.checkChatGPTWebAccount(context.WithValue(ctx, chatGPTWebCheckAdmissionKey{}, admission), account.ID(), false)
		<-h.chatGPTWebAdmissionSlots
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if checkErr != nil {
			if errors.Is(checkErr, context.Canceled) || errors.Is(checkErr, context.DeadlineExceeded) {
				return checkErr
			}
			var detail *ChatGPTWebCheckError
			if errors.As(checkErr, &detail) {
				lastCheckError = detail
				if detail.Code == "worker_unavailable" || detail.Code == "internal_error" {
					break
				}
			}
			continue
		}
		if _, err := validate(ctx, account); err != nil {
			continue
		}
		// Read again: a concurrent disable/reset must not reuse the checker copy.
		checked, err = h.db.GetChatGPTWebAccountState(ctx, account.ID())
		if err != nil {
			continue
		}
		token, requested, _, expired, identityErr = account.ChatGPTWebIdentity()
		proxyURL, usable = h.store.ResolveChatGPTWebProxy(account)
		if !expired && identityErr == nil && usable && chatGPTWebStateReady(*checked, operation, time.Now()) && chatGPTWebStateMatches(*checked, token, requested, proxyURL) {
			c.Set(chatGPTWebAdmissionSelectionGuard, func(selectionCtx context.Context, selected *auth.Account) error {
				_, err := validate(selectionCtx, selected)
				return err
			})
			return ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, err := readKey(ctx); err != nil {
		return err
	}
	if lastCheckError != nil {
		return &chatGPTWebFailure{webCheckCode(lastCheckError.Code), lastCheckError.HTTPStatus, false}
	}
	return chatGPTWebAdmissionUnavailable()
}
