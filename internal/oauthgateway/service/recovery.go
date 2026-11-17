package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

// RecoveryTarget never upserts: Recover must fence identity, generation, enabled
// state and credential version in the same transaction as its write.
type RecoveryTarget interface {
	RecoverySnapshot(context.Context, int64) (RecoverySnapshot, error)
	RefreshBeforeRecovery(context.Context, RecoverySnapshot) (bool, error)
	Recover(context.Context, RecoverySnapshot, json.RawMessage, string) (upstream.ImportResult, error)
}

type RecoverySnapshot struct {
	AccountID         int64             `json:"account_id"`
	Identifier        string            `json:"identifier"`
	Identity          upstream.Identity `json:"identity"`
	Generation        int64             `json:"generation"`
	Family            string            `json:"family"`
	CredentialVersion string            `json:"credential_version"`
}

type Recovery struct {
	Snapshot          RecoverySnapshot `json:"snapshot"`
	Material          upstream.Account `json:"material"`
	Options           upstream.Options `json:"options"`
	Settings          Settings         `json:"settings"`
	Enabled           bool             `json:"enabled"`
	Epoch             int64            `json:"epoch"`
	State             string           `json:"state"`
	Attempts          int              `json:"attempts"`
	PendingGeneration int64            `json:"pending_generation,omitempty"`
	NextAttemptAt     time.Time        `json:"next_attempt_at"`
	JobID             string           `json:"job_id,omitempty"`
	Error             *Problem         `json:"error,omitempty"`
}

type RecoveryView struct {
	AccountID     int64      `json:"account_id"`
	Identifier    string     `json:"identifier"`
	State         string     `json:"state"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	JobID         string     `json:"job_id,omitempty"`
	Error         *Problem   `json:"error,omitempty"`
	CanDisable    bool       `json:"can_disable"`
}

type recoverySignal struct {
	generation int64
	refresh    bool
}

func (a *App) RecoveryEnabled(id int64) bool {
	_, ok := a.enabled.Load(id)
	return ok
}

// RequestRecovery is safe inside runtime locks: no DB, encrypted store, network
// or waiting. At most one signal per opted-in account is retained.
func (a *App) RequestRecovery(id, generation int64, refresh bool) {
	if a.closed.Load() || !a.RecoveryEnabled(id) {
		return
	}
	a.signals.LoadOrStore(id, recoverySignal{generation, refresh})
	if a.closed.Load() {
		a.signals.Delete(id)
		return
	}
	a.notify()
}

func (a *App) Recoveries() ([]RecoveryView, error) {
	s, err := a.store.View()
	if err != nil {
		return nil, err
	}
	out := make([]RecoveryView, 0, len(s.Recoveries))
	for _, r := range s.Recoveries {
		v := RecoveryView{AccountID: r.Snapshot.AccountID, Identifier: r.Material.Identifier, State: r.State, Attempts: r.Attempts, JobID: r.JobID, Error: r.Error, CanDisable: r.Enabled}
		if v.Identifier == "" {
			v.Identifier = r.Snapshot.Identifier
		}
		if !r.NextAttemptAt.IsZero() {
			next := r.NextAttemptAt
			v.NextAttemptAt = &next
		}
		out = append(out, v)
	}
	return out, nil
}

func (a *App) DisableRecovery(id int64) error {
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	err := a.store.Update(func(s *State) error {
		found := false
		for i := range s.Recoveries {
			r := &s.Recoveries[i]
			if r.Snapshot.AccountID != id {
				continue
			}
			found = true
			r.Enabled, r.State, r.Error = false, "disabled", nil
			r.Epoch++
			r.Material.Password, r.Material.TOTPSecret = "", ""
			r.Options = upstream.Options{}
			r.Settings = Settings{}
			r.NextAttemptAt = time.Time{}
		}
		if !found {
			return fault(404, "not_found", "未找到该账号的恢复保管记录。")
		}
		for i := range s.Jobs {
			j := &s.Jobs[i]
			if j.Recovery == nil || j.Recovery.AccountID != id {
				continue
			}
			j.CancelRequested, j.SaveCredentials = true, false
			for g := range j.Groups {
				for k := range j.Groups[g].Request.Accounts {
					j.Groups[g].Request.Accounts[k].Password = ""
					j.Groups[g].Request.Accounts[k].TOTPSecret = ""
				}
				j.Groups[g].Request.Options.Proxy = nil
			}
		}
		return nil
	})
	if err == nil {
		a.enabled.Delete(id)
		a.signals.Delete(id)
		a.notify()
	}
	return err
}

func recoveryInFlight(j Job) bool {
	v := jobView(j)
	return v.CanRefresh || v.CanRetryImport || v.Pending > 0
}

func (a *App) bindRecovery(ctx context.Context, in Input, groups []Group) (*RecoverySnapshot, error) {
	if in.RecoveryAccountID == 0 {
		return nil, nil
	}
	target, ok := a.target.(RecoveryTarget)
	if !ok {
		return nil, fault(409, "recovery_unsupported", "当前本地目标不支持受控原账号恢复。")
	}
	snap, err := target.RecoverySnapshot(ctx, in.RecoveryAccountID)
	if err != nil {
		return nil, err
	}
	if len(groups) != 1 || len(groups[0].Request.Accounts) != 1 || !strings.EqualFold(groups[0].Request.Accounts[0].Identifier, snap.Identifier) {
		return nil, fault(409, "recovery_identity_mismatch", "材料标识与原账号不符，不能替换其他主体。")
	}
	if snap.Identity.ChatGPTUserID == "" || snap.Identity.ChatGPTAccountID == "" || snap.Generation <= 0 || snap.CredentialVersion == "" {
		return nil, fault(409, "recovery_identity_unverified", "无法证明原账号主体或工作区，需人工核验（manual_review）。")
	}
	groups[0].Request.Accounts[0].ExpectedIdentity = &snap.Identity
	return &snap, nil
}

func checkRecoveryCapability(cap upstream.Capabilities) error {
	if err := compatible(cap); err != nil {
		return err
	}
	if !cap.Features["expected_identity_guard"] {
		return fault(409, "recovery_identity_guard_required", "网关未声明原主体/工作区身份锁，需人工核验（manual_review），不能以 team_preferred 替代。")
	}
	return nil
}

func (a *App) checkRecoveryJob(ctx context.Context, job Job) error {
	if job.Recovery == nil {
		return nil
	}
	s, err := a.store.View()
	if err != nil {
		return err
	}
	for _, j := range s.Jobs {
		if j.ID == job.ID && j.CancelRequested {
			return LocalImportError("recovery_cancelled")
		}
	}
	if job.RecoveryEpoch > 0 {
		valid := false
		for _, r := range s.Recoveries {
			if r.Snapshot.AccountID == job.Recovery.AccountID && r.Enabled && r.Epoch == job.RecoveryEpoch {
				valid = true
			}
		}
		if !valid {
			return LocalImportError("recovery_cancelled")
		}
	}
	target, ok := a.target.(RecoveryTarget)
	if !ok {
		return LocalImportError("recovery_identity_unverified")
	}
	current, err := target.RecoverySnapshot(ctx, job.Recovery.AccountID)
	if err != nil {
		return err
	}
	if current != *job.Recovery {
		return LocalImportError("recovery_credentials_changed")
	}
	return nil
}

// stepRecovery consumes one signal or advances one persisted recovery. Unknown
// OAuth/DB outcomes remain attached to their original job, never a new attempt.
func (a *App) stepRecovery(ctx context.Context, state State) (bool, error) {
	target, ok := a.target.(RecoveryTarget)
	if !ok {
		return false, nil
	}
	var id int64
	var signal recoverySignal
	a.signals.Range(func(key, value any) bool { id = key.(int64); signal = value.(recoverySignal); return false })
	if id > 0 {
		a.signals.Delete(id)
		return true, a.store.Update(func(s *State) error {
			for i := range s.Recoveries {
				r := &s.Recoveries[i]
				if r.Snapshot.AccountID != id || !r.Enabled || r.State == "manual_review" || r.State == "recovering" || r.State == "pending" || r.State == "refreshing" {
					continue
				}
				if signal.generation < r.Snapshot.Generation {
					continue
				}
				r.State = "pending"
				r.PendingGeneration = signal.generation
				if signal.refresh {
					r.State = "refreshing"
				}
			}
			return nil
		})
	}
	for _, r := range state.Recoveries {
		if !r.Enabled {
			continue
		}
		if r.State == "recovering" {
			for _, j := range state.Jobs {
				if j.ID != r.JobID {
					continue
				}
				v := jobView(j)
				if v.State != "needs_review" && recoveryInFlight(j) {
					break
				}
				return true, a.store.Update(func(s *State) error {
					for i := range s.Recoveries {
						rr := &s.Recoveries[i]
						if rr.Snapshot.AccountID != r.Snapshot.AccountID || rr.Epoch != r.Epoch || !rr.Enabled {
							continue
						}
						if v.State == "completed" {
							rr.State = "ready"
							rr.Error = nil
						} else {
							rr.State = "manual_review"
							rr.Error = recoveryJobProblem(j)
						}
					}
					return nil
				})
			}
			continue
		}
		if (r.State != "pending" && r.State != "refreshing") || r.NextAttemptAt.After(time.Now()) {
			continue
		}
		busy := false
		for _, j := range state.Jobs {
			if j.Recovery != nil && j.Recovery.AccountID == r.Snapshot.AccountID && recoveryInFlight(j) {
				busy = true
				break
			}
		}
		if busy {
			continue
		}
		opCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
		err := a.startRecovery(opCtx, target, r)
		cancel()
		return true, err
	}
	return false, nil
}

func recoveryJobProblem(j Job) *Problem {
	for _, g := range j.Groups {
		if g.Error != nil {
			return g.Error
		}
		for _, d := range g.Deliveries {
			if d.Error != nil {
				return d.Error
			}
		}
		for _, item := range g.Batch.Items {
			if item.Error != nil {
				return publicError(item.Error)
			}
		}
	}
	return safeError("recovery_requires_review", "bridge", "manual_review")
}

func (a *App) recoveryReview(r Recovery, err error) error {
	return a.store.Update(func(s *State) error {
		for i := range s.Recoveries {
			current := &s.Recoveries[i]
			if current.Snapshot.AccountID == r.Snapshot.AccountID && current.Enabled && current.Epoch == r.Epoch {
				current.State = "manual_review"
				current.Error = publicError(err)
				current.NextAttemptAt = time.Time{}
			}
		}
		return nil
	})
}

func (a *App) startRecovery(ctx context.Context, target RecoveryTarget, r Recovery) error {
	snap, err := target.RecoverySnapshot(ctx, r.Snapshot.AccountID)
	if err != nil {
		return a.recoveryReview(r, err)
	}
	// Routine refresh may advance the generation, but not the original identity.
	if snap.Identity != r.Snapshot.Identity || snap.Family != r.Snapshot.Family || !strings.EqualFold(snap.Identifier, r.Snapshot.Identifier) {
		return a.recoveryReview(r, LocalImportError("recovery_identity_mismatch"))
	}
	if r.PendingGeneration > 0 && snap.Generation != r.PendingGeneration {
		return a.recoveryReview(r, LocalImportError("recovery_credentials_changed"))
	}
	if r.State == "refreshing" {
		eligible, refreshErr := target.RefreshBeforeRecovery(ctx, snap)
		if refreshErr != nil {
			return a.recoveryReview(r, refreshErr)
		}
		if !eligible {
			return a.store.Update(func(s *State) error {
				for i := range s.Recoveries {
					rr := &s.Recoveries[i]
					if rr.Snapshot.AccountID == snap.AccountID && rr.Epoch == r.Epoch && rr.Enabled {
						rr.State = "ready"
						rr.NextAttemptAt = time.Now().Add(15 * time.Minute)
					}
				}
				return nil
			})
		}
		snap, err = target.RecoverySnapshot(ctx, snap.AccountID)
		if err != nil {
			return a.recoveryReview(r, err)
		}
		if snap.Identity != r.Snapshot.Identity || snap.Family != r.Snapshot.Family {
			return a.recoveryReview(r, LocalImportError("recovery_identity_mismatch"))
		}
		if r.PendingGeneration > 0 && snap.Generation != r.PendingGeneration {
			return a.recoveryReview(r, LocalImportError("recovery_credentials_changed"))
		}
	}
	if r.Attempts >= 3 {
		return a.recoveryReview(r, LocalImportError("recovery_attempt_limit"))
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	jobID := "recovery_" + hex.EncodeToString(nonce[:])
	material := r.Material
	material.ExpectedIdentity = &snap.Identity
	now := time.Now().UTC()
	return a.store.Update(func(s *State) error {
		for _, j := range s.Jobs {
			if j.Recovery != nil && j.Recovery.AccountID == snap.AccountID && recoveryInFlight(j) {
				return nil
			}
		}
		for i := range s.Recoveries {
			rr := &s.Recoveries[i]
			if rr.Snapshot.AccountID != snap.AccountID || !rr.Enabled || rr.Epoch != r.Epoch {
				continue
			}
			if len(s.Jobs) >= 200 {
				rr.State = "manual_review"
				rr.Error = safeError("state_capacity", "bridge", "manual_review")
				return nil
			}
			active := 0
			for _, j := range s.Jobs {
				if recoveryInFlight(j) {
					active++
				}
			}
			if active >= 8 {
				rr.NextAttemptAt = now.Add(time.Minute)
				return nil
			}
			rr.Attempts++
			rr.State = "recovering"
			rr.JobID = jobID
			rr.Error = nil
			rr.NextAttemptAt = now.Add(15 * time.Minute)
			j := Job{ID: jobID, Settings: r.Settings, CreatedAt: now, UpdatedAt: now, Recovery: &snap, RecoveryEpoch: rr.Epoch, AutoRecovery: true, SaveCredentials: true,
				Groups: []Group{{Request: upstream.SubmitRequest{RequestID: jobID, Accounts: []upstream.Account{material}, Options: r.Options}, Lines: []int{1}, Masks: []string{mask(material.Identifier)}, Stage: "queued", Deliveries: []Delivery{}}}}
			s.Jobs = append(s.Jobs, j)
			return nil
		}
		return nil
	})
}

func (a *App) retainRecovery(ctx context.Context, job Job, index int, delivery Delivery, result upstream.ImportResult) *Recovery {
	target, ok := a.target.(RecoveryTarget)
	if !ok || delivery.Line < 1 || delivery.Line > len(job.Groups[index].Request.Accounts) {
		return nil
	}
	id := int64(0)
	if job.Recovery != nil {
		id = job.Recovery.AccountID
	} else if len(result.CreatedIDs) == 1 {
		id = result.CreatedIDs[0]
	}
	if id <= 0 {
		return nil
	}
	material := job.Groups[index].Request.Accounts[delivery.Line-1]
	next := &Recovery{Snapshot: RecoverySnapshot{AccountID: id, Identifier: material.Identifier}, Material: material, Options: job.Groups[index].Request.Options, Settings: job.Settings, Enabled: true, Epoch: 1, State: "manual_review", JobID: job.ID, Error: publicError(LocalImportError("recovery_retention_failed"))}
	if material.Password == "" || material.TOTPSecret == "" {
		return nil
	}
	snap, err := target.RecoverySnapshot(ctx, id)
	if err != nil {
		return next
	}
	next.Snapshot = snap
	if !strings.EqualFold(material.Identifier, snap.Identifier) || snap.Identity.ChatGPTUserID == "" || snap.Identity.ChatGPTAccountID == "" {
		return next
	}
	next.Material.ExpectedIdentity = &snap.Identity
	next.State, next.Error = "ready", nil
	return next
}

func registerRecovery(s *State, next Recovery, automatic bool) {
	for i := range s.Recoveries {
		old := s.Recoveries[i]
		if old.Snapshot.AccountID != next.Snapshot.AccountID {
			continue
		}
		next.Epoch = old.Epoch + 1
		if automatic {
			next.Epoch = old.Epoch
			next.Attempts = old.Attempts
			next.NextAttemptAt = old.NextAttemptAt
		}
		s.Recoveries[i] = next
		return
	}
	s.Recoveries = append(s.Recoveries, next)
}
