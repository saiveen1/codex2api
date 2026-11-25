package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

// CodexUADisplay describes only identity information independent of a request.
type CodexUADisplay struct {
	System       string `json:"system"`
	SystemSource string `json:"system_source"`
	Client       string `json:"client"`
	ClientSource string `json:"client_source"`
	UserAgent    string `json:"user_agent,omitempty"`
}

// ResolveCodexUADisplay uses the same resolvers as outbound traffic without
// inventing a downstream request or querying the account store.
func ResolveCodexUADisplay(id int64, selection, environment string, headers map[string]string) CodexUADisplay {
	result := CodexUADisplay{System: "unknown", SystemSource: "unknown", ClientSource: "unknown"}
	settings := CurrentRuntimeSettings()
	account := &auth.Account{DBID: id, CodexUserAgent: selection, CustomHeaders: headers}
	cfg := codexUserAgentConfigFromJSON(settings.CodexUserAgentConfig)
	var ua string
	if selection != "" {
		identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: account})
		if err != nil {
			return result
		}
		ua = identity.UserAgent
		result.ClientSource, result.SystemSource = "preset", "preset"
		if strings.HasPrefix(selection, "raw:") {
			result.ClientSource, result.SystemSource = "manual", "manual"
		}
	} else if settings.ClientCompatMode == ClientCompatModeForce {
		resolved, _, configured, err := codexUserAgentFromConfigChecked(settings.CodexUserAgentConfig, id, codexIdentityVersionFloor(settings), environment)
		if err != nil {
			return result
		}
		if !configured {
			identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: account})
			if err != nil {
				return result
			}
			resolved = identity.UserAgent
		}
		ua = resolved
		result.ClientSource, result.SystemSource = "global", "global"
		if cfg.RawUserAgent != "" {
			result.ClientSource, result.SystemSource = "manual", "manual"
		} else if cfg.Mode == CodexUserAgentModePool {
			result.ClientSource = "pool"
			if _, ok := cfg.AccountUserAgents[id]; ok {
				result.ClientSource, result.SystemSource = "manual", "manual"
			} else {
				kind, explicitOS, pinned := parseCodexPreset(string(cfg.AccountKinds[id]))
				if pinned {
					result.ClientSource = "preset"
				}
				if explicitOS != "" {
					result.SystemSource = "preset"
				} else {
					result.SystemSource = "global"
					if !pinned {
						kind = inferCodexClientKind(codexUserAgentClientName(ua))
					}
					osName := ""
					switch environment {
					case "chrome_macos":
						osName = "Mac OS"
					case "chrome_windows", "firefox_windows":
						osName = "Windows"
					}
					if spec, ok := codexUAKindSpecFor(kind); ok && osName != "" && (cfg.AutoMatchOS == nil || *cfg.AutoMatchOS) {
						for _, platform := range spec.Platforms {
							if platform.OSName == osName && platform.Weight > 0 {
								result.SystemSource = "oauth"
								break
							}
						}
					}
				}
			}
		}
	} else {
		result.SystemSource, result.ClientSource = "request", "request"
	}
	for name, value := range CodexAccountCustomHeaders(account) {
		if http.CanonicalHeaderKey(strings.TrimSpace(name)) == "User-Agent" {
			ua = value
			result.ClientSource, result.SystemSource = "manual", "manual"
			break
		}
	}
	if ua == "" {
		return result
	}
	result.UserAgent = ua
	name := codexUserAgentClientName(ua)
	kind := CodexClientKindCustom
	if name != "" {
		kind = inferCodexClientKind(name)
	}
	switch kind {
	case CodexClientKindTUI, CodexClientKindDesktop, CodexClientKindVSCode, CodexClientKindExec:
		result.Client = string(kind)
	default:
		result.Client = "custom"
	}
	lower := strings.ToLower(ua)
	switch {
	case strings.Contains(lower, "(mac os "):
		result.System = "macos"
	case strings.Contains(lower, "(windows "):
		result.System = "windows"
	case strings.Contains(lower, "(linux "), strings.Contains(lower, "(ubuntu "), strings.Contains(lower, "(nixos "), strings.Contains(lower, "(centos "), strings.Contains(lower, "(debian "), strings.Contains(lower, "(arch linux "), strings.Contains(lower, "(fedora linux "):
		result.System = "linux"
	default:
		result.SystemSource = "unknown"
	}
	return result
}
