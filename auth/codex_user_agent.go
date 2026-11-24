package auth

const CodexUserAgentCredentialKey = "codex_user_agent"

func (s *Store) ApplyAccountCodexUserAgent(dbID int64, value string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.CodexUserAgent = value
	acc.mu.Unlock()
	return true
}
