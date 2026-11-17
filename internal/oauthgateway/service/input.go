package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
var phonePattern = regexp.MustCompile(`^\+[0-9]{7,15}$`)
var secretPattern = regexp.MustCompile(`^[A-Z2-7]{16,64}$`)

func normalize(in Input) ([]Group, Preview, error) {
	p := Preview{Valid: true, Rows: []PreviewRow{}, Errors: []upstream.PreviewIssue{}}
	if in.RecoveryAccountID < 0 {
		return nil, p, fault(400, "invalid_recovery_account", "恢复账号 ID 必须为正整数。")
	}
	if r := in.RetryOf; r != nil {
		if in.RecoveryAccountID != 0 || !requestIDPattern.MatchString(r.JobID) || r.JobID == "." || r.JobID == ".." || r.Line < 1 {
			return nil, p, fault(400, "invalid_retry_of", "重授权必须引用普通新增任务的有效原任务 ID 和行号，不能与账号恢复同时使用。")
		}
	}
	if in.RecoveryAccountID > 0 && (len(in.GroupIDs) > 0 || in.RuntimeProxy != "") {
		return nil, p, fault(400, "invalid_recovery_destination", "修复原账号不能更改分组或运行代理。")
	}
	if (in.SourceText != "") == (in.Accounts != nil) {
		return nil, p, fault(400, "one_material_source_required", "请只提供 TXT 文本或结构化账号数组。")
	}
	accounts := in.Accounts
	lines := make([]int, 0)
	if in.SourceText != "" {
		accounts = []InputAccount{}
		for i, line := range strings.Split(strings.TrimPrefix(in.SourceText, "\ufeff"), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			fields := strings.Split(line, "----")
			if len(fields) != 3 {
				p.Errors = append(p.Errors, upstream.PreviewIssue{Line: i + 1, Code: "invalid_material_format"})
				continue
			}
			accounts = append(accounts, InputAccount{Identifier: fields[0], Password: fields[1], TOTPSecret: fields[2]})
			lines = append(lines, i+1)
		}
	} else {
		for i := range accounts {
			lines = append(lines, i+1)
		}
	}
	if len(accounts) == 0 || len(accounts) > 100 {
		return nil, p, fault(400, "invalid_accounts", "每次任务需要 1–100 个账号。")
	}
	if in.RecoveryAccountID > 0 && len(accounts) != 1 {
		return nil, p, fault(400, "invalid_recovery_material", "修复原账号必须只提供一份账号材料。")
	}
	if in.RetryOf != nil && len(accounts) != 1 {
		return nil, p, fault(400, "invalid_retry_material", "重授权必须重新提供一份账号材料。")
	}
	hasRuntimeOverrides := false
	for _, account := range accounts {
		if account.RuntimeProxy != "" {
			if in.RecoveryAccountID > 0 {
				return nil, p, fault(400, "invalid_recovery_destination", "修复原账号不能更改分组或运行代理。")
			}
			hasRuntimeOverrides = true
		}
	}
	if len(in.GroupIDs) > 100 {
		return nil, p, fault(400, "invalid_groups", "选择的分组过多。")
	}
	for _, id := range in.GroupIDs {
		if id <= 0 {
			return nil, p, fault(400, "invalid_groups", "分组 ID 必须为正整数。")
		}
	}
	if len(in.RuntimeProxy) > 4096 || strings.ContainsAny(in.RuntimeProxy, "\r\n\x00") {
		return nil, p, fault(400, "invalid_proxy", "账号日常请求代理格式无效。")
	}
	opts := in.Options
	if opts.Country == "" {
		opts.Country = "auto"
	}
	if opts.Environment == "" {
		opts.Environment = "auto"
	}
	if opts.ProxyMode == "" {
		opts.ProxyMode = "system"
	}
	if opts.WorkspacePolicy == "" {
		opts.WorkspacePolicy = "team_preferred"
	}
	groups := []Group{}
	indexes := map[string]int{}
	seen := map[string]bool{}
	for i, account := range accounts {
		line := lines[i]
		identifier := strings.ToLower(strings.TrimSpace(account.Identifier))
		secret := strings.ToUpper(account.TOTPSecret)
		validID := phonePattern.MatchString(identifier)
		if !validID {
			address, err := mail.ParseAddress(identifier)
			validID = err == nil && address.Address == identifier && strings.Contains(identifier, "@") && len(identifier) <= 254
		}
		code := ""
		switch {
		case !validID:
			code = "invalid_identifier"
		case account.Password == "" || utf8.RuneCountInString(account.Password) > 1024 || !utf8.ValidString(account.Password):
			code = "invalid_account"
		case !secretPattern.MatchString(secret):
			code = "invalid_totp_secret"
		case seen[identifier]:
			code = "duplicate_identity"
		case len(account.RuntimeProxy) > 4096 || strings.ContainsAny(account.RuntimeProxy, "\r\n\x00"):
			code = "invalid_runtime_proxy"
		}
		seen[identifier] = true
		if code != "" {
			p.Errors = append(p.Errors, upstream.PreviewIssue{Line: line, Code: code})
			continue
		}
		itemOptions := opts
		if proxyProvided(account.Proxy) {
			itemOptions.ProxyMode = "custom"
			itemOptions.Proxy = account.Proxy
		}
		if (itemOptions.ProxyMode == "custom" && !proxyProvided(itemOptions.Proxy)) || (itemOptions.ProxyMode == "system" && proxyProvided(itemOptions.Proxy)) || (itemOptions.ProxyMode != "system" && itemOptions.ProxyMode != "custom") {
			p.Errors = append(p.Errors, upstream.PreviewIssue{Line: line, Code: "invalid_proxy"})
			continue
		}
		raw, err := json.Marshal(itemOptions)
		if err != nil {
			return nil, p, fault(400, "invalid_options", "授权选项格式无效。")
		}
		k := string(raw)
		idx, exists := indexes[k]
		if !exists {
			idx = len(groups)
			indexes[k] = idx
			groups = append(groups, Group{Request: upstream.SubmitRequest{Options: itemOptions}, Stage: "queued", Lines: []int{}, Masks: []string{}, Deliveries: []Delivery{}})
		}
		groups[idx].Request.Accounts = append(groups[idx].Request.Accounts, upstream.Account{Identifier: identifier, Password: account.Password, TOTPSecret: secret})
		groups[idx].Lines = append(groups[idx].Lines, line)
		groups[idx].Masks = append(groups[idx].Masks, mask(identifier))
		if hasRuntimeOverrides {
			runtimeProxy := account.RuntimeProxy
			if runtimeProxy == "" {
				runtimeProxy = in.RuntimeProxy
			}
			groups[idx].RuntimeProxies = append(groups[idx].RuntimeProxies, runtimeProxy)
		}
		p.Rows = append(p.Rows, PreviewRow{Line: line, Identifier: identifier, IdentifierMasked: mask(identifier), ProxyMode: itemOptions.ProxyMode})
	}
	p.Count = len(accounts)
	p.Groups = len(groups)
	p.Valid = len(p.Errors) == 0
	return groups, p, nil
}

func proxyProvided(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value != ""
	}
	return true
}
func mask(identifier string) string {
	if idx := strings.LastIndex(identifier, "@"); idx >= 0 {
		prefix := []rune(identifier[:idx])
		if len(prefix) > 0 {
			return string(prefix[0]) + "***" + identifier[idx:]
		}
	}
	if len(identifier) > 4 {
		return "***" + identifier[len(identifier)-4:]
	}
	return "***"
}
func inputFingerprint(in Input) (string, error) {
	in.RequestID = ""
	raw, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func sortedGroups(ids []int64) []int64 {
	set := map[int64]bool{}
	out := []int64{}
	for _, id := range ids {
		if !set[id] {
			set[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
func compatible(cap upstream.Capabilities) error {
	if cap.Protocol.Name != "codex-account-gateway.oauth" || cap.Protocol.Version != 1 || cap.Protocol.Revision < 3 || !cap.Features["result_item_mapping"] || !cap.Features["request_id_lookup"] {
		return fault(409, "unsupported_contract", "网关需要支持公共 v1 revision 3 协议、逐项结果映射和原请求查询。")
	}
	found := false
	for _, method := range cap.OAuthMethods {
		if method == "password_totp" {
			found = true
		}
	}
	if !found {
		return fault(409, "unsupported_method", "网关未声明支持账密与 TOTP 授权。")
	}
	return nil
}

func checkUploadSize(cap upstream.Capabilities, request upstream.SubmitRequest) error {
	raw, err := json.Marshal(request)
	if err != nil || cap.MaxUploadBytes < 1 || len(raw) > cap.MaxUploadBytes || len(raw) > 1<<20 {
		return fault(400, "request_too_large", "完整授权请求超过网关上传限制，请减少材料或代理选项。")
	}
	return nil
}
