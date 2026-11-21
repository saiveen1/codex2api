package auth

import "time"

// DispatchDiagnostic is a bounded read-only observation at the terminal selection
// decision, not a replay of scheduler ordering or a reservation of capacity.
type DispatchDiagnostic struct {
	Scoped       int                        `json:"scoped"`
	Counts       map[string]int             `json:"counts,omitempty"`
	Samples      []DispatchDiagnosticSample `json:"samples,omitempty"`
	HasCandidate bool                       `json:"has_candidate"`
	Partial      bool                       `json:"partial,omitempty"`
}

type DispatchDiagnosticSample struct {
	AccountID int64  `json:"account_id"`
	Reason    string `json:"reason"`
}

func (s *Store) InspectDispatch(apiKeyID int64, exclude map[int64]bool, rejected func(int64) bool, modelFilter AccountFilter, opts SelectionOptions) DispatchDiagnostic {
	out := DispatchDiagnostic{Counts: make(map[string]int)}
	if s == nil {
		out.Partial = true
		return out
	}
	policy := opts.Policy
	lazy := policy == DispatchPolicyStandard && s.GetLazyMode()
	now := time.Now()
	accounts := s.accountSnapshotAccounts()
	// shortcut: bound on-demand work; mark incomplete for very large pools.
	if len(accounts) > 10000 {
		accounts = accounts[:10000]
		out.Partial = true
	}
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		reason := s.APIKeyAccountBlockReason(apiKeyID, acc)
		switch reason {
		case "group_mismatch", "channel_mismatch", "plan_mismatch":
			out.Counts[reason]++
			continue
		}
		out.Scoped++
		switch {
		case reason != "":
		case exclude[acc.DBID]:
			reason = "retry_excluded"
		case !acc.dispatchableForPolicy(policy):
			reason = acc.ModelCatalogBlockReason()
			if reason == "" {
				if _, held := acc.IntelligenceCheckQuotaHold(opts.Model, policy, now); held {
					reason = "quota_exhausted"
				} else if acc.HasActiveCooldown() {
					reason = "cooldown"
				} else {
					reason = "unclassified"
					out.Partial = true
				}
			}
		case lazy && !s.accountLazySelectable(acc):
			reason = "unclassified"
			out.Partial = true
		case !s.accountHasUsableEgress(acc):
			reason = "egress_unavailable"
		case modelFilter != nil && !modelFilter(acc):
			reason = "model_not_supported"
		case rejected != nil && rejected(acc.DBID):
			reason = "request_filter"
			out.Partial = true
		default:
			acc.mu.RLock()
			limit := acc.DynamicConcurrencyLimit
			acc.mu.RUnlock()
			if limit <= 0 {
				reason = "unclassified"
				out.Partial = true
			} else if accountOccupiedRequests(acc) >= limit {
				reason = "concurrency_saturated"
			} else {
				reason = "available"
				out.HasCandidate, out.Partial = true, true
			}
		}
		out.Counts[reason]++
		if reason == "account_key_denied" && len(out.Samples) < 5 {
			out.Samples = append(out.Samples, DispatchDiagnosticSample{AccountID: acc.DBID, Reason: reason})
		}
	}
	return out
}
