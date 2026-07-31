package stat

import (
	"sync"

	"github.com/on-keyday/kscale/consts"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
)

// AppLifecycle tracks a dataplane's lifecycle state — Initialized (connected, not yet
// started) → Running / Error → Stopped — so a controller can report it as AppStatus in
// StreamStats. This is the authoritative started/stopped signal, driven by
// Start/StopDataplane (and auto-start), and is distinct from a driver's "actually
// serving on an XDP host" heuristic (which only some dp types can compute). The zero
// value is not ready; use NewAppLifecycle.
type AppLifecycle struct {
	mu     sync.Mutex
	status consts.AppStatus
}

// NewAppLifecycle starts in Initialized (connected but not yet started).
func NewAppLifecycle() *AppLifecycle { return &AppLifecycle{status: consts.AppStatusInitialized} }

func (l *AppLifecycle) set(s consts.AppStatus) {
	l.mu.Lock()
	l.status = s
	l.mu.Unlock()
}

// SetRunning / SetStopped / SetError record a lifecycle transition (start ok / stop /
// start failed).
func (l *AppLifecycle) SetRunning() { l.set(consts.AppStatusRunning) }
func (l *AppLifecycle) SetStopped() { l.set(consts.AppStatusStopped) }
func (l *AppLifecycle) SetError()   { l.set(consts.AppStatusError) }

// Status reports the current state.
func (l *AppLifecycle) Status() consts.AppStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

// Stat is a Stats entry carrying ONLY the app status, for a controller to include in its
// StreamStats output. It has no LbId/BoundInterfaces, so GetDestEntryFromStat rejects it
// — it never becomes an l4lb destination; it's purely the status the control plane reads
// for `node` app_status.
func (l *AppLifecycle) Stat() *pbstat.Stats {
	st := uint32(l.Status())
	return &pbstat.Stats{CdnAppRealtime: &pbstat.CdnAppRealtimeStat{Appstat: &st}}
}
