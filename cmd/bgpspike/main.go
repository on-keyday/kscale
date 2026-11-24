// bgpspike runs the minimal BGP speaker against one peer, announcing the
// given IPv4 prefixes. Spike for trying kscale's own speaker against a real
// router before designing where it lives.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/on-keyday/kscale/bgp/speaker"
)

func main() {
	peer := flag.String("peer", "", "peer host:port (port 179)")
	localAS := flag.Uint("local-as", 0, "local AS (2-octet)")
	peerAS := flag.Uint("peer-as", 0, "peer AS (2-octet)")
	routerID := flag.String("router-id", "", "router id (IPv4)")
	nextHop := flag.String("next-hop", "", "next hop for announced prefixes (IPv4)")
	announce := flag.String("announce", "", "comma-separated IPv4 prefixes to announce")
	hold := flag.Uint("hold", 90, "proposed hold time (s)")
	withdrawAfter := flag.Duration("withdraw-after", 0, "withdraw everything after this long (0 = never)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fail := func(msg string, args ...any) {
		log.Error(msg, args...)
		os.Exit(2)
	}
	if *peer == "" || *localAS == 0 || *peerAS == 0 || *localAS > 65535 || *peerAS > 65535 {
		fail("need -peer, -local-as and -peer-as (1..65535)")
	}
	rid, err := netip.ParseAddr(*routerID)
	if err != nil {
		fail("bad -router-id", "err", err)
	}
	nh, err := netip.ParseAddr(*nextHop)
	if err != nil {
		fail("bad -next-hop", "err", err)
	}
	var prefixes []netip.Prefix
	for _, s := range strings.Split(*announce, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			fail("bad -announce", "prefix", s, "err", err)
		}
		prefixes = append(prefixes, p)
	}

	sp := speaker.New(speaker.Config{
		Peer: *peer, LocalAS: uint16(*localAS), PeerAS: uint16(*peerAS),
		RouterID: rid, NextHop: nh, HoldTime: uint16(*hold), Logger: log,
	})
	for _, p := range prefixes {
		sp.Announce(p)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *withdrawAfter > 0 {
		time.AfterFunc(*withdrawAfter, func() {
			log.Info("withdrawing all prefixes")
			for _, p := range prefixes {
				sp.Withdraw(p)
			}
		})
	}
	err = sp.Run(ctx)
	log.Info("bgpspike exiting", "err", err)
}
