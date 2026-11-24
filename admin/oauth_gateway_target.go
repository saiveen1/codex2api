package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/oauthgateway/service"
	"github.com/codex2api/internal/oauthgateway/upstream"
	"github.com/codex2api/internal/openaiidentity"
	"github.com/tidwall/gjson"
)

type oauthGatewayTarget struct{ h *Handler }

func (t oauthGatewayTarget) Groups(ctx context.Context) ([]upstream.Group, error) {
	groups, err := t.h.db.ListAccountGroups(ctx)
	if err != nil {
		return nil, errors.New("local account groups unavailable")
	}
	out := make([]upstream.Group, 0, len(groups))
	for _, group := range groups {
		if group.Channel == database.AccountGroupChannelCodex {
			out = append(out, upstream.Group{ID: group.ID, Name: group.Name})
		}
	}
	return out, nil
}

func (t oauthGatewayTarget) AccountResult(ctx context.Context, id int64) service.AccountResult {
	row, err := t.h.db.GetAccountByID(ctx, id)
	if err != nil || row.Platform != "openai" || row.Type != "oauth" {
		return service.AccountResult{}
	}
	result := service.AccountResult{
		WorkspaceID:    openaiidentity.NormalizeWorkspaceID(row.GetCredential("workspace_id")),
		WorkspaceName:  row.GetCredential("workspace_name"),
		PlanType:       row.GetCredential("plan_type"),
		RuntimeProxyID: t.h.db.OAuthGatewayProxyID(ctx, row.ProxyURL),
	}
	if ids, err := t.h.db.GetAccountGroupIDs(ctx, id); err == nil {
		result.GroupIDs = &ids
	}
	return result
}

func (h *Handler) validateOAuthGatewayDestination(ctx context.Context, groupIDs []int64, runtimeProxy string, accountProxies ...string) error {
	if h.db == nil {
		return database.ErrOAuthGatewayState
	}
	if len(groupIDs) > 100 {
		return oauthGatewayFault(400, "invalid_groups", "目标分组过多。")
	}
	groups, err := (oauthGatewayTarget{h}).Groups(ctx)
	if err != nil {
		return err
	}
	available := make(map[int64]bool, len(groups))
	for _, group := range groups {
		available[group.ID] = true
	}
	for _, id := range groupIDs {
		if !available[id] {
			return oauthGatewayFault(400, "invalid_groups", "请选择存在的 Codex 分组。")
		}
	}
	if len(accountProxies) > 100 {
		return oauthGatewayFault(400, "invalid_accounts", "每次任务最多 100 个账号。")
	}
	requested := make(map[string]bool)
	for _, raw := range append([]string{runtimeProxy}, accountProxies...) {
		if raw == "" {
			continue
		}
		normalized, err := normalizeManagedProxyURL(raw)
		if len(raw) > 4096 || err != nil || normalized != raw {
			return oauthGatewayFault(400, "invalid_runtime_proxy", "账号运行代理地址无效；不会回退直连。")
		}
		requested[raw] = true
	}
	if len(requested) == 0 {
		return nil
	}
	proxies, err := h.db.ListEnabledProxies(ctx)
	if err != nil {
		return errors.New("runtime proxy registry unavailable")
	}
	for _, p := range proxies {
		delete(requested, p.URL)
	}
	if len(requested) > 0 {
		return oauthGatewayFault(400, "runtime_proxy_unavailable", "请先在代理管理中添加并启用每个账号的运行代理；新账号不能使用受限专用或测试失败的代理，不会回退直连。")
	}
	return nil
}

func oauthGatewayImportFailure(code string) (upstream.ImportResult, error) {
	return upstream.ImportResult{Total: 1, Failed: 1}, service.LocalImportError(code)
}

// Only explicit workspace metadata is eligible; the account's own name may be an email.
func oauthGatewayWorkspaceName(raw json.RawMessage, workspaceID string) string {
	paths := []string{"workspace_name"} // A matched batch summary is carried here by ValidateDelivery.
	if field := gjson.GetBytes(raw, "extra.oauth_workspace.id"); field.Type == gjson.String && openaiidentity.NormalizeWorkspaceID(field.Str) == workspaceID && openaiidentity.ValidWebWorkspaceID(workspaceID) {
		paths = append(paths, "extra.oauth_workspace.name")
	}
	paths = append(paths, "credentials.workspace_name", "extra.workspace_name", "credentials.chatgpt_account_name")
	for _, path := range paths {
		if field := gjson.GetBytes(raw, path); field.Type == gjson.String {
			if name := upstream.NormalizeWorkspaceName(field.Str); name != "" {
				return name
			}
		}
	}
	return ""
}

func setOAuthGatewayEnvironment(credentials map[string]any, raw json.RawMessage) {
	if environment := upstream.KnownEnvironment(gjson.GetBytes(raw, upstream.EnvironmentCredential).Str); environment != "" {
		credentials[upstream.EnvironmentCredential] = environment
	}
}

// Import uses the ordinary account parser and database/runtime entry points,
// without making an HTTP request to our own administrator endpoint. This is not
// a recovery upsert: even a freshly authorized duplicate cannot replace a newer
// credential or change the existing account's proxy, groups, status or history.
func (t oauthGatewayTarget) Import(ctx context.Context, raw json.RawMessage, groupIDs []int64, runtimeProxy string, pauseAutoDetection bool) (upstream.ImportResult, error) {
	var result struct {
		Platform string `json:"platform"`
		Type     string `json:"type"`
		sub2apiAccountEntry
	}
	if len(raw) > 1<<20 || json.Unmarshal(raw, &result) != nil || result.Platform != "openai" || result.Type != "oauth" {
		return oauthGatewayImportFailure("invalid_gateway_account")
	}
	c := result.Credentials
	if c.AuthMode != "" || c.AgentIdentity != nil || c.AgentPrivateKey != "" || c.AgentRuntimeID != "" || strings.TrimSpace(c.RefreshToken) == "" || strings.TrimSpace(c.AccessToken) == "" {
		return oauthGatewayImportFailure("invalid_gateway_account")
	}
	tokens := sub2apiAccountEntryToTokens(result.sub2apiAccountEntry)
	if len(tokens) != 1 {
		return oauthGatewayImportFailure("invalid_gateway_account")
	}
	seed := importTokenSeed(tokens[0], nil)
	workspace := openaiidentity.NormalizeWorkspaceID(c.ChatGPTAccountID)
	user := strings.TrimSpace(c.ChatGPTUserID)
	tokenEmail, tokenWorkspace := openaiidentity.TokenIdentity(c.IDToken, c.AccessToken)
	if tokenEmail != "" && (!strings.EqualFold(tokenEmail, seed.email) || (workspace != "" && tokenWorkspace != workspace)) {
		return oauthGatewayImportFailure("gateway_identity_mismatch")
	}
	if (seed.workspaceID != "" && workspace != "" && seed.workspaceID != workspace) || (seed.userID != "" && user != "" && seed.userID != user) {
		return oauthGatewayImportFailure("gateway_identity_mismatch")
	}
	if err := t.h.validateOAuthGatewayDestination(ctx, groupIDs, runtimeProxy); err != nil {
		return oauthGatewayImportFailure("invalid_import_destination")
	}
	if workspace == "" || user == "" {
		if workspace != "" {
			seed.accountID, seed.workspaceID = workspace, workspace
		}
		if user != "" {
			seed.userID = user
		}
		var identityErr error
		seed, identityErr = t.h.prepareTokenCredentialSeed(ctx, seed, runtimeProxy, 0)
		if identityErr != nil {
			return oauthGatewayImportFailure("gateway_identity_mismatch")
		}
		if seed.workspaceDiagnostic != "" || !openaiidentity.ValidWebWorkspaceID(seed.workspaceID) || seed.userID == "" {
			return oauthGatewayImportFailure("gateway_identity_unverified")
		}
		workspace, user = seed.workspaceID, seed.userID
	}
	if !openaiidentity.ValidWebWorkspaceID(workspace) || user == "" || seed.email == "" {
		return oauthGatewayImportFailure("gateway_identity_mismatch")
	}
	seed.accountID, seed.workspaceID, seed.userID = workspace, workspace, user
	workspaceName := oauthGatewayWorkspaceName(raw, workspace)
	if workspaceName == "" {
		workspaceName = upstream.NormalizeWorkspaceName(seed.workspaceName)
		if workspaceName == "" && !seed.identityPrepared && t.h.store != nil {
			if egress, usable := t.h.store.ResolveUsableProxyForAccount(&auth.Account{ProxyURL: runtimeProxy}); usable {
				identity := auth.CodexWorkspaceIdentity{WorkspaceID: workspace, UserID: user, Email: seed.email}
				workspaceName = upstream.NormalizeWorkspaceName(auth.LookupCodexWorkspaceName(ctx, identity, seed.accessToken, egress))
			}
		}
	}
	t.h.mergeDuplicateMu.Lock()
	defer t.h.mergeDuplicateMu.Unlock()
	duplicateID, err := t.h.findOAuthIdentityDuplicate(ctx, seed, 0)
	if err != nil {
		return oauthGatewayImportFailure("local_lookup_failed")
	}
	if duplicateID == 0 {
		duplicateID, err = t.h.findCredentialWorkspaceRouteDuplicate(ctx, seed, 0)
		if err != nil {
			return oauthGatewayImportFailure("local_lookup_failed")
		}
	}
	if duplicateID > 0 {
		return oauthGatewayImportFailure("existing_account_requires_review")
	}
	name := strings.TrimSpace(result.Name)
	if name == "" {
		name = seed.email
	}
	credentials := t.h.newCodexAccountCredentials(seed)
	setOAuthGatewayEnvironment(credentials, raw)
	if workspaceName != "" {
		credentials["workspace_name"] = workspaceName
	}
	id, err := t.h.db.InsertOAuthGatewayAccount(ctx, name, credentials, runtimeProxy, groupIDs, pauseAutoDetection)
	if err != nil {
		// A lost COMMIT response may already have inserted an account. Never
		// classify arbitrary SQL errors as proof that another write is safe.
		return upstream.ImportResult{}, service.LocalImportError("import_outcome_unknown")
	}
	accepted := upstream.ImportResult{Total: 1, Success: 1, CreatedIDs: []int64{id}}
	t.h.invalidateAccountSnapshotCaches()
	t.h.db.InsertAccountEventAsync(id, "added", "oauth_gateway")
	if t.h.store != nil {
		if err := t.h.store.LoadAccountByID(ctx, id); err != nil {
			return accepted, service.LocalImportError("runtime_load_pending")
		}
		t.h.triggerImportedAccountUsageProbe(id, "oauth_gateway")
	}
	return accepted, nil
}
