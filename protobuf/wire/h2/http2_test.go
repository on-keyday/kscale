package h2

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/on-keyday/kscale/util"
)

type PipeNetConn struct {
	r          *util.AsyncPipe
	w          *util.AsyncPipe
	port       uint16
	remotePort uint16
}

func (p *PipeNetConn) Read(data []byte) (n int, err error) {
	return p.r.Read(data)
}

func (p *PipeNetConn) Write(data []byte) (n int, err error) {
	return p.w.Write(data)
}

func (p *PipeNetConn) Close() error {
	err1 := p.r.Close()
	err2 := p.w.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

func (p *PipeNetConn) LocalAddr() net.Addr {
	return &net.TCPAddr{
		IP:   net.IPv4(127, 0, 0, 1),
		Port: int(p.port),
	}
}

func (p *PipeNetConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{
		IP:   net.IPv4(127, 0, 0, 1),
		Port: int(p.remotePort),
	}
}

func (p *PipeNetConn) SetDeadline(t time.Time) error {
	return nil
}

func (p *PipeNetConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (p *PipeNetConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func NewPipeReadWriteCloser() (*PipeNetConn, *PipeNetConn) {
	peer1, peer2 := util.NewAsyncPipe(), util.NewAsyncPipe()
	return &PipeNetConn{r: peer1, w: peer2, port: 10000, remotePort: 10001}, &PipeNetConn{r: peer2, w: peer1, port: 10001, remotePort: 10000}
}

func TestStd(t *testing.T) {
	pipe1, pipe2 := NewPipeReadWriteCloser()
	proto := &http.Protocols{}
	proto.SetHTTP1(false)
	proto.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return pipe1, nil
		},
		ForceAttemptHTTP2: true,
		Protocols:         proto,
	}

	client := &http.Client{Transport: transport}

	// HTTPリクエストを送信
	go func() {
		resp, err := client.Get("http://example.com")
		if err != nil {
			t.Errorf("HTTP request failed: %v", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status code 200, got %d", resp.StatusCode)
		}
		for key, values := range resp.Header {
			for _, value := range values {
				t.Logf("Header: %s: %s", key, value)
			}
		}
		client.CloseIdleConnections()
	}()

	h2ServerConn := NewHTTP2Conn(false)
	if err := h2ServerConn.Start(); err != nil {
		t.Fatalf("Failed to start HTTP/2 connection: %v", err)
	}

	data, err := h2ServerConn.EncodeNonBlock()
	if err != nil {
		t.Fatalf("Failed to encode HTTP/2 frame: %v", err)
	}
	// send
	_, err = pipe2.Write(data)
	if err != nil {
		t.Errorf("Failed to write to pipe: %v", err)
		return
	}

	for {
		// サーバー側でHTTPリクエストを受け取る
		buf := make([]byte, 1024)
		n, err := pipe2.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Errorf("Failed to read from pipe: %v", err)
			return
		}

		err = h2ServerConn.Decode(buf[:n])
		if err != nil {
			t.Fatalf("Failed to decode HTTP/2 frame: %v", err)
		}

		accept, err := h2ServerConn.AcceptStream(false)
		if err != nil {
			t.Fatalf("Failed to accept stream: %v", err)
		}
		if accept != nil {
			peerHeader := accept.PeerHeader()
			if peerHeader.Get(PseudoHeader_Authority.String()) != "example.com" {
				t.Errorf("Expected authority 'example.com', got '%s'", peerHeader.Get(PseudoHeader_Authority.String()))
			}
			accept.WriteHeader(http.Header{
				PseudoHeader_Status.String(): []string{"200"},
				"content-type":               []string{"text/plain"},
			})
			accept.WriteNonBlocking([]byte("Hello, HTTP/2!"))
		}

		data, err = h2ServerConn.EncodeNonBlock()
		if err != nil {
			t.Fatalf("Failed to encode HTTP/2 frame: %v", err)
		}
		if len(data) == 0 {
			continue
		}

		// send
		_, err = pipe2.Write(data)
		if err != nil {
			t.Errorf("Failed to write to pipe: %v", err)
			return
		}
	}
}

func BenchmarkHTTP2Conn(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h2ServerConn := NewHTTP2Conn(false)
		h2ClientConn := NewHTTP2Conn(true)
		if err := h2ServerConn.Start(); err != nil {
			b.Fatalf("Failed to start HTTP/2 connection: %v", err)
		}
		if err := h2ClientConn.Start(); err != nil {
			b.Fatalf("Failed to start HTTP/2 connection: %v", err)
		}

		preface, err := h2ClientConn.EncodeNonBlock()
		if err != nil {
			b.Fatalf("Failed to encode HTTP/2 frame: %v", err)
		}
		preface2, err := h2ServerConn.EncodeNonBlock()
		if err != nil {
			b.Fatalf("Failed to encode HTTP/2 frame: %v", err)
		}
		err = h2ServerConn.Decode(preface)
		if err != nil {
			b.Fatalf("Failed to decode HTTP/2 frame: %v", err)
		}
		err = h2ClientConn.Decode(preface2)
		if err != nil {
			b.Fatalf("Failed to decode HTTP/2 frame: %v", err)
		}
	}
}
