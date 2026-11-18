package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestImageJobCancelFencesClaimFinishAndPublication(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.InitImageJobQueue(ctx); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"queued", "claimed", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			const raw = `{"model":"fixture","input_images":["data:image/png;base64,AA=="]}`
			id, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{APIKeyID: 9, ParamsJSON: raw})
			if err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(time.Minute)
			switch mode {
			case "claimed":
				if ok, e := db.ClaimImageJob(ctx, id, "worker", until); !ok || e != nil {
					t.Fatalf("claim: %v %v", ok, e)
				}
			case "legacy":
				if err := db.MarkImageJobRunning(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			owner, other := int64(9), int64(10)
			if _, err := db.CancelImageJob(ctx, id, &other); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("foreign scope: %v", err)
			}
			if inputs, err := db.CancelImageJob(ctx, id, &owner); err != nil || len(inputs) != 1 {
				t.Fatalf("cancel inputs=%d err=%v", len(inputs), err)
			}
			if inputs, err := db.CancelImageJob(ctx, id, nil); err != nil || len(inputs) != 0 {
				t.Fatalf("repeat: %v %v", inputs, err)
			}
			if ok, err := db.ClaimImageJob(ctx, id, "late", until); ok || err != nil {
				t.Fatalf("late claim: %v %v", ok, err)
			}
			if ok, err := db.RenewImageJobLease(ctx, id, "worker", until); ok || err != nil {
				t.Fatalf("late renewal: %v %v", ok, err)
			}
			if err := db.MarkImageJobRunning(ctx, id); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("late start: %v", err)
			}
			if err := db.CheckRunningImageJob(ctx, id, ""); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("late dispatch: %v", err)
			}
			for _, err := range []error{db.FinishLeasedImageJob(ctx, id, "worker", ImageJobSucceeded, "late", 99), db.MarkImageJobSucceeded(ctx, id, 99), db.MarkImageJobFailed(ctx, id, "late", 99), db.MarkImageJobSucceededWithWarning(ctx, id, "late", 99), db.UpdateImageGenerationJobParamsJSON(ctx, id, raw), db.MarkInterruptedImageJobs(ctx)} {
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.InsertActiveImageJobAsset(ctx, ImageAssetInput{JobID: id}); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("late asset: %v", err)
			}
			job, err := db.GetImageGenerationJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if job.Status != ImageJobCancelled || job.CompletedAt == nil || job.DurationMs != 0 || job.ErrorMessage != "" || job.ParamsJSON != `{"model":"fixture"}` || len(job.Assets) != 0 {
				t.Fatalf("late result changed job: %+v", job)
			}
		})
	}
	if n, err := db.DeleteExpiredImageJobs(ctx, time.Now().Add(time.Hour), 10); err != nil || n != 3 {
		t.Fatalf("retention removed %d: %v", n, err)
	}
}
