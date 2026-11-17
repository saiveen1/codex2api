package proxy

import (
	"context"
	"net/http"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/internal/jsonbody"
	"github.com/gin-gonic/gin"
)

// Snapshot both settings for this attempt: retries can select a different account/group.
// The force flag is independent of payload rule identity (which is nil for anonymous requests).
type codexAttemptPolicy struct {
	identity  *PayloadRuleIdentity
	forceFast bool
}

func (h *Handler) codexAttemptPolicy(c *gin.Context, base *PayloadRuleIdentity, account *auth.Account) codexAttemptPolicy {
	p := codexAttemptPolicy{identity: base.WithSelectedAccount(account, h.store)}
	if account == nil || account.IsRelayStyle() {
		return p
	}
	key := apiKeyRowFromContext(c)
	p.forceFast = (key != nil && key.Limits.ForceFast) || account.IsForceFast() ||
		(h.store != nil && h.store.GroupsForceFast(account.GroupIDSnapshot()))
	return p
}

type forcedFastKey struct{}

func (p codexAttemptPolicy) withContext(ctx context.Context) context.Context {
	ctx = WithPayloadRuleIdentity(ctx, p.identity)
	if p.forceFast {
		ctx = context.WithValue(ctx, forcedFastKey{}, true)
	}
	return ctx
}

func (p codexAttemptPolicy) requestedTier(body []byte, model string, headers http.Header) string {
	if p.forceFast {
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
