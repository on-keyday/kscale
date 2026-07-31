package logbuf

import (
	"log/slog"
	"testing"
	"time"
)

// TestBufferTail: min-level + contains filtering over the ring, newest-N cap with
// oldest-first order, and snapshot isolation (no aliasing of the live ring).
func TestBufferTail(t *testing.T) {
	b := &Buffer{}
	for i, r := range []*Record{
		{Time: time.Unix(1, 0), Level: slog.LevelInfo, Message: "served", Attrs: []Attr{{Key: "path", Value: "/a"}}},
		{Time: time.Unix(2, 0), Level: slog.LevelWarn, Message: "tls handshake error"},
		{Time: time.Unix(3, 0), Level: slog.LevelError, Message: "budget exceeded"},
		{Time: time.Unix(4, 0), Level: slog.LevelInfo, Message: "served", Attrs: []Attr{{Key: "path", Value: "/b"}}},
	} {
		_ = i
		b.dispatch(r)
	}
	if got := b.Tail(0, false, "", 0); len(got) != 4 || got[0].Message != "served" || got[3].Attrs[0].Value != "/b" {
		t.Fatalf("unfiltered tail = %+v", got)
	}
	// Min level: warn includes error.
	if got := b.Tail(slog.LevelWarn, true, "", 0); len(got) != 2 || got[0].Message != "tls handshake error" || got[1].Message != "budget exceeded" {
		t.Fatalf("min-level tail = %+v", got)
	}
	// contains matches attrs too.
	if got := b.Tail(0, false, "/a", 0); len(got) != 1 || got[0].Time != time.Unix(1, 0) {
		t.Fatalf("contains tail = %+v", got)
	}
	// n keeps the NEWEST n, oldest first.
	if got := b.Tail(0, false, "", 2); len(got) != 2 || got[0].Message != "budget exceeded" || got[1].Attrs[0].Value != "/b" {
		t.Fatalf("capped tail = %+v", got)
	}
}
