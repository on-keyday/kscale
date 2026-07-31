package client

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/objtrsf/objproto"
)

// DefaultCertRenewInterval mirrors the dataplane substrate's cadence (dataplane.renewCert).
const DefaultCertRenewInterval = 30 * time.Minute

// ClientCertPath is where a role's enrolled cert is cached (and re-read on reconnect).
// Shared so the renewal loop and enroll agree on the file.
func ClientCertPath(dataDir, role string) string {
	return filepath.Join(dataDir, "client."+role+".boot")
}

// BootHolder guards the latest BootstrapInfo, shared between a client's (re)connect path
// (which sets it after enroll) and RenewCertLoop (which renews it). Standalone clients
// that don't ride the dataplane substrate — metricsgw, nodewatch — use this to get the
// cert auto-renewal the substrate gives the dataplane agents.
type BootHolder struct {
	mu   sync.Mutex
	boot *ca.BootstrapInfo
}

// NewBootHolder returns a holder seeded with boot (may be nil until the first enroll).
func NewBootHolder(boot *ca.BootstrapInfo) *BootHolder { return &BootHolder{boot: boot} }

// Get returns the current BootstrapInfo (nil before the first enroll).
func (h *BootHolder) Get() *ca.BootstrapInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.boot
}

// Set replaces the current BootstrapInfo.
func (h *BootHolder) Set(boot *ca.BootstrapInfo) {
	h.mu.Lock()
	h.boot = boot
	h.mu.Unlock()
}

// RenewCertLoop renews the enrolled cert every `every` via the update-cert protocol
// (which reuses the current cert — no bootstrap token needed), saves the result to
// savePath, and updates holder so the next reconnect uses it. It mirrors
// dataplane.renewCert for standalone clients (metricsgw / nodewatch), which otherwise
// die when the enrolled cert expires (the bootstrap token is single-use, so they can't
// re-enroll). Each renewal uses a fresh connection id with the SAME transport/addr as
// cid, so it doesn't collide with the live connection (a fixed id wedges on the lingering
// prior renewal connection). Errors are logged and non-fatal; returns when ctx is done.
func RenewCertLoop(ctx context.Context, ep objproto.Endpoint, cid objproto.ConnectionID, commonName, app, savePath string, every time.Duration, holder *BootHolder, logger *slog.Logger) {
	if every <= 0 {
		every = DefaultCertRenewInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		boot := holder.Get()
		if boot == nil {
			continue // not enrolled yet
		}
		renewCID, err := objproto.NewRandomConnectionID(cid.Transport, cid.Addr)
		if err != nil {
			logger.Error("cert renewal id (non-fatal)", "error", err)
			continue
		}
		newInfo, err := ca.UpdateCertProtocol(ctx, ep, renewCID, commonName, app, boot)
		if err != nil {
			logger.Error("cert renewal failed (non-fatal)", "error", err)
			continue
		}
		dumped, err := newInfo.DumpResult()
		if err != nil {
			logger.Error("cert renewal dump failed (non-fatal)", "error", err)
			continue
		}
		if savePath != "" {
			if err := os.WriteFile(savePath, dumped, 0o600); err != nil {
				logger.Error("cert renewal save failed (non-fatal)", "error", err)
				continue
			}
		}
		holder.Set(newInfo)
		logger.Info("certificate renewed", "common_name", commonName)
	}
}
