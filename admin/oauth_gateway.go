package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/oauthgateway/service"
	"github.com/codex2api/internal/oauthgateway/upstream"
	"github.com/gin-gonic/gin"
)

type oauthGatewayRuntime struct {
	app     *service.App
	slots   chan struct{}
	stopped atomic.Bool
}

// StartOAuthGateway is optional: missing encryption or a lease owned by another
// replica disables this module, never the existing request-serving application.
func (h *Handler) StartOAuthGateway(ctx context.Context) {
	h.oauthGatewayOnce.Do(func() {
		if h.db == nil {
			h.oauthGatewayInitErr = database.ErrOAuthGatewayState
			return
		}
		openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		store, err := h.db.OpenOAuthGatewayStore(openCtx)
		if err != nil {
			h.oauthGatewayInitErr = err
			log.Println("OAuth gateway unavailable: configure encryption and check task-store ownership")
			return
		}
		app, err := service.New(store, oauthGatewayTarget{h}, false)
		if err != nil {
			_ = store.Close()
			h.oauthGatewayInitErr = err
			log.Println("OAuth gateway unavailable: encrypted task state requires operator review")
			return
		}
		runtime := &oauthGatewayRuntime{app: app, slots: make(chan struct{}, 8)}
		h.oauthGateway = runtime
		if h.store != nil {
			h.store.SetOAuthRecoveryHooks(&auth.OAuthRecoveryHooks{Enabled: app.RecoveryEnabled, Signal: app.RequestRecovery})
		}
		h.oauthGatewayWG.Add(1)
		go func() {
			defer h.oauthGatewayWG.Done()
			defer app.Close()
			defer runtime.stopped.Store(true)
			if err := app.Run(ctx); err != nil {
				log.Println("OAuth gateway worker stopped: reconcile durable state before restarting; no automatic resubmission")
			}
		}()
	})
}

func (h *Handler) WaitOAuthGateway() { h.oauthGatewayWG.Wait() }

func oauthGatewayFault(status int, code, message string) error {
	return &service.Fault{Status: status, Code: code, Message: message}
}

func oauthGatewayRespond(c *gin.Context, data any, err error) {
	if err == nil {
		c.JSON(http.StatusOK, gin.H{"ok": true, "data": data})
		return
	}
	oauthGatewayError(c, err)
}

func oauthGatewayError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	body := upstream.Error{Code: "gateway_unavailable", Message: "操作未完成，请核对配置和原任务；不要重新提交未知结果。", Source: "bridge", Action: "manual_review"}
	var f *service.Fault
	var remote *upstream.Error
	switch {
	case errors.As(err, &f):
		status, body.Code, body.Message, body.Action = f.Status, f.Code, f.Message, "fix_request"
	case errors.As(err, &remote):
		// Gateway CDK rejection must not log the administrator out of codex2api.
		status, body = http.StatusBadGateway, *remote
	case errors.Is(err, database.ErrOAuthGatewayEncryption):
		status, body.Code, body.Message, body.Action = 503, "encryption_required", "请配置并保管 CODEX_CRED_ENCRYPTION_KEY 后重启，以启用加密 OAuth 任务。", "fix_configuration"
	case errors.Is(err, database.ErrOAuthGatewayOwner):
		status, body.Code, body.Message = 503, "state_owned_elsewhere", "OAuth 任务由其他实例持有，请连接持有者实例；不得并行重放。"
	case errors.Is(err, database.ErrOAuthGatewayState):
		status, body.Message = 503, "OAuth 加密任务状态不可用，请核对密钥、数据库和原任务后重启。"
	}
	c.JSON(status, gin.H{"ok": false, "error": body})
}

func decodeOAuthGatewayJSON(c *gin.Context, dst any) bool {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		oauthGatewayError(c, oauthGatewayFault(415, "json_required", "请使用 application/json 请求体。"))
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	if err != nil {
		oauthGatewayError(c, oauthGatewayFault(413, "request_too_large", "请求体超过 1 MiB 或未完整读取。"))
		return false
	}
	raw = bytes.TrimSpace(raw)
	if !utf8.Valid(raw) || len(raw) == 0 || raw[0] != '{' {
		oauthGatewayError(c, oauthGatewayFault(400, "invalid_json", "请求必须是有效 UTF-8 JSON 对象。"))
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(dst) == nil {
		var extra any
		if decoder.Decode(&extra) == io.EOF {
			return true
		}
	}
	oauthGatewayError(c, oauthGatewayFault(400, "invalid_json", "请求包含无效内容、未知字段或多个 JSON 值。"))
	return false
}

func oauthGatewaySameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host == r.Host && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

func (h *Handler) handleOAuthGateway(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	if !oauthGatewaySameOrigin(c.Request) {
		oauthGatewayError(c, oauthGatewayFault(403, "forbidden_origin", "禁止跨站 OAuth 管理请求。"))
		return
	}
	runtime := h.oauthGateway
	if runtime == nil {
		err := h.oauthGatewayInitErr
		if err == nil {
			err = database.ErrOAuthGatewayState
		}
		oauthGatewayError(c, err)
		return
	}
	if runtime.stopped.Load() {
		oauthGatewayError(c, database.ErrOAuthGatewayState)
		return
	}
	select {
	case runtime.slots <- struct{}{}:
		defer func() { <-runtime.slots }()
	default:
		oauthGatewayError(c, oauthGatewayFault(429, "capacity_exceeded", "OAuth 管理请求过多，请稍后查询原任务。"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	app := runtime.app
	path, method := strings.Trim(c.Param("path"), "/"), c.Request.Method
	var data any
	var err error
	switch {
	case method == "GET" && path == "settings":
		data, err = app.Settings()
	case method == "PUT" && path == "settings":
		var input service.Settings
		if !decodeOAuthGatewayJSON(c, &input) {
			return
		}
		data, err = app.SaveSettings(input)
	case method == "GET" && path == "capabilities":
		data, err = app.Capabilities(ctx)
	case method == "GET" && path == "quota":
		data, err = app.Quota(ctx)
	case method == "GET" && path == "groups":
		data, err = app.Groups(ctx)
	case method == "GET" && path == "recovery":
		data, err = app.Recoveries()
	case method == "POST" && path == "connections/test":
		var empty struct{}
		if !decodeOAuthGatewayJSON(c, &empty) {
			return
		}
		data, err = app.Connections(ctx)
	case method == "POST" && path == "proxy-check":
		var input struct {
			Options upstream.Options `json:"options"`
		}
		if !decodeOAuthGatewayJSON(c, &input) {
			return
		}
		data, err = app.CheckProxy(ctx, input.Options)
	case method == "POST" && (path == "preview" || path == "jobs"):
		var input service.Input
		if !decodeOAuthGatewayJSON(c, &input) {
			return
		}
		var accountProxies []string
		for _, account := range input.Accounts {
			if account.RuntimeProxy != "" {
				accountProxies = append(accountProxies, account.RuntimeProxy)
			}
		}
		if input.RecoveryAccountID > 0 {
			if len(input.GroupIDs) > 0 || input.RuntimeProxy != "" || len(accountProxies) > 0 {
				err = oauthGatewayFault(400, "invalid_recovery_destination", "修复原账号不能更改分组或运行代理。")
				break
			}
		} else if err = h.validateOAuthGatewayDestination(ctx, input.GroupIDs, input.RuntimeProxy, accountProxies...); err != nil {
			break
		}
		if path == "preview" {
			data, err = app.Preview(ctx, input)
			break
		}
		var existing bool
		data, existing, err = app.Create(input)
		if err == nil && !existing {
			c.JSON(http.StatusAccepted, gin.H{"ok": true, "data": data})
			return
		}
	case method == "GET" && path == "jobs":
		if c.Query("archived") == "1" {
			data, err = app.ArchivedJobs()
		} else {
			data, err = app.Jobs()
		}
	default:
		parts := strings.Split(path, "/")
		if len(parts) == 3 && parts[0] == "recovery" && parts[2] == "disable" && method == "POST" {
			var empty struct{}
			if !decodeOAuthGatewayJSON(c, &empty) {
				return
			}
			id, parseErr := strconv.ParseInt(parts[1], 10, 64)
			if parseErr != nil || id <= 0 {
				err = oauthGatewayFault(400, "invalid_recovery_account", "恢复账号 ID 必须为正整数。")
			} else {
				err = app.DisableRecovery(id)
				data = struct{}{}
			}
		} else if len(parts) == 2 && parts[0] == "jobs" && method == "GET" {
			data, err = app.Job(parts[1])
		} else if len(parts) == 3 && parts[0] == "jobs" && method == "POST" {
			if parts[2] == "archive" {
				var input struct {
					DiscardCredentials bool `json:"discard_credentials"`
				}
				if !decodeOAuthGatewayJSON(c, &input) {
					return
				}
				data, err = app.Archive(parts[1], input.DiscardCredentials)
			} else {
				var empty struct{}
				if !decodeOAuthGatewayJSON(c, &empty) {
					return
				}
				data, err = app.Action(parts[1], parts[2])
			}
		} else {
			err = oauthGatewayFault(404, "not_found", "接口不存在。")
		}
	}
	oauthGatewayRespond(c, data, err)
}
