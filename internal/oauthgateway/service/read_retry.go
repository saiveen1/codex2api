package service

import (
	"context"
	"errors"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

const maxReadFailures = 5

// Only reads of an already intended task are retried. Submit, cancel and
// local import keep their existing unknown-outcome protections.
func retryableRead(operation string, err error) bool {
	var remote *upstream.Error
	if !errors.As(err, &remote) || remote.Status == 401 || remote.Status == 403 || (remote.Action != "query_original" && remote.Action != "retry_same_request") {
		return false
	}
	switch remote.Code {
	case "transport_error", "upstream_unavailable", "service_unavailable", "rate_limited", "request_timeout", "capacity_exceeded":
		return true
	case "upstream_error":
		return remote.Status >= 500
	case "invalid_response":
		return remote.Status == 0 || remote.Status >= 500
	case "not_found", "job_not_found":
		return operation == "lookup"
	case "result_not_ready", "result_unavailable":
		return operation == "result"
	default:
		return false
	}
}

func readBackoff(failures int) time.Duration {
	switch failures {
	case 1:
		return 5 * time.Second
	case 2:
		return 10 * time.Second
	case 3:
		return 20 * time.Second
	default:
		return 30 * time.Second
	}
}

func resetReadRetry(g *Group) {
	g.ReadOperation = ""
	g.ReadFailures = 0
}

func (a *App) readFailure(ctx context.Context, id string, index int, operation string, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		// A graceful shutdown should leave the original read resumable.
		return nil
	}
	return a.mutateGroup(id, index, func(g *Group) {
		g.Error = publicError(err)
		if !retryableRead(operation, err) {
			resetReadRetry(g)
			g.Stage = "needs_review"
			g.NextCheck = time.Time{}
			return
		}
		if g.ReadOperation != "result" {
			g.ReadOperation = operation
		}
		g.ReadFailures++
		if g.ReadFailures >= maxReadFailures {
			g.Stage = "needs_review"
			g.NextCheck = time.Time{}
			return
		}
		g.NextCheck = time.Now().UTC().Add(readBackoff(g.ReadFailures))
	})
}
