package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestImageJobCancelRunningWorker(t *testing.T) {
	for _, mode := range []string{"legacy", "queue", "pipeline"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("IMAGE_ASSET_DIR", t.TempDir())
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			db := newTestAdminDB(t)
			tc := cache.NewMemory(1)
			defer tc.Close()
			store := auth.NewStore(db, tc, &database.SystemSettings{MaxConcurrency: 2})
			defer store.Stop()
			h := NewHandler(store, db, tc, nil, "admin-secret")
			router := gin.New()
			h.RegisterRoutes(router)
			ctx := context.Background()
			keyID, err := db.InsertAPIKey(ctx, "fixture", "sk-cancel-fixture")
			if err != nil {
				t.Fatal(err)
			}
			key, err := db.GetAPIKeyByID(ctx, keyID)
			if err != nil {
				t.Fatal(err)
			}
			job := imageGenerationJobPayload{Model: "gpt-image-2", Prompt: "fixture", Size: "auto", N: 3, OutputFormat: "png"}
			var input string
			if mode != "legacy" {
				if err := db.InitImageJobQueue(ctx); err != nil {
					t.Fatal(err)
				}
				h.imageQueue = &imageJobQueue{pipeline: mode == "pipeline"}
				if h.imageQueue.pipeline {
					proxy.ConfigureImagePipeline(1)
					defer proxy.ConfigureImagePipeline(0)
					proxy.ConfigureImageExecutionLimit(1)
					defer proxy.ConfigureImageExecutionLimit(0)
					var releaseSlot func()
					ctx, releaseSlot, err = proxy.AcquireImageExecution(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer releaseSlot()
				}
				job.InputImages = []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG(t))}
				if err := prepareQueueInputs(&job); err != nil {
					t.Fatal(err)
				}
				input, err = queueInputPath(job.InputImages[0])
				if err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			id, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{Prompt: job.Prompt, ParamsJSON: string(raw), APIKeyID: keyID})
			if err != nil {
				t.Fatal(err)
			}
			cancelRequest := func(id int64, secret string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/admin/images/jobs/%d/cancel", id), nil)
				r.Header.Set("X-Admin-Key", secret)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, r)
				return w
			}
			if w := cancelRequest(id, ""); w.Code == 200 {
				t.Fatal("unauthenticated admin cancel succeeded")
			}
			if w := cancelRequest(id+100, "admin-secret"); w.Code != 404 {
				t.Fatalf("missing: %d %s", w.Code, w.Body)
			}

			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if calls.Add(1) == 1 {
					close(entered)
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer upstream.Close()
			store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "fixture-key"})
			done := make(chan struct{})
			go func() {
				defer close(done)
				if mode == "legacy" {
					h.runImageGenerationJob(id, job, key, imageJobRunOptions{})
				} else {
					h.runNextQueuedImage(ctx)
				}
			}()
			defer func() {
				// If an assertion fails, unblock only this synthetic worker before closing its fixtures.
				if cancel, ok := h.imageJobCancels.Load(id); ok {
					cancel.(context.CancelFunc)()
				}
				close(release)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("fixture worker did not exit")
				}
			}()
			select {
			case <-entered:
			case <-done:
				row, err := db.GetImageGenerationJob(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				t.Fatalf("worker stopped before fixture upstream: %s", row.ErrorMessage)
			case <-time.After(5 * time.Second):
				t.Fatal("worker never reached fixture upstream")
			}
			for i := 0; i < 2; i++ {
				if w := cancelRequest(id, "admin-secret"); w.Code != 200 || w.Body.String() != `{"status":"cancelled"}` {
					t.Fatalf("cancel: %d %s", w.Code, w.Body)
				}
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("cancel did not interrupt in-flight request")
			}
			persisted, err := db.GetImageGenerationJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != database.ImageJobCancelled || len(persisted.Assets) != 0 || calls.Load() != 1 {
				t.Fatalf("status=%s assets=%d calls=%d", persisted.Status, len(persisted.Assets), calls.Load())
			}
			if _, ok := h.imageJobCancels.Load(id); ok {
				t.Fatal("worker registration leaked")
			}
			if input != "" {
				if _, err := os.Stat(input); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("input survived: %v", err)
				}
			}
			for _, status := range []string{database.ImageJobSucceeded, database.ImageJobFailed} {
				finished, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{})
				if err != nil {
					t.Fatal(err)
				}
				if status == database.ImageJobSucceeded {
					err = db.MarkImageJobSucceeded(ctx, finished, 1)
				} else {
					err = db.MarkImageJobFailed(ctx, finished, "fixture", 1)
				}
				if err != nil {
					t.Fatal(err)
				}
				if w := cancelRequest(finished, "admin-secret"); w.Code != 409 {
					t.Fatalf("%s: %d %s", status, w.Code, w.Body)
				}
			}
		})
	}
}
