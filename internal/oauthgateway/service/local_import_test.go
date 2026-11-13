package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

func TestLocalImportRejectionRequiresExplicitDiscard(t *testing.T) {
	for _, code := range []string{"existing_account_requires_review", "gateway_identity_mismatch", "invalid_gateway_account"} {
		t.Run(code, func(t *testing.T) {
			f := newPair(t)
			a, _, _ := configuredApp(t, f)
			a.target = summaryTarget{Target: f, result: upstream.ImportResult{Total: 1, Failed: 1}, err: LocalImportError(code)}
			input := exampleInput("rejected")
			if _, _, err := a.Create(input); err != nil {
				t.Fatal(err)
			}
			view := drive(t, a, input.RequestID)
			if view.Imported != 0 || view.Failed != 1 || view.CanRetryImport || !view.CanArchive || !view.RetainedMaterial || view.Groups[0].Items[0].ImportState != "import_rejected" {
				t.Fatalf("rejection lost: %+v", view)
			}
			if view.Groups[0].Items[0].Error.Code != code {
				t.Fatal("local error lost its classification")
			}
			if _, err := a.Archive(input.RequestID, false); err == nil {
				t.Fatal("result discarded without confirmation")
			}
			if _, err := a.Archive(input.RequestID, true); err != nil {
				t.Fatal(err)
			}
			if f.submits != 1 {
				t.Fatal("rejection resubmitted OAuth")
			}
		})
	}
}
func TestLocalImportErrorNeverEchoesArbitraryMessage(t *testing.T) {
	local := LocalImportError("existing_account_requires_review")
	local.Message = "synthetic-sensitive-token"
	problem := publicError(local)
	if problem.Code != local.Code || problem.Source != "codex" || strings.Contains(problem.Message, "synthetic-sensitive-token") {
		t.Fatal("unsafe local projection")
	}
	raw, _ := json.Marshal(local)
	var decoded upstream.Error
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if localImportProblem(&decoded) != nil {
		t.Fatal("external response spoofed local adapter error")
	}
	if !LocalImportError("unknown-local-code").Uncertain {
		t.Fatal("unknown local error permitted retry")
	}
}
