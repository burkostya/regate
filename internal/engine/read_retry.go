package engine

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/burkostya/regate/internal/mcp"
)

// readRetryPolicy is private to a single MCP read node. A retry is only
// authorized by the compiled, explicitly acknowledged HCL read contract.
// SDK transport retries remain disabled for every MCP session.
type readRetryPolicy struct {
	maxAttempts int
	delay       time.Duration
}
type readRetryKey struct{}

func withReadRetry(ctx context.Context, attempts int, delay time.Duration) context.Context {
	return context.WithValue(ctx, readRetryKey{}, readRetryPolicy{maxAttempts: attempts, delay: delay})
}

func retryableReadTransportError(err error) bool {
	if err == nil {
		return false
	}
	// Only connection-level uncertainties, not application JSON-RPC errors,
	// denied authorization, malformed content or explicit MCP tool failures.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var network net.Error
	return errors.As(err, &network)
}

func (r *Runtime) callReadWithRetry(ctx context.Context, timeout time.Duration,
	call func(context.Context) (map[string]any, error)) (map[string]any, error) {
	policy, ok := ctx.Value(readRetryKey{}).(readRetryPolicy)
	if !ok || policy.maxAttempts < 2 {
		return call(ctx)
	}
	// This bound is enforced again here to fail closed on malformed internal
	// context, even though configuration compilation already checks it.
	if policy.maxAttempts > 4 || policy.delay < time.Millisecond ||
		policy.delay > 2*time.Second {
		return nil, errors.New("invalid bounded read retry policy")
	}
	started := time.Now()
	recoveringSession := false
	for attempt := 1; attempt <= policy.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 1 {
			timer := time.NewTimer(policy.delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			// Cancellation, revocation, loss of leadership and grant fencing
			// must all prevent a follow-on network call.
			if err := r.checkMCPDispatch(ctx); err != nil {
				return nil, err
			}
		}
		raw, err := call(ctx)
		if err == nil || attempt == policy.maxAttempts || recoveringSession {
			return raw, err
		}
		recoveringSession = errors.Is(err, mcp.ErrSessionExpired)
		if !recoveringSession && !retryableReadTransportError(err) {
			return raw, err
		}
		if recoveringSession && timeout > 0 {
			// A replacement session uses what remains of the original timeout.
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, started.Add(timeout))
			defer cancel()
		}
	}
	return nil, errors.New("unreachable read retry state")
}
