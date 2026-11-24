package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"unicode"

	"github.com/codex2api/internal/openaiidentity"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type Identity struct {
	ChatGPTUserID    string `json:"chatgpt_user_id"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
}
type Account struct {
	Identifier       string    `json:"identifier"`
	Password         string    `json:"password"`
	TOTPSecret       string    `json:"totp_secret"`
	ExpectedIdentity *Identity `json:"expected_identity,omitempty"`
}
type Options struct {
	Country         string          `json:"country,omitempty"`
	Environment     string          `json:"environment,omitempty"`
	ProxyMode       string          `json:"proxy_mode,omitempty"`
	Proxy           json.RawMessage `json:"proxy,omitempty"`
	UserAgent       string          `json:"user_agent,omitempty"`
	Language        string          `json:"language,omitempty"`
	Timezone        string          `json:"timezone,omitempty"`
	WorkspacePolicy string          `json:"workspace_policy,omitempty"`
}
type SubmitRequest struct {
	RequestID  string    `json:"request_id,omitempty"`
	Accounts   []Account `json:"accounts,omitempty"`
	SourceText string    `json:"source_text,omitempty"`
	Options    Options   `json:"options"`
}
type Capabilities struct {
	Protocol struct {
		Name     string `json:"name"`
		Version  int    `json:"version"`
		Revision int    `json:"revision"`
	} `json:"protocol"`
	OAuthMethods      []string        `json:"oauth_methods"`
	Features          map[string]bool `json:"features"`
	Countries         []string        `json:"countries"`
	Environments      []string        `json:"environments"`
	ProxyModes        []string        `json:"proxy_modes"`
	WorkspacePolicies []string        `json:"workspace_policies"`
	MaxBatchSize      int             `json:"max_batch_size"`
	MaxUploadBytes    int             `json:"max_upload_bytes"`
}
type Quota struct {
	Total     int64    `json:"total"`
	Remaining int64    `json:"remaining"`
	Reserved  int64    `json:"reserved"`
	Used      int64    `json:"used"`
	Disabled  bool     `json:"disabled"`
	ExpiresAt *float64 `json:"expires_at"`
}
type PreviewRow struct {
	Line             int    `json:"line"`
	IdentifierMasked string `json:"identifier_masked"`
}
type PreviewIssue struct {
	Line int    `json:"line"`
	Code string `json:"code"`
}
type Preview struct {
	Valid  bool           `json:"valid"`
	Count  int            `json:"count"`
	Rows   []PreviewRow   `json:"rows"`
	Errors []PreviewIssue `json:"errors"`
}
type ProxyCheck struct {
	Usable           bool    `json:"usable"`
	RequestedCountry string  `json:"requested_country"`
	ObservedCountry  *string `json:"observed_country"`
	OpenAIReachable  bool    `json:"openai_reachable"`
	OpenAIHTTPStatus *int    `json:"openai_http_status"`
	TraceHTTPStatus  *int    `json:"trace_http_status"`
	CheckedAt        float64 `json:"checked_at"`
	ElapsedMS        float64 `json:"elapsed_ms"`
	Error            *Error  `json:"error"`
}
type BatchSuccessSummary struct {
	WorkspaceID   string `json:"workspace_id,omitempty"`
	WorkspaceName string `json:"workspace_name,omitempty"`
}

func (s *BatchSuccessSummary) UnmarshalJSON(data []byte) error {
	// Optional display metadata must not invalidate an otherwise valid batch.
	s.WorkspaceName = ""
	s.WorkspaceID = ""
	if field := gjson.GetBytes(data, "workspace_id"); field.Type == gjson.String {
		if id := openaiidentity.NormalizeWorkspaceID(field.Str); openaiidentity.ValidWebWorkspaceID(id) {
			s.WorkspaceID = id
		}
	}
	if field := gjson.GetBytes(data, "workspace_name"); field.Type == gjson.String {
		s.WorkspaceName = NormalizeWorkspaceName(field.Str)
	}
	return nil
}

func NormalizeWorkspaceName(value string) string {
	name := strings.TrimSpace(value)
	if len(name) > 512 || strings.ContainsFunc(name, unicode.IsControl) {
		return ""
	}
	return name
}

type BatchItem struct {
	ItemID           string               `json:"item_id"`
	Line             int                  `json:"line"`
	IdentifierMasked string               `json:"identifier_masked"`
	State            string               `json:"state"`
	Error            *Error               `json:"error"`
	ReceiptID        *string              `json:"receipt_id"`
	Progress         *ItemProgress        `json:"progress,omitempty"`
	SuccessSummary   *BatchSuccessSummary `json:"success_summary,omitempty"`
}

// BatchProfile contains only the displayable authorization options, never proxy credentials.
type BatchProfile struct {
	Environment string `json:"environment,omitempty"`
	Language    string `json:"language,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
}

func (p *BatchProfile) UnmarshalJSON(data []byte) error {
	field := func(key string) string {
		v := gjson.GetBytes(data, key)
		if v.Type != gjson.String || len(v.Str) > 128 || strings.ContainsFunc(v.Str, unicode.IsControl) {
			return ""
		}
		return strings.TrimSpace(v.Str)
	}
	*p = BatchProfile{Environment: field("environment"), Language: field("language"), Timezone: field("timezone")}
	return nil
}

type Batch struct {
	BatchID   string        `json:"batch_id"`
	RequestID string        `json:"request_id"`
	State     string        `json:"state"`
	Total     int           `json:"total"`
	Succeeded int           `json:"succeeded"`
	Failed    int           `json:"failed"`
	Pending   int           `json:"pending"`
	Items     []BatchItem   `json:"items"`
	Options   *BatchProfile `json:"options,omitempty"`
}
type DeliveryItem struct {
	ItemID       string `json:"item_id"`
	Line         int    `json:"line"`
	AccountIndex int    `json:"account_index"`
}
type Delivery struct {
	IncludedCount  int            `json:"included_count"`
	ExpiredCount   int            `json:"expired_count"`
	ExpiredItemIDs []string       `json:"expired_item_ids"`
	Items          []DeliveryItem `json:"items"`
}
type Result struct {
	Data     json.RawMessage `json:"data"`
	Delivery Delivery        `json:"delivery"`
}
type OAuthClient struct{ c *client }

func NewOAuth(baseURL, token string, allowHTTP bool) (*OAuthClient, error) {
	c, e := newClient(baseURL, token, allowHTTP)
	if e != nil {
		return nil, e
	}
	return &OAuthClient{c}, nil
}

func (o *OAuthClient) call(ctx context.Context, method, path string, body any, capBytes int, uncertain bool) (json.RawMessage, json.RawMessage, error) {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil || len(payload) > capBytes {
			return nil, nil, failure("request_too_large", false)
		}
	}
	var r *http.Request
	if body == nil {
		r, err = http.NewRequestWithContext(ctx, method, o.c.base+"/api/v1/oauth"+path, nil)
	} else {
		r, err = http.NewRequestWithContext(ctx, method, o.c.base+"/api/v1/oauth"+path, bytes.NewReader(payload))
	}
	if err != nil {
		return nil, nil, failure("invalid_request", false)
	}
	r.Header.Set("Authorization", "Bearer "+o.c.token)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.c.http.Do(r)
	if err != nil {
		return nil, nil, failure("transport_error", uncertain)
	}
	defer resp.Body.Close()
	b, err := bounded(resp.Body, maxResponse)
	if err != nil {
		return nil, nil, failure("invalid_response", uncertain)
	}
	var env struct {
		OK       *bool           `json:"ok"`
		Data     json.RawMessage `json:"data"`
		Error    *Error          `json:"error"`
		Delivery json.RawMessage `json:"delivery"`
	}
	if json.Unmarshal(b, &env) != nil || env.OK == nil {
		e := failure("invalid_response", uncertain)
		e.Status = resp.StatusCode
		return nil, nil, e
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !*env.OK {
		if env.Error != nil && !*env.OK {
			env.Error.Status = resp.StatusCode
			return nil, nil, env.Error
		}
		e := failure("invalid_response", uncertain)
		e.Status = resp.StatusCode
		return nil, nil, e
	}
	if len(env.Data) == 0 || bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		e := failure("invalid_response", uncertain)
		e.Status = resp.StatusCode
		return nil, nil, e
	}
	return env.Data, env.Delivery, nil
}
func decode(raw []byte, out any, required ...string) error {
	if !object(raw, required...) || json.Unmarshal(raw, out) != nil {
		return failure("invalid_response", false)
	}
	return nil
}
func (o *OAuthClient) Capabilities(ctx context.Context) (v Capabilities, err error) {
	var b json.RawMessage
	b, _, err = o.call(ctx, "GET", "/capabilities", nil, 0, false)
	if err == nil {
		err = decode(b, &v, "protocol", "oauth_methods", "features", "countries", "environments", "proxy_modes", "workspace_policies", "max_batch_size", "max_upload_bytes")
		if err == nil && (v.Protocol.Name == "" || v.Protocol.Version == 0 || v.Protocol.Revision == 0) {
			err = failure("invalid_response", false)
		}
	}
	return
}
func (o *OAuthClient) Quota(ctx context.Context) (v Quota, err error) {
	var b json.RawMessage
	b, _, err = o.call(ctx, "GET", "/quota", nil, 0, false)
	if err == nil {
		err = decode(b, &v, "total", "remaining", "reserved", "used", "disabled")
	}
	return
}
func (o *OAuthClient) Preview(ctx context.Context, r SubmitRequest) (v Preview, err error) {
	var b json.RawMessage
	b, _, err = o.call(ctx, "POST", "/preview", r, 1<<20, false)
	if err == nil {
		err = decode(b, &v, "valid", "count", "rows", "errors")
		if err == nil && (!arrayObjects(b, "rows", "line", "identifier_masked") || !arrayObjects(b, "errors", "line", "code")) {
			err = failure("invalid_response", false)
		}
		for i := range v.Errors {
			v.Errors[i].Code = safeCode(v.Errors[i].Code)
		}
	}
	return
}
func (o *OAuthClient) CheckProxy(ctx context.Context, opts Options) (v ProxyCheck, err error) {
	opts.WorkspacePolicy = ""
	var b json.RawMessage
	b, _, err = o.call(ctx, "POST", "/proxy-check", struct {
		Options Options `json:"options"`
	}{opts}, 16<<10, false)
	if err == nil {
		err = decode(b, &v, "usable", "requested_country", "openai_reachable", "checked_at", "elapsed_ms")
	}
	return
}
func (o *OAuthClient) batchCall(ctx context.Context, method, path string, body any, uncertain bool) (v Batch, err error) {
	var b json.RawMessage
	b, _, err = o.call(ctx, method, path, body, 1<<20, uncertain)
	if err == nil {
		err = decode(b, &v, "batch_id", "request_id", "state", "total", "succeeded", "failed", "pending", "items")
		if err == nil && (!validID(v.BatchID) || !validID(v.RequestID) || v.State == "" || !arrayObjects(b, "items", "item_id", "line", "state")) {
			err = failure("invalid_response", uncertain)
		}
		if err == nil {
			for _, item := range v.Items {
				if !validID(item.ItemID) || item.Line < 1 || item.State == "" {
					err = failure("invalid_response", uncertain)
					break
				}
			}
		}
		if err != nil && uncertain {
			err = failure("invalid_response", true)
		}
	}
	return
}
func (o *OAuthClient) Submit(ctx context.Context, r SubmitRequest) (Batch, error) {
	if !validID(r.RequestID) {
		return Batch{}, failure("invalid_request", false)
	}
	return o.batchCall(ctx, "POST", "/batches", r, true)
}
func (o *OAuthClient) Batch(ctx context.Context, id string) (Batch, error) {
	if !validID(id) {
		return Batch{}, failure("invalid_request", false)
	}
	return o.batchCall(ctx, "GET", "/batches/"+id, nil, false)
}
func (o *OAuthClient) Lookup(ctx context.Context, id string) (Batch, error) {
	if !validID(id) {
		return Batch{}, failure("invalid_request", false)
	}
	return o.batchCall(ctx, "GET", "/requests/"+id, nil, false)
}
func (o *OAuthClient) Cancel(ctx context.Context, id string) (Batch, error) {
	if !validID(id) {
		return Batch{}, failure("invalid_request", false)
	}
	return o.batchCall(ctx, "POST", "/batches/"+id+"/cancel", struct{}{}, false)
}
func (o *OAuthClient) Result(ctx context.Context, id string) (v Result, err error) {
	if !validID(id) {
		return v, failure("invalid_request", false)
	}
	var d json.RawMessage
	v.Data, d, err = o.call(ctx, "GET", "/batches/"+id+"/result", nil, 0, false)
	if err == nil {
		err = decode(d, &v.Delivery, "included_count", "expired_count", "expired_item_ids", "items")
		if err == nil && (!object(v.Data, "type", "version", "accounts") || !arrayObjects(d, "items", "item_id", "line", "account_index")) {
			err = failure("invalid_response", false)
		}
	}
	return
}

// ValidateDelivery returns the accounts indexed exactly as this delivery's account_index.
func ValidateDelivery(result Result, batch Batch) ([]json.RawMessage, error) {
	bad := func() ([]json.RawMessage, error) { return nil, failure("invalid_result", false) }
	var bundle struct {
		Type     string            `json:"type"`
		Version  int               `json:"version"`
		Accounts []json.RawMessage `json:"accounts"`
	}
	if !object(result.Data, "type", "version", "accounts") || json.Unmarshal(result.Data, &bundle) != nil || bundle.Type != "sub2api-data" || bundle.Version != 1 || bundle.Accounts == nil {
		return bad()
	}
	d := result.Delivery
	if d.IncludedCount < 0 || d.ExpiredCount < 0 || d.IncludedCount != len(d.Items) || d.IncludedCount != len(bundle.Accounts) || d.ExpiredCount != len(d.ExpiredItemIDs) {
		return bad()
	}
	original := map[string]BatchItem{}
	for _, item := range batch.Items {
		if item.ItemID == "" {
			return bad()
		}
		if _, ok := original[item.ItemID]; ok {
			return bad()
		}
		original[item.ItemID] = item
	}
	ids := map[string]bool{}
	indices := map[int]bool{}
	for _, m := range d.Items {
		item, ok := original[m.ItemID]
		if !ok || ids[m.ItemID] || item.Line != m.Line || item.State != "succeeded" || m.AccountIndex < 0 || m.AccountIndex >= len(bundle.Accounts) || indices[m.AccountIndex] || !object(bundle.Accounts[m.AccountIndex]) {
			return bad()
		}
		ids[m.ItemID] = true
		account := bundle.Accounts[m.AccountIndex]
		workspaceID := openaiidentity.NormalizeWorkspaceID(gjson.GetBytes(account, "credentials.chatgpt_account_id").String())
		if summary := item.SuccessSummary; summary != nil && summary.WorkspaceName != "" && summary.WorkspaceID == workspaceID && openaiidentity.ValidWebWorkspaceID(workspaceID) {
			// A display name cannot establish or change the credential's workspace.
			account, err := sjson.SetBytes(account, "workspace_name", summary.WorkspaceName)
			if err != nil {
				return bad()
			}
			bundle.Accounts[m.AccountIndex] = account
		}
		indices[m.AccountIndex] = true
	}
	for _, id := range d.ExpiredItemIDs {
		if _, ok := original[id]; !ok || ids[id] {
			return bad()
		}
		ids[id] = true
	}
	return bundle.Accounts, nil
}
