package probe

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSpecsCoverAllKinds: every registered kind is advertised, sorted, with an
// object param schema.
func TestSpecs(t *testing.T) {
	specs := Specs()
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
		if s.Params["type"] != "object" {
			t.Errorf("%s params not an object schema: %+v", s.Name, s.Params)
		}
	}
	for _, want := range []string{"probe_http", "probe_tls", "probe_dns", "probe_quic", "probe_tcp", "probe_ping", "probe_traceroute"} {
		if !IsProbe(want) {
			t.Errorf("missing probe kind %q (have %v)", want, names)
		}
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("specs not sorted: %v", names)
		}
	}
}

func TestRunUnknown(t *testing.T) {
	if _, err := Run(context.Background(), "probe_nope", nil); err == nil {
		t.Fatal("unknown probe should error")
	}
}

// TestValidHost: the shell-probe target sanitizer rejects shell metacharacters,
// spaces, and leading hyphens (flag injection), accepts names/IPs.
func TestValidHost(t *testing.T) {
	for _, ok := range []string{"edge.example.com", "10.3.10.4", "a", "host-1.lab", "2001:db8::1"} {
		if _, err := validHost(ok); err != nil {
			t.Errorf("validHost(%q) rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-rf", "a; rm -rf /", "a b", "a|b", "$(x)", "a`b`", "--flag"} {
		if _, err := validHost(bad); err == nil {
			t.Errorf("validHost(%q) should be rejected", bad)
		}
	}
}

// TestTCPProbeLocal: a probe against a closed local port reports connect failure as
// findings text (not an error), so the LLM can read it.
func TestTCPProbeLocal(t *testing.T) {
	out, err := Run(context.Background(), "probe_tcp", json.RawMessage(`{"host":"127.0.0.1","port":"1"}`))
	if err != nil {
		t.Fatalf("probe_tcp errored instead of reporting: %v", err)
	}
	if !strings.Contains(out, "connect failed") {
		t.Fatalf("expected connect failure text, got: %q", out)
	}
}

// TestTrustStatusUntrusted: a self-signed cert (a stand-in for a dummy-ACME leaf)
// reports untrusted rather than aborting — the point of always-inspect probe_tls.
func TestTrustStatusUntrusted(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "edge.test"},
		DNSNames:     []string{"edge.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	got := trustStatus([]*x509.Certificate{cert}, "edge.test")
	if !strings.HasPrefix(got, "no (") {
		t.Fatalf("self-signed cert trust = %q, want untrusted", got)
	}
}

// TestBadArgs: malformed args JSON is an error.
func TestBadArgs(t *testing.T) {
	if _, err := Run(context.Background(), "probe_http", json.RawMessage(`{not json`)); err == nil {
		t.Fatal("bad args JSON should error")
	}
}

// TestHTTPProbeShowsAllHeaders: the probe must expose every response header —
// the old whitelist hid edge-injected ones (X-Wasm-*), which is exactly what an
// external probe is used to verify.
func TestHTTPProbeShowsAllHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Wasm-Processed", "true")
		w.Header().Set("X-Wasm-Fetch-Status", "200")
		w.Header().Add("X-Multi", "a")
		w.Header().Add("X-Multi", "b")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	out, err := Run(context.Background(), "probe_http", json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatalf("probe_http: %v", err)
	}
	for _, want := range []string{"X-Wasm-Processed: true", "X-Wasm-Fetch-Status: 200", "X-Multi: a, b"} {
		if !strings.Contains(out, want) {
			t.Fatalf("probe output missing %q:\n%s", want, out)
		}
	}
}

// TestHTTPProbeHeaderCap: a header flood folds into a truncation line instead of
// swamping the model's context.
func TestHTTPProbeHeaderCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 100; i++ {
			w.Header().Set(fmt.Sprintf("X-Flood-%03d", i), strings.Repeat("v", 64))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	out, err := Run(context.Background(), "probe_http", json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatalf("probe_http: %v", err)
	}
	if len(out) > maxProbeHeaderBytes+512 {
		t.Fatalf("probe output not capped: %d bytes", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("capped output must say it truncated:\n%s", out)
	}
}
