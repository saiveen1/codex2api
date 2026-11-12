package auth

import "testing"

func TestChatGPTWebEgressNeverUsesResinExemption(t *testing.T) {
	const disabled = "http://disabled.example:8080"
	store := &Store{proxyPoolEnabled: true, proxyPoolSet: buildProxyPoolSet(nil), managedProxySet: buildProxyPoolSet([]string{disabled})}
	old := ResinEgressEnabled()
	t.Cleanup(func() { SetResinEgressEnabled(old) })
	for _, resin := range []bool{false, true} {
		SetResinEgressEnabled(resin)
		for _, account := range []*Account{{DBID: 1}, {DBID: 2, ProxyURL: disabled}} {
			if proxy, usable := store.ResolveChatGPTWebProxy(account); proxy != "" || usable {
				t.Fatalf("web went direct with resin=%t: proxy=%q usable=%t", resin, proxy, usable)
			}
		}
		if resin && !store.AccountHasUsableEgress(&Account{DBID: 3, ProxyURL: disabled}) {
			t.Fatal("native Resin behavior regressed")
		}
	}
}

func TestChatGPTWebCredentialsIncludeEffectiveWorkspace(t *testing.T) {
	account := &Account{AccessToken: " token ", AccountID: "workspace-a", CustomHeaders: map[string]string{"chatgpt-account-id": "workspace-b"}}
	token, workspace := account.ChatGPTWebCredentials()
	if token != "token" || workspace != "workspace-b" {
		t.Fatalf("invalid snapshot: token length=%d workspace=%q", len(token), workspace)
	}
}
