package openaiidentity

import "testing"

func TestRequestedWebWorkspaceCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, legacy string
		headers      map[string]string
		want         string
		bad          bool
	}{
		{"legacy user", "user-old", nil, "", false},
		{"missing claim", "", nil, "", false},
		{"known default", "workspace-a", nil, "workspace-a", false},
		{"explicit team", "user-old", map[string]string{"ChatGPT-Account-ID": "workspace-team"}, "workspace-team", false},
		{"explicit user forbidden", "workspace-a", map[string]string{"ChatGPT-Account-ID": "user-invalid"}, "", true},
		{"conflicting headers", "", map[string]string{"ChatGPT-Account-ID": "a", "chatgpt-account-id": "b"}, "", true},
		{"unsafe workspace", "workspace\nsecret", nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RequestedWebWorkspace(tc.legacy, tc.headers)
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("got %q %v; want %q bad=%v", got, err, tc.want, tc.bad)
			}
		})
	}
}
