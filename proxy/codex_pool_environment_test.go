package proxy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func TestCodexPoolAuthorizationOS(t *testing.T) {
	for _, kind := range codexUAKindOrder {
		for environment, osName := range map[string]string{"chrome_macos": "Mac OS", "chrome_windows": "Windows", "firefox_windows": "Windows"} {
			t.Run(string(kind)+"/"+environment, func(t *testing.T) {
				cfg := CodexUserAgentConfig{Mode: "pool", PoolMix: map[string]int{string(kind): 1}}
				spec := codexUACatalog[kind]
				seen := map[string]bool{}
				for id := int64(1); id <= 100; id++ {
					ua, version, ok, err := codexPoolPersona(cfg, id, "", environment)
					if err != nil || !ok || !strings.Contains(ua, " ("+osName+" ") {
						t.Fatalf("id %d: %q, %v", id, ua, err)
					}
					seed := fmt.Sprint(id)
					terminal := pickCodexUAWeighted(spec.Terminals, "terminal:"+seed)
					if !strings.HasPrefix(ua, spec.ClientName+"/") || !strings.Contains(ua, ") "+terminal+" (") {
						t.Fatalf("unrelated choices changed: %q", ua)
					}
					again, againVersion, _, err := codexPoolPersona(cfg, id, "", environment)
					if err != nil || again != ua || againVersion != version {
						t.Fatal("unstable account persona")
					}
					seen[ua] = true
				}
				if len(seen) < 2 {
					t.Fatal("OS binding collapsed the remaining choices")
				}
			})
		}
	}
}

func TestCodexPoolAuthorizationOSCompatibility(t *testing.T) {
	const raw = `{"mode":"pool"}`
	for id := int64(1); id <= 50; id++ {
		base, v, _, err := codexUserAgentFromConfigChecked(raw, id, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, environment := range []string{"", "auto", "custom_ua", "unknown"} {
			ua, version, _, err := codexUserAgentFromConfigChecked(raw, id, "", environment)
			if err != nil || ua != base || version != v {
				t.Fatalf("legacy behavior changed: %q", environment)
			}
		}
		for _, environment := range []string{"chrome_macos", "chrome_windows"} {
			ua, _, _, err := codexUserAgentFromConfigChecked(`{"mode":"pool","auto_match_os":false}`, id, "", environment)
			if err != nil || ua != base {
				t.Fatal("disabled switch changed selection")
			}
			matched, _, _, err := codexUserAgentFromConfigChecked(raw, id, "", environment)
			if err != nil || codexUserAgentClientName(matched) != codexUserAgentClientName(base) {
				t.Fatal("OS binding changed client pool weights")
			}
		}
	}
	for _, config := range []string{
		`{"mode":"pool","account_user_agents":{"11":"manual/1.2.3"}}`,
		`{"mode":"pool","raw_user_agent":"manual/1.2.3"}`,
	} {
		ua, _, _, err := codexUserAgentFromConfigChecked(config, 11, "", "chrome_macos")
		if err != nil || ua != "manual/1.2.3" {
			t.Fatal("manual UA overridden")
		}
	}
	const single = `{"client_kind":"codex-desktop","os_name":"Windows"}`
	base, _, _, _ := codexUserAgentFromConfigChecked(single, 11, "")
	ua, _, _, err := codexUserAgentFromConfigChecked(single, 11, "", "chrome_macos")
	if err != nil || ua != base {
		t.Fatal("single persona changed")
	}
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"mode":"pool","auto_match_os":false}`)
	if err != nil || !strings.Contains(normalized, `"auto_match_os":false`) {
		t.Fatal("switch lost during normalization")
	}
}

func TestCodexPoolAuthorizationOSPreviewMatchesOutbound(t *testing.T) {
	db := codexTestVersionDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	ctx := context.Background()
	for _, environment := range []string{"chrome_macos", "chrome_windows"} {
		id, err := db.InsertAccountWithCredentials(ctx, "os-fixture", map[string]any{
			"access_token": "fixture-access-token", "oauth_gateway_environment": environment,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.LoadAccountByID(ctx, id); err != nil {
			t.Fatal(err)
		}
		account := store.FindByID(id)
		if account == nil || account.GetOAuthGatewayEnvironment() != environment {
			t.Fatal("binding not loaded")
		}
		selection, expected := "codex-desktop@windows", "Windows"
		if environment == "chrome_windows" {
			selection, expected = "codex-tui@macos", "Mac OS"
		}
		config := fmt.Sprintf(`{"mode":"pool","account_kinds":{"%d":%q}}`, id, selection)
		ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModeForce, CodexUserAgentConfig: config})
		identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: account})
		if err != nil {
			t.Fatal(err)
		}
		preview, err := PreviewCodexUserAgentConfig(config, "", []int64{id}, map[int64]string{id: account.GetOAuthGatewayEnvironment()})
		if err != nil || len(preview.Samples) != 1 || identity.UserAgent != preview.Samples[0].UserAgent || identity.Version != preview.Samples[0].Version {
			t.Fatalf("preview differs: %+v / %+v / %v", identity, preview, err)
		}
		if !strings.Contains(identity.UserAgent, " ("+expected+" ") {
			t.Fatalf("explicit OS lost: %s", identity.UserAgent)
		}
	}
}

func TestCodexPoolAuthorizationOSMissingCandidateFallsBack(t *testing.T) {
	kind := CodexClientKindTUI
	original := codexUACatalog[kind]
	spec := *original
	spec.Platforms = []codexUAPlatform{{OSName: "Windows", OSVersion: "10.0.26200", Arch: "x86_64", Weight: 1}}
	codexUACatalog[kind] = &spec
	t.Cleanup(func() { codexUACatalog[kind] = original })
	cfg := CodexUserAgentConfig{Mode: "pool", PoolMix: map[string]int{string(kind): 1}}
	for id := int64(1); id <= 10; id++ {
		baseline, version, ok, err := codexPoolPersona(cfg, id, "", "")
		if err != nil || !ok {
			t.Fatalf("baseline: %v", err)
		}
		got, gotVersion, ok, err := codexPoolPersona(cfg, id, "", "chrome_macos")
		if err != nil || !ok || got != baseline || gotVersion != version {
			t.Fatalf("missing candidate must preserve original selection: %q / %q, %v", got, baseline, err)
		}
	}
	cfg.AccountKinds = map[int64]CodexClientKind{1: "codex-tui@macos"}
	if _, _, _, err := codexPoolPersona(cfg, 1, "", "chrome_windows"); err == nil {
		t.Fatal("explicit OS without a candidate must not fall back")
	}
}

func TestCodexPoolExplicitOS(t *testing.T) {
	codexTestIdentityCache(t)
	for _, tc := range []struct{ selection, osName, environment string }{
		{"codex-tui@macos", "Mac OS", "chrome_windows"},
		{"codex-desktop@windows", "Windows", "chrome_macos"},
		{"codex-vscode@linux", "Ubuntu", "chrome_macos"},
	} {
		config := fmt.Sprintf(`{"mode":"pool","account_kinds":{"1":%q}}`, tc.selection)
		if _, err := NormalizeCodexUserAgentConfigJSON(config); err != nil {
			t.Fatal(err)
		}
		ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModeForce, CodexUserAgentConfig: config})
		preview, err := PreviewCodexUserAgentConfig(config, "", []int64{1}, map[int64]string{1: tc.environment})
		if err != nil || len(preview.Samples) != 1 {
			t.Fatalf("%s: preview %+v: %v", tc.selection, preview, err)
		}
		ua, version, ok, err := codexUserAgentFromConfigChecked(config, 1, "", tc.environment)
		if err != nil || !ok || ua != preview.Samples[0].UserAgent || version != preview.Samples[0].Version {
			t.Fatalf("%s: outbound %q preview %+v: %v", tc.selection, ua, preview, err)
		}
		kind, os, _ := parseCodexPreset(tc.selection)
		matched := false
		for _, platform := range codexPresetPlatforms(codexUACatalog[kind], os) {
			if strings.Contains(ua, fmt.Sprintf(" (%s %s; %s)", platform.OSName, platform.OSVersion, platform.Arch)) {
				matched = true
				cli, app, e := resolveCodexCurrentVersions(codexUACatalog[kind], codexVersionSelection{OSName: platform.OSName, Arch: platform.Arch})
				if e != nil || version != cli || !strings.HasSuffix(ua, "; "+app+")") {
					t.Fatalf("wrong version pair: %q %v", ua, e)
				}
			}
		}
		if !matched || (os != "linux" && !strings.Contains(ua, " ("+tc.osName+" ")) {
			t.Fatalf("wrong platform for %s: %s", tc.selection, ua)
		}
	}
	for _, selection := range []string{"codex-desktop@linux", "codex-tui@", "codex-tui@freebsd", "custom@macos", "codex-tui@macos@windows"} {
		config := fmt.Sprintf(`{"mode":"pool","account_kinds":{"1":%q}}`, selection)
		if _, err := NormalizeCodexUserAgentConfigJSON(config); err == nil {
			t.Fatalf("accepted %s", selection)
		}
	}
}
