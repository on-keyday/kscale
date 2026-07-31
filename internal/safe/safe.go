// Package safe runs daemon goroutines that survive a panic. ksdk recovered panics in its
// long-lived agent loops; kscale's re-homed goroutines did not, so a single panic in any
// reconcile/stream/ticker loop would crash the whole daemon. Go() recovers and logs
// (with a stack) instead, so one bad loop degrades rather than kills the process.
package safe

import (
	"log/slog"
	"runtime/debug"
)

// Go runs fn in a goroutine that recovers from a panic, logging it with a stack trace
// under the given name. The goroutine still exits on a panic (it does not auto-restart) —
// the point is to keep one loop's panic from taking down the daemon.
func Go(logger *slog.Logger, name string, fn func()) {
	go func() {
		defer Recover(logger, name)
		fn()
	}()
}

// Recover is the deferred half of Go, exposed for goroutines that are spawned by
// generated code or need their own defer (e.g. `defer safe.Recover(logger, "x")`).
func Recover(logger *slog.Logger, name string) {
	if r := recover(); r != nil {
		logger.Error("goroutine panicked (recovered)", "goroutine", name, "panic", r, "stack", string(debug.Stack()))
	}
}
