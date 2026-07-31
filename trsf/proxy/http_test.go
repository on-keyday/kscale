package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/on-keyday/kscale/rpc"
	"github.com/on-keyday/objtrsf/trsf/mock"
)

// TestHTTPOverTransport proves the salvaged transport-proxy machinery end to end
// over a real (in-memory) objtrsf connection: an http.Server served over the
// streamListener on one side, scraped via the DialerWrapper from the other —
// exactly how the popcache agent serves /metrics over the peer connection.
func TestHTTPOverTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, server := mock.SetupClientServer(t)
	mock.BackgroundIO(t, client, server)

	// Server side: serve an http handler over the transport-proxy listener.
	lis := NewTransferProxyListener(ctx)
	go http.Serve(lis, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "metrics-ok path=%s", r.URL.Path)
	}))
	// Route the one accepted stream into the listener (after consuming the magic,
	// like the popcache agent's accept-loop does).
	go func() {
		stream, err := server.AcceptBidirectionalStream(ctx)
		if err != nil {
			return
		}
		magic, err := rpc.DecodeMagic(ctx, stream)
		if err != nil || magic != "HTTP" {
			stream.CloseBoth()
			return
		}
		_ = lis.Send(stream)
	}()

	// Client side: dial /metrics over the transport and do an HTTP GET.
	dialer := NewDialerWrapper(ctx, client)
	conn, err := dialer.DialContext(ctx, "tcp", "metrics")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req, _ := http.NewRequest("GET", "http://node/metrics", nil)
	if err := req.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got, want := string(body), "metrics-ok path=/metrics"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
