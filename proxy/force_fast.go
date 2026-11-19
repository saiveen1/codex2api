package proxy

import (
	"context"
	"net/http"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/internal/jsonbody"
	"github.com/gin-gonic/gin"
)

// An attempt can select a different account/group on retry.
type codexAttemptPolicy struct {
	identity        *PayloadRuleIdentity
	forceFastSource string
}

func (h *Handler) codexAttemptPolicy(c *gin.Context, base *PayloadRuleIdentity, account *auth.Account) codexAttemptPolicy {
	p := codexAttemptPolicy{identity: base.WithSelectedAccount(account, h.store)}
	if account == nil || account.IsRelayStyle() {
		return p
	}
	key := apiKeyRowFromContext(c)
	// Passthrough continues to the next scope; it never vetoes Force Fast.
	switch {
	case key != nil && key.Limits.ForceFast:
		p.forceFastSource = "key"
	case h.store != nil && h.store.GroupsForceFast(account.GroupIDSnapshot()):
		p.forceFastSource = "group"
	case account.IsForceFast():
		p.forceFastSource = "account"
	}
	return p
}

func (p codexAttemptPolicy) forcesFast() bool {
	return p.forceFastSource != ""
}

type forcedFastKey struct{}

func (p codexAttemptPolicy) withContext(ctx context.Context) context.Context {
	ctx = WithPayloadRuleIdentity(ctx, p.identity)
	if p.forcesFast() {
		ctx = context.WithValue(ctx, forcedFastKey{}, true)
	}
	return ctx
}

func (p codexAttemptPolicy) requestedTier(body []byte, model string, headers http.Header) string {
	if p.forcesFast() {
		return "priority"
	}
	return EffectiveRequestedServiceTier(body, model, headers, p.identity)
}

func applyForcedFast(ctx context.Context, body []byte) []byte {
	if ctx.Value(forcedFastKey{}) == true {
		return jsonbody.SetString(body, "service_tier", "priority")
	}
	return body
}
