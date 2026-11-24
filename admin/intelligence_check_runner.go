package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	ic "github.com/codex2api/internal/intelligencecheck"
	"github.com/codex2api/proxy"
)

func (h *Handler) StartIntelligenceChecks(ctx context.Context, deviceConfig ...*proxy.DeviceProfileConfig) {
	if len(deviceConfig) > 0 {
		h.intelligenceDeviceConfig = deviceConfig[0]
	}
	if h.db == nil || h.store == nil {
		return
	}
	h.intelligenceOnce.Do(func() {
		h.intelligenceWG.Add(1)
		go func() {
			defer h.intelligenceWG.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				if err := h.intelligenceTick(ctx); err != nil && ctx.Err() == nil {
					log.Printf("降智检测调度失败: %v", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}
func (h *Handler) WaitIntelligenceChecks() { h.intelligenceWG.Wait() }
func (h *Handler) intelligenceTick(ctx context.Context) error {
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := h.db.ExpireIntelligenceCheckLeases(opCtx, time.Now()); err != nil {
		return err
	}
	if _, err := h.db.EnqueueDueIntelligenceChecks(opCtx, time.Now(), 100, h.intelligencePreflight()); err != nil {
		return err
	}
	for i := 0; i < 100 && ctx.Err() == nil; i++ {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return err
		}
		owner := hex.EncodeToString(token[:])
		run, err := h.db.ClaimIntelligenceCheck(opCtx, owner, time.Now(), 30*time.Second)
		if err != nil {
			return err
		}
		if run == nil {
			return nil
		}
		account := h.store.FindByID(run.AccountID)
		models := h.matchedIntelligenceModels(opCtx, account, run.Model)
		if len(models) > 0 && !h.store.TryAcquireIntelligenceCheck(account) {
			if err := h.db.RequeueIntelligenceCheck(opCtx, run.ID, owner, time.Now()); err != nil {
				return err
			}
			continue
		}
		h.intelligenceWG.Add(1)
		go func(run database.IntelligenceCheckRun) {
			defer h.intelligenceWG.Done()
			if len(models) > 0 {
				defer h.store.Release(account)
			}
			h.runMatchedIntelligenceCheck(ctx, run, account, models)
		}(*run)
	}
	return nil
}

// A detector must satisfy both the account allowlist and the source's text-test catalog.
func (h *Handler) matchedIntelligenceModels(ctx context.Context, account *auth.Account, configured string) []string {
	models := intelligenceModelCandidates(account, configured)
	if len(models) == 0 {
		return nil
	}
	options := h.qualityTestOptionsForAccount(ctx, account)
	return slices.DeleteFunc(models, func(model string) bool { return !slices.Contains(options.Models, model) })
}

func (h *Handler) intelligencePreflight() database.IntelligenceCheckPreflight {
	return database.IntelligenceCheckPreflight{
		MatchedModels: func(ctx context.Context, id int64, configured string) []string {
			return h.matchedIntelligenceModels(ctx, h.store.FindByID(id), configured)
		},
		QuotaHold: func(id int64, model string) (time.Time, bool) {
			return h.intelligenceQuotaHold(h.store.FindByID(id), model)
		},
		IsAvailable: func(id int64) bool {
			return intelligenceAccountAvailable(h.store.FindByID(id))
		},
	}
}

// intelligenceQuotaHold reports whether a detector request to account would
// only be rejected for rate-limit or quota reasons, and when that should end.
func (h *Handler) intelligenceQuotaHold(account *auth.Account, model string) (time.Time, bool) {
	return account.IntelligenceCheckQuotaHold(model, proxy.DispatchPolicyForModel(model), time.Now())
}

func intelligenceAccountAvailable(account *auth.Account) bool {
	return account != nil && account.IsAvailable()
}

func (h *Handler) runIntelligenceCheck(parent context.Context, run database.IntelligenceCheckRun, account *auth.Account) {
	h.runMatchedIntelligenceCheck(parent, run, account, h.matchedIntelligenceModels(parent, account, run.Model))
}

func (h *Handler) runMatchedIntelligenceCheck(parent context.Context, run database.IntelligenceCheckRun, account *auth.Account, models []string) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(run.Settings.TimeoutSeconds)*time.Second)
	result := intelligenceExecution{}
	if h.db != nil && !run.RecordOnly {
		if err := h.db.ValidateIntelligenceCheckAccountSettings(ctx, run.Settings, run.Channel, run.AccountID); err != nil {
			result.Error = "配置错误：" + err.Error()
			result.Interrupted = true
		}
	}
	if result.Error == "" && account != nil && len(models) == 0 {
		result = intelligenceExecution{Skipped: true, Error: "无匹配模型：候选模型与账号允许的检测模型没有交集"}
	}
	// A rate-limited or exhausted account says nothing about model quality, so
	// neither outcome below is graded. An automatic check that was queued before
	// the limit was known is not sent; an explicit manual check still is.
	if result.Error == "" && run.Source == "automatic" && run.RequireNormal && !intelligenceAccountAvailable(account) {
		result.Error = "账号当前不可调度，已跳过仅正常账号的自动检测"
		result.Interrupted = true
	}
	if result.Error == "" {
		result = h.executeIntelligenceCandidates(ctx, account, models, run.ReasoningEffort, run.Question.Prompt, !run.RecordOnly, run.Source == "automatic")
		if result.Error != "" && !result.Skipped && !result.Interrupted && !result.CredentialRejected {
			if _, held := h.intelligenceQuotaHold(account, result.RequestedModel); held {
				result.Error = "账号限流或额度耗尽，本次检测不计入结果：" + result.Error
				result.Interrupted = true
			}
		}
	}
	cancel()
	observation := ic.Observation{CredentialRejected: result.CredentialRejected, Interrupted: result.Interrupted, Output: result.Output, Error: result.Error, Complete: result.Complete, DurationMS: result.DurationMS, InputTokens: int64(result.InputTokens), OutputTokens: int64(result.OutputTokens), ReasoningTokens: int64(result.ReasoningTokens), ResponseModel: result.ResponseModel, EffectiveContext: result.EffectiveContext}
	observation.Skipped, observation.RequestedModel = result.Skipped, result.RequestedModel
	observation.MatchedModels = strings.Join(models, ",")
	if parent.Err() != nil {
		observation.Interrupted = true
		observation.Error = "服务关闭，检测已中断"
		observation.Complete = false
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	committed, err := h.db.FinishIntelligenceCheck(finishCtx, run.ID, run.LeaseOwner, observation, time.Now())
	if err != nil {
		log.Printf("降智检测 %d 保存失败: %v", run.ID, err)
		return
	}
	if committed && !run.RecordOnly {
		if err = h.store.RefreshIntelligenceAccount(finishCtx, run.AccountID); err != nil {
			log.Printf("降智检测 %d 调度同步失败，等待 outbox: %v", run.ID, err)
		}
		h.invalidateAccountSnapshotCaches()
	}
}
