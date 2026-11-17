package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestChatGPTWebSedimentAssetEndToEnd(t *testing.T) {
	const asset = "sediment://file_00000000123456789abcdef012345678"
	f := newWebFixture(t, func(w http.ResponseWriter, r *http.Request, req chatGPTWebRequest) {
		enc := json.NewEncoder(w)
		_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: "submitted", ConversationID: "conversation-sediment"})
		for _, stage := range []string{"generated", "downloading"} {
			_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: stage, AssetIDs: []string{asset}})
		}
		_ = enc.Encode(chatGPTWebEvent{Type: "image", Image: &chatGPTWebImage{B64JSON: tinyPNGBase64, MIMEType: "image/png", Width: 1, Height: 1, AssetID: asset}})
		_ = enc.Encode(chatGPTWebEvent{Type: "completed"})
	})
	response := f.request(t, "/v1/images/generations", `{"prompt":"synthetic"}`, "sediment-contract")
	rows, err := f.db.ListChatGPTWebExecutions(context.Background(), f.account.ID(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || gjson.Get(response.Body.String(), "data.0.b64_json").String() != tinyPNGBase64 {
		t.Fatalf("valid Worker sediment response rejected: http=%d body=%s ledger=%+v", response.Code, response.Body.String(), rows)
	}
	if len(rows) != 1 || rows[0].Stage != "completed" || len(rows[0].AssetIDs) != 1 || rows[0].AssetIDs[0] != asset {
		t.Fatalf("asset lost: %+v", rows)
	}
	if f.account.OccupiedRequests.Load() != 0 || f.account.ActiveRequests.Load() != 0 {
		t.Fatal("concurrency leaked")
	}
}

func TestChatGPTWebInvalidAssetDoesNotPoisonFailureRecord(t *testing.T) {
	for _, asset := range []string{"https://example.invalid/secret?token=sensitive", "sediment://../file", "sediment://file_x?sig=sensitive"} {
		t.Run(asset, func(t *testing.T) {
			f := newWebFixture(t, func(w http.ResponseWriter, r *http.Request, req chatGPTWebRequest) {
				enc := json.NewEncoder(w)
				_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: "submitted", ConversationID: "known-conversation"})
				_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: "generated", AssetIDs: []string{asset}})
			})
			response := f.request(t, "/v1/images/generations", `{"prompt":"synthetic"}`, "invalid-asset")
			if response.Code != 502 || gjson.Get(response.Body.String(), "error.code").String() != "worker_protocol_error" {
				t.Fatalf("wrong diagnostic: %s", response.Body.String())
			}
			rows, err := f.db.ListChatGPTWebExecutions(context.Background(), f.account.ID(), 10)
			if err != nil || len(rows) != 1 || rows[0].Stage != "unknown" || rows[0].ErrorCode != "worker_protocol_error" || len(rows[0].AssetIDs) != 0 {
				t.Fatalf("failed event poisoned record: %+v %v", rows, err)
			}
			if f.account.OccupiedRequests.Load() != 0 || f.account.ActiveRequests.Load() != 0 {
				t.Fatal("failure leaked concurrency")
			}
		})
	}
}

func TestChatGPTWebSedimentDownloadRecoveryKeepsExecution(t *testing.T) {
	const asset = "sediment://file_resume"
	var attempts atomic.Int32
	var firstID string
	f := newWebFixture(t, func(w http.ResponseWriter, r *http.Request, req chatGPTWebRequest) {
		enc := json.NewEncoder(w)
		if attempts.Add(1) == 1 {
			firstID = req.ExecutionID
			_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: "submitted", ConversationID: "conv-resume"})
			_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: "generated", AssetIDs: []string{asset}})
			_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: "downloading"})
			_ = enc.Encode(chatGPTWebEvent{Type: "error", Code: "image_download_failed", HTTPStatus: 502})
			return
		}
		if req.ExecutionID != firstID {
			t.Error("resume changed immutable execution identity")
		}
		_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: "generated", AssetIDs: []string{asset, asset}})
		_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: "downloading"})
		_ = enc.Encode(chatGPTWebEvent{Type: "image", Image: &chatGPTWebImage{B64JSON: tinyPNGBase64, MIMEType: "image/png", Width: 1, Height: 1, AssetID: asset}})
		_ = enc.Encode(chatGPTWebEvent{Type: "completed"})
	})
	first := f.request(t, "/v1/images/generations", `{"prompt":"recover synthetic"}`, "same-sediment")
	if first.Code != 502 || gjson.Get(first.Body.String(), "error.code").String() != "image_download_failed" {
		t.Fatalf("wrong first outcome %s", first.Body.String())
	}
	// Advance only the isolated fixture lease; never modify production fences.
	raw, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.Exec(`UPDATE chatgpt_web_executions SET lease_until=? WHERE id=?`, time.Now().Add(-time.Minute).UnixNano(), firstID); err != nil {
		t.Fatal(err)
	}
	second := f.request(t, "/v1/images/generations", `{"prompt":"recover synthetic"}`, "same-sediment")
	if second.Code != 200 {
		t.Fatalf("recovery: %s", second.Body.String())
	}
	rows, err := f.db.ListChatGPTWebExecutions(context.Background(), f.account.ID(), 10)
	if err != nil || len(rows) != 1 || rows[0].Stage != "completed" || len(rows[0].AssetIDs) != 1 || attempts.Load() != 2 {
		t.Fatalf("incorrect replay %+v %v", rows, err)
	}
	third := f.request(t, "/v1/images/generations", `{"prompt":"recover synthetic"}`, "same-sediment")
	if third.Code != 409 || attempts.Load() != 2 {
		t.Fatal("completed execution regenerated")
	}
}

func TestChatGPTWebExpiredObservationRefreshesOnDemand(t *testing.T) {
	f := newWebFixture(t, nil)
	raw, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.Exec(`UPDATE chatgpt_web_account_states SET observed_at=? WHERE account_id=?`, time.Now().Add(-31*time.Minute).UnixNano(), f.account.ID()); err != nil {
		t.Fatal(err)
	}
	r := f.request(t, "/v1/images/generations", `{"prompt":"synthetic"}`, "stale-observation")
	if r.Code != 200 || f.calls.Load() != 1 || f.checks.Load() != 2 {
		t.Fatalf("expired observation was not checked before generation: %s", r.Body.String())
	}
	if f.account.OccupiedRequests.Load() != 0 || f.account.ActiveRequests.Load() != 0 {
		t.Fatal("expired observation leaked concurrency")
	}
}
