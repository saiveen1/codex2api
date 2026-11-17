package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/managedweb"
)

var chatGPTWebSafeExecutionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

func ChatGPTWebValidExecutionID(id string) bool { return chatGPTWebSafeExecutionID.MatchString(id) }

type ChatGPTWebWorkerObservation struct {
	Verified        bool   `json:"verified"`
	Reason          string `json:"reason,omitempty"`
	Stage           string `json:"stage,omitempty"`
	ResultAvailable *bool  `json:"result_available,omitempty"`
	ResultUnknown   *bool  `json:"result_unknown,omitempty"`
	AssetCount      *int   `json:"asset_count,omitempty"`
}

// ObserveChatGPTWebExecution never submits work or sends account credentials.
func ObserveChatGPTWebExecution(ctx context.Context, e *database.ChatGPTWebExecution, managed *managedweb.Config) ChatGPTWebWorkerObservation {
	unverified := ChatGPTWebWorkerObservation{Reason: "unavailable"}
	cfg, err := loadChatGPTWebConfig()
	if managed != nil {
		cfg = chatGPTWebConfig{enabled: managed.Enabled, endpoint: managed.WorkerURL, secret: managed.WorkerSecret, timeout: time.Duration(managed.TimeoutSeconds) * time.Second}
		err = nil
	}
	if err != nil || !cfg.enabled || cfg.endpoint == "" || cfg.secret == "" || !ChatGPTWebValidExecutionID(e.ID) {
		unverified.Reason = "not_configured"
		return unverified
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.endpoint+"/v1/executions/"+e.ID, nil)
	if err != nil {
		return unverified
	}
	req.Header.Set("Authorization", "Bearer "+cfg.secret)
	client := newChatGPTWebHTTPClient()
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return unverified
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		unverified.Reason = "not_found"
		return unverified
	}
	if resp.StatusCode != http.StatusOK {
		return unverified
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(body) > 65536 {
		unverified.Reason = "invalid_response"
		return unverified
	}
	var result struct {
		ID                  string            `json:"execution_id"`
		AccountID           int64             `json:"account_id"`
		CredentialRevision  string            `json:"credential_revision"`
		EnvironmentRevision int64             `json:"environment_revision"`
		Stage               string            `json:"stage"`
		ResultAvailable     *bool             `json:"result_available"`
		ResultUnknown       *bool             `json:"result_unknown"`
		AssetIDs            []json.RawMessage `json:"asset_ids"`
	}
	if json.Unmarshal(body, &result) != nil || result.ResultAvailable == nil || result.ResultUnknown == nil || result.AssetIDs == nil {
		unverified.Reason = "invalid_response"
		return unverified
	}
	if result.ID != e.ID || result.AccountID != e.AccountID || result.CredentialRevision == "" || result.CredentialRevision != e.CredentialRevision || result.EnvironmentRevision != e.EnvironmentRevision {
		unverified.Reason = "binding_mismatch"
		return unverified
	}
	switch result.Stage {
	case "prepared", "preparing", "submitting", "submitted", "streaming", "polling", "generated", "downloading", "completed", "failed", "result_unknown":
	default:
		unverified.Reason = "invalid_response"
		return unverified
	}
	count := len(result.AssetIDs)
	return ChatGPTWebWorkerObservation{Verified: true, Stage: result.Stage, ResultAvailable: result.ResultAvailable, ResultUnknown: result.ResultUnknown, AssetCount: &count}
}
