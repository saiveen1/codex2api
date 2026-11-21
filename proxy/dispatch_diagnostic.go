package proxy

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"sync"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

type dispatchDecision struct {
	Reason            string  `json:"reason"`
	RequestedModel    string  `json:"requested_model,omitempty"`
	EffectiveModel    string  `json:"effective_model,omitempty"`
	Channel           string  `json:"channel,omitempty"`
	GroupIDs          []int64 `json:"group_ids,omitempty"`
	SelectedAccountID int64   `json:"selected_account_id,omitempty"`
	Attempted         bool    `json:"attempted"`
	auth.DispatchDiagnostic
	status int
}

// Only validated model identifiers belong in the diagnostic envelope.
var dispatchDiagnosticModel = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,95}$`)

func boundedDispatchModel(model string) string {
	if dispatchDiagnosticModel.MatchString(model) {
		return model
	}
	return ""
}

// Record rejected outcomes from the actual filter. Re-evaluating it after a
// miss could refresh accounts or mutate the request's scope-budget evidence.
type dispatchFilterEvidence struct {
	mu       sync.Mutex
	rejected map[int64]bool
}

func (e *dispatchFilterEvidence) wrap(filter auth.AccountFilter) auth.AccountFilter {
	if filter == nil {
		return nil
	}
	return func(acc *auth.Account) bool {
		if filter(acc) {
			return true
		}
		if acc != nil {
			e.mu.Lock()
			if e.rejected == nil {
				e.rejected = make(map[int64]bool)
			}
			if len(e.rejected) < 256 {
				e.rejected[acc.ID()] = true
			}
			e.mu.Unlock()
		}
		return false
	}
}

func (e *dispatchFilterEvidence) blocked(id int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rejected[id]
}

func schedulerDecision(err error) (string, int) {
	switch {
	case errors.Is(err, auth.ErrQuotaExhausted):
		return "quota_exhausted", http.StatusTooManyRequests
	case errors.Is(err, auth.ErrSchedulerQueueFull):
		return "queue_full", http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return "queue_timeout", http.StatusServiceUnavailable
	}
	return "", 0
}

func recordDispatchDecision(c *gin.Context, reason string, status int, requested, effective string, stream bool, report auth.DispatchDiagnostic, selected ...int64) {
	a := upstreamTraceFromContext(c.Request.Context())
	if a == nil || (c.Request.Context().Err() != nil && !continuousRetryDeadlineExceeded(c.Request.Context())) {
		return
	}
	d := dispatchDecision{Reason: reason, RequestedModel: boundedDispatchModel(requested), EffectiveModel: boundedDispatchModel(effective),
		Channel: requestUpstreamChannel(c), DispatchDiagnostic: report, status: status}
	if row := apiKeyRowFromContext(c); row != nil {
		d.GroupIDs = slices.Clone(row.AllowedGroupIDs[:min(32, len(row.AllowedGroupIDs))])
		d.Partial = d.Partial || len(row.AllowedGroupIDs) > len(d.GroupIDs)
	}
	if len(selected) > 0 {
		d.SelectedAccountID = selected[0]
	}
	a.mu.Lock()
	d.Attempted = a.attempts > 0 || a.diagnosticUsageLogged
	a.dispatchDecision, a.dispatchStream = &d, stream
	a.mu.Unlock()
}
