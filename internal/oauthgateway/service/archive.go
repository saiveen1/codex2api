package service

import "time"

const maxArchivedJobs = 10000

func archiveView(j ArchivedJob) JobView {
	return JobView{
		ID: j.ID, State: j.State, Total: j.Total, Imported: j.Imported,
		Failed: j.Failed, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
		Archived: true, ArchivedAt: &j.ArchivedAt, Groups: []GroupView{}, RetryOf: j.RetryOf,
	}
}

func archivable(j Job, v JobView) bool {
	if v.Total == 0 || v.Pending != 0 || v.CanRefresh || v.CanRetryImport || v.State == "needs_review" {
		return false
	}
	for _, g := range j.Groups {
		switch g.Stage {
		case "complete", "failed", "cancelled":
		default:
			return false
		}
	}
	return true
}

func hasRetainedMaterial(j Job) bool {
	for _, g := range j.Groups {
		for _, delivery := range g.Deliveries {
			if len(delivery.Account) > 0 {
				return true
			}
		}
		for _, account := range g.Request.Accounts {
			if account.Password != "" || account.TOTPSecret != "" {
				return true
			}
		}
	}
	return false
}

func (a *App) ArchivedJobs() ([]JobView, error) {
	state, err := a.store.View()
	if err != nil {
		return nil, err
	}
	views := make([]JobView, 0, len(state.Archived))
	for i := len(state.Archived) - 1; i >= 0; i-- {
		views = append(views, archiveView(state.Archived[i]))
	}
	return views, nil
}

// Archive removes resolved task material, but retains its request identity so
// a delayed client replay cannot create and charge for a second authorization.
func (a *App) Archive(id string, discardCredentials bool) (JobView, error) {
	var view JobView
	err := a.store.Update(func(state *State) error {
		for _, previous := range state.Archived {
			if previous.ID == id {
				view = archiveView(previous)
				return nil
			}
		}
		for i, job := range state.Jobs {
			if job.ID != id {
				continue
			}
			current := jobView(job)
			if !current.CanArchive {
				return fault(409, "cannot_archive", "任务尚未确定结束，或仍有可重试的导入，不能归档。")
			}
			if current.RetainedMaterial && !discardCredentials {
				return fault(409, "discard_confirmation_required", "归档会清除该任务保留的登录或授权结果材料，请明确确认。")
			}
			if len(state.Archived) >= maxArchivedJobs {
				return fault(409, "archive_capacity", "归档记录达到上限，未删除任务或去重记录。")
			}
			archived := ArchivedJob{
				ID: job.ID, Fingerprint: job.Fingerprint, State: current.State,
				Total: current.Total, Imported: current.Imported, Failed: current.Failed,
				CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt, ArchivedAt: time.Now().UTC(),
				RetryOf: job.RetryOf,
			}
			if state.Version < 3 {
				state.Version = 3
			}
			state.Archived = append(state.Archived, archived)
			copy(state.Jobs[i:], state.Jobs[i+1:])
			state.Jobs[len(state.Jobs)-1] = Job{}
			state.Jobs = state.Jobs[:len(state.Jobs)-1]
			view = archiveView(archived)
			return nil
		}
		return fault(404, "not_found", "任务不存在。")
	})
	return view, err
}
