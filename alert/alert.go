// Package alert is the control plane's deterministic alert evaluator. On a timer it
// re-derives the set of active alerts purely from state the CP already holds — broker
// membership, the stat cache, the reconcile status, and the CA's cert validity — so there
// is no LLM and no extra reporting path. New alerts are logged (WARN/critical→ERROR) so they
// flow into the live log feed, and the current set is exposed via the `alert` resource.
//
// This exists because the failures that bite hardest are the SILENT ones (a cert renewal
// that quietly stalled until every node's cert expired — see notes/bugs/bug_2026_06_30_*). An
// alert turns "only in the logs" into "surfaced".
package alert

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/consts"
	"github.com/on-keyday/kscale/dpbroker"
	"github.com/on-keyday/kscale/reconcile"
	"github.com/on-keyday/kscale/service/stats"
)

const (
	KindCertExpiring   = "cert_expiring"
	KindReconcileError = "reconcile_error"
	KindAppError       = "app_error"
	KindResourceHigh   = "resource_high"

	SevWarning  = "warning"
	SevCritical = "critical"
)

// Alert is one active condition. Since is when it was first observed.
type Alert struct {
	Kind     string
	Severity string
	Node     string
	Message  string
	Since    time.Time
}

func key(kind, node string) string { return kind + "\x00" + node }

// Evaluator periodically derives and tracks the active alert set.
type Evaluator struct {
	Broker    *dpbroker.Broker
	Stat      *stats.Cache
	Reconcile *reconcile.Status
	CA        *ca.CA
	Logger    *slog.Logger

	// Thresholds (zero -> a sensible default in Run).
	CertExpiryWarn time.Duration // alert when a connected node's cert expires within this
	CPUHighPct     float64       // alert when mean CPU% exceeds this
	LoadPerCore    float64       // alert when load1 / cores exceeds this

	mu     sync.Mutex
	active map[string]Alert
}

// Run evaluates every `every` until ctx ends, logging alert transitions. Defaults are
// filled in for any unset threshold.
func (e *Evaluator) Run(ctx context.Context, every time.Duration) {
	if e.CertExpiryWarn == 0 {
		e.CertExpiryWarn = 8 * time.Hour
	}
	if e.CPUHighPct == 0 {
		e.CPUHighPct = 90
	}
	if e.LoadPerCore == 0 {
		e.LoadPerCore = 2
	}
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		e.tick(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (e *Evaluator) tick(now time.Time) {
	next := e.evaluate(now)
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, a := range next {
		if old, ok := e.active[k]; ok {
			a.Since = old.Since // keep first-seen time
			next[k] = a
			continue
		}
		// newly firing — surface it.
		msg := "alert"
		args := []any{"kind", a.Kind, "node", a.Node, "detail", a.Message}
		if a.Severity == SevCritical {
			e.Logger.Error(msg, args...)
		} else {
			e.Logger.Warn(msg, args...)
		}
	}
	for k, old := range e.active {
		if _, ok := next[k]; !ok {
			e.Logger.Info("alert resolved", "kind", old.Kind, "node", old.Node)
		}
	}
	e.active = next
}

// evaluate runs all rules over the current CP state and returns the firing alerts.
func (e *Evaluator) evaluate(now time.Time) map[string]Alert {
	out := map[string]Alert{}
	add := func(kind, sev, node, msg string) {
		out[key(kind, node)] = Alert{Kind: kind, Severity: sev, Node: node, Message: msg, Since: now}
	}

	// Latest cert expiry per CN (renewals issue new serials; the active one is the latest).
	expiry := map[string]time.Time{}
	if e.CA != nil {
		for _, c := range e.CA.ListCertificates() {
			if c.ExpiresAt.After(expiry[c.CommonName]) {
				expiry[c.CommonName] = c.ExpiresAt
			}
		}
	}

	for _, n := range e.Broker.Nodes() {
		cn := n.CommonName

		// 1. cert expiring soon (also catches a stalled renewal: if renewal worked the
		// cert would always be ~a full TTL out, never inside the warn window).
		if ea, ok := expiry[cn]; ok {
			if left := ea.Sub(now); left < e.CertExpiryWarn {
				sev := SevWarning
				if left < e.CertExpiryWarn/3 {
					sev = SevCritical
				}
				add(KindCertExpiring, sev, cn, fmt.Sprintf("cert expires in %s — renewal may be stalled", left.Round(time.Minute)))
			}
		}

		// 2. reconcile push failing for this node.
		if errs := e.Reconcile.ErrorsForNode(cn); len(errs) > 0 {
			add(KindReconcileError, SevWarning, cn, strings.Join(errs, "; "))
		}

		// 3 & 4: app state + host resource, from the stat cache.
		sb := e.Stat.Get(cn)
		for _, st := range sb.Stats {
			if st == nil {
				continue
			}
			if st.CdnAppRealtime != nil && st.CdnAppRealtime.Appstat != nil &&
				*st.CdnAppRealtime.Appstat == uint32(consts.AppStatusError) {
				add(KindAppError, SevCritical, cn, "dataplane app is in Error state")
			}
			if st.HostRealtime != nil {
				if cpu := meanCPU(st.HostRealtime.CpuUsages); cpu > e.CPUHighPct {
					add(KindResourceHigh, SevWarning, cn, fmt.Sprintf("CPU %.0f%%", cpu))
				}
				if cores := len(st.HostRealtime.CpuUsages); cores > 0 && len(st.HostRealtime.LoadAvg) > 0 {
					if per := st.HostRealtime.LoadAvg[0] / float64(cores); per > e.LoadPerCore {
						add(KindResourceHigh, SevWarning, cn, fmt.Sprintf("load %.2f over %d cores", st.HostRealtime.LoadAvg[0], cores))
					}
				}
			}
		}
	}
	return out
}

func meanCPU(cpus []float64) float64 {
	if len(cpus) == 0 {
		return 0
	}
	var s float64
	for _, c := range cpus {
		s += c
	}
	return s / float64(len(cpus))
}

// Active returns the current alerts, criticals first then by node.
func (e *Evaluator) Active() []Alert {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Alert, 0, len(e.active))
	for _, a := range e.active {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == SevCritical // critical first
		}
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}
