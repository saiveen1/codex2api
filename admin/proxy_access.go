package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// UpdateProxyAccess replaces access policy. Wire PATCH /proxies/:id/access on the authenticated admin group.
// Both fields are required so an omitted/null allowlist cannot accidentally erase a restriction.
func (h *Handler) UpdateProxyAccess(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的代理 ID")
		return
	}
	var req struct {
		Exclusive         *bool    `json:"exclusive"`
		AllowedAccountIDs *[]int64 `json:"allowed_account_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Exclusive == nil || req.AllowedAccountIDs == nil {
		writeError(c, http.StatusBadRequest, "exclusive 和 allowed_account_ids 必须同时提供")
		return
	}
	if h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "代理运行时不可用")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	row, err := h.store.UpdateProxyAccess(ctx, id, *req.Exclusive, *req.AllowedAccountIDs)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeError(c, http.StatusNotFound, "代理不存在")
		case errors.Is(err, database.ErrProxyAccessInvalid):
			writeError(c, http.StatusBadRequest, err.Error())
		case errors.Is(err, database.ErrProxyAccessConflict):
			writeError(c, http.StatusConflict, "代理仍被全局代理配置引用，或已绑定白名单外账号；请先移除这些引用")
		default:
			writeInternalError(c, err)
		}
		return
	}
	security.SecurityAuditLog("PROXY_ACCESS_UPDATE", fmt.Sprintf("proxy_id=%d exclusive=%t allowed_count=%d ip=%s", id, row.Exclusive, len(row.AllowedAccountIDs), c.ClientIP()))
	if err := h.reloadProxyPool(); err != nil {
		// The policy is already published: do not roll back or reopen the node on reload failure.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "访问限制已保存并生效，但代理池刷新失败", "saved": true})
		return
	}
	c.JSON(http.StatusOK, row)
}
