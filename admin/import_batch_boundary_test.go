package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestFolderImportSharedIDAcrossBatches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	h := &Handler{db: db, store: auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2}),
		probeUsage: func(context.Context, *auth.Account) error { return nil },
	}
	// Separate multipart requests are the actual boundary between folder batches.
	for _, name := range []string{"first", "second", "first"} {
		content := fmt.Sprintf(`{"accounts":[{"name":%q,"credentials":{"email":%q,"refresh_token":%q,"access_token":%q,"chatgpt_account_id":"shared-export-id"}}]}`, name, name+"@example.invalid", "fixture-rt-"+name, "fixture-at-"+name)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = newMultipartJSONRequest(t, name+".json", content)
		h.importAccountsJSON(c, importSettings{})
		if w.Code != http.StatusOK {
			t.Fatalf("import %s: status %d", name, w.Code)
		}
	}
	rows, err := db.ListActive(context.Background())
	if err != nil || len(rows) != 2 {
		t.Fatalf("after two batches and replay: got %d rows, err %v", len(rows), err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		seen[row.GetCredential("refresh_token")] = true
	}
	if !seen["fixture-rt-first"] || !seen["fixture-rt-second"] {
		t.Fatal("distinct imported credentials were lost or overwritten")
	}
}
