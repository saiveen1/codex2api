package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

const savedReauthorizationFingerprint = "saved-reauthorization:"

func (j Job) usesSavedRecovery() bool {
	return j.AutoRecovery || strings.HasPrefix(j.Fingerprint, savedReauthorizationFingerprint)
}

func (r Recovery) canReauthorize(now time.Time) bool {
	return r.Enabled && r.State == "ready" && r.Attempts < 3 && !r.NextAttemptAt.After(now) && r.Material.Password != "" && r.Material.TOTPSecret != "" && settingsView(r.Settings).Configured
}

// ReauthorizeSaved records an explicit request before the existing worker can send it.
func (a *App) ReauthorizeSaved(ctx context.Context, id int64, requestID string) (JobView, bool, error) {
	if !strings.HasPrefix(requestID, "saved-") || len(requestID) <= len("saved-") || !requestIDPattern.MatchString(requestID) {
		return JobView{}, false, fault(400, "invalid_request_id", "请提供以 saved- 开头的有效请求 ID。")
	}
	target, ok := a.target.(RecoveryTarget)
	if !ok || a.closed.Load() {
		return JobView{}, false, fault(503, "recovery_unsupported", "当前恢复服务不可用。")
	}
	fp := savedReauthorizationFingerprint + strconv.FormatInt(id, 10)
	var view JobView
	existing := false
	err := a.store.Update(func(s *State) error {
		for _, j := range s.Jobs {
			if j.ID == requestID {
				if j.Fingerprint != fp {
					return fault(409, "idempotency_conflict", "此请求 ID 已绑定其他任务。")
				}
				view, existing = jobViewWithRetries(j, retryReferences(*s)), true
				return nil
			}
		}
		for _, j := range s.Archived {
			if j.ID == requestID {
				if j.Fingerprint != fp {
					return fault(409, "idempotency_conflict", "此请求 ID 已绑定其他任务。")
				}
				view, existing = archiveView(j), true
				return nil
			}
		}
		var r *Recovery
		for i := range s.Recoveries {
			if s.Recoveries[i].Snapshot.AccountID == id {
				r = &s.Recoveries[i]
				break
			}
		}
		if r == nil || !r.Enabled {
			return fault(404, "not_found", "该账号没有启用的恢复保管记录。")
		}
		now := time.Now().UTC()
		if !r.canReauthorize(now) {
			return fault(409, "recovery_not_ready", "恢复记录尚不可用：请等待冷却结束，或先处理未决任务与人工核查。")
		}
		active := 0
		for _, j := range s.Jobs {
			if !recoveryInFlight(j) && jobView(j).State != "needs_review" {
				continue
			}
			active++
			if j.Recovery != nil && j.Recovery.AccountID == id {
				return fault(409, "account_busy", "原账号已有未解决的恢复任务。")
			}
			for _, g := range j.Groups {
				for _, account := range g.Request.Accounts {
					if strings.EqualFold(account.Identifier, r.Material.Identifier) {
						return fault(409, "account_busy", "相同账号已有未解决的授权任务。")
					}
				}
			}
		}
		if len(s.Jobs) >= maxJobs || active >= 8 {
			return fault(429, "capacity_exceeded", "任务已达到容量上限，请处理已有任务后再试。")
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		material := r.Material
		// The worker validates this snapshot again before sending or applying the result.
		snap, err := target.RecoverySnapshot(ctx, id)
		if err != nil {
			return fault(409, "recovery_account_unavailable", "无法读取可恢复的原账号身份，未发起重新授权。")
		}
		if snap.Identity != r.Snapshot.Identity || snap.Family != r.Snapshot.Family || !strings.EqualFold(snap.Identifier, r.Snapshot.Identifier) || !strings.EqualFold(material.Identifier, snap.Identifier) || snap.Generation < r.Snapshot.Generation || snap.Identity.ChatGPTUserID == "" || snap.Identity.ChatGPTAccountID == "" || snap.CredentialVersion == "" {
			return fault(409, "recovery_identity_mismatch", "原主体、工作区或凭据版本变化，未发起重新授权。")
		}
		material.ExpectedIdentity = &snap.Identity
		job := Job{ID: requestID, Fingerprint: fp, Settings: r.Settings, Recovery: &snap, RecoveryEpoch: r.Epoch, SaveCredentials: true, CreatedAt: now, UpdatedAt: now,
			Groups: []Group{{Request: upstream.SubmitRequest{RequestID: "bridge_" + hex.EncodeToString(nonce[:]), Accounts: []upstream.Account{material}, Options: r.Options}, Lines: []int{1}, Masks: []string{mask(material.Identifier)}, Stage: "queued", Deliveries: []Delivery{}}}}
		r.State, r.JobID, r.Error = "recovering", requestID, nil
		r.Attempts++
		r.NextAttemptAt = now.Add(15 * time.Minute)
		s.Jobs = append(s.Jobs, job)
		view = jobView(job)
		return nil
	})
	if err == nil && !existing {
		a.notify()
	}
	return view, existing, err
}
