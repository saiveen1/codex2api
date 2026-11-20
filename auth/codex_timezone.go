package auth

import (
	"strings"
	"time"

	"github.com/codex2api/database"
)

// AccountTimezoneCredentialKey 是账号绑定时区在 credentials 中的存储键。Claude 账号
// 早已用它做身份一致性标签；Codex 官方账号复用同一键，含义是"该账号对上游呈现的
// 时区"，空 = 不绑定、下游客户端的时区原样透传。
const AccountTimezoneCredentialKey = "timezone"

// NormalizeAccountTimezone 归一化账号绑定时区：空串、"Local" 与无法加载的 IANA 名字
// 一律返回空（= 不绑定）。宁可静默回落也不要让一个坏值把整个账号的请求打断。
func NormalizeAccountTimezone(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "Local") {
		return ""
	}
	if _, err := time.LoadLocation(value); err != nil {
		return ""
	}
	return value
}

// EffectiveCodexTimezone 返回 Codex 官方出站路径生效的绑定时区。
// 中转型账号（OpenAI Responses / Grok / Antigravity / Claude）不走 Codex 官方出站，恒为空。
func (a *Account) EffectiveCodexTimezone() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.isRelayStyleLocked() {
		return ""
	}
	return strings.TrimSpace(a.Timezone)
}

// SetAccountTimezoneDefaults changes the inherited context timezone without editing accounts.
func (s *Store) SetAccountTimezoneDefaults(timezone string, useFixedProxy bool) {
	timezone = NormalizeAccountTimezone(timezone)
	s.mu.Lock()
	s.accountDefaultTimezone = timezone
	s.useFixedProxyTimezone = useFixedProxy
	s.mu.Unlock()
}

func (s *Store) reloadFixedProxyTimezonesLocked(rows []*database.ProxyRow) {
	s.fixedProxyTimezones = make(map[string]string)
	for _, row := range rows {
		if row == nil || !row.Enabled || row.TestStatus != database.ProxyTestStatusSuccess {
			continue
		}
		if timezone := NormalizeAccountTimezone(row.TestTimezone); timezone != "" {
			s.fixedProxyTimezones[proxyRowURL(row)] = timezone
		}
	}
}

// EffectiveCodexContextTimezone supplies the environment context, not device attestation.
func (a *Account) EffectiveCodexContextTimezone(proxyOverride string) string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	if a.isRelayStyleLocked() {
		a.mu.RUnlock()
		return ""
	}
	explicit, s, id, pin := strings.TrimSpace(a.Timezone), a.proxyStore, a.DBID, strings.TrimSpace(a.ProxyURL)
	a.mu.RUnlock()
	if explicit != "" || s == nil {
		return explicit
	}
	proxyOverride = strings.TrimSpace(proxyOverride)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.useFixedProxyTimezone && pin != "" && (proxyOverride == "" || proxyOverride == pin) && !ResinEgressEnabled() && s.proxyAllowedForAccountLocked(id, pin) {
		if timezone := s.fixedProxyTimezones[pin]; timezone != "" {
			return timezone
		}
	}
	return s.accountDefaultTimezone
}

// ApplyAccountTimezone 把管理端改动的绑定时区同步到运行时账号，
// 避免等到下一次全量重载才生效。
func (s *Store) ApplyAccountTimezone(dbID int64, timezone string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.Timezone = NormalizeAccountTimezone(timezone)
	acc.mu.Unlock()
	return true
}
