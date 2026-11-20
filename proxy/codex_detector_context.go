package proxy

import "context"

// codexDetectorRequestKey marks an administrator-triggered model fingerprint
// probe. Detector requests keep their measurement payload isolated from user
// payload rules and telemetry, while their outbound protocol identity (device
// fingerprint and timezone) follows ordinary Codex requests.
type codexDetectorRequestKey struct{}

// WithCodexDetectorRequest marks ctx as a detector probe context.
func WithCodexDetectorRequest(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexDetectorRequestKey{}, true)
}

func isCodexDetectorRequest(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(codexDetectorRequestKey{}).(bool)
	return marked
}

type recordOnlyDetectorKey struct{}

// WithRecordOnlyDetector marks a trial that must not refresh runtime settings.
func WithRecordOnlyDetector(ctx context.Context) context.Context {
	return context.WithValue(ctx, recordOnlyDetectorKey{}, true)
}

func isRecordOnlyDetector(ctx context.Context) bool {
	marked, _ := ctx.Value(recordOnlyDetectorKey{}).(bool)
	return marked
}
