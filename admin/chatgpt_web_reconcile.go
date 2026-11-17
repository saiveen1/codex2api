package admin

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/codex2api/managedweb"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func (h *Handler) reconcileChatGPTWebExecution(c *gin.Context, id string, accountID int64) {
	if !proxy.ChatGPTWebValidExecutionID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid execution ID"})
		return
	}
	e, err := h.db.GetChatGPTWebExecutionForAdmin(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && accountID != 0 && e.AccountID != accountID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Execution not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Unable to load web execution"})
		return
	}
	var cfg *managedweb.Config
	if h.chatGPTWebManager != nil {
		value, runtimeErr := h.chatGPTWebManager.Runtime()
		if runtimeErr != nil {
			value = managedweb.Config{}
		}
		cfg = &value
	}
	worker := proxy.ObserveChatGPTWebExecution(c.Request.Context(), e, cfg)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"execution":                            gin.H{"id": e.ID, "api_key_id": e.APIKeyID, "account_id": e.AccountID, "stage": e.Stage, "error_code": e.ErrorCode, "recovery_ids_known": e.ConversationID != "" || e.TaskID != "" || len(e.AssetIDs) > 0, "result_unknown": e.ResultUnknown, "asset_count": len(e.AssetIDs), "created_at": e.CreatedAt, "updated_at": e.UpdatedAt},
		"worker":                               worker,
		"worker_completion_is_client_delivery": false,
	})
}
