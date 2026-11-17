package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"slices"
)

const chatGPTWebBatchLimit = 1000

type chatGPTWebBatchRequest struct {
	Scope      string          `json:"scope"`
	Enabled    *bool           `json:"enabled"`
	AccountIDs json.RawMessage `json:"account_ids"`
	GroupID    json.RawMessage `json:"group_id"`
	APIKeyID   json.RawMessage `json:"api_key_id"`
}

type chatGPTWebBatchSelection struct {
	scope      string
	enabled    bool
	accountIDs []int64
	groupID    int64
	apiKeyID   int64
}

type chatGPTWebBatchError struct {
	status  int
	code    string
	message string
}

func (e *chatGPTWebBatchError) write(c *gin.Context) {
	c.JSON(e.status, gin.H{"code": e.code, "error": e.message, "stage": "admission"})
}

func chatGPTWebBatchInvalid() *chatGPTWebBatchError {
	return &chatGPTWebBatchError{http.StatusBadRequest, "invalid_request", "Expected enabled boolean and exactly one valid scope selector"}
}

func chatGPTWebBatchInternal() *chatGPTWebBatchError {
	return &chatGPTWebBatchError{http.StatusInternalServerError, "internal_error", "Unable to resolve Web account scope"}
}

func chatGPTWebBatchTooLarge() *chatGPTWebBatchError {
	return &chatGPTWebBatchError{http.StatusBadRequest, "scope_too_large", "Web account scope exceeds 1000 accounts"}
}

func decodeChatGPTWebBatch(r io.Reader) (chatGPTWebBatchSelection, *chatGPTWebBatchError) {
	var body chatGPTWebBatchRequest
	var selection chatGPTWebBatchSelection
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || body.Enabled == nil || dec.Decode(&struct{}{}) != io.EOF {
		return selection, chatGPTWebBatchInvalid()
	}
	selection.scope, selection.enabled = body.Scope, *body.Enabled
	switch body.Scope {
	case "accounts":
		if len(body.GroupID) != 0 || len(body.APIKeyID) != 0 || json.Unmarshal(body.AccountIDs, &selection.accountIDs) != nil || len(selection.accountIDs) == 0 {
			return selection, chatGPTWebBatchInvalid()
		}
		if len(selection.accountIDs) > chatGPTWebBatchLimit {
			return selection, chatGPTWebBatchTooLarge()
		}
		seen := make(map[int64]bool, len(selection.accountIDs))
		ids := make([]int64, 0, len(selection.accountIDs))
		for _, id := range selection.accountIDs {
			if id <= 0 {
				return selection, chatGPTWebBatchInvalid()
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		selection.accountIDs = ids
	case "group":
		if len(body.AccountIDs) != 0 || len(body.APIKeyID) != 0 || json.Unmarshal(body.GroupID, &selection.groupID) != nil || selection.groupID <= 0 {
			return selection, chatGPTWebBatchInvalid()
		}
	case "api_key":
		if len(body.AccountIDs) != 0 || len(body.GroupID) != 0 || json.Unmarshal(body.APIKeyID, &selection.apiKeyID) != nil || selection.apiKeyID <= 0 {
			return selection, chatGPTWebBatchInvalid()
		}
	default:
		return selection, chatGPTWebBatchInvalid()
	}
	return selection, nil
}

// Match the database setter's OAuth shape, without runtime readiness gates.
func chatGPTWebBatchEligible(row *database.AccountRow) bool {
	return auth.ChatGPTWebOAuthAccountEligible(row)
}

func resolveChatGPTWebBatch(ctx context.Context, db *database.DB, selection chatGPTWebBatchSelection) ([]int64, *chatGPTWebBatchError) {
	if selection.scope == "accounts" {
		// Validate the entire explicit selection before changing any account.
		for _, id := range selection.accountIDs {
			row, err := db.GetAccountByID(ctx, id)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, &chatGPTWebBatchError{http.StatusNotFound, "account_not_found", "A selected account was not found"}
			}
			if err != nil {
				return nil, chatGPTWebBatchInternal()
			}
			if !chatGPTWebBatchEligible(row) {
				return nil, &chatGPTWebBatchError{http.StatusBadRequest, "account_ineligible", "A selected account is not an OpenAI OAuth account"}
			}
		}
		return selection.accountIDs, nil
	}

	var key *database.APIKeyRow
	if selection.scope == "group" {
		missing, err := db.VerifyAccountGroupIDs(ctx, []int64{selection.groupID})
		if err != nil {
			return nil, chatGPTWebBatchInternal()
		}
		if len(missing) != 0 {
			return nil, &chatGPTWebBatchError{http.StatusNotFound, "group_not_found", "Account group not found"}
		}
	} else {
		var err error
		key, err = db.GetAPIKeyByID(ctx, selection.apiKeyID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &chatGPTWebBatchError{http.StatusNotFound, "api_key_not_found", "API Key not found"}
		}
		if err != nil {
			return nil, chatGPTWebBatchInternal()
		}
		if key.Limits.ResolveUpstreamChannel() != database.UpstreamChannelChatGPTWeb {
			return nil, &chatGPTWebBatchError{http.StatusBadRequest, "api_key_ineligible", "API Key must use the ChatGPT Web channel"}
		}
	}

	rows, err := db.ListActiveByChannel(ctx, database.UpstreamChannelCodex)
	if err != nil {
		return nil, chatGPTWebBatchInternal()
	}
	memberships, err := db.ListAccountGroupMemberships(ctx)
	if err != nil {
		return nil, chatGPTWebBatchInternal()
	}
	return selectChatGPTWebBatchRows(selection, rows, memberships, key)
}

func selectChatGPTWebBatchRows(selection chatGPTWebBatchSelection, rows []*database.AccountRow, memberships map[int64][]int64, key *database.APIKeyRow) ([]int64, *chatGPTWebBatchError) {
	ids := make([]int64, 0)
	for _, row := range rows {
		if !chatGPTWebBatchEligible(row) {
			continue
		}
		groups := memberships[row.ID]
		if selection.scope == "group" {
			if !slices.Contains(groups, selection.groupID) {
				continue
			}
		} else {
			account := &auth.Account{DBID: row.ID, PlanType: row.GetCredential("plan_type"), GroupIDs: groups}
			account.SetAllowedAPIKeyIDs(row.GetCredentialInt64Slice("allowed_api_key_ids"))
			if !auth.APIKeyRowAllowsChatGPTWebAccount(key, account) {
				continue
			}
		}
		ids = append(ids, row.ID)
		if len(ids) > chatGPTWebBatchLimit {
			return nil, chatGPTWebBatchTooLarge()
		}
	}
	return ids, nil
}

type chatGPTWebBatchResult struct {
	AccountID int64                            `json:"account_id"`
	State     *database.ChatGPTWebAccountState `json:"state,omitempty"`
	Code      string                           `json:"code,omitempty"`
	Error     string                           `json:"error,omitempty"`
	Changed   bool                             `json:"changed"`
}

type chatGPTWebBatchResponse struct {
	Matched   int                     `json:"matched"`
	Updated   int                     `json:"updated"`
	Unchanged int                     `json:"unchanged"`
	Failed    int                     `json:"failed"`
	Results   []chatGPTWebBatchResult `json:"results"`
}

type chatGPTWebBatchStateStore interface {
	GetChatGPTWebAccountState(context.Context, int64) (*database.ChatGPTWebAccountState, error)
	SetChatGPTWebAccountEnabled(context.Context, int64, bool) (*database.ChatGPTWebAccountState, error)
}

func applyChatGPTWebBatch(ctx context.Context, db chatGPTWebBatchStateStore, ids []int64, enabled bool) chatGPTWebBatchResponse {
	out := chatGPTWebBatchResponse{Matched: len(ids), Results: make([]chatGPTWebBatchResult, 0, len(ids))}
	for _, id := range ids {
		result := chatGPTWebBatchResult{AccountID: id}
		state, err := db.GetChatGPTWebAccountState(ctx, id)
		missing := errors.Is(err, sql.ErrNoRows)
		if err == nil && state.Enabled == enabled {
			result.State = state
			out.Unchanged++
		} else {
			if err == nil || missing {
				state, err = db.SetChatGPTWebAccountEnabled(ctx, id, enabled)
			}
			if err != nil {
				result.Code, result.Error = "internal_error", "Unable to update Web account"
				if errors.Is(err, sql.ErrNoRows) {
					result.Code, result.Error = "account_not_found", "Account no longer available"
				}
				out.Failed++
			} else {
				result.State, result.Changed = state, true
				out.Updated++
			}
		}
		out.Results = append(out.Results, result)
	}
	return out
}

// BatchSetChatGPTWebAccounts only toggles persisted opt-in state. It never calls
// workers, refreshes tokens, or performs an environment/generation check.
func (h *Handler) BatchSetChatGPTWebAccounts(c *gin.Context) {
	selection, failure := decodeChatGPTWebBatch(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10))
	if failure != nil {
		failure.write(c)
		return
	}
	ids, failure := resolveChatGPTWebBatch(c.Request.Context(), h.db, selection)
	if failure != nil {
		failure.write(c)
		return
	}
	c.JSON(http.StatusOK, applyChatGPTWebBatch(c.Request.Context(), h.db, ids, selection.enabled))
}
