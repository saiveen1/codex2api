package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestBatchItemFailureEvidenceRoundTrip(t *testing.T) {
	const base = `{"state":"failed","progress":%s,"error":{"code":"oauth_failed","message":"secret-synthetic","source":"execution","action":"manual_review","retryable":false,"retry_after":null,"evidence":%s}}`
	for _, tc := range []struct {
		name, progress, evidence, stage string
		want                            *FailureEvidence
	}{
		{"expired sign-in", `{"stage":"totp_verify","session":"secret-synthetic"}`, `{"http_status":409,"response_error_code":"invalid_state","page_type":"api_accounts_mfa_verify","response_error_message":"secret-synthetic"}`, "totp_verify", &FailureEvidence{"invalid_state", 409, "api_accounts_mfa_verify"}},
		{"legacy", `null`, `null`, "", nil},
		{"invalid optional fields", `true`, `{"http_status":"409","response_error_code":"secret-synthetic","page_type":"secret-synthetic"}`, "", nil},
		{"partial evidence", `{"stage":"secret-synthetic"}`, `{"http_status":901,"response_error_code":"invalid_credentials","page_type":false}`, "", &FailureEvidence{UpstreamCode: "invalid_credentials"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(fmt.Sprintf(base, tc.progress, tc.evidence))
			for i := 0; i < 2; i++ {
				var item BatchItem
				if err := json.Unmarshal(data, &item); err != nil {
					t.Fatal(err)
				}
				stage := ""
				if item.Progress != nil {
					stage = item.Progress.Stage
				}
				if item.State != "failed" || stage != tc.stage || item.Error == nil || item.Error.Code != "oauth_failed" || item.Error.Retryable {
					t.Fatalf("unexpected item: %+v", item)
				}
				got := item.Error.Evidence
				if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
					t.Fatalf("evidence = %+v, want %+v", got, tc.want)
				}
				var err error
				data, err = json.Marshal(item)
				if err != nil || strings.Contains(string(data), "secret-synthetic") {
					t.Fatal("unsafe persisted item", err)
				}
			}
		})
	}
}
