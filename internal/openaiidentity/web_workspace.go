package openaiidentity

import (
	"errors"
	"strings"
)

// ValidWebWorkspaceID excludes login user IDs without requiring JWT claims.
func ValidWebWorkspaceID(value string) bool {
	if value == "" || len(value) > 160 || strings.HasPrefix(strings.ToLower(value), "user-") {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// RequestedWebWorkspace preserves explicit routing. Only an absent/legacy login
// identity may request authenticated upstream default discovery.
func RequestedWebWorkspace(legacy string, headers map[string]string) (string, error) {
	override, err := WorkspaceOverrideFromHeadersChecked(headers)
	if err != nil {
		return "", err
	}
	if override != "" {
		if !ValidWebWorkspaceID(override) {
			return "", errors.New("invalid explicit web workspace")
		}
		return override, nil
	}
	workspace := NormalizeWorkspaceID(legacy)
	if workspace != "" && !ValidWebWorkspaceID(workspace) {
		return "", errors.New("invalid web workspace")
	}
	return workspace, nil
}
