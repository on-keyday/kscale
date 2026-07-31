package logs

import (
	"testing"
	"time"

	pb "github.com/on-keyday/kscale/protobuf/proto"
)

func rec(ts int64) *pb.LogRecord { return &pb.LogRecord{TimeUnixNano: ts} }

// popAll drains everything currently eligible at `now`.
func popAll(m *mergeBuffer, now time.Time) []int64 {
	var out []int64
	for {
		r := m.pop(now)
		if r == nil {
			return out
		}
		out = append(out, r.TimeUnixNano)
	}
}

func eq(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMergeSortsBacklogBursts: the visible symptom — two nodes each replay
// their whole backlog as a burst; arrival order is "all of A, then all of B",
// but the merge must interleave them by timestamp (safe immediately, no
// watermark wait, because both sources have records buffered).
func TestMergeSortsBacklogBursts(t *testing.T) {
	m := newMergeBuffer(2, time.Second)
	now := time.Now()
	for _, ts := range []int64{10, 30, 50} {
		m.push(0, rec(ts), now)
	}
	for _, ts := range []int64{20, 40, 60} {
		m.push(1, rec(ts), now)
	}
	// 60 is source 1's last buffered record: with source 0 empty-but-live it is
	// no longer provably next, so it waits for the watermark.
	if got := popAll(m, now); !eq(got, []int64{10, 20, 30, 40, 50}) {
		t.Fatalf("merged = %v, want 10..50 in order (60 held back)", got)
	}
	if got := popAll(m, now.Add(2*time.Second)); !eq(got, []int64{60}) {
		t.Fatalf("after watermark = %v, want [60]", got)
	}
}

// TestMergeIdleSourceHoldsUntilWatermark: a lone chatty source pays at most the
// watermark — its record is held while the other source is silent, then
// released; a LATE record from the silent source that is older still comes out
// first if it arrives within the watermark.
func TestMergeIdleSourceHoldsUntilWatermark(t *testing.T) {
	m := newMergeBuffer(2, time.Second)
	now := time.Now()
	m.push(0, rec(100), now)
	if r := m.pop(now); r != nil {
		t.Fatalf("emitted %d while the other source was silent and fresh", r.TimeUnixNano)
	}
	// The silent source catches up with an OLDER record: order is preserved.
	// Only 90 comes out — popping it leaves source 1 silent again, so 100 goes
	// back to waiting (source 1 could still deliver e.g. 95).
	m.push(1, rec(90), now.Add(500*time.Millisecond))
	if got := popAll(m, now.Add(500*time.Millisecond)); !eq(got, []int64{90}) {
		t.Fatalf("late-but-older record misordered: %v", got)
	}

	// Nothing else arrives: the watermark (measured from 100's ARRIVAL, not the
	// last pop) releases the held record.
	if got := popAll(m, now.Add(time.Second)); !eq(got, []int64{100}) {
		t.Fatalf("watermark did not release the held record: %v", got)
	}
}

// TestMergeFinishedSourceNeverBlocks: a source whose stream ended (node
// dropped) is excluded from the safety condition — the survivors stream
// without watermark latency — and drained() flips once everything is out.
func TestMergeFinishedSourceNeverBlocks(t *testing.T) {
	m := newMergeBuffer(2, time.Hour) // watermark effectively infinite
	now := time.Now()
	m.finish(1)
	m.push(0, rec(10), now)
	m.push(0, rec(20), now)
	if got := popAll(m, now); !eq(got, []int64{10, 20}) {
		t.Fatalf("finished source blocked the merge: %v", got)
	}
	if m.drained() {
		t.Fatal("drained with source 0 still live")
	}
	m.finish(0)
	if !m.drained() {
		t.Fatal("not drained after all sources finished and emptied")
	}
}
