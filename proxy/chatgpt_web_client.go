package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/managedweb"
)

const chatGPTWebProtocolVersion = "1"
const chatGPTWebMaxImageBytes = 20 << 20
const chatGPTWebMaxResponseBytes = 32 << 20

var errChatGPTWebUnavailable = errors.New("ChatGPT Web image provider is disabled or not configured")

type chatGPTWebConfig struct {
	enabled  bool
	endpoint string
	secret   string
	timeout  time.Duration
}

// SetChatGPTWebManager is wired once before serving requests. A failed manager
// initialization fails closed instead of reverting to stale environment settings.
func (h *Handler) SetChatGPTWebManager(manager *managedweb.Manager) {
	h.chatGPTWebRuntime = func() (chatGPTWebConfig, error) {
		if manager == nil {
			return chatGPTWebConfig{}, errChatGPTWebUnavailable
		}
		cfg, err := manager.Runtime()
		return chatGPTWebConfig{enabled: cfg.Enabled, endpoint: cfg.WorkerURL, secret: cfg.WorkerSecret, timeout: time.Duration(cfg.TimeoutSeconds) * time.Second}, err
	}
}

func (h *Handler) loadChatGPTWebConfig() (chatGPTWebConfig, error) {
	if h.chatGPTWebRuntime != nil {
		return h.chatGPTWebRuntime()
	}
	return loadChatGPTWebConfig()
}

func loadChatGPTWebConfig() (chatGPTWebConfig, error) {
	cfg := chatGPTWebConfig{timeout: 180 * time.Second}
	cfg.enabled = strings.EqualFold(strings.TrimSpace(os.Getenv("CHATGPT_WEB_ENABLED")), "true") || os.Getenv("CHATGPT_WEB_ENABLED") == "1"
	if !cfg.enabled {
		return cfg, errChatGPTWebUnavailable
	}
	u, err := url.Parse(strings.TrimSpace(os.Getenv("CHATGPT_WEB_WORKER_URL")))
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return cfg, errChatGPTWebUnavailable
	}
	cfg.secret = strings.TrimSpace(os.Getenv("CHATGPT_WEB_WORKER_SECRET"))
	if len(cfg.secret) < 16 || strings.ContainsAny(cfg.secret, "\r\n") {
		return cfg, errChatGPTWebUnavailable
	}
	cfg.endpoint = strings.TrimRight(u.String(), "/")
	if raw := os.Getenv("CHATGPT_WEB_TIMEOUT_SECONDS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 10 || n > 600 {
			return cfg, errChatGPTWebUnavailable
		}
		cfg.timeout = time.Duration(n) * time.Second
	}
	return cfg, nil
}

// ChatGPTWebDeploymentStatus deliberately excludes worker URLs and credentials.
func ChatGPTWebDeploymentStatus() map[string]any {
	cfg, err := loadChatGPTWebConfig()
	return map[string]any{"enabled": cfg.enabled, "configured": err == nil, "protocol_version": chatGPTWebProtocolVersion, "supported_models": []string{"gpt-image-2"}}
}

func chatGPTWebHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// The worker is an administrative endpoint, not a user-selected upstream. No
// environment proxy, public HTTP destination, redirect or TLS bypass is allowed.
func newChatGPTWebHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid worker address")
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("worker address unavailable")
		}
		for _, ip := range ips {
			if !(ip.IsLoopback() || ip.IsPrivate()) || ip.IsLinkLocalUnicast() {
				return nil, errors.New("worker must use a private address")
			}
		}
		d := net.Dialer{Timeout: 5 * time.Second}
		for _, ip := range ips {
			conn, e := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
			if e == nil {
				return conn, nil
			}
			err = e
		}
		return nil, errors.New("worker connection failed")
	}
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type chatGPTWebEnvironment struct {
	DeviceID  string `json:"device_id"`
	SessionID string `json:"session_id"`
}
type chatGPTWebAccountRequest struct {
	AccountID           int64                 `json:"account_id"`
	AccessToken         string                `json:"access_token"`
	ChatGPTAccountID    string                `json:"chatgpt_account_id"`
	CredentialRevision  string                `json:"credential_revision"`
	EnvironmentRevision int64                 `json:"environment_revision"`
	ProxyURL            string                `json:"proxy_url"`
	Environment         chatGPTWebEnvironment `json:"environment"`
	TimeoutSeconds      int                   `json:"timeout_seconds"`
}
type chatGPTWebImage struct {
	B64JSON  string `json:"b64_json"`
	MIMEType string `json:"mime_type"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	AssetID  string `json:"asset_id,omitempty"`
}
type chatGPTWebRequest struct {
	chatGPTWebAccountRequest
	ExecutionID  string            `json:"execution_id"`
	Operation    string            `json:"operation"`
	Model        string            `json:"model"`
	Prompt       string            `json:"prompt"`
	Images       []chatGPTWebImage `json:"images,omitempty"`
	N            int               `json:"n"`
	Size         string            `json:"size"`
	Quality      string            `json:"quality"`
	OutputFormat string            `json:"output_format"`
}
type chatGPTWebEvent struct {
	Type           string           `json:"type"`
	Stage          string           `json:"stage"`
	ConversationID string           `json:"conversation_id"`
	TaskID         string           `json:"task_id"`
	AssetIDs       []string         `json:"asset_ids"`
	Image          *chatGPTWebImage `json:"image"`
	Code           string           `json:"code"`
	HTTPStatus     int              `json:"http_status"`
	ResultUnknown  bool             `json:"result_unknown"`
	UsageKnown     bool             `json:"usage_known"`
}
type chatGPTWebFailure struct {
	code    string
	status  int
	unknown bool
}

func (e *chatGPTWebFailure) Error() string { return chatGPTWebErrorMessage(e.code) }
func chatGPTWebErrorMessage(code string) string {
	switch code {
	case "auth_required":
		return "ChatGPT Web authentication requires verification"
	case "environment_challenge":
		return "ChatGPT Web environment requires unsupported verification; no browser fallback was attempted"
	case "quota_exhausted":
		return "ChatGPT Web image quota is exhausted"
	case "unsupported_model":
		return "ChatGPT Web currently supports only gpt-image-2"
	case "unsupported_parameter":
		return "Requested image options cannot be preserved by the ChatGPT Web provider"
	case "execution_conflict":
		return "This image execution already exists; inspect its status instead of generating again"
	case "image_download_failed":
		return "The image may have been generated but download failed; do not regenerate automatically"
	case "content_policy_violation":
		return "The upstream refused this image request"
	case "upstream_text_reply":
		return "The upstream returned text instead of an image"
	case "submission_unknown", "image_stream_interrupted", "image_poll_timeout":
		return "Image submission outcome is unknown; do not submit the same generation again"
	case "invalid_request":
		return "Invalid ChatGPT Web image request"
	default:
		return "ChatGPT Web image execution failed; inspect the execution status"
	}
}
func chatGPTWebSafeCode(code string) string {
	switch code {
	case "auth_required", "environment_challenge", "quota_exhausted", "unsupported_model", "unsupported_parameter", "submission_unknown", "image_stream_interrupted", "image_poll_timeout", "image_download_failed", "content_policy_violation", "upstream_text_reply", "execution_conflict", "invalid_request":
		return code
	}
	return "upstream_error"
}
func chatGPTWebFailureFromEvent(e chatGPTWebEvent) *chatGPTWebFailure {
	code := chatGPTWebSafeCode(e.Code)
	status := e.HTTPStatus
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	return &chatGPTWebFailure{code, status, e.ResultUnknown || code == "submission_unknown"}
}

func chatGPTWebPost(ctx context.Context, client *http.Client, cfg chatGPTWebConfig, path string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("cannot encode worker request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid worker request")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("worker connection failed")
	}
	return resp, nil
}

// CheckChatGPTWebAccount observes the web channel only. Credential refresh is
// owned by the existing account lifecycle, never by the protocol service.
func (h *Handler) CheckChatGPTWebAccount(ctx context.Context, accountID int64) (*database.ChatGPTWebAccountState, error) {
	cfg, err := h.loadChatGPTWebConfig()
	if err != nil {
		return nil, err
	}
	if h.db == nil || h.store == nil {
		return nil, errors.New("account state is unavailable")
	}
	state, err := h.db.GetChatGPTWebAccountState(ctx, accountID)
	if err != nil || !state.Enabled {
		return nil, errors.New("enable this account for ChatGPT Web before checking")
	}
	account := h.store.FindByID(accountID)
	if account == nil || account.IsRelayStyle() {
		return nil, errors.New("account is not eligible for ChatGPT Web")
	}
	token, workspace := account.ChatGPTWebCredentials()
	if token == "" {
		return nil, errors.New("account has no access token")
	}
	proxyURL, usable := h.store.ResolveChatGPTWebProxy(account)
	if !usable {
		return nil, errors.New("ChatGPT Web account has no usable egress")
	}
	owner := "check-" + NewUpstreamSessionUUID()
	lease, err := h.db.AcquireChatGPTWebAccountLease(ctx, accountID, owner, time.Now().Add(40*time.Second))
	if err != nil || !lease {
		return nil, errors.New("account web environment is busy")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.db.ReleaseChatGPTWebAccountLease(cleanup, accountID, owner)
	}()
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request := chatGPTWebAccountRequest{AccountID: accountID, AccessToken: token, ChatGPTAccountID: workspace, CredentialRevision: chatGPTWebHash(token), EnvironmentRevision: state.EnvironmentRevision, ProxyURL: proxyURL, Environment: chatGPTWebEnvironment{state.DeviceID, state.SessionID}, TimeoutSeconds: 30}
	client := newChatGPTWebHTTPClient()
	defer client.CloseIdleConnections()
	resp, err := chatGPTWebPost(checkCtx, client, cfg, "/v1/accounts/check", request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("web environment check failed")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(data) > 65536 {
		return nil, errors.New("invalid web environment observation")
	}
	var observation struct {
		Status              string     `json:"status"`
		Capabilities        []string   `json:"capabilities"`
		ImageQuotaRemaining *int       `json:"image_quota_remaining"`
		QuotaResetAt        *time.Time `json:"quota_reset_at"`
		ErrorCode           string     `json:"error_code"`
	}
	if json.Unmarshal(data, &observation) != nil {
		return nil, errors.New("invalid web environment observation")
	}
	switch observation.Status {
	case "ready", "unverified", "auth_required", "challenge", "quota_exhausted":
	default:
		return nil, errors.New("invalid web environment state")
	}
	caps := []string{}
	for _, capability := range observation.Capabilities {
		if capability == "generate" || capability == "edit" {
			caps = append(caps, capability)
		}
	}
	if observation.Status == "ready" && len(caps) == 0 {
		observation.Status = "unverified"
	}
	if observation.ImageQuotaRemaining != nil && *observation.ImageQuotaRemaining < 0 {
		return nil, errors.New("invalid web quota observation")
	}
	now := time.Now().UTC()
	state.Status = observation.Status
	state.Capabilities = caps
	state.ImageQuotaRemaining = observation.ImageQuotaRemaining
	state.QuotaResetAt = observation.QuotaResetAt
	state.ObservedAt = &now
	state.CredentialRevision = request.CredentialRevision
	state.WorkspaceFingerprint = chatGPTWebHash(workspace)
	state.ProxyFingerprint = chatGPTWebHash(proxyURL)
	state.ErrorCode = ""
	if observation.ErrorCode != "" {
		state.ErrorCode = chatGPTWebSafeCode(observation.ErrorCode)
	}
	saved, err := h.db.SaveChatGPTWebAccountObservation(ctx, state, token)
	if err != nil || !saved {
		return nil, fmt.Errorf("web observation changed concurrently; check again")
	}
	return state, nil
}
