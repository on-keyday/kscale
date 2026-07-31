package logbuf

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestCaptureStructured verifies a subscriber receives the structured record —
// message plus attrs from both With(...) and the call site, with group keys dotted
// — while the inner handler still runs.
func TestCaptureStructured(t *testing.T) {
	buf := &Buffer{}
	logger := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), buf))

	ch, cancel := buf.Subscribe()
	defer cancel()

	logger.With("agent", "l4lb").WithGroup("net").Info("vip applied", "vip", "192.0.2.1", "count", 3)

	select {
	case rec := <-ch:
		if rec.Message != "vip applied" {
			t.Fatalf("message = %q", rec.Message)
		}
		if rec.Level != slog.LevelInfo {
			t.Fatalf("level = %v", rec.Level)
		}
		got := map[string]string{}
		for _, a := range rec.Attrs {
			got[a.Key] = a.Value
		}
		// "agent" was added via With before the group; call-site attrs are inside "net".
		if got["agent"] != "l4lb" {
			t.Errorf("agent attr = %q, want l4lb (attrs=%v)", got["agent"], rec.Attrs)
		}
		if got["net.vip"] != "192.0.2.1" {
			t.Errorf("net.vip attr = %q, want 192.0.2.1 (attrs=%v)", got["net.vip"], rec.Attrs)
		}
		if got["net.count"] != "3" {
			t.Errorf("net.count attr = %q, want 3 (attrs=%v)", got["net.count"], rec.Attrs)
		}
	case <-time.After(time.Second):
		t.Fatal("no record received")
	}
}

// TestUnsubscribe verifies unsubscribe closes the channel and drops the subscriber.
func TestUnsubscribe(t *testing.T) {
	buf := &Buffer{}
	ch, cancel := buf.Subscribe()
	if n := buf.SubscriberCount(); n != 1 {
		t.Fatalf("subscriber count = %d, want 1", n)
	}
	cancel()
	if n := buf.SubscriberCount(); n != 0 {
		t.Fatalf("subscriber count after cancel = %d, want 0", n)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
}
