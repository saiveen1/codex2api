package admin

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/openaiidentity"
)

type tokenCredentialSeed struct {
	refreshToken    string
	sessionToken    string
	accessToken     string
	accessTokenType string
	idToken         string
	accountID       string
	workspaceID     string
	// userID 是 OpenAI 用户 ID（user-...），仅作为账号元数据保存。
	userID string
	// allowDuplicate 仅允许有效工作区（Token workspace 或请求头覆盖）为空的账号重复。
	allowDuplicate        bool
	email                 string
	planType              string
	expiresAt             time.Time
	expiresAtRaw          string
	expiresIn             int64
	subscriptionExpiresAt time.Time
	codex7DUsedPercent    string
	codex7DResetAt        string
	codex5HUsedPercent    string
	codex5HResetAt        string
	codex5HUsageUpdatedAt string
	codexUsageUpdatedAt   string
	customHeaders         map[string]string
	workspaceDiagnostic   string
	identityPrepared      bool
	invalidWorkspace      bool
}

func normalizeTokenCredentialSeed(seed tokenCredentialSeed) tokenCredentialSeed {
	seed.refreshToken = strings.TrimSpace(seed.refreshToken)
	seed.sessionToken = strings.TrimSpace(seed.sessionToken)
	seed.accessToken = strings.TrimSpace(seed.accessToken)
	seed.accessTokenType = strings.TrimSpace(seed.accessTokenType)
	seed.idToken = strings.TrimSpace(seed.idToken)
	seed.accountID = strings.TrimSpace(seed.accountID)
	seed.workspaceID = strings.TrimSpace(seed.workspaceID)
	if value := openaiidentity.NormalizeWorkspaceID(seed.workspaceID); value != "" && !openaiidentity.ValidWebWorkspaceID(value) {
		seed.invalidWorkspace = true
	}
	seed.userID = strings.TrimSpace(seed.userID)
	seed.email = strings.TrimSpace(seed.email)
	seed.planType = strings.TrimSpace(seed.planType)
	seed.expiresAtRaw = strings.TrimSpace(seed.expiresAtRaw)
	seed.codex7DUsedPercent = strings.TrimSpace(seed.codex7DUsedPercent)
	seed.codex7DResetAt = strings.TrimSpace(seed.codex7DResetAt)
	seed.codex5HUsedPercent = strings.TrimSpace(seed.codex5HUsedPercent)
	seed.codex5HResetAt = strings.TrimSpace(seed.codex5HResetAt)
	seed.codex5HUsageUpdatedAt = strings.TrimSpace(seed.codex5HUsageUpdatedAt)
	seed.codexUsageUpdatedAt = strings.TrimSpace(seed.codexUsageUpdatedAt)
	if seed.accessTokenType == "" {
		seed.accessTokenType = accessTokenTypeForToken(seed.accessToken)
	}

	accessTokenForJWT := seed.accessToken
	if seed.accessTokenType == accessTokenTypeCodexAT {
		accessTokenForJWT = ""
	}
	tokenEmail, tokenWorkspaceID := openaiidentity.TokenIdentity(seed.idToken, accessTokenForJWT)
	if seed.workspaceID == "" {
		if tokenWorkspaceID != "" {
			if seed.email == "" {
				seed.email = tokenEmail
				seed.workspaceID = tokenWorkspaceID
			} else if strings.EqualFold(seed.email, tokenEmail) {
				seed.workspaceID = tokenWorkspaceID
			}
		}
	}
	if info := accountInfoFromTokens(seed.idToken, accessTokenForJWT); info != nil && (seed.email == "" || info.Email == "" || strings.EqualFold(seed.email, info.Email)) {
		if tokenWorkspaceID != "" {
			info.Email = tokenEmail
			info.ChatGPTAccountID = tokenWorkspaceID
		}
		if seed.accountID == "" {
			seed.accountID = info.ChatGPTAccountID
		}
		if seed.userID == "" {
			seed.userID = info.UserID
		}
		if seed.email == "" {
			seed.email = info.Email
		}
		if seed.planType == "" {
			seed.planType = info.PlanType
		}
		if seed.subscriptionExpiresAt.IsZero() && !info.SubscriptionExpiresAt.IsZero() {
			seed.subscriptionExpiresAt = info.SubscriptionExpiresAt
		}
	}
	// 付费套餐下已过去的到期时间是续费前的陈旧 JWT claim，导入时不落库。(issue #360)
	if auth.StaleSubscriptionExpiry(seed.planType, seed.subscriptionExpiresAt, time.Now()) {
		seed.subscriptionExpiresAt = time.Time{}
	}

	if seed.expiresAt.IsZero() && seed.expiresIn > 0 {
		seed.expiresAt = time.Now().Add(time.Duration(seed.expiresIn) * time.Second)
	}
	if seed.expiresAt.IsZero() && seed.accessToken != "" && seed.accessTokenType != accessTokenTypeCodexAT {
		if info := auth.ParseAccessToken(seed.accessToken); info != nil && !info.ExpiresAt.IsZero() {
			seed.expiresAt = info.ExpiresAt
		}
	}
	if seed.expiresAt.IsZero() && seed.expiresAtRaw != "" {
		seed.expiresAt = parseCredentialExpiresAt(seed.expiresAtRaw)
	}
	if seed.expiresAt.IsZero() && seed.accessToken != "" {
		seed.expiresAt = time.Now().Add(time.Hour)
	}

	identity := auth.NormalizeCodexWorkspaceIdentity(auth.CodexWorkspaceIdentity{
		AccountID: seed.accountID, WorkspaceID: seed.workspaceID, UserID: seed.userID, Email: seed.email, Headers: seed.customHeaders,
	}, seed.idToken, accessTokenForJWT)
	seed.accountID, seed.workspaceID, seed.userID, seed.email = identity.AccountID, identity.WorkspaceID, identity.UserID, identity.Email
	return seed
}

func effectiveWorkspaceIDFromSeed(seed tokenCredentialSeed) string {
	seed = normalizeTokenCredentialSeed(seed)
	return openaiidentity.EffectiveWorkspaceID(seed.workspaceID, seed.customHeaders)
}

// prepareTokenCredentialSeed is the shared import boundary, before dedupe or writes.
// Metadata outages remain visible pending diagnostics, not native account failures.
func (h *Handler) prepareTokenCredentialSeed(ctx context.Context, seed tokenCredentialSeed, proxyURL string, accountID int64) (tokenCredentialSeed, error) {
	seed = normalizeTokenCredentialSeed(seed)
	if seed.invalidWorkspace {
		return seed, errors.New("workspace identity: invalid_supplied_workspace")
	}
	if seed.identityPrepared {
		return seed, nil
	}
	seed.identityPrepared = true
	identity := auth.NormalizeCodexWorkspaceIdentity(auth.CodexWorkspaceIdentity{AccountID: seed.accountID, WorkspaceID: seed.workspaceID, UserID: seed.userID, Email: seed.email, Headers: seed.customHeaders}, seed.idToken, seed.accessToken)
	if _, err := openaiidentity.RequestedWebWorkspace(identity.WorkspaceID, identity.Headers); err != nil {
		return seed, errors.New("workspace identity: invalid_target")
	}
	if seed.accessTokenType == accessTokenTypeCodexAT {
		return seed, nil
	}
	// Fixture/RT-only imports cannot authenticate metadata and must remain offline.
	info := auth.ParseAccessToken(seed.accessToken)
	if info == nil || info.ExpiresAt.IsZero() {
		return seed, nil
	}
	if h == nil || h.store == nil {
		seed.workspaceDiagnostic = "workspace identity: proxy_policy_unavailable"
		return seed, nil
	}
	selected, usable := h.store.ResolveUsableProxyForAccount(&auth.Account{DBID: accountID, ProxyURL: proxyURL})
	if !usable {
		seed.workspaceDiagnostic = "workspace identity: proxy_denied"
		log.Printf("Codex import identity pending: %s", seed.workspaceDiagnostic)
		return seed, nil
	}
	proxyURL = selected
	identity, err := auth.HydrateCodexWorkspaceIdentity(ctx, identity, seed.accessToken, proxyURL)
	if err != nil {
		if auth.WorkspaceIdentityRejected(err) {
			return seed, err
		}
		seed.workspaceDiagnostic = err.Error()
		log.Printf("Codex import identity pending: %s", seed.workspaceDiagnostic)
		return seed, nil
	}
	seed.accountID, seed.workspaceID, seed.userID, seed.email = identity.AccountID, identity.WorkspaceID, identity.UserID, identity.Email
	return seed, nil
}

// PAT keeps its existing per-token timeout but now also obeys the import budget.
const patWhoAmIHydrateTimeout = 6 * time.Second

// patWhoAmIHydrator 在一次批量导入内为 PAT 补全工作区身份。网络层失败（超时 / 代理不通）
// 对同批其余 token 大概率同样失败，命中一次就熔断，剩余 token 直接按纯 AT 入库；
// 端点按 token 返回 4xx 只说明这枚 token 没权限，不影响其它 token。
type patWhoAmIHydrator struct {
	proxyURL string
	tripped  bool
	deadline time.Time
	attempts int
}

func (h *Handler) newImportPATWhoAmIHydrator(proxyURL string) *patWhoAmIHydrator {
	w := &patWhoAmIHydrator{proxyURL: proxyURL}
	if h == nil || h.store == nil {
		w.tripped = true
		return w
	}
	selected, usable := h.store.ResolveUsableProxyForAccount(&auth.Account{ProxyURL: proxyURL})
	w.proxyURL, w.tripped = selected, !usable
	return w
}

func (w *patWhoAmIHydrator) hydrate(ctx context.Context, seed tokenCredentialSeed) tokenCredentialSeed {
	if seed.accessTokenType != accessTokenTypeCodexAT {
		return seed
	}
	if w != nil {
		if w.deadline.IsZero() {
			w.deadline = time.Now().Add(8 * time.Second)
		}
		if w.attempts >= 16 || !time.Now().Before(w.deadline) {
			w.tripped = true
		}
	}
	if w == nil || w.tripped {
		return seed
	}
	w.attempts++
	ctx, cancel := context.WithDeadline(ctx, w.deadline)
	defer cancel()
	seed, err := hydrateSeedWithWhoAmI(ctx, seed, w.proxyURL)
	if err == nil {
		return seed
	}
	var statusErr *patWhoAmIStatusError
	if errors.As(err, &statusErr) {
		log.Printf("PAT whoami 身份获取失败（该 token 按纯 AT 入库）: %v", err)
		return seed
	}
	w.tripped = true
	log.Printf("PAT whoami 网络失败，本批剩余 token 跳过身份补全: %v", err)
	return seed
}

// hydrateSeedWithWhoAmI 用 /whoami 补全 PAT 的工作区 / 用户 / 邮箱 / 套餐。
// 返回的 error 仅供调用方决定是否熔断；seed 总是可用的（失败时原样返回）。
func hydrateSeedWithWhoAmI(ctx context.Context, seed tokenCredentialSeed, proxyURL string) (tokenCredentialSeed, error) {
	if seed.accessTokenType != accessTokenTypeCodexAT || strings.TrimSpace(seed.accessToken) == "" {
		return seed, nil
	}
	if seed.workspaceID != "" && seed.email != "" && seed.planType != "" {
		return seed, nil
	}
	whoamiCtx, cancel := context.WithTimeout(ctx, patWhoAmIHydrateTimeout)
	defer cancel()
	meta, err := QueryPersonalAccessTokenMetadata(whoamiCtx, seed.accessToken, proxyURL)
	if err != nil {
		return seed, err
	}
	if meta == nil {
		return seed, nil
	}
	if seed.workspaceID == "" && meta.ChatGPTAccountID != "" {
		seed.workspaceID = meta.ChatGPTAccountID
	}
	if seed.accountID == "" && meta.ChatGPTAccountID != "" {
		seed.accountID = meta.ChatGPTAccountID
	}
	if seed.userID == "" && meta.ChatGPTUserID != "" {
		seed.userID = meta.ChatGPTUserID
	}
	if seed.email == "" && meta.Email != nil {
		seed.email = strings.TrimSpace(*meta.Email)
	}
	if seed.planType == "" && meta.ChatGPTPlanType != "" {
		seed.planType = meta.ChatGPTPlanType
	}
	return seed, nil
}

const accessTokenTypeCodexAT = "codex_at"

func accessTokenTypeForToken(accessToken string) string {
	if strings.HasPrefix(strings.TrimSpace(accessToken), "at-") {
		return accessTokenTypeCodexAT
	}
	return ""
}

func accountInfoFromTokens(idToken, accessToken string) *auth.AccountInfo {
	info := auth.ParseIDToken(strings.TrimSpace(idToken))
	if info == nil {
		info = &auth.AccountInfo{}
	}
	if atInfo := auth.ParseAccessToken(strings.TrimSpace(accessToken)); atInfo != nil {
		if info.ChatGPTAccountID == "" {
			info.ChatGPTAccountID = atInfo.ChatGPTAccountID
		}
		if info.UserID == "" {
			info.UserID = atInfo.UserID
		}
		if info.Email == "" {
			info.Email = atInfo.Email
		}
		if info.PlanType == "" {
			info.PlanType = atInfo.PlanType
		}
		if info.SubscriptionExpiresAt.IsZero() && !atInfo.SubscriptionExpiresAt.IsZero() {
			info.SubscriptionExpiresAt = atInfo.SubscriptionExpiresAt
		}
	}
	return info
}

func tokenCredentialMap(seed tokenCredentialSeed) map[string]interface{} {
	seed = normalizeTokenCredentialSeed(seed)
	credentials := make(map[string]interface{})
	// Empty diagnostics clear a previous pending result on a successful reimport.
	credentials["codex_workspace_error"] = seed.workspaceDiagnostic
	if seed.refreshToken != "" {
		credentials["refresh_token"] = seed.refreshToken
	}
	if seed.sessionToken != "" {
		credentials["session_token"] = seed.sessionToken
	}
	if seed.accessToken != "" {
		credentials["access_token"] = seed.accessToken
	}
	if seed.accessTokenType != "" {
		credentials["access_token_type"] = seed.accessTokenType
	}
	if seed.idToken != "" {
		credentials["id_token"] = seed.idToken
	}
	if !seed.expiresAt.IsZero() {
		credentials["expires_at"] = seed.expiresAt.Format(time.RFC3339)
	}
	// Empty values clear a legacy user-ID workspace on credential replacement.
	if seed.accountID != "" || seed.userID != "" {
		credentials["account_id"] = seed.accountID
	}
	if seed.workspaceID != "" || seed.userID != "" {
		credentials["workspace_id"] = seed.workspaceID
	}
	if seed.userID != "" {
		credentials["user_id"] = seed.userID
	}
	if seed.allowDuplicate && effectiveWorkspaceIDFromSeed(seed) == "" {
		credentials["allow_duplicate"] = "true"
	}
	if seed.email != "" {
		credentials["email"] = seed.email
	}
	if seed.planType != "" {
		credentials["plan_type"] = seed.planType
	}
	if !seed.subscriptionExpiresAt.IsZero() {
		credentials["subscription_expires_at"] = seed.subscriptionExpiresAt.Format(time.RFC3339)
	}
	if seed.codex7DUsedPercent != "" {
		credentials["codex_7d_used_percent"] = seed.codex7DUsedPercent
	}
	if seed.codex7DResetAt != "" {
		credentials["codex_7d_reset_at"] = seed.codex7DResetAt
	}
	if seed.codex5HUsedPercent != "" {
		credentials["codex_5h_used_percent"] = seed.codex5HUsedPercent
	}
	if seed.codex5HResetAt != "" {
		credentials["codex_5h_reset_at"] = seed.codex5HResetAt
	}
	if seed.codex5HUsageUpdatedAt != "" {
		credentials["codex_5h_usage_updated_at"] = seed.codex5HUsageUpdatedAt
	}
	if seed.codexUsageUpdatedAt != "" {
		credentials["codex_usage_updated_at"] = seed.codexUsageUpdatedAt
	}
	if len(seed.customHeaders) > 0 {
		credentials["custom_headers"] = cloneCustomHeaders(seed.customHeaders)
	}
	return credentials
}

// defaultCodexFingerprintModeForNewAccount 返回系统设置里配置的「新导入账号默认
// 指纹收敛档位」（off/device/session/full，缺省 off）。
func (h *Handler) defaultCodexFingerprintModeForNewAccount() string {
	if h == nil || h.store == nil {
		return auth.CodexFingerprintModeOff
	}
	return auth.NormalizeCodexFingerprintMode(h.store.GetCodexFingerprintDefaultMode())
}

// newCodexAccountCredentials 为新建/新导入的 Codex 账号生成 credentials，并盖上
// 系统默认指纹收敛档位。仅用于插入新账号；更新已有账号凭证仍走 tokenCredentialMap，
// 避免重新导入时覆盖用户在账号上手动调整过的档位。
func (h *Handler) newCodexAccountCredentials(seed tokenCredentialSeed) map[string]interface{} {
	credentials := tokenCredentialMap(seed)
	if mode := h.defaultCodexFingerprintModeForNewAccount(); mode != auth.CodexFingerprintModeOff {
		credentials[auth.CodexFingerprintModeCredentialKey] = mode
	}
	return credentials
}

// newCodexAccountFromSeed 构造刚插入账号的内存态，指纹档位与
// newCodexAccountCredentials 落库内容保持一致（默认档位无需重载即生效）。
func (h *Handler) newCodexAccountFromSeed(id int64, proxyURL string, seed tokenCredentialSeed) *auth.Account {
	account := accountFromCredentialSeed(id, proxyURL, seed)
	account.CodexFingerprintMode = h.defaultCodexFingerprintModeForNewAccount()
	return account
}

func accountFromCredentialSeed(id int64, proxyURL string, seed tokenCredentialSeed) *auth.Account {
	seed = normalizeTokenCredentialSeed(seed)
	account := &auth.Account{
		DBID:                  id,
		RefreshToken:          seed.refreshToken,
		SessionToken:          seed.sessionToken,
		AccessToken:           seed.accessToken,
		ExpiresAt:             seed.expiresAt,
		AccountID:             seed.accountID,
		Email:                 seed.email,
		PlanType:              seed.planType,
		ProxyURL:              proxyURL,
		CustomHeaders:         cloneCustomHeaders(seed.customHeaders),
		Status:                auth.StatusReady,
		SubscriptionExpiresAt: seed.subscriptionExpiresAt,
	}
	if pct, ok := parseSeedUsagePercent(seed.codex7DUsedPercent); ok {
		updatedAt := parseSeedRFC3339(seed.codexUsageUpdatedAt)
		account.SetUsageSnapshot(pct, updatedAt)
		if resetAt := parseSeedRFC3339(seed.codex7DResetAt); !resetAt.IsZero() {
			account.SetReset7dAt(resetAt)
		}
	}
	if pct, ok := parseSeedUsagePercent(seed.codex5HUsedPercent); ok {
		account.SetUsageSnapshot5hAt(pct, parseSeedRFC3339(seed.codex5HResetAt), parseSeedRFC3339(seed.codex5HUsageUpdatedAt))
	}
	return account
}

func parseSeedUsagePercent(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func parseSeedRFC3339(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func parseCredentialExpiresAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}

	if unixSeconds, err := strconv.ParseFloat(raw, 64); err == nil && unixSeconds > 0 {
		if unixSeconds >= 1e12 {
			return time.UnixMilli(int64(unixSeconds))
		}
		return time.Unix(int64(unixSeconds), 0)
	}

	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

func mergeImportCustomHeaders(base, overrides map[string]string) map[string]string {
	result := cloneCustomHeaders(base)
	if len(overrides) == 0 {
		return result
	}
	if result == nil {
		result = make(map[string]string)
	}
	for name, value := range overrides {
		for old := range result {
			if strings.EqualFold(strings.TrimSpace(old), strings.TrimSpace(name)) {
				delete(result, old)
			}
		}
		result[name] = value
	}
	return result
}

func (h *Handler) prepareImportedTokenSeeds(ctx context.Context, tokens []importToken, settings importSettings) ([]importToken, error) {
	conflicts := conflictingImportChatGPTIDs(tokens)
	for i := range tokens {
		if tokens[i].isAgentIdentity() {
			continue
		}
		seed := importTokenSeed(tokens[i], conflicts)
		headers, err := normalizeCustomHeaders(mergeImportCustomHeaders(seed.customHeaders, settings.customHeaders))
		if err != nil {
			return nil, errors.New("import identity: invalid custom headers")
		}
		seed.customHeaders = headers
		seed, err = h.prepareTokenCredentialSeed(ctx, seed, settings.proxyForToken(tokens[i]), 0)
		if err != nil {
			return nil, err
		}
		tokens[i].preparedSeed = &seed
	}
	return tokens, nil
}

func joinImportWorkspaceWarnings(warnings ...string) string {
	var nonempty []string
	for _, warning := range warnings {
		if warning != "" {
			nonempty = append(nonempty, warning)
		}
	}
	return strings.Join(nonempty, "; ")
}
