package service

import "strings"

func requestedCountry(raw string) string {
	if strings.EqualFold(raw, "auto") {
		return "auto"
	}
	code := strings.ToUpper(raw)
	if len(code) == 2 && code[0] >= 'A' && code[0] <= 'Z' && code[1] >= 'A' && code[1] <= 'Z' {
		return code
	}
	return ""
}

func jobView(j Job) JobView {
	v := JobView{ID: j.ID, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, Groups: []GroupView{}, RetryOf: j.RetryOf}
	switch {
	case j.AutoRecovery:
		v.JobType = "auto_recovery"
	case j.Recovery != nil:
		v.JobType = "manual_recovery"
	case j.RetryOf != nil:
		v.JobType = "reauthorize"
	default:
		v.JobType = "import"
	}
	if j.Recovery != nil {
		v.RecoveryAccountID = j.Recovery.AccountID
		v.AutoRecovery = j.AutoRecovery
	} else {
		ids := sortedGroups(j.GroupIDs)
		v.GroupIDs = &ids
	}
	if len(j.Groups) > 0 {
		v.RequestedCountry = requestedCountry(j.Groups[0].Request.Options.Country)
	}
	review := false
	allCancelled := true
	allQueued := true
	for index, g := range j.Groups {
		gv := GroupView{Index: index, State: g.Stage, BatchID: g.Batch.BatchID, RequestID: g.Request.RequestID, Total: len(g.Lines), Error: g.Error, Items: []ItemView{}}
		gv.ReadOperation = g.ReadOperation
		gv.ReadFailures = g.ReadFailures
		if g.ReadFailures > 0 && g.Stage != "needs_review" && !g.NextCheck.IsZero() {
			next := g.NextCheck
			gv.NextCheckAt = &next
		}
		if g.Stage == "complete" {
			gv.State = "completed"
		}
		if g.Stage != "cancelled" {
			allCancelled = false
		}
		if g.Stage != "queued" {
			allQueued = false
		}
		switch g.Stage {
		case "queued", "checking", "submitting", "submit_unknown", "running", "needs_review":
			v.CanRefresh = true
		}
		if g.Stage == "needs_review" || g.Stage == "submit_unknown" {
			review = true
		}
		if !j.CancelRequested && (g.Stage == "queued" || g.Stage == "checking" || g.Stage == "submit_unknown" || (g.Batch.Pending > 0 && !g.CancelSent)) {
			v.CanCancel = true
		}
		groupCanReauthorize := j.Recovery == nil && !j.AutoRecovery && g.Stage == "complete" && g.Batch.BatchID != "" && g.Batch.Pending == 0
		switch g.Batch.State {
		case "queued", "pending", "running", "submit_unknown", "needs_review", "manual_review":
			groupCanReauthorize = false
		}
		for i, line := range g.Lines {
			item := ItemView{Line: line, IdentifierMasked: g.Masks[i], State: g.Stage, ImportState: "not_ready"}
			if i < len(g.Request.Accounts) {
				item.Identifier = g.Request.Accounts[i].Identifier
			}
			for _, remote := range g.Batch.Items {
				if remote.Line == i+1 {
					item.State = remote.State
					item.CanReauthorize = groupCanReauthorize && item.Identifier != "" && confirmedOAuthFailure(remote)
					if remote.Error != nil {
						item.Error = publicError(remote.Error)
						item.Diagnostic = failureDiagnostic(remote)
					}
					break
				}
			}
			for _, d := range g.Deliveries {
				if d.Line == i+1 {
					item.CanReauthorize = false
					item.ImportState = d.State
					if d.State == "imported" && d.Result != nil {
						v.AccountResults = append(v.AccountResults, *d.Result)
						if j.Recovery != nil && d.Result.GroupIDs != nil {
							v.GroupIDs = d.Result.GroupIDs
						}
					} else if d.State == "imported" && len(d.CreatedIDs) == 1 && d.CreatedIDs[0] > 0 {
						// Pre-summary jobs still have the ID returned by the original commit.
						result := AccountResult{Line: line, AccountID: d.CreatedIDs[0], Outcome: "imported"}
						if item.Identifier != "" {
							result.IdentifierMasked = mask(item.Identifier)
						}
						v.AccountResults = append(v.AccountResults, result)
					}
					if d.Error != nil {
						item.Error = d.Error
					}
					break
				}
			}
			v.Total++
			switch item.ImportState {
			case "imported":
				v.Imported++
				gv.Imported++
			case "import_failed":
				v.Failed++
				v.CanRetryImport = true
			case "expired", "import_rejected":
				v.Failed++
			case "import_unknown":
				v.Pending++
				review = true
			default:
				if g.Stage == "failed" || g.Stage == "cancelled" || item.State == "failed" || item.State == "blocked" || item.State == "cancelled" {
					v.Failed++
				} else {
					v.Pending++
				}
			}
			if item.State == "needs_review" {
				review = true
			}
			gv.Items = append(gv.Items, item)
		}
		v.Groups = append(v.Groups, gv)
	}
	switch {
	case review:
		v.State = "needs_review"
	case v.Pending == 0 && v.Imported == v.Total:
		v.State = "completed"
	case v.Pending == 0 && allCancelled:
		v.State = "cancelled"
	case v.Pending == 0 && v.Imported == 0:
		v.State = "failed"
	case v.Imported > 0:
		v.State = "partial"
	case allQueued:
		v.State = "queued"
	default:
		v.State = "running"
	}
	if v.CanRefresh || v.Pending != 0 || review {
		for gi := range v.Groups {
			for ii := range v.Groups[gi].Items {
				v.Groups[gi].Items[ii].CanReauthorize = false
			}
		}
	}
	v.CanArchive = archivable(j, v)
	v.RetainedMaterial = hasRetainedMaterial(j)
	return v
}

func purgeMaterials(j *Job) {
	for i := range j.Groups {
		g := &j.Groups[i]
		switch g.Stage {
		case "failed", "cancelled", "complete":
			for k := range g.Request.Accounts {
				g.Request.Accounts[k].Password = ""
				g.Request.Accounts[k].TOTPSecret = ""
			}
			g.Request.Options.Proxy = nil
		}
	}
	v := jobView(*j)
	if !v.CanRefresh && !v.CanRetryImport {
		j.Settings = Settings{}
		j.RuntimeProxy = ""
		for i := range j.Groups {
			j.Groups[i].RuntimeProxies = nil
		}
	}
}
