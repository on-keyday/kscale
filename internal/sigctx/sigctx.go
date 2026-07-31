// Package sigctx provides the daemon root context: cancelled on SIGINT/SIGTERM so the
// process shuts down gracefully (background loops get ctx.Done(), goroutines waiting on
// the context exit instead of leaking). Ported from ksdk's signal.NotifyContext wiring
// (cmd/agent/main.go), which kscale's re-homed mains had dropped.
package sigctx

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// Context returns a context cancelled on the first SIGINT/SIGTERM, plus the stop func to
// release the signal handler (call via defer). Use as the root context of a daemon main.
func Context() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
