package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	_ "golang.org/x/image/webp"
)

func isChatGPTWebRequest(c *gin.Context) bool {
	return requestUpstreamChannel(c) == database.UpstreamChannelChatGPTWeb
}

func rejectChatGPTWebEndpoint(c *gin.Context) bool {
	if !isChatGPTWebRequest(c) {
		return false
	}
	path := c.Request.URL.Path
	chat := c.Request.Method == http.MethodPost && (path == "/v1/chat/completions" || path == "/chat/completions")
	if row := apiKeyRowFromContext(c); chat && row != nil && row.Limits.ExternalScheduler {
		writeOpenAICodedError(c, http.StatusBadRequest, "unsupported_parameter", "ChatGPT Web does not support external scheduler account overrides")
		c.Abort()
		return true
	}
	allowed := chat || strings.HasPrefix(path, "/v1/images/") || strings.HasPrefix(path, "/images/") || strings.HasPrefix(path, "/api/image-studio/") || path == "/v1/models" || path == "/models"
	if allowed && !strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
		return false
	}
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "code": "unsupported_channel", "message": "ChatGPT Web keys support only text Chat Completions, Images and image job endpoints"}})
	return true
}

type chatGPTWebImageOptions struct {
	Model, Size, Quality, OutputFormat string
	N                                  int
}

func parseChatGPTWebImageOptions(raw []byte, model, mask string) (chatGPTWebImageOptions, error) {
	o := chatGPTWebImageOptions{Model: model, Size: "auto", Quality: "auto", OutputFormat: "png", N: 1}
	invalid := func() (chatGPTWebImageOptions, error) {
		return o, &chatGPTWebFailure{"unsupported_parameter", 400, false}
	}
	if !isChatGPTWebImageModel(model) {
		return o, &chatGPTWebFailure{"unsupported_model", 400, false}
	}
	if mask != "" {
		return invalid()
	}
	for _, field := range []string{"mask", "background", "input_fidelity", "moderation", "output_compression", "partial_images"} {
		v := gjson.GetBytes(raw, field)
		if v.Exists() && v.Type != gjson.Null {
			return invalid()
		}
	}
	if v := gjson.GetBytes(raw, "n"); v.Exists() {
		if v.Type != gjson.Number || v.Float() != float64(v.Int()) || v.Int() < 1 || v.Int() > 4 {
			return o, &chatGPTWebFailure{"invalid_request", 400, false}
		}
		o.N = int(v.Int())
	}
	for field, dest := range map[string]*string{"size": &o.Size, "quality": &o.Quality, "output_format": &o.OutputFormat} {
		if v := gjson.GetBytes(raw, field); v.Exists() {
			if v.Type != gjson.String {
				return invalid()
			}
			if s := strings.TrimSpace(v.String()); s != "" {
				*dest = s
			}
		}
	}
	switch o.Size {
	case "auto", "1:1", "3:2", "2:3", "3:4", "4:3", "16:9", "9:16":
	default:
		return invalid()
	}
	// Web UI quality and exact pixels are not equivalent to Images API knobs.
	if o.Quality != "auto" {
		return invalid()
	}
	switch o.OutputFormat {
	case "png", "jpeg", "webp":
	default:
		return invalid()
	}
	return o, nil
}
func chatGPTWebFormOptions(c *gin.Context) []byte {
	values := map[string]any{}
	for _, key := range []string{"size", "quality", "output_format", "background", "input_fidelity", "moderation", "output_compression", "partial_images"} {
		if value, ok := c.GetPostForm(key); ok {
			values[key] = value
		}
	}
	if value, ok := c.GetPostForm("n"); ok {
		var n json.Number
		if json.Unmarshal([]byte(value), &n) == nil {
			values["n"] = n
		} else {
			values["n"] = value
		}
	}
	b, _ := json.Marshal(values)
	return b
}

func chatGPTWebImageFromBytes(data []byte) (chatGPTWebImage, error) {
	if len(data) == 0 || len(data) > chatGPTWebMaxImageBytes {
		return chatGPTWebImage{}, errors.New("image must be between 1 byte and 20 MiB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
		return chatGPTWebImage{}, errors.New("invalid or oversized image")
	}
	if format != "png" && format != "jpeg" && format != "webp" {
		return chatGPTWebImage{}, errors.New("only PNG, JPEG and WebP images are supported")
	}
	return chatGPTWebImage{B64JSON: base64.StdEncoding.EncodeToString(data), MIMEType: "image/" + format, Width: cfg.Width, Height: cfg.Height}, nil
}
func chatGPTWebDecodeImage(value string) (chatGPTWebImage, error) {
	if len(value) > ((chatGPTWebMaxImageBytes+2)/3)*4+128 {
		return chatGPTWebImage{}, errors.New("image is too large")
	}
	declared := ""
	if strings.HasPrefix(value, "data:") {
		parts := strings.SplitN(value, ",", 2)
		if len(parts) != 2 || !strings.HasSuffix(parts[0], ";base64") {
			return chatGPTWebImage{}, errors.New("invalid image data URL")
		}
		declared = strings.TrimSuffix(strings.TrimPrefix(parts[0], "data:"), ";base64")
		value = parts[1]
	}
	data, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return chatGPTWebImage{}, errors.New("invalid base64 image")
	}
	im, err := chatGPTWebImageFromBytes(data)
	if err != nil {
		return im, err
	}
	if declared != "" && declared != "application/octet-stream" && declared != im.MIMEType {
		return chatGPTWebImage{}, errors.New("image MIME type does not match content")
	}
	return im, nil
}
func chatGPTWebPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if v := ip.To4(); v != nil {
		return !(v[0] == 0 || v[0] >= 224 || (v[0] == 100 && v[1] >= 64 && v[1] <= 127) || (v[0] == 192 && v[1] == 0) || (v[0] == 198 && (v[1] == 18 || v[1] == 19)) || (v[0] == 198 && v[1] == 51 && v[2] == 100) || (v[0] == 203 && v[1] == 0 && v[2] == 113))
	}
	// Keep IPv6 public inputs in global-unicast space; reject documentation and
	// NAT64/transition space rather than opening a private-network bypass.
	return ip[0]&0xe0 == 0x20 && !(ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8) && !(ip[0] == 0x20 && ip[1] == 0x02)
}
func chatGPTWebFetchImage(ctx context.Context, raw string) (chatGPTWebImage, error) {
	if strings.HasPrefix(raw, "data:") {
		return chatGPTWebDecodeImage(raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return chatGPTWebImage{}, errors.New("invalid image URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid image address")
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("image address unavailable")
		}
		for _, ip := range ips {
			if !chatGPTWebPublicIP(ip) {
				return nil, errors.New("image URL must resolve to a public address")
			}
		}
		d := net.Dialer{Timeout: 5 * time.Second}
		for _, ip := range ips {
			conn, e := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
			if e == nil {
				return conn, nil
			}
		}
		return nil, errors.New("image connection failed")
	}
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return chatGPTWebImage{}, errors.New("invalid image URL")
	}
	req.Header.Set("Accept", "image/png,image/jpeg,image/webp")
	resp, err := client.Do(req)
	if err != nil {
		return chatGPTWebImage{}, errors.New("image URL could not be fetched safely")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > chatGPTWebMaxImageBytes {
		return chatGPTWebImage{}, errors.New("image URL returned an unsupported response")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, chatGPTWebMaxImageBytes+1))
	if err != nil {
		return chatGPTWebImage{}, errors.New("image download failed")
	}
	return chatGPTWebImageFromBytes(data)
}
