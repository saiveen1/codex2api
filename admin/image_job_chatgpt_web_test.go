package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestChatGPTWebJobSnapshotAndIndexedBatch(t *testing.T) {
	web := &database.APIKeyRow{Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb}}
	native := &database.APIKeyRow{}
	for _, tc := range []struct {
		snapshot string
		key      *database.APIKeyRow
		valid    bool
	}{
		{"", native, true}, {"", web, false}, {"auto", web, false}, {"chatgpt_web", native, false}, {"chatgpt_web", web, true},
	} {
		if (validateImageJobChannelSnapshot(imageGenerationJobPayload{ChannelSnapshot: tc.snapshot}, tc.key) == nil) != tc.valid {
			t.Fatalf("snapshot=%q key=%s", tc.snapshot, imageJobChannel(tc.key))
		}
	}
	req := imageGenerationJobPayload{ChannelSnapshot: "chatgpt_web"}
	if !reflect.DeepEqual(imageJobOutputContext(context.Background(), req, 42, 0), imageJobOutputContext(context.Background(), req, 42, 0)) || reflect.DeepEqual(imageJobOutputContext(context.Background(), req, 42, 0), imageJobOutputContext(context.Background(), req, 42, 1)) {
		t.Fatal("output IDs not stable and distinct")
	}
	var indexes []int
	body, _, notes, err := runImageJobBatchIndexed(context.Background(), 4, true, func(i int) ([]byte, int, error) {
		indexes = append(indexes, i)
		if i == 1 {
			return nil, 502, errors.New("unknown")
		}
		return []byte(`{"data":[{"b64_json":"a"}]}`), 200, nil
	})
	if err != nil || len(notes) != 1 || len(indexes) != 2 || indexes[1] != 1 || !strings.Contains(string(body), `"completed_n":1`) {
		t.Fatalf("partial result body=%s notes=%v indexes=%v err=%v", body, notes, indexes, err)
	}
}

func TestChatGPTWebJobOptionsPreserved(t *testing.T) {
	req := imageGenerationJobPayload{ChannelSnapshot: "chatgpt_web", Model: "gpt-image-2", Prompt: "test", Size: "1001x1001", Mask: json.RawMessage(`{"image_url":"x"}`), Moderation: json.RawMessage(`"auto"`), InputImages: []string{"data:image/png;base64,AA=="}}
	for _, build := range []func(imageGenerationJobPayload) ([]byte, error){buildAdminImageGenerationRequest, buildAdminImageEditRequest} {
		body, err := build(req)
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if json.Unmarshal(body, &parsed) != nil || parsed["size"] != "1001x1001" || parsed["mask"] == nil || parsed["moderation"] == nil {
			t.Fatalf("lost user options: %s", body)
		}
	}
}

func TestChatGPTWebPublicPipelineEditNoReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("IMAGE_JOB_WORKERS", "2")
	root := t.TempDir()
	t.Setenv("IMAGE_ASSET_DIR", root)
	previous := imagestore.CurrentConfig()
	t.Cleanup(func() {
		_ = imagestore.Configure(previous)
		proxy.ConfigureImagePipeline(0)
		proxy.ConfigureImageExecutionLimit(0)
	})
	if err := imagestore.Configure(imagestore.Config{Backend: imagestore.BackendLocal, LocalDir: root}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var operations []string
	png := base64.StdEncoding.EncodeToString(tinyPNG(t))
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/accounts/check" {
			var check struct {
				WorkspaceID string `json:"chatgpt_account_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&check); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			fmt.Fprintf(w, `{"status":"ready","capabilities":["generate","edit"],"image_quota_remaining":10,"workspace_id":%q,"stage":"complete","upstream_status":200}`, check.WorkspaceID)
			return
		}
		var payload struct {
			Operation   string `json:"operation"`
			ExecutionID string `json:"execution_id"`
		}
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Error("invalid worker request")
			return
		}
		operations = append(operations, payload.Operation)
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		if n == 1 {
			fmt.Fprintf(w, "{\"type\":\"stage\",\"stage\":\"submitted\",\"conversation_id\":\"fixture-conversation\"}\n{\"type\":\"stage\",\"stage\":\"generated\",\"asset_ids\":[\"file_output\"]}\n{\"type\":\"stage\",\"stage\":\"downloading\"}\n{\"type\":\"image\",\"image\":{\"b64_json\":%q,\"mime_type\":\"image/png\",\"width\":1,\"height\":1,\"asset_id\":\"file_output\"}}\n{\"type\":\"completed\"}\n", png)
			return
		}
		fmt.Fprintln(w, `{"type":"stage","stage":"submitted","conversation_id":"fixture-conversation-2"}`)
	}))
	defer worker.Close()
	t.Setenv("CHATGPT_WEB_ENABLED", "true")
	t.Setenv("CHATGPT_WEB_WORKER_URL", worker.URL)
	t.Setenv("CHATGPT_WEB_WORKER_SECRET", "fixture-worker-secret-123")
	t.Setenv("CHATGPT_WEB_TIMEOUT_SECONDS", "10")
	db := newTestAdminDB(t)
	ctx := context.Background()
	if err := db.InitImageJobQueue(ctx); err != nil {
		t.Fatal(err)
	}
	accountID, err := db.InsertAccountWithCredentials(ctx, "fixture", map[string]any{"access_token": "fixture-token", "account_id": "fixture-account", "plan_type": "free"}, "")
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "web", Key: "sk-fixture-web", Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb}})
	if err != nil {
		t.Fatal(err)
	}
	tc := cache.NewMemory(1)
	defer tc.Close()
	store := auth.NewStore(db, tc, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: accountID, AccessToken: "fixture-token", AccountID: "fixture-account", PlanType: "free", Status: auth.StatusReady})
	h := NewHandler(store, db, tc, nil, "admin-secret")
	h.imageQueue = &imageJobQueue{pipeline: true, intake: make(chan struct{}, 2)}
	proxy.ConfigureImagePipeline(1)
	proxy.ConfigureImageExecutionLimit(2)
	if _, err = db.SetChatGPTWebAccountEnabled(ctx, accountID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = h.imageProxy.CheckChatGPTWebAccount(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/api/image-studio/edit-jobs", h.imageProxy.APIKeyAuthMiddleware(), h.CreatePortalImageEditJob)
	raw := fmt.Sprintf(`{"model":"gpt-image-2","prompt":"edit fixture","n":3,"upstream_channel_snapshot":"codex","input_images":["data:image/png;base64,%s"]}`, png)
	request := httptest.NewRequest("POST", "/api/image-studio/edit-jobs", strings.NewReader(raw))
	request.Header.Set("Authorization", "Bearer sk-fixture-web")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("accept %d %s", response.Code, response.Body.String())
	}
	var receipt struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetImageGenerationJob(ctx, receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var saved imageGenerationJobPayload
	if err = json.Unmarshal([]byte(job.ParamsJSON), &saved); err != nil || saved.ChannelSnapshot != "chatgpt_web" {
		t.Fatalf("forged snapshot: %+v %v", saved, err)
	}
	runCtx, release, err := proxy.AcquireImageExecution(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	h.runNextQueuedImage(runCtx)
	job, err = db.GetImageGenerationJob(ctx, receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != database.ImageJobSucceeded || len(job.Assets) != 1 || calls.Load() != 2 || len(operations) != 2 || operations[0] != "edit" || operations[1] != "edit" {
		t.Fatalf("partial status=%s assets=%d calls=%d operations=%v error=%s", job.Status, len(job.Assets), calls.Load(), operations, job.ErrorMessage)
	}
	entries, err := db.ListChatGPTWebExecutions(ctx, accountID, 10)
	if err != nil || len(entries) != 2 || entries[0].ID == entries[1].ID {
		t.Fatalf("execution IDs: %+v %v", entries, err)
	}
	key, err := db.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	saved.InputImages = []string{"data:image/png;base64," + png}
	h.runPipelinedImageJob(runCtx, receipt.Job.ID, "", saved, key)
	if calls.Load() != 2 {
		t.Fatalf("recovery resubmitted output: %d", calls.Load())
	}
	// The ambiguous edit keeps its account lease; use a second fixture account.
	secondID, err := db.InsertAccountWithCredentials(ctx, "fixture2", map[string]any{"access_token": "fixture-token-2", "account_id": "fixture-account-2", "plan_type": "free"}, "")
	if err != nil {
		t.Fatal(err)
	}
	store.AddAccount(&auth.Account{DBID: secondID, AccessToken: "fixture-token-2", AccountID: "fixture-account-2", PlanType: "free", Status: auth.StatusReady})
	if _, err = db.SetChatGPTWebAccountEnabled(ctx, secondID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = h.imageProxy.CheckChatGPTWebAccount(ctx, secondID); err != nil {
		t.Fatal(err)
	}
	// A direct PNG generation that disconnects after submission must not retry JPEG.
	generation := imageGenerationJobPayload{ChannelSnapshot: "chatgpt_web", Model: "gpt-image-2", Prompt: "fixture", OutputFormat: "png", N: 1}
	params, _ := json.Marshal(generation)
	generationID, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{Prompt: generation.Prompt, ParamsJSON: string(params), APIKeyID: keyID})
	if err != nil {
		t.Fatal(err)
	}
	h.runImageGenerationJob(generationID, generation, key, imageJobRunOptions{})
	if calls.Load() != 3 {
		t.Fatalf("PNG failure retried or did not submit: %d", calls.Load())
	}
	// A queued web job cannot move to native after its key changes.
	queuedID, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{Prompt: generation.Prompt, ParamsJSON: string(params), APIKeyID: keyID})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.UpdateAPIKeyLimits(ctx, keyID, database.APIKeyLimits{}); err != nil {
		t.Fatal(err)
	}
	h.runNextQueuedImage(runCtx)
	queued, err := db.GetImageGenerationJob(ctx, queuedID)
	if err != nil || queued.Status != database.ImageJobFailed || calls.Load() != 3 {
		t.Fatalf("changed channel accepted: job=%+v err=%v calls=%d", queued, err, calls.Load())
	}
}
