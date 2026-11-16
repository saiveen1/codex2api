package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/codex2api/managedweb"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

type chatGPTWebManager interface {
	View() managedweb.View
	Runtime() (managedweb.Config, error)
	Configure(context.Context, managedweb.Config) error
	Close() error
}

func (h *Handler) StartChatGPTWeb(ctx context.Context, directory string, images *proxy.Handler) error {
	manager, err := managedweb.New(directory)
	images.SetChatGPTWebManager(manager)
	if err != nil {
		return err
	}
	h.chatGPTWebManager = manager
	return manager.Ensure(ctx)
}

func (h *Handler) CloseChatGPTWeb() error {
	if h.chatGPTWebManager == nil {
		return nil
	}
	return h.chatGPTWebManager.Close()
}

func (h *Handler) ConfigureChatGPTWeb(c *gin.Context) {
	if h.chatGPTWebManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Web service management is unavailable; check the persistent data directory"})
		return
	}
	var body struct {
		Enabled        *bool  `json:"enabled"`
		Mode           string `json:"mode"`
		WorkerURL      string `json:"worker_url"`
		WorkerSecret   string `json:"worker_secret"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || body.Enabled == nil || dec.Decode(&struct{}{}) != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Expected enabled, mode, worker_url, worker_secret and timeout_seconds only"})
		return
	}
	cfg := managedweb.Config{Enabled: *body.Enabled, Mode: body.Mode, WorkerURL: body.WorkerURL, WorkerSecret: body.WorkerSecret, TimeoutSeconds: body.TimeoutSeconds}
	if err := h.chatGPTWebManager.Configure(c.Request.Context(), cfg); err != nil {
		// Manager errors are fixed safe messages, never native process output or credentials.
		status := http.StatusBadRequest
		if errors.Is(err, managedweb.ErrConflict) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	h.GetChatGPTWebStatus(c)
}
