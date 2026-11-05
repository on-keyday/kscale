package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/kscale/dpbroker"
	"github.com/on-keyday/kscale/internal/safe"
	"github.com/on-keyday/kscale/peer"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/rpc"
	"github.com/on-keyday/kscale/service/l4route"
)

// L4RouteDesired is the desired-state source the L4Route controller reconciles
// (satisfied by the generated l4_route store).
type L4RouteDesired interface {
	Desired() []*pbaccess.ResourceL4RouteActionGetResponseDTO
	OnChange(func())
}

// L4Route pushes every l4lb node its FULL port-route set
// (DataplaneService.SetPortRoutes). A route's destination is the target node's
// reported interface address, so the set is recomputed on a tick as well as on a
// declaration change and on l4lb (re)connect; a node only gets a push when its
// set actually changed. A route whose node does not resolve (not connected, no
// bound interface yet) is left out — the port falls back to the consistent hash.
// That is NOT recorded as the l4lb's error: the status feeds node_run's start
// gate, and a workload node being down must not keep the l4lb from starting.
// `l4-route apply` and `l4-route diff` report unresolved routes instead; the
// status carries only SetPortRoutes push failures.
func L4Route(ctx context.Context, store L4RouteDesired, cache l4route.StatSource, broker *dpbroker.Broker, status *Status, logger *slog.Logger) {
	var mu sync.Mutex
	pushed := map[string]string{} // l4lb CN -> fingerprint of the last successful push

	reconcile := func() {
		mu.Lock()
		defer mu.Unlock()
		req := &pb.DataplaneServiceSetPortRoutesRequest{}
		var unresolved []string // logged once per change of the set
		items := store.Desired()
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		for _, it := range items {
			t, err := l4route.Resolve(it, broker, cache)
			if err != nil {
				unresolved = append(unresolved, fmt.Sprintf("%s (%s -> %s): %v", it.Name, it.Port, it.Node, err))
				continue
			}
			req.Routes = append(req.Routes, &pb.PortRoute{
				Proto: t.Proto,
				Port:  uint32(t.Port),
				Dest:  &pb.Destination{IpAddr: t.Dest.IPAddr.String(), HardwareAddr: t.Dest.HardwareAddr},
			})
		}
		fp := fingerprint(req)
		for _, p := range broker.List("l4lb") {
			cn := p.CommonName()
			if last, ok := pushed[cn]; ok && last == fp {
				continue
			}
			c := pb.NewDataplaneServiceClient(rpc.NewTrsfStreamSource(p.Streams(), logger))
			_, err := c.SetPortRoutes(ctx, req)
			status.Record("l4_route", cn, time.Now(), err)
			if err != nil {
				delete(pushed, cn) // retried on the next tick
				logger.Error("reconcile l4_route: SetPortRoutes failed", "node", cn, "routes", len(req.Routes), "error", err)
				continue
			}
			pushed[cn] = fp
			logger.Info("reconcile l4_route: pushed port routes", "node", cn, "routes", len(req.Routes), "unresolved", unresolved)
		}
	}

	store.OnChange(reconcile)
	broker.OnConnect(func(dpType string, p *peer.Peer) {
		if dpType == "l4lb" {
			mu.Lock()
			delete(pushed, p.CommonName()) // a reconnected l4lb may have restarted: push again
			mu.Unlock()
		}
		reconcile() // any node joining may make a route resolvable
	})

	go func() {
		defer safe.Recover(logger, "l4_route")
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reconcile()
			}
		}
	}()
}

func fingerprint(req *pb.DataplaneServiceSetPortRoutesRequest) string {
	var b strings.Builder
	for _, r := range req.Routes {
		fmt.Fprintf(&b, "%s:%d>%s/%x;", r.Proto, r.Port, r.Dest.IpAddr, r.Dest.HardwareAddr)
	}
	return b.String()
}
