package proxy

import (
	"strings"
	"testing"
)

func TestCodexUADisplay(t *testing.T) {
	codexTestIdentityCache(t)
	for _, tc := range []struct {
		name, mode, config, selection, environment, system, systemSource, client, clientSource string
		headers                                                                                map[string]string
	}{
		{"pool mac", ClientCompatModeForce, `{"mode":"pool","pool_mix":{"codex-tui":1}}`, "", "chrome_macos", "macos", "oauth", "codex-tui", "pool", nil},
		{"pool windows", ClientCompatModeForce, `{"mode":"pool","pool_mix":{"codex-tui":1}}`, "", "chrome_windows", "windows", "oauth", "codex-tui", "pool", nil},
		{"disabled match", ClientCompatModeForce, `{"mode":"pool","pool_mix":{"codex-tui":1},"auto_match_os":false}`, "", "chrome_macos", "", "global", "codex-tui", "pool", nil},
		{"pool preset", ClientCompatModeForce, `{"mode":"pool","account_kinds":{"1":"codex-vscode@linux"}}`, "", "chrome_macos", "linux", "preset", "codex-vscode", "preset", nil},
		{"single override", ClientCompatModeForce, `{"client_kind":"codex-desktop","os_name":"Windows"}`, "", "chrome_macos", "windows", "global", "codex-desktop", "global", nil},
		{"account preset", ClientCompatModePreserve, `{}`, "codex-vscode@linux", "chrome_macos", "linux", "preset", "codex-vscode", "preset", nil},
		{"manual", ClientCompatModeForce, `{"mode":"pool"}`, "raw:example/1.0 (Windows 11; x86_64)", "chrome_macos", "windows", "manual", "custom", "manual", nil},
		{"header override", ClientCompatModeForce, `{"mode":"pool"}`, "", "chrome_macos", "windows", "manual", "custom", "manual", map[string]string{"User-Agent": "example/1.0 (Windows 11; x86_64)"}},
		{"request", ClientCompatModePreserve, `{"mode":"pool"}`, "", "chrome_macos", "unknown", "request", "", "request", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: tc.mode, CodexUserAgentConfig: tc.config})
			got := ResolveCodexUADisplay(1, tc.selection, tc.environment, tc.headers)
			if (tc.system != "" && got.System != tc.system) || got.SystemSource != tc.systemSource || got.Client != tc.client || got.ClientSource != tc.clientSource {
				t.Fatalf("display: %+v", got)
			}
			if tc.clientSource == "request" {
				if got.UserAgent != "" {
					t.Fatalf("request-dependent UA: %q", got.UserAgent)
				}
			} else if got.UserAgent == "" {
				t.Fatal("missing fixed UA")
			}
			if strings.HasPrefix(tc.name, "pool ") && tc.name != "pool preset" {
				ua, _, _, err := codexUserAgentFromConfigChecked(tc.config, 1, "", tc.environment)
				if err != nil || got.UserAgent != ua {
					t.Fatalf("display differs from resolver: %+v %q %v", got, ua, err)
				}
			}
		})
	}
}

func TestCodexUADisplayOAuthWithoutCandidate(t *testing.T) {
	codexTestIdentityCache(t)
	original := codexUACatalog[CodexClientKindTUI]
	spec := *original
	spec.Platforms = []codexUAPlatform{{OSName: "Windows", OSVersion: "10.0.26200", Arch: "x86_64", Weight: 1}}
	codexUACatalog[CodexClientKindTUI] = &spec
	t.Cleanup(func() { codexUACatalog[CodexClientKindTUI] = original })
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModeForce, CodexUserAgentConfig: `{"mode":"pool","pool_mix":{"codex-tui":1}}`})
	got := ResolveCodexUADisplay(1, "", "chrome_macos", nil)
	if got.System != "windows" || got.SystemSource != "global" || got.ClientSource != "pool" {
		t.Fatalf("unapplied OAuth environment: %+v", got)
	}
}
