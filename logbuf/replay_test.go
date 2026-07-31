package logbuf

import (
	"fmt"
	"testing"
)

// TestBufferReplaysBacklog: a subscriber gets the retained history first, then live.
func TestBufferReplaysBacklog(t *testing.T) {
	b := &Buffer{}
	for i := 0; i < 3; i++ {
		b.dispatch(&Record{Message: fmt.Sprintf("m%d", i)})
	}
	ch, cancel := b.Subscribe()
	defer cancel()
	for i := 0; i < 3; i++ {
		select {
		case r := <-ch:
			if r.Message != fmt.Sprintf("m%d", i) {
				t.Fatalf("backlog[%d] = %q", i, r.Message)
			}
		default:
			t.Fatalf("missing backlog record %d", i)
		}
	}
	b.dispatch(&Record{Message: "live"})
	select {
	case r := <-ch:
		if r.Message != "live" {
			t.Fatalf("live = %q", r.Message)
		}
	default:
		t.Fatal("missing live record")
	}
}

// TestBufferRingCap: the ring keeps at most recentLimit (oldest dropped).
func TestBufferRingCap(t *testing.T) {
	b := &Buffer{}
	for i := 0; i < recentLimit+50; i++ {
		b.dispatch(&Record{Message: fmt.Sprintf("m%d", i)})
	}
	ch, cancel := b.Subscribe()
	defer cancel()
	got := 0
	for {
		select {
		case <-ch:
			got++
			continue
		default:
		}
		break
	}
	if got != recentLimit {
		t.Fatalf("backlog = %d, want %d", got, recentLimit)
	}
}
