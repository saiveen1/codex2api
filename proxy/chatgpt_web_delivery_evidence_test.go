package proxy

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
)

func TestChatGPTWebMultiAssetTerminalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, code       string
		success, unknown bool
	}{
		{"duplicate", "", true, false}, {"missing_submission_evidence", "worker_protocol_error", false, false}, {"corrupt_extra", "image_download_failed", false, false}, {"undeclared_extra", "worker_protocol_error", false, false}, {"refused_after_image", "content_policy_violation", false, false}, {"eof_with_image", "image_stream_interrupted", false, false}, {"eof_before_evidence", "submission_unknown", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWebFixture(t, func(w http.ResponseWriter, _ *http.Request, _ chatGPTWebRequest) {
				enc := json.NewEncoder(w)
				if tc.name == "eof_before_evidence" {
					return
				}
				ids := []string{"file_a", "file_b"}
				if tc.name != "missing_submission_evidence" {
					for _, stage := range []string{"submitted", "generated", "downloading"} {
						event := chatGPTWebEvent{Type: "stage", Stage: stage, ConversationID: "fixture"}
						if stage != "submitted" {
							event.AssetIDs = ids
						}
						_ = enc.Encode(event)
					}
				}
				im := chatGPTWebImage{B64JSON: tinyPNGBase64, MIMEType: "image/png", Width: 1, Height: 1, AssetID: ids[0]}
				_ = enc.Encode(chatGPTWebEvent{Type: "image", Image: &im})
				if tc.name == "eof_with_image" {
					return
				}
				if tc.name == "refused_after_image" {
					_ = enc.Encode(chatGPTWebEvent{Type: "error", Code: "content_policy_violation", HTTPStatus: 400})
					return
				}
				if tc.name != "duplicate" {
					im.AssetID = ids[1]
				}
				if tc.name == "corrupt_extra" {
					im.B64JSON = "bad"
				}
				if tc.name == "undeclared_extra" {
					im.AssetID = "file_not_declared"
				}
				_ = enc.Encode(chatGPTWebEvent{Type: "image", Image: &im})
				fmt.Fprintln(w, `{"type":"completed"}`)
			})
			r := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "same")
			body := r.Body.String()
			if tc.success {
				if r.Code != 200 || gjson.Get(body, "data.#").Int() != 1 {
					t.Fatal(body)
				}
				return
			}
			if r.Code == 200 || gjson.Get(body, "data").Exists() || gjson.Get(body, "error.code").String() != tc.code || !gjson.Get(body, "error.result_unknown").Exists() || gjson.Get(body, "error.result_unknown").Bool() != tc.unknown || gjson.Get(body, "error.execution_id").String() == "" || gjson.Get(body, "error.last_confirmed_stage").String() == "" {
				t.Fatal(body)
			}
			if gjson.Get(body, "error.validated_image").Bool() == tc.unknown {
				t.Fatal("incorrect evidence: " + body)
			}
			if tc.name == "eof_with_image" {
				f.db.FlushUsageLogs()
				raw, err := sql.Open("sqlite", f.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				var saved string
				if err := raw.QueryRow(`SELECT diagnostic_context FROM usage_logs WHERE account_id=? AND status_code>=400 ORDER BY id DESC LIMIT 1`, f.account.ID()).Scan(&saved); err != nil {
					t.Fatal(err)
				}
				if gjson.Get(saved, "execution_id").String() != gjson.Get(body, "error.execution_id").String() || gjson.Get(saved, "last_confirmed_stage").String() != "downloading" || gjson.Get(saved, "error_code").String() != tc.code || gjson.Get(saved, "outcome").String() != "failed" {
					t.Fatalf("missing durable evidence: %s", saved)
				}
			}
			again := f.request(t, "/v1/images/generations", `{"prompt":"square"}`, "same")
			if again.Code != 409 || f.calls.Load() != 1 {
				t.Fatal("replayed failed/unknown execution")
			}
		})
	}
}
