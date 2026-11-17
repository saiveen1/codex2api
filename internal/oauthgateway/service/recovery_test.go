package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

type recoveryTestTarget struct {
	*fakePair
	snapshot                RecoverySnapshot
	unavailable             bool
	unknown                 bool
	refreshEligible         bool
	refreshCalls, recovered int
}

func (r *recoveryTestTarget) RecoverySnapshot(context.Context, int64) (RecoverySnapshot, error) {
	if r.unavailable {
		return RecoverySnapshot{}, LocalImportError("recovery_account_unavailable")
	}
	return r.snapshot, nil
}
func (r *recoveryTestTarget) RefreshBeforeRecovery(context.Context, RecoverySnapshot) (bool, error) {
	r.refreshCalls++
	return r.refreshEligible, nil
}
func (r *recoveryTestTarget) Recover(_ context.Context, expected RecoverySnapshot, _ json.RawMessage, _ string) (upstream.ImportResult, error) {
	if r.unavailable || expected != r.snapshot {
		return upstream.ImportResult{Total: 1, Failed: 1}, LocalImportError("recovery_credentials_changed")
	}
	r.recovered++
	if r.unknown {
		return upstream.ImportResult{}, LocalImportError("import_outcome_unknown")
	}
	r.snapshot.Generation++
	r.snapshot.CredentialVersion = "recovered"
	return upstream.ImportResult{Total: 1, Updated: 1}, nil
}
func (r *recoveryTestTarget) Import(ctx context.Context, raw json.RawMessage, ids []int64, proxy string) (upstream.ImportResult, error) {
	result, err := r.fakePair.Import(ctx, raw, ids, proxy)
	if err == nil && result.Success == 1 {
		result.CreatedIDs = []int64{r.snapshot.AccountID}
	}
	return result, err
}
func newRecoveryFixture(t *testing.T) (*App, *memoryStore, *recoveryTestTarget) {
	t.Helper()
	f := newPair(t)
	a, s, _ := configuredApp(t, f)
	target := &recoveryTestTarget{fakePair: f, snapshot: RecoverySnapshot{AccountID: 42, Identifier: "one@example.invalid", Identity: upstream.Identity{ChatGPTUserID: "user-original", ChatGPTAccountID: "workspace-original"}, Generation: 1, Family: "family-original", CredentialVersion: "original"}, refreshEligible: true}
	a.target = target
	return a, s, target
}
func enrollRecovery(t *testing.T, a *App) {
	t.Helper()
	in := exampleInput("enroll")
	in.SaveCredentials = true
	if _, _, err := a.Create(in); err != nil {
		t.Fatal(err)
	}
	if v := drive(t, a, in.RequestID); v.Imported != 1 {
		t.Fatal(v.State)
	}
}
func signalRecovery(t *testing.T, a *App, refresh bool) string {
	t.Helper()
	a.RequestRecovery(42, 1, refresh)
	for range 2 {
		if _, err := a.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	s, err := a.store.View()
	if err != nil || len(s.Recoveries) != 1 || s.Recoveries[0].State != "recovering" {
		t.Fatal("not queued", err)
	}
	return s.Recoveries[0].JobID
}

func TestOAuthRecoveryIdentifiersLegacyAndNoSecrets(t *testing.T) {
	a, s, _ := newRecoveryFixture(t)
	in := exampleInput("legacy-identifiers")
	in.Accounts = append(in.Accounts, InputAccount{Identifier: "+12025550123", Password: "phone-password", TOTPSecret: "JBSWY3DPEHPK3PXP"})
	p, err := a.Preview(context.Background(), in)
	if err != nil || p.Rows[0].Identifier != "one@example.invalid" || p.Rows[1].Identifier != "+12025550123" {
		t.Fatal("preview identifiers", err)
	}
	if _, _, err = a.Create(in); err != nil {
		t.Fatal(err)
	}
	drive(t, a, in.RequestID)
	old, _ := s.View()
	old.Version = 2
	restarted, err := New(&memoryStore{state: old}, a.target, false)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	v, err := restarted.Job(in.RequestID)
	if err != nil || v.Groups[0].Items[0].Identifier != "one@example.invalid" || v.Groups[0].Items[1].Identifier != "+12025550123" {
		t.Fatal("legacy mapping lost")
	}
	raw, _ := json.Marshal(v)
	for _, secret := range []string{"keep spaces", "phone-password", "JBSWY3DPEHPK3PXP", "synthetic-result-token", "synthetic-oauth", "expected_identity"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("public detail leaked secret")
		}
	}
}

func TestOAuthRecoveryOptInArchiveRestartDisable(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "opt-in"}[optIn], func(t *testing.T) {
			a, s, target := newRecoveryFixture(t)
			in := exampleInput("retention")
			in.SaveCredentials = optIn
			if _, _, err := a.Create(in); err != nil {
				t.Fatal(err)
			}
			drive(t, a, in.RequestID)
			state, _ := s.View()
			if state.Jobs[0].Groups[0].Request.Accounts[0].Password != "" {
				t.Fatal("job retained password")
			}
			if a.RecoveryEnabled(42) != optIn || (len(state.Recoveries) == 1) != optIn {
				t.Fatal("incorrect opt-in")
			}
			if _, err := a.Archive(in.RequestID, false); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(s.reopen(t), target, false)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if restarted.RecoveryEnabled(42) != optIn {
				t.Fatal("restart lost consent")
			}
			if !optIn {
				list, _ := restarted.Recoveries()
				if list == nil || len(list) != 0 {
					t.Fatal("empty recovery must be []")
				}
				return
			}
			state, _ = restarted.store.View()
			if state.Recoveries[0].Material.Password != " keep spaces " {
				t.Fatal("archive lost recovery material")
			}
			if err = restarted.DisableRecovery(42); err != nil {
				t.Fatal(err)
			}
			state, _ = restarted.store.View()
			r := state.Recoveries[0]
			if r.Enabled || r.Material.Password != "" || r.Material.TOTPSecret != "" || r.Options.Proxy != nil {
				t.Fatal("disable retained secrets")
			}
			restarted.RequestRecovery(42, 1, false)
			if worked, err := restarted.Step(context.Background()); err != nil || worked {
				t.Fatal("disabled recovery scheduled")
			}
		})
	}
}

func TestOAuthRecoveryOriginalIdentityAndConcurrentDedup(t *testing.T) {
	a, _, target := newRecoveryFixture(t)
	enrollRecovery(t, a)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() { defer wg.Done(); a.RequestRecovery(42, 1, false) }()
	}
	wg.Wait()
	for range 2 {
		if _, err := a.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := a.Recoveries()
	id := list[0].JobID
	v := drive(t, a, id)
	if v.Imported != 1 || !v.AutoRecovery || v.RecoveryAccountID != 42 || target.recovered != 1 || target.submits != 2 {
		t.Fatalf("restore outcome %+v", v)
	}
	for _, req := range target.requests {
		if req.RequestID == id && (req.Accounts[0].ExpectedIdentity == nil || *req.Accounts[0].ExpectedIdentity != target.snapshot.Identity) {
			t.Fatal("missing identity guard")
		}
	}
	list, _ = a.Recoveries()
	if list[0].Attempts != 1 || list[0].NextAttemptAt == nil {
		t.Fatal("attempt/cooldown not retained")
	}
	raw, _ := json.Marshal(list)
	if bytes.Contains(raw, []byte("JBSWY3DPEHPK3PXP")) || bytes.Contains(raw, []byte("keep spaces")) {
		t.Fatal("recovery view leaked login")
	}
}

func TestOAuthRecoveryRefreshBeforePasswordAndCapabilityGuard(t *testing.T) {
	a, _, target := newRecoveryFixture(t)
	enrollRecovery(t, a)
	target.refreshEligible = false
	a.RequestRecovery(42, 1, true)
	for range 2 {
		if _, err := a.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if target.refreshCalls != 1 || target.submits != 1 {
		t.Fatal("password login preceded normal refresh")
	}
	cap := capabilities()
	cap.Features["automatic_reauthorization"] = false
	if err := checkRecoveryCapability(cap); err != nil {
		t.Fatal("ordinary guarded password_totp wrongly disabled")
	}
	delete(cap.Features, "expected_identity_guard")
	if checkRecoveryCapability(cap) == nil {
		t.Fatal("unguarded recovery allowed")
	}
	cap = capabilities()
	cap.OAuthMethods = nil
	if checkRecoveryCapability(cap) == nil {
		t.Fatal("missing password_totp allowed")
	}
}

func TestOAuthRecoveryFencesInFlightResults(t *testing.T) {
	for _, scenario := range []string{"generation", "identity", "deleted-disabled", "disable", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			a, _, target := newRecoveryFixture(t)
			enrollRecovery(t, a)
			id := signalRecovery(t, a, false)
			stepNow(t, a, id)
			stepNow(t, a, id) // accepted authorization and durable delivery
			switch scenario {
			case "generation":
				target.snapshot.Generation++
			case "identity":
				target.snapshot.Identity.ChatGPTAccountID = "different-workspace"
			case "deleted-disabled":
				target.unavailable = true
			case "disable":
				if err := a.DisableRecovery(42); err != nil {
					t.Fatal(err)
				}
			case "cancel": // A succeeded remote batch is not cancellable, but the write fence still checks persisted cancellation.
				if err := a.changeJob(id, func(j *Job) error { j.CancelRequested = true; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			v := drive(t, a, id)
			if target.recovered != 0 || v.Imported != 0 {
				t.Fatal("cancelled/newer/deleted identity overwritten")
			}
		})
	}
}

func TestOAuthRecoveryRestartUnknownSubmitAndCommitNeverReplay(t *testing.T) {
	for _, unknownDB := range []bool{false, true} {
		t.Run(map[bool]string{false: "submit", true: "commit"}[unknownDB], func(t *testing.T) {
			a, s, target := newRecoveryFixture(t)
			enrollRecovery(t, a)
			target.lostSubmit = !unknownDB
			target.unknown = unknownDB
			id := signalRecovery(t, a, false)
			stepNow(t, a, id)
			if unknownDB {
				stepNow(t, a, id)
				stepNow(t, a, id)
			}
			restarted, err := New(s.reopen(t), target, false)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			v := drive(t, restarted, id)
			if unknownDB {
				if v.State != "needs_review" || v.CanRetryImport || target.recovered != 1 {
					t.Fatal("unknown commit became replayable")
				}
				if _, err := restarted.Action(id, "refresh"); err != nil {
					t.Fatal(err)
				}
				drive(t, restarted, id)
				if target.recovered != 1 {
					t.Fatal("DB write replayed")
				}
			} else if v.Imported != 1 || target.lookups != 1 {
				t.Fatal("unknown submit did not lookup original")
			}
			if target.submits != 2 {
				t.Fatal("authorization replayed")
			}
		})
	}
}

func TestOAuthRecoveryDestinationValidationAndAttemptLimit(t *testing.T) {
	a, s, _ := newRecoveryFixture(t)
	in := exampleInput("manual-repair")
	in.RecoveryAccountID = 42
	for _, proxy := range []string{"", "http://invalid.example.invalid:80"} {
		in.RuntimeProxy = proxy
		_, _, err := a.Create(in)
		var f *Fault
		if !errors.As(err, &f) || f.Code != "invalid_recovery_destination" {
			t.Fatal("destination change accepted", err)
		}
	}
	in.RuntimeProxy = ""
	in.GroupIDs = nil
	in.Accounts[0].Identifier = "other@example.invalid"
	if _, _, err := a.Create(in); err == nil {
		t.Fatal("identity conflict accepted")
	}
	enrollRecovery(t, a)
	if err := s.Update(func(s *State) error {
		s.Recoveries[0].Attempts = 3
		s.Recoveries[0].State = "pending"
		s.Recoveries[0].NextAttemptAt = time.Time{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, _ := a.Recoveries()
	if list[0].State != "manual_review" || list[0].Error.Code != "recovery_attempt_limit" {
		t.Fatal("unbounded automatic attempts")
	}
}

func TestOAuthRecoveryWorkerFailureKeepsCleanupProtection(t *testing.T) {
	a, store, _ := newRecoveryFixture(t)
	enrollRecovery(t, a)
	store.beforeCommit = func(*State) error { return errors.New("synthetic persistence failure") }
	a.RequestRecovery(42, 1, false)
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("worker did not stop on persistence failure")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if !a.RecoveryEnabled(42) || a.RecoveryEnabled(43) {
		t.Fatal("closed worker lost opt-in cleanup protection")
	}
	a.RequestRecovery(42, 1, false)
	if _, ok := a.signals.Load(int64(42)); ok {
		t.Fatal("closed worker accepted a signal")
	}
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("closed worker restarted")
	}
}
