package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestChatGPTWebCatalogIsIndependentOfReadiness(t *testing.T) {
	h := &Handler{chatGPTWebRuntime: func() (chatGPTWebConfig, error) { t.Fatal("catalog consulted runtime"); return chatGPTWebConfig{}, nil }}
	want := []string{"gpt-image-2", "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "gpt-image-2.6", "gpt-image-2.6-flare", "gpt-image-2.6-sunburst"}
	want = append(want, h.ChatGPTWebChatModels(context.Background())...)
	row := &database.APIKeyRow{ID: 1, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb}}
	got := h.scopedModelRecords(context.Background(), row)
	if len(got) != len(want) {
		t.Fatalf("catalog requires runtime/DB/account: %v", got)
	}
	for _, model := range want {
		if got[model] == nil {
			t.Fatalf("missing %s", model)
		}
	}
	for _, tc := range []struct {
		name   string
		limits database.APIKeyLimits
		count  int
	}{
		{"single allow", database.APIKeyLimits{ModelAllow: []string{"gpt-image-2"}}, 1},
		{"alias allow", database.APIKeyLimits{ModelAllow: []string{"gpt-image-2.6-sunburst"}}, 1},
		{"deny", database.APIKeyLimits{ModelDeny: []string{"gpt-image-2.6-sunburst"}}, len(want) - 1},
		{"chat allow", database.APIKeyLimits{ModelAllow: []string{"gpt-6"}}, 1},
		{"blocked images", database.APIKeyLimits{ImageGenerationPolicy: database.ImageGenerationPolicyBlock}, len(chatGPTWebChatSlugs)},
		{"stripped images", database.APIKeyLimits{ImageGenerationPolicy: database.ImageGenerationPolicyStrip}, len(chatGPTWebChatSlugs)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.limits.UpstreamChannel = database.UpstreamChannelChatGPTWeb
			row.Limits = tc.limits
			records := h.scopedModelRecords(context.Background(), row)
			if len(records) != tc.count {
				t.Fatalf("wrong key-filtered catalog: %v", records)
			}
			for id := range records {
				if !slices.Contains(want, id) || checkAPIKeyModel(id, tc.limits) != "" {
					t.Fatalf("unauthorized model %s", id)
				}
			}
		})
	}
}

func TestChatGPTWebCatalogReadsDoNotProbeOrMutate(t *testing.T) {
	f := newWebFixture(t, nil)
	ctx := context.Background()
	_, err := f.db.SetChatGPTWebAccountEnabled(ctx, f.account.ID(), false)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.db.ListChatGPTWebAccountStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checks := f.checks.Load()
	for i := 0; i < 3; i++ {
		if got := f.handler.scopedModelRecords(ctx, f.key); len(got) != len(chatGPTWebImageModels)+len(chatGPTWebChatSlugs) {
			t.Fatalf("disabled account emptied catalog: %v", got)
		}
	}
	after, err := f.db.ListChatGPTWebAccountStates(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("catalog mutated account states", err)
	}
	if f.checks.Load() != checks || f.calls.Load() != 0 {
		t.Fatal("catalog caused upstream traffic")
	}
}

func TestChatGPTWebAliasesReachWorkerUnchanged(t *testing.T) {
	for _, model := range chatGPTWebImageModels {
		t.Run(model, func(t *testing.T) {
			f := newWebFixture(t, func(w http.ResponseWriter, r *http.Request, req chatGPTWebRequest) {
				if req.Model != model {
					t.Errorf("model changed: %s", req.Model)
				}
				writeWebFixtureSuccess(w)
			})
			body, _ := json.Marshal(map[string]any{"model": model, "prompt": "synthetic image"})
			r := f.request(t, "/v1/images/generations", string(body), "alias-test")
			if r.Code != 200 || f.calls.Load() != 1 || gjson.Get(r.Body.String(), "data.0.b64_json").String() != tinyPNGBase64 {
				t.Fatalf("alias failed: %d %s", r.Code, r.Body.String())
			}
			if f.checks.Load() != 1 {
				t.Fatal("ready account was needlessly rechecked")
			}
			// An alias is part of the immutable request, not a license to replay.
			other := chatGPTWebImageModels[(slices.Index(chatGPTWebImageModels, model)+1)%len(chatGPTWebImageModels)]
			r = f.request(t, "/v1/images/generations", fmt.Sprintf(`{"model":%q,"prompt":"synthetic image"}`, other), "alias-test")
			if r.Code != 409 || f.calls.Load() != 1 {
				t.Fatal("different alias reused a submitted execution")
			}
		})
	}
}

func TestChatGPTWebAliasParserAndDeploymentCatalog(t *testing.T) {
	for _, model := range chatGPTWebImageModels {
		if o, err := parseChatGPTWebImageOptions([]byte(`{}`), model, ""); err != nil || o.Model != model {
			t.Fatalf("model=%s options=%+v err=%v", model, o, err)
		}
	}
	for _, model := range []string{"gpt-6", "gpt-image-2.6-exact", "gpt-image-2.6-flare-4k", "unknown"} {
		if _, err := parseChatGPTWebImageOptions(nil, model, ""); err == nil {
			t.Fatalf("unsupported %s accepted", model)
		}
	}
	t.Setenv("CHATGPT_WEB_ENABLED", "false")
	models := ChatGPTWebDeploymentStatus()["supported_models"].([]string)
	if !reflect.DeepEqual(models, chatGPTWebImageModels) {
		t.Fatal("deployment/catalog drift")
	}
	models[0] = "changed"
	if chatGPTWebImageModels[0] != "gpt-image-2" {
		t.Fatal("deployment response mutated shared catalog")
	}
}
