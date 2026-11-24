package service

import (
	"context"
	"errors"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

func (a *App) Run(ctx context.Context) error {
	if a.closed.Load() {
		return errors.New("task worker is closed")
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	a.notify()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-a.wake:
		}
		worked, err := a.Step(ctx)
		if err != nil {
			return errors.New("task worker stopped because durable state could not be updated")
		}
		if worked {
			a.notify()
		}
	}
}

// Step advances one durable operation. It never retries a new authorization.
func (a *App) Step(ctx context.Context) (bool, error) {
	a.workMu.Lock()
	defer a.workMu.Unlock()
	if a.closed.Load() {
		return false, errors.New("task worker is closed")
	}
	if ctx.Err() != nil {
		return false, nil
	}
	state, err := a.store.View()
	if err != nil {
		return false, err
	}
	if worked, err := a.stepRecovery(ctx, state); worked || err != nil {
		return worked, err
	}
	now := time.Now()
	for _, job := range state.Jobs {
		for index, g := range job.Groups {
			if g.NextCheck.After(now) {
				continue
			}
			switch g.Stage {
			case "queued", "checking", "submitting", "submit_unknown", "running":
			default:
				continue
			}
			if now.Sub(job.CreatedAt) > 24*time.Hour {
				return true, a.failGroup(job.ID, index, "needs_review", safeError("task_deadline_exceeded", "bridge", "manual_review"))
			}
			opCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
			err = a.advance(opCtx, job, index)
			cancel()
			return true, err
		}
	}
	return false, nil
}
func (a *App) mutateGroup(id string, index int, change func(*Group)) error {
	return a.changeJob(id, func(j *Job) error { change(&j.Groups[index]); return nil })
}
func (a *App) failGroup(id string, index int, stage string, e *Problem) error {
	return a.mutateGroup(id, index, func(g *Group) { g.Stage = stage; g.Error = e; g.NextCheck = time.Now().Add(5 * time.Second) })
}

func (a *App) advance(ctx context.Context, job Job, index int) error {
	g := job.Groups[index]
	if job.CancelRequested && (g.Stage == "queued" || g.Stage == "checking") {
		return a.failGroup(job.ID, index, "cancelled", nil)
	}
	o, c, err := a.clients(job.Settings)
	if err != nil {
		return a.failGroup(job.ID, index, "needs_review", publicError(err))
	}
	if g.Stage == "queued" || g.Stage == "checking" {
		return a.prepare(ctx, o, c, job, index)
	}
	if g.Stage == "submitting" || g.Stage == "submit_unknown" {
		batch, err := o.Lookup(ctx, g.Request.RequestID)
		if err != nil {
			return a.readFailure(ctx, job.ID, index, "lookup", err)
		}
		return a.acceptBatch(job, index, batch)
	}
	if job.CancelRequested && g.Batch.BatchID != "" && !g.CancelSent && g.Batch.Pending > 0 {
		if err = a.mutateGroup(job.ID, index, func(g *Group) { g.CancelSent = true }); err != nil {
			return err
		}
		batch, e := o.Cancel(ctx, g.Batch.BatchID)
		if e != nil {
			return a.failGroup(job.ID, index, "needs_review", publicError(e))
		}
		return a.acceptBatch(job, index, batch)
	}
	for d, delivery := range g.Deliveries {
		if delivery.State == "ready" {
			return a.importOne(ctx, c, job, index, d)
		}
		if delivery.State == "importing" {
			return a.mutateGroup(job.ID, index, func(g *Group) {
				g.Deliveries[d].State = "import_unknown"
				g.Deliveries[d].Error = safeError("import_outcome_unknown", "codex", "manual_review")
				g.Stage = "needs_review"
			})
		}
	}
	batch, err := o.Batch(ctx, g.Batch.BatchID)
	if err != nil {
		return a.readFailure(ctx, job.ID, index, "status", err)
	}
	if err = validateBatch(g, batch); err != nil {
		return a.failGroup(job.ID, index, "needs_review", publicError(err))
	}
	keepReturnedEnvironment(&batch, g.Batch)
	// Persist status before attempting delivery; a read failure does not reauthorize.
	if err = a.mutateGroup(job.ID, index, func(g *Group) {
		g.Batch = batch
		if g.ReadOperation != "result" {
			resetReadRetry(g)
			g.Error = nil
		}
		g.NextCheck = time.Now().Add(5 * time.Second)
	}); err != nil {
		return err
	}
	missing := false
	for _, item := range batch.Items {
		if item.State == "succeeded" && !hasDelivery(g, item.ItemID) {
			missing = true
		}
	}
	if missing {
		result, e := o.Result(ctx, batch.BatchID)
		if e != nil {
			return a.readFailure(ctx, job.ID, index, "result", e)
		}
		observedBatch := batch
		accounts, e := upstream.ValidateDelivery(result, batch)
		if e != nil {
			// A result may include an item that completed after the status read.
			fresh, refreshErr := o.Batch(ctx, batch.BatchID)
			if refreshErr != nil {
				return a.readFailure(ctx, job.ID, index, "result", refreshErr)
			}
			if refreshErr = validateBatch(g, fresh); refreshErr != nil {
				return a.failGroup(job.ID, index, "needs_review", publicError(refreshErr))
			}
			keepReturnedEnvironment(&fresh, batch)
			accounts, e = upstream.ValidateDelivery(result, fresh)
			if e != nil {
				return a.failGroup(job.ID, index, "needs_review", publicError(e))
			}
			batch = fresh
		}
		covered := map[string]bool{}
		for _, mapping := range result.Delivery.Items {
			covered[mapping.ItemID] = true
		}
		for _, id := range result.Delivery.ExpiredItemIDs {
			covered[id] = true
		}
		for _, item := range observedBatch.Items {
			if item.State == "succeeded" && !hasDelivery(g, item.ItemID) && !covered[item.ItemID] {
				return a.failGroup(job.ID, index, "needs_review", safeError("incomplete_delivery", "delivery", "manual_review"))
			}
		}
		return a.mutateGroup(job.ID, index, func(g *Group) {
			g.Batch = batch
			resetReadRetry(g)
			g.Error = nil
			for _, mapping := range result.Delivery.Items {
				if !hasDelivery(*g, mapping.ItemID) {
					g.Deliveries = append(g.Deliveries, Delivery{ItemID: mapping.ItemID, Line: mapping.Line, Account: accounts[mapping.AccountIndex], EnvironmentChecked: true, State: "ready"})
				}
			}
			for _, id := range result.Delivery.ExpiredItemIDs {
				if !hasDelivery(*g, id) {
					for _, item := range batch.Items {
						if item.ItemID == id {
							g.Deliveries = append(g.Deliveries, Delivery{ItemID: id, Line: item.Line, State: "expired", Error: safeError("result_expired", "delivery", "manual_review")})
						}
					}
				}
			}
			g.NextCheck = time.Time{}
		})
	}
	if batch.Pending == 0 {
		return a.mutateGroup(job.ID, index, func(current *Group) {
			stage := "complete"
			for _, item := range current.Batch.Items {
				if item.State == "needs_review" {
					stage = "needs_review"
				}
			}
			for _, delivery := range current.Deliveries {
				if delivery.State == "ready" || delivery.State == "importing" {
					current.Stage = "running"
					current.NextCheck = time.Time{}
					return
				}
				if delivery.State == "import_unknown" {
					stage = "needs_review"
				}
			}
			current.Stage = stage
		})
	}
	return nil
}
func hasDelivery(g Group, id string) bool {
	for _, d := range g.Deliveries {
		if d.ItemID == id {
			return true
		}
	}
	return false
}
func validateBatch(g Group, b upstream.Batch) error {
	if b.RequestID != g.Request.RequestID || b.Total != len(g.Lines) || len(b.Items) != len(g.Lines) || b.BatchID == "" {
		return fault(502, "invalid_batch", "网关批次与原请求不匹配，已停止自动处理。")
	}
	if g.Batch.BatchID != "" && b.BatchID != g.Batch.BatchID {
		return fault(502, "invalid_batch", "网关返回了不同批次，已停止自动处理。")
	}
	ids := map[string]bool{}
	lines := map[int]bool{}
	for _, item := range b.Items {
		if item.ItemID == "" || ids[item.ItemID] || item.Line < 1 || item.Line > len(g.Lines) || lines[item.Line] {
			return fault(502, "invalid_batch", "网关项目映射无效，已停止自动处理。")
		}
		ids[item.ItemID] = true
		lines[item.Line] = true
		if len(g.Batch.Items) > 0 {
			found := false
			for _, old := range g.Batch.Items {
				if old.ItemID == item.ItemID && old.Line == item.Line {
					found = true
				}
			}
			if !found {
				return fault(502, "invalid_batch", "网关项目关联发生变化，已停止自动处理。")
			}
		}
	}
	return nil
}

// Some status replies omit options already returned for this same batch.
func keepReturnedEnvironment(next *upstream.Batch, previous upstream.Batch) {
	if next.BatchID == "" || next.BatchID != previous.BatchID || previous.Options == nil {
		return
	}
	if next.Options == nil && upstream.KnownEnvironment(previous.Options.Environment) == "" && previous.Options.Timezone == "" {
		return
	}
	if next.Options == nil {
		next.Options = &upstream.BatchProfile{}
	}
	if next.Options.Environment == "" && upstream.KnownEnvironment(previous.Options.Environment) != "" {
		next.Options.Environment = previous.Options.Environment
	}
	if next.Options.Timezone == "" {
		next.Options.Timezone = previous.Options.Timezone
	}
}

func (a *App) acceptBatch(job Job, index int, batch upstream.Batch) error {
	if err := validateBatch(job.Groups[index], batch); err != nil {
		return a.failGroup(job.ID, index, "needs_review", publicError(err))
	}
	keepReturnedEnvironment(&batch, job.Groups[index].Batch)
	return a.mutateGroup(job.ID, index, func(g *Group) {
		g.Batch = batch
		g.Stage = "running"
		g.Error = nil
		resetReadRetry(g)
		g.NextCheck = time.Now().Add(5 * time.Second)
	})
}
func (a *App) prepare(ctx context.Context, o *upstream.OAuthClient, c Target, job Job, index int) error {
	g := job.Groups[index]
	if err := a.mutateGroup(job.ID, index, func(g *Group) { g.Stage = "checking" }); err != nil {
		return err
	}
	stop := func(err error) error {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return a.failGroup(job.ID, index, "failed", publicError(err))
	}
	cap, err := o.Capabilities(ctx)
	if err != nil {
		return stop(err)
	}
	if err = compatible(cap); err != nil {
		return stop(err)
	}
	if job.Recovery != nil {
		if err = checkRecoveryCapability(cap); err != nil {
			return a.failGroup(job.ID, index, "needs_review", publicError(err))
		}
		if err = a.checkRecoveryJob(ctx, job); err != nil {
			return a.failGroup(job.ID, index, "needs_review", publicError(err))
		}
	}
	if err := checkUploadSize(cap, g.Request); err != nil {
		return stop(err)
	}
	if cap.MaxBatchSize < 1 || len(g.Request.Accounts) > cap.MaxBatchSize {
		return stop(fault(400, "batch_too_large", "单个分批超过网关容量。"))
	}
	groups, err := c.Groups(ctx)
	if err != nil {
		return stop(err)
	}
	for _, wanted := range job.GroupIDs {
		found := false
		for _, available := range groups {
			if available.ID == wanted {
				found = true
			}
		}
		if !found {
			return stop(fault(400, "invalid_groups", "目标分组不存在，未执行 OAuth。"))
		}
	}
	preview, err := o.Preview(ctx, g.Request)
	if err != nil {
		return stop(err)
	}
	if !preview.Valid {
		return stop(fault(400, "preview_rejected", "网关材料校验未通过，未执行 OAuth。"))
	}
	if g.Request.Options.ProxyMode == "custom" {
		if !cap.Features["proxy_check"] {
			return stop(fault(409, "proxy_check_unsupported", "网关不支持执行机代理检测，未执行 OAuth。"))
		}
		check, e := o.CheckProxy(ctx, g.Request.Options)
		if e != nil {
			return stop(e)
		}
		if !check.Usable {
			if check.Error != nil {
				return stop(check.Error)
			}
			return stop(fault(409, "proxy_check_failed", "执行机代理检测未通过，未执行 OAuth。"))
		}
	}
	quota, err := o.Quota(ctx)
	if err != nil {
		return stop(err)
	}
	if quota.Disabled || quota.Remaining < int64(len(g.Request.Accounts)) {
		return stop(fault(409, "insufficient_quota", "本分批的可用授权次数不足，未提交 OAuth。"))
	}
	if ctx.Err() != nil {
		return stop(fault(504, "preflight_timeout", "提交前检查超时，未提交 OAuth。"))
	}
	cancelled := false
	if err = a.changeJob(job.ID, func(j *Job) error {
		if j.CancelRequested {
			j.Groups[index].Stage = "cancelled"
			cancelled = true
		} else {
			j.Groups[index].Stage = "submitting"
		}
		return nil
	}); err != nil {
		return err
	}
	if cancelled {
		return nil
	}
	batch, err := o.Submit(ctx, g.Request)
	if err != nil {
		e := publicError(err)
		stage := "failed"
		if e.Code == "idempotency_conflict" {
			stage = "needs_review"
		} else if e.Uncertain || e.Action == "query_original" || e.Action == "retry_same_request" {
			stage = "submit_unknown"
		}
		return a.failGroup(job.ID, index, stage, e)
	}
	return a.acceptBatch(job, index, batch)
}
func (a *App) importOne(ctx context.Context, c Target, job Job, index, d int) error {
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	if err := a.checkRecoveryJob(ctx, job); err != nil {
		return a.mutateGroup(job.ID, index, func(g *Group) {
			g.Deliveries[d].State = "import_rejected"
			g.Deliveries[d].Account = nil
			g.Deliveries[d].Error = publicError(err)
		})
	}
	delivery := job.Groups[index].Deliveries[d]
	runtimeProxy := job.RuntimeProxy
	if proxies := job.Groups[index].RuntimeProxies; len(proxies) > 0 {
		if len(proxies) != len(job.Groups[index].Lines) || delivery.Line < 1 || delivery.Line > len(proxies) {
			return a.mutateGroup(job.ID, index, func(g *Group) {
				g.Deliveries[d].State = "import_rejected"
				g.Deliveries[d].Account = nil
				g.Deliveries[d].Error = &Problem{Code: "runtime_proxy_mapping_invalid", Message: "账号与运行代理映射不完整，请核对原任务；未执行导入。", Source: "codex", Action: "manual_review"}
			})
		}
		runtimeProxy = proxies[delivery.Line-1]
	}
	// Old pending deliveries may contain a remote-supplied field. Rebind from the saved batch only.
	if !delivery.EnvironmentChecked {
		bound, err := upstream.BindDeliveryEnvironment(delivery.Account, job.Groups[index].Batch.Options)
		if err != nil {
			return a.failGroup(job.ID, index, "needs_review", publicError(err))
		}
		delivery.Account = bound
	}
	// Rebind even previously checked deliveries: their timezone field was never trusted.
	bound, err := upstream.BindDeliveryTimezone(delivery.Account, job.Groups[index].Batch.Options)
	if err != nil {
		return a.failGroup(job.ID, index, "needs_review", publicError(err))
	}
	delivery.Account = bound
	if err := a.mutateGroup(job.ID, index, func(g *Group) {
		g.Deliveries[d].Account = delivery.Account
		g.Deliveries[d].EnvironmentChecked = true
		g.Deliveries[d].State = "importing"
	}); err != nil {
		return err
	}
	var result upstream.ImportResult
	var importErr error
	if job.Recovery != nil {
		result, importErr = a.target.(RecoveryTarget).Recover(ctx, *job.Recovery, delivery.Account, job.ID, job.PauseAutoDetection)
	} else {
		result, importErr = c.Import(ctx, delivery.Account, job.GroupIDs, runtimeProxy, job.PauseAutoDetection)
	}
	var remote *upstream.Error
	knownError := errors.As(importErr, &remote) && !remote.Uncertain
	valid := validImportSummary(result)
	accepted := valid && result.Failed == 0 && (importErr == nil || knownError)
	var retained *Recovery
	if accepted && job.SaveCredentials {
		retained = a.retainRecovery(ctx, job, index, delivery, result)
		if (retained == nil || retained.Error != nil) && importErr == nil {
			importErr = LocalImportError("recovery_retention_failed")
		}
	}
	var summary *AccountResult
	if accepted {
		summary = committedAccountResult(ctx, c, job, job.Groups[index], delivery.Line, result)
	}
	err = a.store.Update(func(s *State) error {
		for j := range s.Jobs {
			if s.Jobs[j].ID != job.ID {
				continue
			}
			g := &s.Jobs[j].Groups[index]
			item := &g.Deliveries[d]
			g.NextCheck = time.Time{}
			if accepted {
				item.State = "imported"
				item.CreatedIDs = result.CreatedIDs
				item.Account = nil
				item.Error = nil
				if importErr != nil {
					item.Error = publicError(importErr)
				}
				if retained != nil {
					registerRecovery(s, *retained, job.usesSavedRecovery())
				}
				if summary != nil {
					summary.RecoveryState = "not_enrolled"
					if job.SaveCredentials {
						summary.RecoveryState = "retention_failed"
					}
					for i := range s.Recoveries {
						r := &s.Recoveries[i]
						if r.Snapshot.AccountID != summary.AccountID {
							continue
						}
						if summary.Outcome == reauthorizationSucceeded {
							now := time.Now().UTC()
							r.LastOutcome, r.LastOutcomeAt = reauthorizationSucceeded, &now
						}
						summary.RecoveryState = r.State
						break
					}
					if summary.AccountID == 0 && !job.SaveCredentials {
						summary.RecoveryState = ""
					}
					item.Result = summary
				}
			} else {
				item.State = "import_unknown"
				item.Error = safeError("import_outcome_unknown", "codex", "manual_review")
				if importErr != nil {
					item.Error = publicError(importErr)
				}
				if knownError && valid && result.Failed == 1 {
					item.State = "import_failed"
					if local := localImportProblem(remote); local != nil && local.Action == "manual_review" {
						item.State = "import_rejected"
					}
				} else {
					g.Stage = "needs_review"
				}
			}
			purgeMaterials(&s.Jobs[j])
			s.Jobs[j].UpdatedAt = time.Now().UTC()
			return nil
		}
		return fault(404, "not_found", "任务不存在。")
	})
	if err == nil && retained != nil {
		a.enabled.Store(retained.Snapshot.AccountID, true)
	}
	return err
}

// This validation belonged to the removed HTTP target; keep it at the injected boundary.
func validImportSummary(v upstream.ImportResult) bool {
	if v.Total != 1 || v.Success < 0 || v.Success > 1 || v.Updated < 0 || v.Updated > 1 || v.Duplicate < 0 || v.Duplicate > 1 || v.Failed < 0 || v.Failed > 1 || v.Success+v.Updated+v.Duplicate+v.Failed != 1 || len(v.CreatedIDs) > v.Success {
		return false
	}
	for _, id := range v.CreatedIDs {
		if id <= 0 {
			return false
		}
	}
	return true
}
