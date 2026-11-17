package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestChatGPTWebPreparedSelectionRechecksRevokedKey(t *testing.T) {
	f := newWebFixture(t, nil)
	ctx := t.Context()
	if _, err := f.db.SetChatGPTWebAccountEnabled(ctx, f.account.ID(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetChatGPTWebAccountEnabled(ctx, f.account.ID(), true); err != nil {
		t.Fatal(err)
	}
	c := webAdmissionContext(f, ctx)
	if err := f.handler.prepareChatGPTWebAccounts(c, "generate", "gpt-image-2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(chatGPTWebAdmissionSelectionGuard); !ok {
		t.Fatal("missing post-check selection guard")
	}
	if err := f.db.UpdateAPIKey(ctx, f.key.ID, database.APIKeyUpdate{EnabledSet: true, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	account, _, _, err := f.handler.takeChatGPTWebAccount(c, "generate", "revoked-selection", time.Now().Add(time.Minute), 0)
	if err == nil || account != nil || f.calls.Load() != 0 {
		t.Fatal("revoked key selected an account after preparation")
	}
	if f.account.ActiveRequests.Load() != 0 || f.account.OccupiedRequests.Load() != 0 {
		t.Fatal("rejected selection leaked capacity")
	}
}
