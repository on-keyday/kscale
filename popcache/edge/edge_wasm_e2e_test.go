package edge

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
)

// fakeGeoLocator returns a fixed location for any IP, to exercise the
// geoloc_lookup host callback without a geoip database.
type fakeGeoLocator struct{ info GeoInfo }

func (f fakeGeoLocator) Lookup(netip.Addr) (*GeoInfo, error) {
	g := f.info
	return &g, nil
}

// fakePopcacheConfig is a static PopcacheConfigProvider for the ABI test.
type fakePopcacheConfig struct{ info PopcacheConfigInfo }

func (f fakePopcacheConfig) PopcacheConfig() *PopcacheConfigInfo {
	c := f.info
	return &c
}

// TestEdgeWasmABIRoundTrip drives the real edge_app.wasm through the full
// host<->wasm ABI end to end:
//
//	host encode RequestInfo/ResponseInfo (ebm2go, request_info.go)
//	  -> guest decode (ebm2rust, edge.rs / edge_app)
//	  -> guest emits a ChangeSet (ebm2rust)
//	  -> host decode + apply the ChangeSet (ebm2go DecodeExact)
//
// A clean ProcessRequest/ProcessResponse proves the host->guest encode/decode
// works for both messages; the injected "X-Wasm-Processed" response header
// proves the guest->host ChangeSet direction round-trips too. It also covers
// the read-context TLS field (X-Wasm-Sni) and the geoloc_lookup host callback
// (X-Wasm-Geo). This is the verification gate for the edge ABI + capabilities.
func TestEdgeWasmABIRoundTrip(t *testing.T) {
	wasmPath := filepath.Join("..", "wasm", "edge_app.wasm")
	binary, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Skipf("edge_app.wasm not built (%v); run edge_app/build.sh first", err)
	}

	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	geo := fakeGeoLocator{info: GeoInfo{ASN: 64500, ASNOrg: "TestNet", Country: "JP", City: "Tokyo"}}
	ec, err := NewEdgeComputing(rt, 5*time.Second, logger, geo)
	if err != nil {
		t.Fatalf("NewEdgeComputing: %v", err)
	}
	defer ec.Close()

	// Inject a serving-config provider so the module's get_popcache_config
	// callback has something to return.
	ec.SetPopcacheConfigProvider(fakePopcacheConfig{info: PopcacheConfigInfo{
		Origin: "http://origin.test:8080", HTTPPort: 80, HTTPSPort: 443, HTTP3Port: 443,
	}})

	// Register the module for "GET /" — the path the module special-cases.
	if err := ec.Register(ctx, ModuleSpec{ID: 1, Method: "GET", Path: "/", MatchType: MatchTypeExact, FilePath: "edge_app.wasm"}, binary); err != nil {
		t.Fatalf("Register: %v", err)
	}

	const reqBody = "hello wasm body"
	req := httptest.NewRequest(http.MethodGet, "https://example.test/", strings.NewReader(reqBody))
	req.Header.Set("X-Test", "abc")
	// Mirror the real datapath: net/http lifts Host out of Header into r.Host,
	// so the server puts it back before invoking the edge function
	// (popcache/server/server.go: `req.Header.Set("Host", req.Host)`).
	req.Header.Set("Host", req.Host)
	// Populate TLS read-context, as popcache does when it terminates TLS.
	req.TLS = &tls.ConnectionState{
		Version:            tls.VersionTLS13,
		CipherSuite:        tls.TLS_AES_128_GCM_SHA256,
		ServerName:         "example.test",
		NegotiatedProtocol: "h2",
	}
	// Local address the request landed on, as the http.Server records it.
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("203.0.113.5"), Port: 8443}))

	reqID, err := ec.StartRequest(ctx, req)
	if err != nil {
		t.Fatalf("StartRequest: %v", err)
	}
	defer ec.FinishRequest(ctx, reqID)

	// host encode RequestInfo (ebm2go) -> guest decode (ebm2rust).
	if err := ec.ProcessRequest(ctx, reqID, 42, req); err != nil {
		t.Fatalf("ProcessRequest — request-info ABI round-trip failed: %v", err)
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Request:    req,
	}
	// host encode ResponseInfo (ebm2go) -> guest decode (ebm2rust).
	if err := ec.ProcessResponse(ctx, reqID, resp); err != nil {
		t.Fatalf("ProcessResponse — response-info ABI round-trip failed: %v", err)
	}

	// host decode the guest-emitted ChangeSet (ebm2go) and apply it to resp.
	applied := 0
	if err := ec.ModifyResponse(ctx, reqID, func(diffs []DiffData) error {
		for i := range diffs {
			if err := DefaultModifyResponse(&diffs[i], resp); err != nil {
				return err
			}
			applied++
		}
		return nil
	}, true); err != nil {
		t.Fatalf("ModifyResponse: %v", err)
	}

	if got := resp.Header.Get("X-Wasm-Processed"); got != "true" {
		t.Fatalf("guest->host ChangeSet did not round-trip: X-Wasm-Processed=%q (applied %d diff(s))", got, applied)
	}
	// The module echoes the request's TLS SNI it decoded, proving the TLS
	// read-context field survived host-encode -> guest-decode.
	if got := resp.Header.Get("X-Wasm-Sni"); got != "example.test" {
		t.Fatalf("TLS read-context did not round-trip: X-Wasm-Sni=%q, want %q", got, "example.test")
	}
	// The module geo-located the client IP via the geoloc_lookup host callback
	// and echoed the country; proves the input+output host-callback ABI works.
	if got := resp.Header.Get("X-Wasm-Geo"); got != "JP" {
		t.Fatalf("geoloc host callback did not round-trip: X-Wasm-Geo=%q, want %q", got, "JP")
	}
	// The module echoes the local address it decoded, proving the localAddr
	// read-context field round-trips.
	if got := resp.Header.Get("X-Wasm-Local"); got != "203.0.113.5:8443" {
		t.Fatalf("localAddr read-context did not round-trip: X-Wasm-Local=%q, want %q", got, "203.0.113.5:8443")
	}
	// The module lazily pulled the request body and reported its length,
	// exercising the get_request_body host callback.
	if got := resp.Header.Get("X-Wasm-ReqBody-Len"); got != strconv.Itoa(len(reqBody)) {
		t.Fatalf("request body pull did not round-trip: X-Wasm-ReqBody-Len=%q, want %d", got, len(reqBody))
	}
	// The module fetched the popcache serving config via get_popcache_config. It
	// deliberately does NOT echo the origin (leaking the origin URL in a response
	// header would be bad on a real deployment); instead it emits the http3 port
	// gated on the origin being non-empty, which still proves the host callback
	// round-tripped.
	if got := resp.Header.Get("X-Wasm-Origin"); got != "" {
		t.Fatalf("origin must not leak into the response: X-Wasm-Origin=%q, want empty", got)
	}
	if got := resp.Header.Get("X-Wasm-Http3-Port"); got != "443" {
		t.Fatalf("popcache config port did not round-trip: X-Wasm-Http3-Port=%q, want %q", got, "443")
	}
	t.Logf("edge ABI round-trip OK: applied %d response diff(s), X-Wasm-Processed=%s, X-Wasm-Sni=%s, X-Wasm-Geo=%s",
		applied, resp.Header.Get("X-Wasm-Processed"), resp.Header.Get("X-Wasm-Sni"), resp.Header.Get("X-Wasm-Geo"))
}

// runWasmRequest drives one full request lifecycle through ec (the module is
// registered for "GET /") with the given extra request headers, returning the
// response after the module's ChangeSet has been applied.
func runWasmRequest(t *testing.T, ec EdgeComputer, hdr map[string]string) *http.Response {
	t.Helper()
	ctx := context.Background()
	req := httptest.NewRequest(http.MethodGet, "https://example.test/", nil)
	req.Header.Set("Host", req.Host)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("203.0.113.5"), Port: 8443}))

	reqID, err := ec.StartRequest(ctx, req)
	if err != nil {
		t.Fatalf("StartRequest: %v", err)
	}
	defer ec.FinishRequest(ctx, reqID)
	// popID mirrors production: the popcache server passes its OWN ServerID
	// here, not the module id — fetch must resolve the module from the route
	// match, never from RequestInfo.PopID (regression guard: 42 != module id 1).
	if err := ec.ProcessRequest(ctx, reqID, 42, req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req}
	if err := ec.ProcessResponse(ctx, reqID, resp); err != nil {
		t.Fatalf("ProcessResponse: %v", err)
	}
	if err := ec.ModifyResponse(ctx, reqID, func(diffs []DiffData) error {
		for i := range diffs {
			if err := DefaultModifyResponse(&diffs[i], resp); err != nil {
				return err
			}
		}
		return nil
	}, true); err != nil {
		t.Fatalf("ModifyResponse: %v", err)
	}
	return resp
}

// TestEdgeWasmFetchHostCall exercises the outbound fetch host call end to end:
// the module fetches the URL named in X-Fetch-Url and echoes the outcome in
// X-Wasm-Fetch-Status / X-Wasm-Fetch-Result. Covers the happy path against an
// allowlisted origin, the default-deny allowlist rejection, and the response
// body size cap (a transport error, not a truncation).
func TestEdgeWasmFetchHostCall(t *testing.T) {
	wasmPath := filepath.Join("..", "wasm", "edge_app.wasm")
	binary, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Skipf("edge_app.wasm not built (%v); run edge_app/build.sh first", err)
	}

	const helloBody = "hello from origin"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hello":
			if r.Header.Get("X-From-Wasm") != "1" {
				http.Error(w, "missing X-From-Wasm", http.StatusBadRequest)
				return
			}
			w.Write([]byte(helloBody))
		case "/big":
			w.Write(make([]byte, maxFetchBodyBytes+1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()

	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	geo := fakeGeoLocator{info: GeoInfo{ASN: 64500, ASNOrg: "TestNet", Country: "JP", City: "Tokyo"}}
	ec, err := NewEdgeComputing(rt, 5*time.Second, logger, geo)
	if err != nil {
		t.Fatalf("NewEdgeComputing: %v", err)
	}
	defer ec.Close()

	// Path-scoped entries on the test origin's host: /hello and /big are
	// reachable, everything else on the same host is not. The deny cases below
	// must be rejected by the host call, not by DNS.
	if err := ec.Register(ctx, ModuleSpec{ID: 1, Method: "GET", Path: "/", MatchType: MatchTypeExact, FilePath: "edge_app.wasm",
		AllowedHosts: []string{"127.0.0.1/hello", "127.0.0.1/big"}}, binary); err != nil {
		t.Fatalf("Register: %v", err)
	}

	t.Run("allowed", func(t *testing.T) {
		resp := runWasmRequest(t, ec, map[string]string{"X-Fetch-Url": origin.URL + "/hello"})
		if got := resp.Header.Get("X-Wasm-Fetch-Status"); got != "200" {
			t.Fatalf("fetch status: got %q want 200 (result=%q)", got, resp.Header.Get("X-Wasm-Fetch-Result"))
		}
		if got := resp.Header.Get("X-Wasm-Fetch-Result"); got != strconv.Itoa(len(helloBody)) {
			t.Fatalf("fetch body length: got %q want %d", got, len(helloBody))
		}
	})

	t.Run("denied by allowlist host", func(t *testing.T) {
		resp := runWasmRequest(t, ec, map[string]string{"X-Fetch-Url": "http://blocked.test/x"})
		if got := resp.Header.Get("X-Wasm-Fetch-Status"); got != "0" {
			t.Fatalf("fetch to non-allowlisted host must be rejected: status=%q", got)
		}
		if got := resp.Header.Get("X-Wasm-Fetch-Result"); !strings.Contains(got, "rejected") {
			t.Fatalf("rejection should surface as a host rejection: result=%q", got)
		}
	})

	t.Run("denied by allowlist path", func(t *testing.T) {
		// Same (allowlisted) host, but a path outside every entry's prefix.
		resp := runWasmRequest(t, ec, map[string]string{"X-Fetch-Url": origin.URL + "/other"})
		if got := resp.Header.Get("X-Wasm-Fetch-Status"); got != "0" {
			t.Fatalf("fetch outside the allowed path prefixes must be rejected: status=%q", got)
		}
		if got := resp.Header.Get("X-Wasm-Fetch-Result"); !strings.Contains(got, "rejected") {
			t.Fatalf("rejection should surface as a host rejection: result=%q", got)
		}
	})

	t.Run("slow origin does not burn the compute budget", func(t *testing.T) {
		// A separate edge computer with a compute budget far smaller than the
		// origin's latency: the fetch only succeeds because the compute clock
		// pauses while the module is blocked in the host call.
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			w.Write([]byte("slow ok"))
		}))
		defer slow.Close()
		rt2 := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
		defer rt2.Close(ctx)
		ec2, err := NewEdgeComputing(rt2, 50*time.Millisecond, logger, geo)
		if err != nil {
			t.Fatalf("NewEdgeComputing: %v", err)
		}
		defer ec2.Close()
		if err := ec2.Register(ctx, ModuleSpec{ID: 1, Method: "GET", Path: "/", MatchType: MatchTypeExact, FilePath: "edge_app.wasm", AllowedHosts: []string{"127.0.0.1"}}, binary); err != nil {
			t.Fatalf("Register: %v", err)
		}
		resp := runWasmRequest(t, ec2, map[string]string{"X-Fetch-Url": slow.URL + "/"})
		if got := resp.Header.Get("X-Wasm-Fetch-Status"); got != "200" {
			t.Fatalf("fetch through a slow origin must not eat the compute budget: status=%q result=%q",
				got, resp.Header.Get("X-Wasm-Fetch-Result"))
		}
	})

	t.Run("body over cap", func(t *testing.T) {
		resp := runWasmRequest(t, ec, map[string]string{"X-Fetch-Url": origin.URL + "/big"})
		if got := resp.Header.Get("X-Wasm-Fetch-Status"); got != "0" {
			t.Fatalf("over-cap body must be a transport error: status=%q", got)
		}
		if got := resp.Header.Get("X-Wasm-Fetch-Result"); !strings.Contains(got, "exceeds") {
			t.Fatalf("over-cap error should mention the cap: result=%q", got)
		}
	})
}
