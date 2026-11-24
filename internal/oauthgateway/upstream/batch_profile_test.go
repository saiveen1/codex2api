package upstream

import (
	"encoding/json"
	"testing"
)

func TestBatchProfileAllowlist(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{}`, `null`},
		{`{"options":{"environment":"chrome_macos","language":"en-US,en;q=0.9","timezone":"America/New_York","proxy":"private"}}`, `{"environment":"chrome_macos","language":"en-US,en;q=0.9","timezone":"America/New_York"}`},
		{`{"options":{"timezone":42,"environment":"bad\nvalue","language":"en"}}`, `{"language":"en"}`},
	} {
		var batch Batch
		if err := json.Unmarshal([]byte(tc.raw), &batch); err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(batch.Options)
		if err != nil || string(got) != tc.want {
			t.Errorf("got %s, %v; want %s", got, err, tc.want)
		}
	}
}

func TestKnownEnvironmentMatchesDocumentedExplicitSystems(t *testing.T) {
	for _, value := range []string{"chrome_macos", "chrome_windows", "firefox_windows"} {
		if got := KnownEnvironment(value); got != value {
			t.Fatalf("documented explicit environment %q discarded", value)
		}
	}
	for _, value := range []string{"", "auto", "custom_ua", "future_environment"} {
		if got := KnownEnvironment(value); got != "" {
			t.Fatalf("unresolved environment %q must not bind a system", value)
		}
	}
}
