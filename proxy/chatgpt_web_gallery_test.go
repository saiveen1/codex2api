package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/tidwall/gjson"
)

func TestChatGPTWebCloudGalleryFinalizedWithPrompt(t *testing.T) {
	f := newWebFixture(t, nil)
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("ETag", `"fixture"`); w.WriteHeader(200) }))
	t.Cleanup(s3.Close)
	if err := imagestore.Configure(imagestore.Config{Backend: imagestore.BackendS3, Endpoint: s3.URL, Region: "us-east-1", Bucket: "fixture", AccessKey: "fixture", SecretKey: "fixture", ForcePathStyle: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = imagestore.Configure(imagestore.Config{Backend: imagestore.BackendLocal, LocalDir: t.TempDir()})
	})
	r := f.request(t, "/v1/images/generations", `{"prompt":"test gallery prompt","response_format":"url"}`, "gallery")
	if r.Code != 200 || !strings.HasPrefix(gjson.Get(r.Body.String(), "data.0.url").String(), s3.URL) {
		t.Fatalf("bad cloud response: %s", r.Body.String())
	}
	assets, err := f.db.ListImageAssets(context.Background(), 1, 10, 0)
	if err != nil || len(assets.Assets) != 1 {
		t.Fatalf("assets=%+v err=%v", assets, err)
	}
	job, err := f.db.GetImageGenerationJob(context.Background(), assets.Assets[0].JobID)
	if err != nil || job.Status != database.ImageJobSucceeded || job.Prompt != "test gallery prompt" {
		t.Fatalf("gallery job=%+v err=%v", job, err)
	}
}
