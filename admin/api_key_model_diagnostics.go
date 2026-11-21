package admin

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// GetAPIKeyModelDiagnostics checks only the local catalog using the current key policy.
func (h *Handler) GetAPIKeyModelDiagnostics(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid API key ID"})
		return
	}
	ctx := c.Request.Context()
	row, err := h.db.GetAPIKeyByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "API key configuration unavailable"})
		return
	}
	// The existing DB lookup also loads the key value. Only pass policy onward.
	row.Key = ""
	current := h.authCacheProxy.InspectModelCatalog(ctx, row)
	last, status := h.authCacheProxy.LastEmptyModelCatalog(ctx, id)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, struct {
		Current         proxy.ModelCatalogDiagnostic  `json:"current"`
		LastEmpty       *proxy.ModelCatalogDiagnostic `json:"last_empty,omitempty"`
		LastEmptyStatus string                        `json:"last_empty_status"`
	}{current, last, status})
}
