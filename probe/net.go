package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

const probeTimeout = 15 * time.Second

func init() {
	register(kind{
		spec: Spec{
			Name:        "probe_http",
			Description: "HTTP(S) request to a URL from an external vantage. Reports status, redirects, timing, HTTP version, and ALL response headers (so edge-injected headers like X-Wasm-* are visible). Use to check whether the public edge responds and what the edge actually returned.",
			Params: obj(map[string]any{
				"url":              str("full URL, e.g. https://edge.example.com/"),
				"method":           str("HEAD or GET (default HEAD)"),
				"follow_redirects": boolp("follow 3xx (default false — report the redirect)"),
				"insecure":         boolp("skip TLS cert verification (default false; set true for a dummy/untrusted-CA endpoint)"),
			}, "url"),
		},
		run: probeHTTP,
	})
	register(kind{
		spec: Spec{
			Name:        "probe_tls",
			Description: "TLS handshake to host:port. Always completes the handshake WITHOUT trust verification (so it inspects even a dummy/untrusted-CA cert) and reports negotiated version, ALPN (h2/h3), the leaf cert subject/issuer/validity, and separately whether the chain is trusted by the system roots. Use to check the public TLS terminator's health.",
			Params: obj(map[string]any{
				"host": str("hostname"),
				"port": inti("port (default 443)"),
				"sni":  str("SNI server name (default = host)"),
			}, "host"),
		},
		run: probeTLS,
	})
	register(kind{
		spec: Spec{
			Name:        "probe_dns",
			Description: "DNS lookup of a name. type is A, AAAA, CNAME, or TXT (default A). Optional resolver as ip:port. Use to check public-name resolution.",
			Params: obj(map[string]any{
				"name":     str("name to resolve"),
				"type":     str("A | AAAA | CNAME | TXT (default A)"),
				"resolver": str("resolver ip:port (default system)"),
			}, "name"),
		},
		run: probeDNS,
	})
	register(kind{
		spec: Spec{
			Name:        "probe_quic",
			Description: "QUIC/HTTP3 handshake to host:port. Reports whether h3 is reachable and the negotiated params. Use to check whether HTTP/3 is live externally (this stack is QUIC-heavy).",
			Params: obj(map[string]any{
				"host":     str("hostname"),
				"port":     inti("port (default 443)"),
				"sni":      str("SNI server name (default = host)"),
				"insecure": boolp("skip TLS cert verification (default false; set true for a dummy/untrusted-CA endpoint)"),
			}, "host"),
		},
		run: probeQUIC,
	})
	register(kind{
		spec: Spec{
			Name:        "probe_tcp",
			Description: "TCP connect to host:port, reporting whether it accepts and the connect latency (a privilege-free 'ping' for a specific service port).",
			Params: obj(map[string]any{
				"host": str("hostname or IP"),
				"port": inti("port"),
			}, "host", "port"),
		},
		run: probeTCP,
	})
}

// argInt returns args[key] as an int, or def if absent/unparseable.
func argInt(args map[string]string, key string, def int) int {
	if v, ok := args[key]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func probeHTTP(ctx context.Context, args map[string]string) (string, error) {
	url := strings.TrimSpace(args["url"])
	if url == "" {
		return "", fmt.Errorf("url is required")
	}
	method := strings.ToUpper(strings.TrimSpace(args["method"]))
	if method != "GET" {
		method = "HEAD"
	}
	follow := args["follow_redirects"] == "true"

	client := &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			if follow {
				return nil
			}
			return http.ErrUseLastResponse
		},
	}
	if args["insecure"] == "true" {
		// A dummy/untrusted-CA endpoint (e.g. an ACME test setup): skip verification
		// so the request goes through instead of failing at "unknown authority".
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return "", err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("HTTP %s %s\n  request failed: %v", method, url, err), nil
	}
	defer resp.Body.Close()
	elapsed := time.Since(start).Round(time.Millisecond)
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %s %s\n", method, url)
	fmt.Fprintf(&b, "  status: %s  (%s, %s)\n", resp.Status, resp.Proto, elapsed)
	writeAllHeaders(&b, resp.Header)
	return strings.TrimRight(b.String(), "\n"), nil
}

// maxProbeHeaderBytes caps the rendered response-header block so a verbose (or
// hostile) endpoint can't flood the model's context through a probe result.
const maxProbeHeaderBytes = 2048

// writeAllHeaders renders EVERY response header (sorted, multi-values joined) —
// the old curated whitelist hid anything unusual (e.g. the X-Wasm-* headers the
// edge modules inject), which is exactly what one probes for. Excess beyond
// maxProbeHeaderBytes folds into a "... N more" line.
func writeAllHeaders(b *strings.Builder, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	used := 0
	for i, k := range keys {
		line := fmt.Sprintf("  %s: %s\n", k, strings.Join(h.Values(k), ", "))
		if used+len(line) > maxProbeHeaderBytes {
			fmt.Fprintf(b, "  ... %d more header(s) truncated\n", len(keys)-i)
			return
		}
		b.WriteString(line)
		used += len(line)
	}
}

func probeTLS(ctx context.Context, args map[string]string) (string, error) {
	host := strings.TrimSpace(args["host"])
	if host == "" {
		return "", fmt.Errorf("host is required")
	}
	port := argInt(args, "port", 443)
	sni := strings.TrimSpace(args["sni"])
	if sni == "" {
		sni = host
	}
	dialer := &net.Dialer{Timeout: probeTimeout}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Sprintf("TLS %s:%d (sni %s)\n  connect failed: %v", host, port, sni, err), nil
	}
	defer raw.Close()
	// Always skip verification for the handshake: this is a diagnostic — we want the
	// cert even from a dummy/untrusted CA (an untrusted root would otherwise abort
	// the handshake at "unknown authority" before we could read it). Trust is
	// reported separately below.
	conn := tls.Client(raw, &tls.Config{ServerName: sni, NextProtos: []string{"h2", "http/1.1"}, InsecureSkipVerify: true})
	if err := conn.HandshakeContext(ctx); err != nil {
		return fmt.Sprintf("TLS %s:%d (sni %s)\n  handshake failed: %v", host, port, sni, err), nil
	}
	defer conn.Close()
	st := conn.ConnectionState()
	var b strings.Builder
	fmt.Fprintf(&b, "TLS %s:%d (sni %s)\n", host, port, sni)
	fmt.Fprintf(&b, "  version: %s  alpn: %q\n", tlsVersion(st.Version), st.NegotiatedProtocol)
	if len(st.PeerCertificates) > 0 {
		leaf := st.PeerCertificates[0]
		days := int(time.Until(leaf.NotAfter).Hours() / 24)
		fmt.Fprintf(&b, "  subject: %s\n", leaf.Subject)
		fmt.Fprintf(&b, "  issuer:  %s\n", leaf.Issuer)
		fmt.Fprintf(&b, "  valid:   %s → %s (%d days left)\n",
			leaf.NotBefore.Format("2006-01-02"), leaf.NotAfter.Format("2006-01-02"), days)
		if len(leaf.DNSNames) > 0 {
			fmt.Fprintf(&b, "  SANs:    %s\n", strings.Join(leaf.DNSNames, ", "))
		}
		fmt.Fprintf(&b, "  chain:   %d cert(s)\n", len(st.PeerCertificates))
		fmt.Fprintf(&b, "  trusted: %s\n", trustStatus(st.PeerCertificates, sni))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// trustStatus reports whether the presented chain verifies against the system roots
// for sni — "yes", or "no (<reason>)" (e.g. an untrusted dummy-ACME root).
func trustStatus(chain []*x509.Certificate, sni string) string {
	if len(chain) == 0 {
		return "no (no certificates presented)"
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{DNSName: sni, Intermediates: inter}); err != nil {
		return "no (" + err.Error() + ")"
	}
	return "yes"
}

func tlsVersion(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS1.3"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS10:
		return "TLS1.0"
	}
	return fmt.Sprintf("0x%04x", v)
}

func probeDNS(ctx context.Context, args map[string]string) (string, error) {
	name := strings.TrimSpace(args["name"])
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	typ := strings.ToUpper(strings.TrimSpace(args["type"]))
	if typ == "" {
		typ = "A"
	}
	res := net.DefaultResolver
	if r := strings.TrimSpace(args["resolver"]); r != "" {
		res = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: probeTimeout}
				return d.DialContext(ctx, network, r)
			},
		}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var b strings.Builder
	fmt.Fprintf(&b, "DNS %s %s\n", typ, name)
	switch typ {
	case "A", "AAAA":
		ips, err := res.LookupIP(ctx, map[string]string{"A": "ip4", "AAAA": "ip6"}[typ], name)
		if err != nil {
			return b.String() + "  lookup failed: " + err.Error(), nil
		}
		strs := make([]string, len(ips))
		for i, ip := range ips {
			strs[i] = ip.String()
		}
		sort.Strings(strs)
		for _, s := range strs {
			fmt.Fprintf(&b, "  %s\n", s)
		}
	case "CNAME":
		cname, err := res.LookupCNAME(ctx, name)
		if err != nil {
			return b.String() + "  lookup failed: " + err.Error(), nil
		}
		fmt.Fprintf(&b, "  %s\n", cname)
	case "TXT":
		txts, err := res.LookupTXT(ctx, name)
		if err != nil {
			return b.String() + "  lookup failed: " + err.Error(), nil
		}
		for _, t := range txts {
			fmt.Fprintf(&b, "  %q\n", t)
		}
	default:
		return "", fmt.Errorf("unsupported record type %q (A|AAAA|CNAME|TXT)", typ)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func probeQUIC(ctx context.Context, args map[string]string) (string, error) {
	host := strings.TrimSpace(args["host"])
	if host == "" {
		return "", fmt.Errorf("host is required")
	}
	port := argInt(args, "port", 443)
	sni := strings.TrimSpace(args["sni"])
	if sni == "" {
		sni = host
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{ServerName: sni, InsecureSkipVerify: args["insecure"] == "true"},
		QUICConfig:      &quic.Config{HandshakeIdleTimeout: probeTimeout},
	}
	defer tr.Close()
	client := &http.Client{Transport: tr, Timeout: probeTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+addr+"/", nil)
	if err != nil {
		return "", err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("QUIC/HTTP3 %s (sni %s)\n  handshake/request failed: %v", addr, sni, err), nil
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	elapsed := time.Since(start).Round(time.Millisecond)
	return fmt.Sprintf("QUIC/HTTP3 %s (sni %s)\n  h3 reachable: status %s (%s, %s)",
		addr, sni, resp.Status, resp.Proto, elapsed), nil
}

func probeTCP(ctx context.Context, args map[string]string) (string, error) {
	host := strings.TrimSpace(args["host"])
	port := argInt(args, "port", 0)
	if host == "" || port == 0 {
		return "", fmt.Errorf("host and port are required")
	}
	d := net.Dialer{Timeout: probeTimeout}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Sprintf("TCP %s:%d\n  connect failed: %v", host, port, err), nil
	}
	conn.Close()
	return fmt.Sprintf("TCP %s:%d\n  connect ok in %s", host, port, time.Since(start).Round(time.Millisecond)), nil
}
