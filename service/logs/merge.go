// merge.go — the timestamp-ordered fan-in behind the multi-node logs stream.
// Each source (one node's StreamLogs) is internally time-ordered: a single
// process's slog records, ring-buffer backlog replayed first, then live. The
// multi-node merge therefore reduces to a k-way merge on the per-source heads —
// EXCEPT that a source with nothing buffered might still deliver an older
// record later (its backlog burst in flight, or it is just quiet), so the
// global minimum is only provably next once every live source has shown a
// record at or past it. A watermark bounds how long an idle source may hold
// everyone else back: a record buffered longer than the watermark is emitted
// regardless. Result: output is timestamp-ordered whenever sources are within
// a watermark of each other (backlog bursts sort fully), and a lone chatty
// source pays at most the watermark in latency. Ordering fidelity is bounded
// by the nodes' clocks — records carry their source's timestamps, so cross-node
// order is only as true as NTP keeps them.
package logs

import (
	"time"

	pb "github.com/on-keyday/kscale/protobuf/proto"
)

// streamWatermark is the longest a buffered record waits for an idle source to
// prove it has nothing older. The emit loop checks eligibility on a coarse
// ticker, so effective latency is watermark + one tick.
const streamWatermark = time.Second

type mergeSrc struct {
	queue    []*pb.LogRecord
	arrivals []time.Time // when each queued record arrived (watermark clock)
	done     bool        // stream ended: never blocks others again
}

// mergeBuffer holds the per-source queues and the emission policy. Not
// goroutine-safe — driven from the single merge loop.
type mergeBuffer struct {
	watermark time.Duration
	srcs      []*mergeSrc
}

func newMergeBuffer(n int, watermark time.Duration) *mergeBuffer {
	srcs := make([]*mergeSrc, n)
	for i := range srcs {
		srcs[i] = &mergeSrc{}
	}
	return &mergeBuffer{watermark: watermark, srcs: srcs}
}

func (m *mergeBuffer) push(id int, rec *pb.LogRecord, now time.Time) {
	s := m.srcs[id]
	s.queue = append(s.queue, rec)
	s.arrivals = append(s.arrivals, now)
}

func (m *mergeBuffer) finish(id int) { m.srcs[id].done = true }

// drained reports whether nothing more can ever be emitted (every source ended
// and every queue empty).
func (m *mergeBuffer) drained() bool {
	for _, s := range m.srcs {
		if !s.done || len(s.queue) > 0 {
			return false
		}
	}
	return true
}

// pop returns the next record to emit — the globally-smallest buffered
// timestamp, once it is either safe (every live source has something buffered,
// so nothing older can appear) or has waited out the watermark. nil when
// nothing is eligible yet.
func (m *mergeBuffer) pop(now time.Time) *pb.LogRecord {
	min := -1
	for i, s := range m.srcs {
		if len(s.queue) == 0 {
			continue
		}
		if min < 0 || s.queue[0].TimeUnixNano < m.srcs[min].queue[0].TimeUnixNano {
			min = i
		}
	}
	if min < 0 {
		return nil
	}
	s := m.srcs[min]
	safe := true
	for i, o := range m.srcs {
		if i == min || o.done || len(o.queue) > 0 {
			continue // a non-empty source's head is ≥ ours (we are the minimum)
		}
		safe = false // live but silent: could still deliver something older
		break
	}
	if !safe && now.Sub(s.arrivals[0]) < m.watermark {
		return nil
	}
	rec := s.queue[0]
	s.queue = s.queue[1:]
	s.arrivals = s.arrivals[1:]
	return rec
}
