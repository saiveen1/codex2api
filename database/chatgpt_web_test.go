package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newChatGPTWebTestDB(t *testing.T) (*DB, string, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "web.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-db.logStop: // Restart tests explicitly close the first DB handle.
			return
		default:
			_ = db.Close()
		}
	})
	id, err := db.InsertAccountWithCredentials(context.Background(), "web-test", map[string]any{"access_token": "test-AT", "refresh_token": "test-RT", "plan_type": "free", "account_id": "workspace-base"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db, path, id
}

func TestChatGPTWebChannelLimits(t *testing.T) {
	for _, raw := range []string{"chatgpt_web", " ChatGPT_Web "} {
		l := APIKeyLimits{UpstreamChannel: raw}
		if l.ResolveUpstreamChannel() != UpstreamChannelChatGPTWeb || l.IsZero() {
			t.Fatalf("web channel lost: %#v", l)
		}
		if got := decodeAPIKeyLimits(encodeAPIKeyLimits(l)); got.ResolveUpstreamChannel() != UpstreamChannelChatGPTWeb {
			t.Fatalf("roundtrip lost channel: %#v", got)
		}
	}
	for _, raw := range []string{"", "auto", "unknown"} {
		if got := (APIKeyLimits{UpstreamChannel: raw}).ResolveUpstreamChannel(); got != UpstreamChannelAuto {
			t.Fatalf("native default changed: %q", got)
		}
	}
	if got := (APIKeyLimits{UpstreamChannel: "codex"}).ResolveUpstreamChannel(); got != UpstreamChannelCodex {
		t.Fatal("codex changed")
	}
}

func TestChatGPTWebStateEnableAndCAS(t *testing.T) {
	db, _, id := newChatGPTWebTestDB(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := db.ensureChatGPTWebSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.GetChatGPTWebAccountState(ctx, id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing should be disabled: %v", err)
	}
	if states, err := db.ListChatGPTWebAccountStates(ctx); err != nil || len(states) != 0 {
		t.Fatalf("migration backfilled accounts: %+v %v", states, err)
	}
	s, err := db.SetChatGPTWebAccountEnabled(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "unverified" || s.ObservedAt != nil || len(s.Capabilities) != 0 || s.ImageQuotaRemaining != nil {
		t.Fatalf("enabling forged ready: %+v", s)
	}
	if _, err := uuid.Parse(s.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(s.SessionID); err != nil {
		t.Fatal(err)
	}
	device, session, env := s.DeviceID, s.SessionID, s.EnvironmentRevision
	quota := 3
	observed := time.Now().UTC().Add(-time.Second)
	s.Status, s.Capabilities, s.ImageQuotaRemaining, s.ObservedAt, s.ProxyFingerprint = "ready", []string{"edit", "generate"}, &quota, &observed, chatGPTWebHash("direct")
	s.WorkspaceFingerprint = chatGPTWebHash("workspace-base")
	s.WorkspaceID, s.WorkspaceBindingFingerprint = "workspace-base", chatGPTWebHash("workspace-base")
	forged := *s
	forged.ImageQuotaRemaining = nil
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, &forged, "test-AT"); ok || err == nil {
		t.Fatal("ready with unknown quota accepted")
	}
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, s, "test-AT"); !ok || err != nil {
		t.Fatalf("observation: %v %v", ok, err)
	}
	current, err := db.GetChatGPTWebAccountState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if current.CredentialRevision != chatGPTWebHash("test-AT") || current.WorkspaceFingerprint != chatGPTWebHash("workspace-base") || current.Status != "ready" {
		t.Fatalf("bad observation: %+v", current)
	}
	noop, err := db.SetChatGPTWebAccountEnabled(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if noop.Status != "ready" || noop.EnvironmentRevision != env || noop.DeviceID != device || noop.SessionID != session {
		t.Fatal("no-op invalidated/rotated environment")
	}
	older := observed.Add(-time.Minute)
	stale := *s
	stale.ObservedAt = &older
	stale.Status = "challenge"
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, &stale, "test-AT"); ok || err != nil {
		t.Fatalf("stale observation: %v %v", ok, err)
	}
	if err := db.UpdateCredentials(ctx, id, map[string]any{"access_token": "new-AT"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, s, "test-AT"); ok || err != nil {
		t.Fatalf("stale token won: %v %v", ok, err)
	}
	after, err := db.GetChatGPTWebAccountState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, current) {
		t.Fatal("stale checks overwrote observation")
	}
	disabled, err := db.SetChatGPTWebAccountEnabled(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled || disabled.Status != "disabled" || disabled.EnvironmentRevision != env+1 || disabled.ObservedAt != nil {
		t.Fatalf("bad disabled state: %+v", disabled)
	}
	reenabled, err := db.SetChatGPTWebAccountEnabled(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if reenabled.Status != "unverified" || reenabled.EnvironmentRevision != env+2 || reenabled.DeviceID != device || reenabled.SessionID != session {
		t.Fatalf("bad reenable: %+v", reenabled)
	}
	s.CredentialRevision = ""
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, s, "new-AT"); ok || err != nil {
		t.Fatalf("stale environment won: %v %v", ok, err)
	}
	if _, err := db.SetChatGPTWebAccountEnabled(ctx, id+1000, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing account accepted: %v", err)
	}
	for _, upstream := range []string{"openai-responses", "grok", "claude", "antigravity"} {
		other, err := db.InsertAccountWithCredentials(ctx, upstream, map[string]any{"upstream_type": upstream, "access_token": "not-web"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.SetChatGPTWebAccountEnabled(ctx, other, true); err == nil {
			t.Fatalf("incompatible %q enabled", upstream)
		}
	}
}

func TestChatGPTWebWorkspaceObservationCAS(t *testing.T) {
	db, _, id := newChatGPTWebTestDB(t)
	ctx := context.Background()
	s, err := db.SetChatGPTWebAccountEnabled(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAccountCustomHeaders(ctx, id, map[string]string{"cHaTgPt-aCcOuNt-iD": " workspace-A "}); err != nil {
		t.Fatal(err)
	}
	quota := 2
	t0 := time.Now().Add(-time.Minute)
	s.Status, s.Capabilities, s.ImageQuotaRemaining, s.ObservedAt = "ready", []string{"generate"}, &quota, &t0
	s.ProxyFingerprint, s.WorkspaceFingerprint = chatGPTWebHash("direct"), chatGPTWebHash("workspace-A")
	s.WorkspaceID, s.WorkspaceBindingFingerprint = "workspace-A", chatGPTWebHash("workspace-A")
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, s, "test-AT"); !ok || err != nil {
		t.Fatalf("workspace A: %v %v", ok, err)
	}
	before, err := db.GetChatGPTWebAccountState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if before.WorkspaceFingerprint != chatGPTWebHash("workspace-A") {
		t.Fatal("workspace fingerprint did not roundtrip")
	}
	if err := db.UpdateAccountCustomHeaders(ctx, id, map[string]string{"CHATGPT-ACCOUNT-ID": "workspace-B"}); err != nil {
		t.Fatal(err)
	}
	t1 := time.Now().Add(-30 * time.Second)
	s.ObservedAt = &t1
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, s, "test-AT"); ok || err != nil {
		t.Fatalf("header-only stale workspace A accepted: %v %v", ok, err)
	}
	after, err := db.GetChatGPTWebAccountState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("stale workspace overwrote existing observation")
	}
	s.WorkspaceFingerprint = chatGPTWebHash("workspace-B")
	s.WorkspaceID, s.WorkspaceBindingFingerprint = "workspace-B", chatGPTWebHash("workspace-B")
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, s, "test-AT"); !ok || err != nil {
		t.Fatalf("workspace B: %v %v", ok, err)
	}
	if err := db.UpdateAccountCustomHeaders(ctx, id, map[string]string{"chatgpt-account-id": "workspace-A", "Chatgpt-Account-Id": "workspace-B"}); err != nil {
		t.Fatal(err)
	}
	t2 := time.Now()
	s.ObservedAt = &t2
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, s, "test-AT"); ok || err != nil {
		t.Fatalf("ambiguous override accepted: %v %v", ok, err)
	}
	if err := db.UpdateAccountCustomHeaders(ctx, id, map[string]string{"CHATGPT-ACCOUNT-ID": "workspace-B"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateCredentials(ctx, id, map[string]any{"account_id": ""}); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.SaveChatGPTWebAccountObservation(ctx, s, "test-AT"); !ok || err != nil {
		t.Fatalf("verified explicit workspace should not require a legacy base account_id: %v %v", ok, err)
	}
}

func TestChatGPTWebWorkspaceLegacyMigration(t *testing.T) {
	db, path, id := newChatGPTWebTestDB(t)
	ctx := context.Background()
	if _, err := db.SetChatGPTWebAccountEnabled(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	e := chatGPTWebTestExecution(t, db, id, "legacy-exec")
	if _, err := db.conn.ExecContext(ctx, `UPDATE chatgpt_web_account_states SET status='ready', workspace_fingerprint=''; ALTER TABLE chatgpt_web_account_states DROP COLUMN workspace_fingerprint; ALTER TABLE chatgpt_web_executions DROP COLUMN workspace_fingerprint`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.ensureChatGPTWebSchema(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := reopened.GetChatGPTWebAccountState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "unverified" || s.WorkspaceFingerprint != "" {
		t.Fatalf("legacy state not invalidated: %+v", s)
	}
	stored, err := reopened.GetChatGPTWebExecution(ctx, e.ID, e.APIKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WorkspaceFingerprint != "" || stored.Stage != "prepared" {
		t.Fatalf("legacy execution changed: %+v", stored)
	}
	stored.Stage = "submitting"
	if ok, err := reopened.SaveChatGPTWebExecution(ctx, stored, "prepared"); ok || err == nil {
		t.Fatal("legacy unbound execution resumed")
	}
}

func TestChatGPTWebLeaseConcurrencyAndExpiry(t *testing.T) {
	db, path, id := newChatGPTWebTestDB(t)
	ctx := context.Background()
	peer, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	until := time.Now().Add(time.Minute)
	var wins atomic.Int32
	var winner atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn := db
			if i%2 == 1 {
				conn = peer
			}
			ok, err := conn.AcquireChatGPTWebAccountLease(ctx, id, fmt.Sprintf("owner-%d", i), until)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				wins.Add(1)
				winner.Store(int32(i))
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("exclusive lease winners=%d", wins.Load())
	}
	owner := fmt.Sprintf("owner-%d", winner.Load())
	if err := peer.ReleaseChatGPTWebAccountLease(ctx, id, "wrong-owner"); err != nil {
		t.Fatal(err)
	}
	if ok, err := peer.AcquireChatGPTWebAccountLease(ctx, id, "replacement", until); ok || err != nil {
		t.Fatalf("wrong-owner release worked: %v %v", ok, err)
	}
	if ok, err := peer.AcquireChatGPTWebAccountLease(ctx, id, owner, until.Add(time.Minute)); !ok || err != nil {
		t.Fatalf("renewal failed: %v %v", ok, err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE chatgpt_web_account_leases SET expires_at=$2 WHERE account_id=$1`, id, time.Now().Add(-time.Second).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if ok, err := peer.AcquireChatGPTWebAccountLease(ctx, id, "replacement", until); !ok || err != nil {
		t.Fatalf("expired lease not claimable: %v %v", ok, err)
	}
	if err := db.ReleaseChatGPTWebAccountLease(ctx, id, owner); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.AcquireChatGPTWebAccountLease(ctx, id, "third-owner", until); ok || err != nil {
		t.Fatalf("stale release removed new owner: %v %v", ok, err)
	}
	if err := peer.ReleaseChatGPTWebAccountLease(ctx, id, "replacement"); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.AcquireChatGPTWebAccountLease(ctx, id, "third-owner", until); !ok || err != nil {
		t.Fatalf("release failed: %v %v", ok, err)
	}
}

func chatGPTWebTestExecution(t *testing.T, db *DB, id int64, name string) *ChatGPTWebExecution {
	t.Helper()
	e := &ChatGPTWebExecution{ID: name, APIKeyID: 42, AccountID: id, RequestHash: chatGPTWebHash("request"), CredentialRevision: chatGPTWebHash("test-AT"), EnvironmentRevision: 1, ProxyFingerprint: chatGPTWebHash("direct"), WorkspaceFingerprint: chatGPTWebHash("workspace-base"), Operation: "generate", Stage: "prepared", LeaseOwner: "run-1", LeaseUntil: time.Now().UTC().Add(time.Minute)}
	if ok, err := db.AcquireChatGPTWebAccountLease(context.Background(), id, e.LeaseOwner, e.LeaseUntil); !ok || err != nil {
		t.Fatalf("lease: %v %v", ok, err)
	}
	if ok, err := db.CreateChatGPTWebExecution(context.Background(), e); !ok || err != nil {
		t.Fatalf("create: %v %v", ok, err)
	}
	return e
}

func TestChatGPTWebExecutionBindingsAndCAS(t *testing.T) {
	db, _, id := newChatGPTWebTestDB(t)
	ctx := context.Background()
	e := chatGPTWebTestExecution(t, db, id, "exec-1")
	if ok, err := db.CreateChatGPTWebExecution(ctx, e); ok || err != nil {
		t.Fatalf("duplicate: %v %v", ok, err)
	}
	mutations := []func(*ChatGPTWebExecution){func(e *ChatGPTWebExecution) { e.APIKeyID++ }, func(e *ChatGPTWebExecution) { e.AccountID++ }, func(e *ChatGPTWebExecution) { e.RequestHash = chatGPTWebHash("other") }, func(e *ChatGPTWebExecution) { e.CredentialRevision = chatGPTWebHash("new-AT") }, func(e *ChatGPTWebExecution) { e.EnvironmentRevision++ }, func(e *ChatGPTWebExecution) { e.ProxyFingerprint = chatGPTWebHash("other-exit") }, func(e *ChatGPTWebExecution) { e.WorkspaceFingerprint = chatGPTWebHash("other-workspace") }, func(e *ChatGPTWebExecution) { e.Operation = "edit" }}
	for i, mutate := range mutations {
		other := *e
		mutate(&other)
		if ok, err := db.CreateChatGPTWebExecution(ctx, &other); ok || !errors.Is(err, ErrChatGPTWebExecutionConflict) {
			t.Fatalf("binding %d accepted: %v %v", i, ok, err)
		}
		other.Stage = "submitting"
		if ok, err := db.SaveChatGPTWebExecution(ctx, &other, "prepared"); ok || !errors.Is(err, ErrChatGPTWebExecutionConflict) {
			t.Fatalf("save binding %d accepted: %v %v", i, ok, err)
		}
	}
	if _, err := db.GetChatGPTWebExecution(ctx, e.ID, e.APIKeyID+1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("key leaked execution: %v", err)
	}
	if _, err := db.GetChatGPTWebExecution(ctx, e.ID, 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("zero key bypass")
	}
	other := *e
	other.LeaseOwner = "intruder"
	other.Stage = "submitting"
	if ok, err := db.SaveChatGPTWebExecution(ctx, &other, "prepared"); ok || err != nil {
		t.Fatalf("live owner replaced: %v %v", ok, err)
	}
	e.Stage = "submitting"
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := db.SaveChatGPTWebExecution(ctx, e, "prepared"); err != nil {
				t.Error(err)
			} else if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("stage CAS winners=%d", wins.Load())
	}
	rollback := *e
	rollback.Stage = "prepared"
	if ok, err := db.SaveChatGPTWebExecution(ctx, &rollback, "submitting"); ok || !errors.Is(err, ErrChatGPTWebTransition) {
		t.Fatalf("stage rollback: %v %v", ok, err)
	}
	for _, stage := range []string{"submitted", "generated", "downloading", "completed"} {
		previous := e.Stage
		e.Stage = stage
		e.ConversationID = "conversation-1"
		e.TaskID = "task-1"
		if stage != "submitted" {
			e.AssetIDs = []string{"file-output-1", "file-output-2"}
		}
		if ok, err := db.SaveChatGPTWebExecution(ctx, e, previous); !ok || err != nil {
			t.Fatalf("%s -> %s: %v %v", previous, stage, ok, err)
		}
	}
	saved, err := db.GetChatGPTWebExecution(ctx, e.ID, e.APIKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Stage != "completed" || len(saved.AssetIDs) != 2 || saved.CreatedAt.IsZero() {
		t.Fatalf("bad ledger: %+v", saved)
	}
	if ok, err := db.CreateChatGPTWebExecution(ctx, saved); ok || err != nil {
		t.Fatalf("completed duplicate was not idempotent: %v %v", ok, err)
	}
	rows, err := db.ListChatGPTWebExecutions(ctx, id, 1000)
	if err != nil || len(rows) != 1 {
		t.Fatalf("admin list: %+v %v", rows, err)
	}
	rows, err = db.ListChatGPTWebExecutions(ctx, id+100, 1)
	if err != nil || len(rows) != 0 {
		t.Fatal("account list isolation")
	}
	bad := *e
	bad.ID = "bad-url"
	bad.Stage = "prepared"
	bad.AssetIDs = []string{"https://cdn.example/secret?signature=bad"}
	if ok, err := db.CreateChatGPTWebExecution(ctx, &bad); ok || err == nil {
		t.Fatal("raw URL accepted")
	}
}

func TestChatGPTWebExecutionRestartSubmissionFence(t *testing.T) {
	db, path, id := newChatGPTWebTestDB(t)
	ctx := context.Background()
	e := chatGPTWebTestExecution(t, db, id, "restart-exec")
	e.Stage = "submitting"
	if ok, err := db.SaveChatGPTWebExecution(ctx, e, "prepared"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	past := time.Now().Add(-time.Minute).UnixNano()
	if _, err := db.conn.ExecContext(ctx, `UPDATE chatgpt_web_account_leases SET expires_at=$1; UPDATE chatgpt_web_executions SET lease_until=$1`, past); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.ensureChatGPTWebSchema(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := reopened.GetChatGPTWebExecution(ctx, e.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if current.Stage != "submitting" {
		t.Fatalf("restart reset fence: %+v", current)
	}
	current.LeaseOwner = "run-2"
	current.LeaseUntil = time.Now().Add(time.Minute)
	if ok, err := reopened.AcquireChatGPTWebAccountLease(ctx, id, current.LeaseOwner, current.LeaseUntil); !ok || err != nil {
		t.Fatal(ok, err)
	}
	current.Stage = "prepared"
	if ok, err := reopened.SaveChatGPTWebExecution(ctx, current, "submitting"); ok || !errors.Is(err, ErrChatGPTWebTransition) {
		t.Fatalf("restart permits submit retry: %v %v", ok, err)
	}
	current.Stage = "submitting"
	if ok, err := reopened.SaveChatGPTWebExecution(ctx, current, "submitting"); ok || !errors.Is(err, ErrChatGPTWebTransition) {
		t.Fatal("restart renewed ambiguous submitting")
	}
	current.Stage = "unknown"
	current.ErrorCode = "submission_unknown"
	if ok, err := reopened.SaveChatGPTWebExecution(ctx, current, "submitting"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	current, err = reopened.GetChatGPTWebExecution(ctx, e.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !current.ResultUnknown {
		t.Fatal("unknown lost")
	}
	current.Stage = "prepared"
	if ok, err := reopened.SaveChatGPTWebExecution(ctx, current, "unknown"); ok || !errors.Is(err, ErrChatGPTWebTransition) {
		t.Fatal("unknown permits submission reset")
	}
}

func TestChatGPTWebExecutionDownloadOnlyRecovery(t *testing.T) {
	db, _, id := newChatGPTWebTestDB(t)
	ctx := context.Background()
	e := chatGPTWebTestExecution(t, db, id, "recover-assets")
	for _, stage := range []string{"submitting", "submitted", "generated", "downloading", "failed"} {
		prev := e.Stage
		e.Stage = stage
		if stage == "submitted" {
			e.ConversationID = "conversation-1"
		}
		if stage == "generated" {
			e.AssetIDs = []string{"file-output-1"}
		}
		if ok, err := db.SaveChatGPTWebExecution(ctx, e, prev); !ok || err != nil {
			t.Fatal(stage, ok, err)
		}
	}
	e.Stage = "downloading"
	if ok, err := db.SaveChatGPTWebExecution(ctx, e, "failed"); !ok || err != nil {
		t.Fatalf("known asset resume: %v %v", ok, err)
	}
	e.Stage = "completed"
	e.AssetIDs = nil
	if ok, err := db.SaveChatGPTWebExecution(ctx, e, "downloading"); ok || !errors.Is(err, ErrChatGPTWebExecutionConflict) {
		t.Fatal("known IDs removed")
	}
}
