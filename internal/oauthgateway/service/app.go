package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

const maxJobs = 200

type App struct {
	store      Store
	target     Target
	allowHTTP  bool
	wake       chan struct{}
	workMu     sync.Mutex
	recoveryMu sync.Mutex
	enabled    sync.Map
	signals    sync.Map
	closed     atomic.Bool
}

func New(store Store, target Target, allowHTTP bool) (*App, error) {
	if store == nil || target == nil {
		return nil, errors.New("OAuth gateway requires a store and local target")
	}
	a := &App{store: store, target: target, allowHTTP: allowHTTP, wake: make(chan struct{}, 1)}
	err := store.Update(func(s *State) error {
		if (s.Version < 1 || s.Version > 6) || (s.Version == 1 && len(s.Archived) > 0) || (s.Version < 3 && len(s.Recoveries) > 0) {
			return errors.New("unsupported encrypted state version")
		}
		if s.Version < 5 && len(retryReferences(*s)) != 0 {
			return errors.New("reauthorization references require encrypted state version 5")
		}
		if s.Settings.OAuthURL == "" {
			if s.Settings.OAuthToken != "" {
				return errors.New("OAuth credential has no configured destination")
			}
			s.Settings.OAuthURL = DefaultOAuthURL
		}
		for j := range s.Jobs {
			if s.Version < 6 && s.Jobs[j].PauseAutoDetection {
				return errors.New("paused detection imports require encrypted state version 6")
			}
			// Legacy flags were never consent to retention. V3 is a one-way,
			// fail-closed migration: older binaries reject it rather than purge it.
			if s.Version < 3 {
				s.Jobs[j].SaveCredentials = false
			}
			for g := range s.Jobs[j].Groups {
				group := &s.Jobs[j].Groups[g]
				if s.Version < 4 && len(group.RuntimeProxies) > 0 {
					return errors.New("per-account runtime proxies require encrypted state version 4")
				}
				if group.Stage == "checking" {
					group.Stage = "queued"
				}
				if group.Stage == "submitting" {
					group.Stage = "submit_unknown"
				}
				for d := range group.Deliveries {
					if group.Deliveries[d].State == "importing" {
						group.Deliveries[d].State = "import_unknown"
						group.Deliveries[d].Error = safeError("import_outcome_unknown", "codex", "manual_review")
						group.Stage = "needs_review"
					}
				}
			}
			purgeMaterials(&s.Jobs[j])
		}
		if s.Version < 3 {
			s.Version = 3
		}
		for _, r := range s.Recoveries {
			if r.Enabled {
				a.enabled.Store(r.Snapshot.AccountID, true)
			}
		}
		return nil
	})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return a, nil
}
func (a *App) Close() error {
	a.closed.Store(true)
	a.signals.Range(func(key, _ any) bool { a.signals.Delete(key); return true })
	return a.store.Close()
}
func (a *App) notify() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}
func (a *App) settings() (Settings, error) {
	s, err := a.store.View()
	if err != nil {
		return Settings{}, err
	}
	return s.Settings, nil
}
func settingsView(s Settings) SettingsView {
	return SettingsView{OAuthURL: s.OAuthURL, OAuthTokenSet: s.OAuthToken != "", Configured: s.OAuthURL != "" && s.OAuthToken != "", MaxJobs: maxJobs, MaxArchivedJobs: maxArchivedJobs, AutoRecoverySupported: true, AutoRecoveryReason: "支持显式保管材料账号的受控原账号恢复；先尝试正常刷新，再核验 password_totp、原主体/工作区身份锁和凭据版本。未保管的历史账号不会自动启用，封禁或身份不可证明时需人工处理。"}
}
func (a *App) Settings() (SettingsView, error) { s, err := a.settings(); return settingsView(s), err }
func (a *App) SaveSettings(next Settings) (SettingsView, error) {
	var result SettingsView
	err := a.store.Update(func(s *State) error {
		old := s.Settings
		// A masked settings update must not silently select a different host.
		if next.OAuthURL == "" {
			next.OAuthURL = old.OAuthURL
		}
		var err error
		next.OAuthURL, err = upstream.ValidateBaseURL(next.OAuthURL, a.allowHTTP)
		if err != nil {
			return fault(400, "invalid_oauth_url", "OAuth 网关地址无效；默认仅允许 HTTPS 或本机 HTTP。")
		}
		if next.OAuthToken == "" {
			if next.OAuthURL != old.OAuthURL {
				return fault(400, "credential_confirmation_required", "更换目标地址时必须重新输入对应凭证。")
			}
			next.OAuthToken = old.OAuthToken
		}
		if next.OAuthToken == "" || len(next.OAuthToken) > 4096 || strings.ContainsAny(next.OAuthToken, "\r\n\x00") {
			return fault(400, "invalid_credentials", "请设置有效的 OAuth 凭证。")
		}
		s.Settings = next
		result = settingsView(next)
		return nil
	})
	return result, err
}
func (a *App) clients(s Settings) (*upstream.OAuthClient, Target, error) {
	if !settingsView(s).Configured {
		return nil, nil, fault(409, "not_configured", "请先保存 OAuth 连接设置。")
	}
	oauth, err := upstream.NewOAuth(s.OAuthURL, s.OAuthToken, a.allowHTTP)
	if err != nil {
		return nil, nil, err
	}
	return oauth, a.target, nil
}
func (a *App) Capabilities(ctx context.Context) (upstream.Capabilities, error) {
	s, e := a.settings()
	if e != nil {
		return upstream.Capabilities{}, e
	}
	o, _, e := a.clients(s)
	if e != nil {
		return upstream.Capabilities{}, e
	}
	cap, e := o.Capabilities(ctx)
	if e == nil {
		e = compatible(cap)
	}
	return cap, e
}
func (a *App) Quota(ctx context.Context) (upstream.Quota, error) {
	s, e := a.settings()
	if e != nil {
		return upstream.Quota{}, e
	}
	o, _, e := a.clients(s)
	if e != nil {
		return upstream.Quota{}, e
	}
	return o.Quota(ctx)
}
func (a *App) Groups(ctx context.Context) ([]upstream.Group, error) {
	return a.target.Groups(ctx)
}
func (a *App) CheckProxy(ctx context.Context, options upstream.Options) (upstream.ProxyCheck, error) {
	s, e := a.settings()
	if e != nil {
		return upstream.ProxyCheck{}, e
	}
	o, _, e := a.clients(s)
	if e != nil {
		return upstream.ProxyCheck{}, e
	}
	cap, e := o.Capabilities(ctx)
	if e != nil {
		return upstream.ProxyCheck{}, e
	}
	if e = compatible(cap); e != nil {
		return upstream.ProxyCheck{}, e
	}
	if !cap.Features["proxy_check"] {
		return upstream.ProxyCheck{}, fault(409, "proxy_check_unsupported", "网关未声明执行机代理检测能力，不能用本机检测替代。")
	}
	if options.ProxyMode == "" {
		options.ProxyMode = "custom"
	}
	if options.ProxyMode != "custom" || !proxyProvided(options.Proxy) {
		return upstream.ProxyCheck{}, fault(400, "invalid_proxy", "请提供自带代理。")
	}
	options.WorkspacePolicy = ""
	return o.CheckProxy(ctx, options)
}
func (a *App) Connections(ctx context.Context) (map[string]any, error) {
	s, e := a.settings()
	if e != nil {
		return nil, e
	}
	o, c, e := a.clients(s)
	if e != nil {
		return nil, e
	}
	out := map[string]any{}
	oresult := map[string]any{"ok": false}
	cresult := map[string]any{"ok": false}
	cap, err := o.Capabilities(ctx)
	if err == nil {
		err = compatible(cap)
	}
	if err == nil {
		quota, qe := o.Quota(ctx)
		if qe == nil {
			oresult["ok"] = true
			oresult["capabilities"] = cap
			oresult["quota"] = quota
		} else {
			err = qe
		}
	}
	if err != nil {
		oresult["error"] = publicError(err)
	}
	groups, err := c.Groups(ctx)
	if err == nil {
		cresult["ok"] = true
		cresult["groups"] = groups
	} else {
		cresult["error"] = publicError(err)
	}
	out["oauth"] = oresult
	out["codex"] = cresult
	return out, nil
}
func (a *App) Preview(ctx context.Context, in Input) (Preview, error) {
	groups, p, e := normalize(in)
	if e != nil || !p.Valid {
		return p, e
	}
	s, e := a.store.View()
	if e != nil {
		return p, e
	}
	if in.RetryOf != nil {
		if e = checkRetry(s, *in.RetryOf, p.Rows[0].Identifier); e != nil {
			return p, e
		}
	}
	o, _, e := a.clients(s.Settings)
	if e != nil {
		return p, e
	}
	cap, e := o.Capabilities(ctx)
	if e != nil {
		return p, e
	}
	if e = compatible(cap); e != nil {
		return p, e
	}
	if in.RecoveryAccountID > 0 {
		if _, e = a.bindRecovery(ctx, in, groups); e != nil {
			return p, e
		}
		if e = checkRecoveryCapability(cap); e != nil {
			return p, e
		}
	}
	for _, g := range groups {
		if err := checkUploadSize(cap, g.Request); err != nil {
			return p, err
		}
		if cap.MaxBatchSize < 1 || len(g.Request.Accounts) > cap.MaxBatchSize {
			return p, fault(400, "batch_too_large", "单个代理分批超过网关限制，请减少本次账号数量。")
		}
		remote, err := o.Preview(ctx, g.Request)
		if err != nil {
			return p, err
		}
		if !remote.Valid {
			p.Valid = false
			for _, issue := range remote.Errors {
				line := 0
				if issue.Line > 0 && issue.Line <= len(g.Lines) {
					line = g.Lines[issue.Line-1]
				}
				p.Errors = append(p.Errors, upstream.PreviewIssue{Line: line, Code: safeIssueCode(issue.Code)})
			}
		}
	}
	return p, nil
}
func safeIssueCode(code string) string {
	switch code {
	case "invalid_material_format", "invalid_identifier", "invalid_account", "invalid_totp_secret", "duplicate_identity", "invalid_proxy", "invalid_options", "invalid_expected_identity":
		return code
	}
	return "gateway_validation_failed"
}
func (a *App) Create(in Input) (JobView, bool, error) {
	if !requestIDPattern.MatchString(in.RequestID) || in.RequestID == "." || in.RequestID == ".." {
		return JobView{}, false, fault(400, "invalid_request_id", "request_id 需要 1–64 位字母、数字或 _ . : -。")
	}
	groups, p, e := normalize(in)
	if e != nil {
		return JobView{}, false, e
	}
	if !p.Valid {
		return JobView{}, false, fault(400, "invalid_material", "账号材料校验失败，请先预览并修正。")
	}
	fp, e := inputFingerprint(in)
	if e != nil {
		return JobView{}, false, e
	}
	// Preserve idempotent lookup even if the account was deleted after submit.
	if state, err := a.store.View(); err != nil {
		return JobView{}, false, err
	} else {
		for _, j := range state.Jobs {
			if j.ID == in.RequestID && j.Fingerprint == fp {
				return jobViewWithRetries(j, retryReferences(state)), true, nil
			}
		}
		for _, j := range state.Archived {
			if j.ID == in.RequestID && j.Fingerprint == fp {
				return archiveView(j), true, nil
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recovery, e := a.bindRecovery(ctx, in, groups)
	if e != nil {
		return JobView{}, false, e
	}
	var view JobView
	existing := false
	e = a.store.Update(func(s *State) error {
		for _, j := range s.Jobs {
			if j.ID == in.RequestID {
				if j.Fingerprint != fp {
					return fault(409, "idempotency_conflict", "此请求 ID 已绑定其他材料，请查找原任务。")
				}
				view = jobViewWithRetries(j, retryReferences(*s))
				existing = true
				return nil
			}
		}
		for _, archived := range s.Archived {
			if archived.ID == in.RequestID {
				if archived.Fingerprint != fp {
					return fault(409, "idempotency_conflict", "此请求 ID 已归档且材料不同，不能重新绑定。")
				}
				view = archiveView(archived)
				existing = true
				return nil
			}
		}
		if in.RetryOf != nil {
			if err := checkRetry(*s, *in.RetryOf, p.Rows[0].Identifier); err != nil {
				return err
			}
		}
		if !settingsView(s.Settings).Configured {
			return fault(409, "not_configured", "请先配置 OAuth 连接。")
		}
		if len(s.Jobs) >= maxJobs {
			return fault(409, "state_capacity", "任务记录已达到首版上限，请管理员归档后处理。")
		}
		incoming := map[string]bool{}
		for _, g := range groups {
			for _, account := range g.Request.Accounts {
				incoming[account.Identifier] = true
			}
		}
		active := 0
		for _, j := range s.Jobs {
			if recovery != nil && j.Recovery != nil && recovery.AccountID == j.Recovery.AccountID && recoveryInFlight(j) {
				return fault(409, "account_busy", "原账号已有未解决的恢复任务，请处理原任务。")
			}
			v := jobView(j)
			if v.CanRefresh || v.CanRetryImport {
				active++
				// The checked source row is resolved, even if a sibling needs retry-import.
				if in.RetryOf != nil && j.ID == in.RetryOf.JobID {
					continue
				}
				for _, g := range j.Groups {
					for _, account := range g.Request.Accounts {
						if incoming[account.Identifier] {
							return fault(409, "account_busy", "相同账号还有未解决的任务，请处理原任务。")
						}
					}
				}
			}
		}
		if active >= 8 {
			return fault(429, "capacity_exceeded", "最多同时保留 8 个未解决任务。")
		}
		for i := range groups {
			var nonce [16]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				return errors.New("cannot allocate upstream request ID")
			}
			groups[i].Request.RequestID = "bridge_" + hex.EncodeToString(nonce[:])
		}
		now := time.Now().UTC()
		job := Job{ID: in.RequestID, Fingerprint: fp, Settings: s.Settings, Groups: groups, GroupIDs: sortedGroups(in.GroupIDs), RuntimeProxy: in.RuntimeProxy, CreatedAt: now, UpdatedAt: now}
		job.PauseAutoDetection = in.PauseAutoDetection
		if job.PauseAutoDetection {
			// Older workers must not import a paused account as eligible for detection.
			s.Version = 6
		}
		job.SaveCredentials, job.Recovery = in.SaveCredentials, recovery
		if in.RetryOf != nil {
			retry := *in.RetryOf
			job.RetryOf = &retry
			// Older workers would drop the reference, losing durable deduplication.
			s.Version = max(s.Version, 5)
		}
		if recovery != nil {
			for _, r := range s.Recoveries {
				if r.Snapshot.AccountID == recovery.AccountID && r.Enabled {
					job.RecoveryEpoch = r.Epoch
				}
			}
		}
		for _, group := range groups {
			if s.Version < 4 && len(group.RuntimeProxies) > 0 {
				// Older workers must refuse this state rather than ignore account pins.
				s.Version = 4
				break
			}
		}
		s.Jobs = append(s.Jobs, job)
		view = jobView(job)
		return nil
	})
	if e == nil {
		a.notify()
	}
	return view, existing, e
}
func (a *App) Jobs() ([]JobView, error) {
	s, e := a.store.View()
	if e != nil {
		return nil, e
	}
	out := make([]JobView, 0, len(s.Jobs))
	refs := retryReferences(s)
	for i := len(s.Jobs) - 1; i >= 0; i-- {
		out = append(out, jobViewWithRetries(s.Jobs[i], refs))
	}
	return out, nil
}
func (a *App) Job(id string) (JobView, error) {
	s, e := a.store.View()
	if e != nil {
		return JobView{}, e
	}
	for _, j := range s.Jobs {
		if j.ID == id {
			return jobViewWithRetries(j, retryReferences(s)), nil
		}
	}
	for _, archived := range s.Archived {
		if archived.ID == id {
			return archiveView(archived), nil
		}
	}
	return JobView{}, fault(404, "not_found", "任务不存在。")
}
func (a *App) changeJob(id string, change func(*Job) error) error {
	return a.store.Update(func(s *State) error {
		for i := range s.Jobs {
			if s.Jobs[i].ID == id {
				if e := change(&s.Jobs[i]); e != nil {
					return e
				}
				s.Jobs[i].UpdatedAt = time.Now().UTC()
				purgeMaterials(&s.Jobs[i])
				return nil
			}
		}
		return fault(404, "not_found", "任务不存在。")
	})
}
func (a *App) Action(id, action string) (JobView, error) {
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	e := a.changeJob(id, func(j *Job) error {
		v := jobView(*j)
		switch action {
		case "refresh":
			if !v.CanRefresh {
				return fault(409, "task_terminal", "任务已结束，无需查询。")
			}
			for i := range j.Groups {
				g := &j.Groups[i]
				if g.Error != nil && g.Error.Code == "idempotency_conflict" {
					return fault(409, "idempotency_conflict", "远端请求内容冲突，必须人工核验，不能自动绑定查回结果。")
				}
				if g.Stage == "needs_review" {
					if g.Batch.BatchID == "" {
						g.Stage = "submit_unknown"
					} else {
						g.Stage = "running"
					}
				}
				g.NextCheck = time.Time{}
				resetReadRetry(g)
			}
		case "cancel":
			if !v.CanCancel {
				return fault(409, "cannot_cancel", "任务没有可取消的项目。")
			}
			j.CancelRequested = true
			for i := range j.Groups {
				j.Groups[i].NextCheck = time.Time{}
			}
		case "retry-import":
			if !v.CanRetryImport {
				return fault(409, "cannot_retry_import", "没有确定失败且可重试的导入；未知结果需人工核验。")
			}
			for i := range j.Groups {
				g := &j.Groups[i]
				for d := range g.Deliveries {
					if g.Deliveries[d].State == "import_failed" {
						g.Deliveries[d].State = "ready"
						g.Deliveries[d].Error = nil
						g.Stage = "running"
						g.NextCheck = time.Time{}
					}
				}
			}
		default:
			return fault(404, "not_found", "操作不存在。")
		}
		return nil
	})
	if e != nil {
		return JobView{}, e
	}
	a.notify()
	return a.Job(id)
}
func safeError(code, source, action string) *Problem {
	return &Problem{Code: code, Message: "操作未完成，请按错误码及处理动作检查原任务。", Source: source, Action: action}
}
func publicError(err error) *Problem {
	var remote *upstream.Error
	if errors.As(err, &remote) {
		if local := localImportProblem(remote); local != nil {
			return local
		}
		// Local adapters may construct errors directly, bypassing the wire decoder.
		raw, marshalErr := json.Marshal(remote)
		var clean upstream.Error
		if marshalErr == nil && json.Unmarshal(raw, &clean) == nil {
			clean.Status, clean.Uncertain = remote.Status, remote.Uncertain
			copy := Problem(clean)
			return &copy
		}
		return safeError("upstream_error", "execution", "manual_review")
	}
	var f *Fault
	if errors.As(err, &f) {
		return &Problem{Code: f.Code, Message: f.Message, Source: "bridge", Action: "fix_request"}
	}
	return safeError("bridge_error", "bridge", "manual_review")
}
