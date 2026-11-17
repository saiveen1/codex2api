package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codex2api/internal/oauthgateway/service"
)

func TestOAuthGatewayPerAccountRuntimeProxyValidation(t *testing.T) {
	h, r, _ := newOAuthGatewayAdminFixture(t)
	good := "http://one-runtime.example.invalid:8080"
	other := "http://two-runtime.example.invalid:8080"
	if _, err := h.db.InsertProxy(t.Context(), good, "one"); err != nil {
		t.Fatal(err)
	}
	id, err := h.db.InsertProxy(t.Context(), other, "two")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.validateOAuthGatewayDestination(t.Context(), nil, good, other); err != nil {
		t.Fatal(err)
	}
	owner, err := h.db.InsertAccount(t.Context(), "proxy-owner", "synthetic-owner-refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"unknown", "disabled", "failed", "exclusive"} {
		t.Run(mode, func(t *testing.T) {
			candidate := other
			enabled := true
			if err := h.db.UpdateProxy(t.Context(), id, nil, nil, &enabled); err != nil {
				t.Fatal(err)
			}
			if err := h.db.UpdateProxyTestResult(t.Context(), id, other, "untested", "", "", 0); err != nil {
				t.Fatal(err)
			}
			if _, err := h.db.UpdateProxyAccess(t.Context(), id, false, nil); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unknown":
				candidate = "http://missing.example.invalid:8080"
			case "disabled":
				enabled = false
				if err := h.db.UpdateProxy(t.Context(), id, nil, nil, &enabled); err != nil {
					t.Fatal(err)
				}
			case "failed":
				if err := h.db.UpdateProxyTestResult(t.Context(), id, other, "error", "", "", 0); err != nil {
					t.Fatal(err)
				}
			case "exclusive":
				if _, err := h.db.UpdateProxyAccess(t.Context(), id, true, []int64{owner}); err != nil {
					t.Fatal(err)
				}
			}
			in := service.Input{RequestID: "runtime-validation", RuntimeProxy: good, Accounts: []service.InputAccount{{Identifier: "one@example.invalid", Password: "synthetic", TOTPSecret: "JBSWY3DPEHPK3PXP", RuntimeProxy: candidate}}}
			raw, _ := json.Marshal(in)
			for _, endpoint := range []string{"preview", "jobs"} {
				w := oauthGatewayRequest(r, "POST", endpoint, string(raw))
				if w.Code != 400 || !strings.Contains(w.Body.String(), "runtime_proxy_unavailable") {
					t.Fatalf("%s %s accepted: %d %s", mode, endpoint, w.Code, w.Body.String())
				}
			}
		})
	}
	in := service.Input{RequestID: "repair-pin", RecoveryAccountID: 42, Accounts: []service.InputAccount{{Identifier: "one@example.invalid", Password: "synthetic", TOTPSecret: "JBSWY3DPEHPK3PXP", RuntimeProxy: good}}}
	raw, _ := json.Marshal(in)
	w := oauthGatewayRequest(r, "POST", "jobs", string(raw))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_recovery_destination") {
		t.Fatalf("repair allowed runtime pin: %d %s", w.Code, w.Body.String())
	}
}
