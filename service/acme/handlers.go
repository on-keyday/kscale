package acme

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/on-keyday/kscale/ca"
	"github.com/on-keyday/kscale/dpbroker"
	"github.com/on-keyday/kscale/popcache/encrypt"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/rpc"
)

// splitDomains parses the comma-separated `domain` arg into the SAN list for one cert,
// e.g. "*.example.com, example.com" -> ["*.example.com", "example.com"]. Without this the
// whole string went to the ACME new-order as a SINGLE bogus identifier (-> 400). ksdk's
// obtain already took a []string of domains; this restores multi-domain (apex + wildcard)
// certs.
func splitDomains(s string) []string {
	var out []string
	for _, d := range strings.Split(s, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// Handlers is the AcmeService business: Obtain runs the lego client (the salvaged
// popcache/encrypt.AcmeAgent) with the dns-dataplane DNS-01 provider, then
// distributes the obtained cert/key to every popcache node over the transport
// (DataplaneService.SendFile) and reloads its TLS (PopcacheControlService).
type Handlers struct {
	pb.UnimplementedAcmeServiceServer
	CA     *ca.CA
	Broker *dpbroker.Broker
	Logger *slog.Logger
	// Ctx bounds the DNS-01 challenge fan-out RPCs (lego's provider interface
	// carries no context); set to the control plane's lifetime context.
	Ctx context.Context
}

var _ pb.AcmeServiceServer = (*Handlers)(nil)

func (h *Handlers) Obtain(ctx context.Context, req *pbaccess.ResourceAcmeActionObtainArgsDTO) (*pbaccess.ResourceAcmeActionObtainResponseDTO, error) {
	provider := &dnsProvider{broker: h.Broker, logger: h.Logger, ctx: h.Ctx}
	agent, err := encrypt.SetupAcme(h.CA, provider, req.Email, req.CaDirUrl)
	if err != nil {
		return nil, fmt.Errorf("acme setup: %w", err)
	}
	domains := splitDomains(req.Domain)
	if len(domains) == 0 {
		return nil, fmt.Errorf("acme obtain: no domain given")
	}
	res, err := agent.GetCertificate(ctx, h.Logger, domains)
	if err != nil {
		return nil, fmt.Errorf("acme obtain %s: %w", req.Domain, err)
	}

	// Distribute the obtained cert/key to every popcache node over the transport. Written
	// to the data directory ROOT — NOT an "acme/" subdir, which the agent doesn't create
	// and popcache doesn't look in. Named "<domain>.crt"/".key" after the first (sanitized)
	// domain ("*" isn't filename-safe).
	sanitized := strings.ReplaceAll(domains[0], "*", "wildcard")
	certName := sanitized + ".crt"
	keyName := sanitized + ".key"
	peers := h.Broker.List("popcache")
	var distributed, failed []string
	for _, p := range peers {
		cn := p.CommonName()
		src := rpc.NewTrsfStreamSource(p.Streams(), h.Logger)
		dp := pb.NewDataplaneServiceClient(src)
		if _, err := dp.SendFile(ctx, &pb.DataplaneServiceSendFileRequest{FileName: certName, SaveAs: certName, Content: res.CertPEM}); err != nil {
			h.Logger.Error("acme: distribute cert failed", "node", cn, "error", err)
			failed = append(failed, cn+": cert "+err.Error())
			continue
		}
		if _, err := dp.SendFile(ctx, &pb.DataplaneServiceSendFileRequest{FileName: keyName, SaveAs: keyName, Content: res.KeyPEM}); err != nil {
			h.Logger.Error("acme: distribute key failed", "node", cn, "error", err)
			failed = append(failed, cn+": key "+err.Error())
			continue
		}
		pc := pb.NewPopcacheControlServiceClient(src)
		if _, err := pc.SetCertPath(ctx, &pb.PopcacheControlServiceSetCertPathRequest{Path: certName}); err != nil {
			h.Logger.Error("acme: set cert path failed", "node", cn, "error", err)
			failed = append(failed, cn+": set-cert-path "+err.Error())
			continue
		}
		if _, err := pc.SetKeyPath(ctx, &pb.PopcacheControlServiceSetKeyPathRequest{Path: keyName}); err != nil {
			h.Logger.Error("acme: set key path failed", "node", cn, "error", err)
			failed = append(failed, cn+": set-key-path "+err.Error())
			continue
		}
		// Reload is best-effort: a STOPPED popcache can't reload its TLS now, but it'll pick
		// up the staged cert/key path when it next starts — so the cert IS distributed (files
		// written, paths set); don't drop the node to "failed" just because reload no-op'd.
		if _, err := pc.ReloadTlsCert(ctx, &wkt.Empty{}); err != nil {
			h.Logger.Warn("acme: tls reload deferred (node not running?)", "node", cn, "error", err)
		}
		distributed = append(distributed, cn)
	}

	// Make a 0 visible instead of a silent OK: say whether no popcache was connected at all
	// vs. distribution failing on connected nodes (with the per-node reason).
	status := fmt.Sprintf("obtained cert for %s; distributed to %d/%d popcache node(s)", req.Domain, len(distributed), len(peers))
	switch {
	case len(peers) == 0:
		status += " (no popcache nodes connected — cert obtained but not distributed)"
	case len(failed) > 0:
		status += "; failures: " + strings.Join(failed, "; ")
	}
	return &pbaccess.ResourceAcmeActionObtainResponseDTO{
		Status:        status,
		DistributedTo: distributed,
	}, nil
}

// setup builds the AcmeAgent for a management op. No email is needed — the account is
// loaded from CA storage (created on the first obtain); reconstructing it is cheap, so
// each op is stateless (matching obtain's auto-setup). caDirURL selects the account.
func (h *Handlers) setup(caDirURL string) (*encrypt.AcmeAgent, error) {
	provider := &dnsProvider{broker: h.Broker, logger: h.Logger, ctx: h.Ctx}
	return encrypt.SetupAcme(h.CA, provider, "", caDirURL)
}

func (h *Handlers) List(ctx context.Context, req *pbaccess.ResourceAcmeActionListArgsDTO) (*pbaccess.ResourceAcmeActionListResponseDTO, error) {
	agent, err := h.setup(req.CaDirUrl)
	if err != nil {
		return nil, fmt.Errorf("acme setup: %w", err)
	}
	idxs, err := agent.ListCertificates(ctx, h.Logger)
	if err != nil {
		return nil, err
	}
	certs := make([]string, 0, len(idxs))
	for _, idx := range idxs {
		certs = append(certs, string(idx.Cert.Name.Name))
	}
	return &pbaccess.ResourceAcmeActionListResponseDTO{Certificates: certs}, nil
}

func (h *Handlers) GetCert(ctx context.Context, req *pbaccess.ResourceAcmeActionGetCertArgsDTO) (*pbaccess.ResourceAcmeActionGetCertResponseDTO, error) {
	agent, err := h.setup(req.CaDirUrl)
	if err != nil {
		return nil, fmt.Errorf("acme setup: %w", err)
	}
	idx, err := agent.GetCertificateInfo(ctx, h.Logger, splitDomains(req.Domain))
	if err != nil {
		return nil, err
	}
	return &pbaccess.ResourceAcmeActionGetCertResponseDTO{Certificate: string(idx.Cert.Name.Name)}, nil
}

func (h *Handlers) DeleteCert(ctx context.Context, req *pbaccess.ResourceAcmeActionDeleteCertArgsDTO) (*pbaccess.ResourceAcmeActionDeleteCertResponseDTO, error) {
	agent, err := h.setup(req.CaDirUrl)
	if err != nil {
		return nil, fmt.Errorf("acme setup: %w", err)
	}
	if err := agent.DeleteCertificate(ctx, h.Logger, splitDomains(req.Domain)); err != nil {
		return nil, err
	}
	return &pbaccess.ResourceAcmeActionDeleteCertResponseDTO{Status: "deleted cert for " + req.Domain}, nil
}

func (h *Handlers) DeleteAccount(ctx context.Context, req *pbaccess.ResourceAcmeActionDeleteAccountArgsDTO) (*pbaccess.ResourceAcmeActionDeleteAccountResponseDTO, error) {
	agent, err := h.setup(req.CaDirUrl)
	if err != nil {
		return nil, fmt.Errorf("acme setup: %w", err)
	}
	if err := agent.DeleteAccount(ctx, h.Logger); err != nil {
		return nil, err
	}
	return &pbaccess.ResourceAcmeActionDeleteAccountResponseDTO{Status: "deleted acme account"}, nil
}
