package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestChatGPTWebOverlappingSingleAndMultiAssetDelivery(t *testing.T) {
	f := newWebFixture(t, nil)
	other := webAdmissionAddAccount(t, f, webAdmissionGroup(t, f, "parallel"), nil)
	if _, err := f.db.SetChatGPTWebAccountEnabled(t.Context(), other.ID(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handler.CheckChatGPTWebAccount(t.Context(), other.ID()); err != nil {
		t.Fatal(err)
	}
	entered := make(chan int64, 2)
	release := make(chan struct{})
	setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
		var req chatGPTWebRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.N != 1 {
			t.Error("worker must receive one logical output")
		}
		entered <- req.AccountID
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		ids := []string{"file_a"}
		if req.Prompt == "multi" {
			ids = append(ids, "sediment://file_b")
		}
		_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: "submitted", ConversationID: "conv-" + req.Prompt})
		_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: "generated", AssetIDs: ids})
		_ = enc.Encode(chatGPTWebEvent{Type: "stage", Stage: "downloading"})
		for _, id := range ids {
			_ = enc.Encode(chatGPTWebEvent{Type: "image", Image: &chatGPTWebImage{B64JSON: tinyPNGBase64, MIMEType: "image/png", Width: 1, Height: 1, AssetID: id}})
		}
		_ = enc.Encode(chatGPTWebEvent{Type: "completed"})
	})
	result := make(chan string, 2)
	for _, name := range []string{"single", "multi"} {
		go func() {
			rec := f.request(t, "/v1/images/generations", `{"stream":true,"prompt":"`+name+`","n":1}`, name)
			result <- rec.Body.String()
		}()
	}
	seen := map[int64]bool{}
	timeout := time.After(4 * time.Second)
	for range 2 {
		select {
		case id := <-entered:
			seen[id] = true
		case <-timeout:
			close(release)
			t.Fatal("two executions did not overlap")
		}
	}
	close(release)
	if len(seen) != 2 {
		t.Error("overlap used the same account")
	}
	for range 2 {
		body := <-result
		if strings.Contains(body, "event: error") || strings.Count(body, "event: image_generation.completed") != 1 {
			t.Errorf("expected one successful delivery: %s", body)
		}
	}
	rows, err := f.db.ListChatGPTWebExecutions(t.Context(), 0, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("execution count: %d err=%v", len(rows), err)
	}
	assets := 0
	for _, row := range rows {
		if row.Stage != "completed" || row.ResultUnknown {
			t.Errorf("execution outcome: %s/%s/unknown=%t", row.Stage, row.ErrorCode, row.ResultUnknown)
		}
		assets += len(row.AssetIDs)
	}
	if assets != 3 {
		t.Fatalf("lost asset references: %d", assets)
	}
}
