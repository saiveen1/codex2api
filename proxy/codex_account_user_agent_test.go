package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func TestAccountCodexUserAgentOverridesIdentity(t *testing.T) {
	codexTestIdentityCache(t)
	settings := CurrentRuntimeSettings()
	settings.ClientCompatMode = ClientCompatModePreserve
	settings.CodexUserAgentConfig = `{"mode":"pool","pool_mix":{"codex-tui":1}}`
	ApplyRuntimeSettings(settings)
	downstream := http.Header{"User-Agent": {"codex-tui/0.160.0 (Linux; x86_64) unknown"}, "Originator": {"codex-tui"}}
	for _, tc := range []struct{ selection, prefix string }{
		{"codex-tui", "codex-tui/"}, {"codex-vscode", "codex_vscode/"},
		{"codex-desktop", "Codex Desktop/"}, {"codex-exec", "codex_exec/"},
		{"raw:example/1.2 (synthetic)", "example/1.2 (synthetic)"},
	} {
		account := &auth.Account{DBID: 1, CodexUserAgent: tc.selection}
		identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: account, Headers: downstream, DeviceConfig: &DeviceProfileConfig{UserAgent: "device/1.0", StabilizeDeviceProfile: true}})
		if err != nil || !identity.Generated || !strings.HasPrefix(identity.UserAgent, tc.prefix) {
			t.Fatalf("%s: identity=%+v err=%v", tc.selection, identity, err)
		}
		if !strings.HasPrefix(tc.selection, "raw:") {
			ua, version, _, err := buildCodexStructuredUserAgent(CodexUserAgentConfig{ClientKind: tc.selection}, "")
			if err != nil || identity.UserAgent != ua || identity.Version != version {
				t.Fatalf("preset pair: %+v %v", identity, err)
			}
		}
		account.CustomHeaders = map[string]string{"User-Agent": "custom/1.0"}
		req, _ := http.NewRequest(http.MethodPost, "https://example.invalid", nil)
		if err := applyCodexRequestHeaders(req, account, "token", "", "", nil, downstream); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("User-Agent"); got != "custom/1.0" {
			t.Fatalf("custom headers lost: %q", got)
		}
		maintenance, err := ResolveCodexMaintenanceIdentity(account, downstream)
		if err != nil || maintenance.UserAgent != "custom/1.0" {
			t.Fatalf("maintenance: %+v %v", maintenance, err)
		}
	}
	for _, value := range []string{"raw:", "raw: ", "raw: agent", "raw:agent ", "raw:agent\tvalue", "raw:agent\r\nInjected: yes", "codex-TUI", "custom"} {
		if err := ValidateAccountCodexUserAgent(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestAccountCodexUserAgentExplicitOS(t *testing.T) {
	codexTestIdentityCache(t)
	for _, tc := range []struct{ selection, osName string }{
		{"codex-tui@macos", "Mac OS"},
		{"codex-desktop@windows", "Windows"},
		{"codex-vscode@linux", "Ubuntu"},
	} {
		account := &auth.Account{DBID: 1, CodexUserAgent: tc.selection}
		identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: account})
		if err != nil || !strings.Contains(identity.UserAgent, " ("+tc.osName+" ") {
			t.Fatalf("%s: %+v %v", tc.selection, identity, err)
		}
		kind, os, _ := parseCodexPreset(tc.selection)
		spec := codexUACatalog[kind]
		matched := false
		for _, platform := range codexPresetPlatforms(spec, os) {
			if strings.Contains(identity.UserAgent, " ("+platform.OSName+" "+platform.OSVersion+"; "+platform.Arch+")") {
				cli, app, e := resolveCodexCurrentVersions(spec, codexVersionSelection{OSName: platform.OSName, Arch: platform.Arch, VersionFloor: codexIdentityVersionFloor(CurrentRuntimeSettings())})
				if e != nil || identity.Version != cli || !strings.HasSuffix(identity.UserAgent, "; "+app+")") {
					t.Fatalf("wrong pair: %+v %v", identity, e)
				}
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("platform not in catalog: %+v", identity)
		}
	}
	for _, value := range []string{"codex-desktop@linux", "codex-tui@", "codex-tui@Macos", "codex-tui@macos@windows"} {
		if ValidateAccountCodexUserAgent(value) == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
