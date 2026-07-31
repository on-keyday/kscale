// Package epclean periodically evicts stale objproto endpoint state so it does not grow
// unboundedly. Ported from ksdk agent/system/cleaner.go, which kscale's re-home dropped:
// without it, completed handshakes, inactive connections, and proxy entries accumulate on
// the endpoint forever.
package epclean

import (
	"context"
	"log/slog"
	"time"

	"github.com/on-keyday/objtrsf/objproto"
)

// Endpoint is the subset of objproto.Endpoint the cleaner needs.
type Endpoint interface {
	DeleteHandshakeBefore(limit time.Time) []objproto.HandshakeInfo
	DeleteInactiveConnectionsBefore(limit time.Time) []objproto.Connection
	DeleteProxyBefore(limit time.Time) []objproto.ProxyInfo
}

// Run evicts, every 10s until ctx is cancelled: handshakes idle >30s, connections inactive
// >2m, and proxy entries older than >2m (ksdk's thresholds). Returns on ctx.Done().
func Run(ctx context.Context, ep Endpoint, logger *slog.Logger) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if h := ep.DeleteHandshakeBefore(now.Add(-30 * time.Second)); len(h) > 0 {
				logger.Debug("cleaner: evicted stale handshakes", "count", len(h))
			}
			if c := ep.DeleteInactiveConnectionsBefore(now.Add(-2 * time.Minute)); len(c) > 0 {
				logger.Debug("cleaner: evicted inactive connections", "count", len(c))
			}
			if p := ep.DeleteProxyBefore(now.Add(-2 * time.Minute)); len(p) > 0 {
				logger.Debug("cleaner: evicted stale proxies", "count", len(p))
			}
		}
	}
}
