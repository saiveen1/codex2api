package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func (h *Handler) GetChatGPTWebStatus(c *gin.Context) {
	c.JSON(http.StatusOK, proxy.ChatGPTWebDeploymentStatus())
}

func (h *Handler) ListChatGPTWebAccounts(c *gin.Context) {
	states, err := h.db.ListChatGPTWebAccountStates(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": "Unable to load web account states"})
		return
	}
	if states == nil {
		c.JSON(200, gin.H{"accounts": []any{}})
		return
	}
	c.JSON(200, gin.H{"accounts": states})
}

func chatGPTWebID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(400, gin.H{"error": "Invalid account ID"})
		return 0, false
	}
	return id, true
}

func (h *Handler) SetChatGPTWebAccount(c *gin.Context) {
	id, ok := chatGPTWebID(c)
	if !ok {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	dec := json.NewDecoder(io.LimitReader(c.Request.Body, 4097))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || body.Enabled == nil || dec.Decode(&struct{}{}) != io.EOF {
		c.JSON(400, gin.H{"error": "Expected enabled boolean only"})
		return
	}
	state, err := h.db.SetChatGPTWebAccountEnabled(c.Request.Context(), id, *body.Enabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(404, gin.H{"error": "Account not found"})
		} else {
			c.JSON(400, gin.H{"error": "Unable to update web account"})
		}
		return
	}
	c.JSON(200, state)
}

func (h *Handler) CheckChatGPTWebAccount(c *gin.Context) {
	id, ok := chatGPTWebID(c)
	if !ok {
		return
	}
	if h.imageProxy == nil {
		c.JSON(503, gin.H{"error": "Web environment check unavailable"})
		return
	}
	state, err := h.imageProxy.CheckChatGPTWebAccount(c.Request.Context(), id)
	if err != nil {
		c.JSON(502, gin.H{"error": "Web environment check failed"})
		return
	}
	c.JSON(200, state)
}

func (h *Handler) ListChatGPTWebExecutions(c *gin.Context) {
	var id int64
	if raw := c.Query("account_id"); raw != "" {
		var err error
		id, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			c.JSON(400, gin.H{"error": "Invalid account ID"})
			return
		}
	}
	rows, err := h.db.ListChatGPTWebExecutions(c.Request.Context(), id, 50)
	if err != nil {
		c.JSON(500, gin.H{"error": "Unable to load web executions"})
		return
	}
	if rows == nil {
		c.JSON(200, gin.H{"executions": []any{}})
		return
	}
	c.JSON(200, gin.H{"executions": rows})
}
