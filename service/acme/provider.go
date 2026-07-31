// Package acme is the control-plane ACME (Let's Encrypt) certificate provisioning
// business: it drives the salvaged lego client (popcache/encrypt) with a DNS-01
// challenge provider that fans the _acme-challenge TXT record out to the dns
// dataplane (DnsControlService), then distributes the obtained cert to popcache
// over the transport (DataplaneService.SendFile + ReloadTlsCert). Re-home of
// ksdk's agent/system/acme.go without the agent framework.
package acme

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"

	"github.com/on-keyday/kscale/dpbroker"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	"github.com/on-keyday/kscale/rpc"
)

// dnsProvider implements lego's challenge.Provider (Present/CleanUp) for DNS-01 by
// fanning the _acme-challenge TXT record out to every connected dns dataplane node
// via DnsControlService.SetupAcmeChallenge / CleanupAcmeChallenge. ctx bounds the
// fan-out RPCs (lego's interface carries no context).
type dnsProvider struct {
	broker *dpbroker.Broker
	logger *slog.Logger
	ctx    context.Context
}

// keyAuthToValue derives the DNS-01 TXT record value from lego's key
// authorization: base64url(sha256(keyAuth)) (RFC 8555 §8.4). Matches ksdk.
func keyAuthToValue(keyAuth string) string {
	sum := sha256.Sum256([]byte(keyAuth))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (p *dnsProvider) fanout(setup bool, domain, value string) error {
	peers := p.broker.List("dns")
	if len(peers) == 0 {
		return fmt.Errorf("acme dns-01: no dns dataplane node connected")
	}
	for _, peer := range peers {
		c := pb.NewDnsControlServiceClient(rpc.NewTrsfStreamSource(peer.Streams(), p.logger))
		var err error
		if setup {
			_, err = c.SetupAcmeChallenge(p.ctx, &pb.DnsControlServiceSetupAcmeChallengeRequest{Domain: domain, Token: value})
		} else {
			_, err = c.CleanupAcmeChallenge(p.ctx, &pb.DnsControlServiceCleanupAcmeChallengeRequest{Domain: domain, Token: value})
		}
		if err != nil {
			return fmt.Errorf("acme dns-01 %s on %s: %w", domain, peer.CommonName(), err)
		}
	}
	return nil
}

func (p *dnsProvider) Present(domain, token, keyAuth string) error {
	return p.fanout(true, domain, keyAuthToValue(keyAuth))
}

func (p *dnsProvider) CleanUp(domain, token, keyAuth string) error {
	return p.fanout(false, domain, keyAuthToValue(keyAuth))
}
