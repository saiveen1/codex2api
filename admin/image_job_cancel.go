package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func (h *Handler) CancelImageGenerationJob(c *gin.Context) {
	h.cancelImageJob(c, nil)
}

func (h *Handler) CancelPortalImageJob(c *gin.Context) {
	key := portalAPIKeyFromContext(c)
	if key == nil || key.ID <= 0 {
		writeError(c, http.StatusUnauthorized, "缺少或无效的 API Key")
		return
	}
	h.cancelImageJob(c, &key.ID)
}

func (h *Handler) cancelImageJob(c *gin.Context, keyID *int64) {
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	inputs, err := h.db.CancelImageJob(c.Request.Context(), id, keyID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(c, http.StatusNotFound, "任务不存在")
	case errors.Is(err, database.ErrImageJobFinished):
		writeError(c, http.StatusConflict, "任务已结束")
	case err != nil:
		writeInternalError(c, err)
	default:
		if cancel, ok := h.imageJobCancels.Load(id); ok {
			cancel.(context.CancelFunc)()
		}
		if h.imageQueue != nil {
			cleanupQueueInputs(inputs)
		}
		c.JSON(http.StatusOK, gin.H{"status": database.ImageJobCancelled})
	}
}

// Register before checking/claiming the durable state. Cancellation before
// registration is caught by that check; cancellation after it signals ctx.
func (h *Handler) trackImageJob(parent context.Context, id int64, owner string) (context.Context, context.CancelFunc) {
	ctx, stop := context.WithCancel(parent)
	h.imageJobCancels.Store(id, stop)
	ctx = proxy.WithImageJobAttemptCheck(ctx, func(attempt context.Context) error {
		err := h.db.CheckRunningImageJob(attempt, id, owner)
		if err != nil {
			stop()
		}
		return err
	})
	return ctx, func() { h.imageJobCancels.Delete(id); stop() }
}
