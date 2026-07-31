// Package logbuf is the kscale-native structured log fan-out — the clean re-home
// of ksdk's util.GlobalBuffer, but capturing slog.Records (structured) instead of
// pre-formatted text bytes. A Handler tees every record to its inner handler (so
// stderr logging is unchanged) AND dispatches a structured copy to any subscribers;
// the dataplane substrate's StreamLogs RPC subscribes and ships those records
// northbound with their fields intact.
package logbuf

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// LevelVar is the process-wide dynamic log level. The stderr handler built by
// NewStderrLogger honors it (slog.LevelVar is a Leveler), so SetLevel changes verbosity
// LIVE — both the stderr output and the streamable buffer capture, since the tee
// Handler's Enabled delegates to the inner handler that reads this var. One per process.
var LevelVar = new(slog.LevelVar)

// SetLevel changes the live log level; GetLevel reads it. ParseLevel maps the usual
// names (debug/info/warn/error, case-insensitive) for a control surface.
func SetLevel(l slog.Level) { LevelVar.Set(l) }
func GetLevel() slog.Level  { return LevelVar.Level() }

// ParseLevel maps a name to a slog.Level (debug/info/warn/error, case-insensitive).
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	var z slog.Level
	return z, &parseLevelError{s}
}

type parseLevelError struct{ s string }

func (e *parseLevelError) Error() string {
	return "invalid log level " + e.s + " (want debug/info/warn/error)"
}

// Attr is one structured attribute (group attributes are flattened to dotted keys).
type Attr struct {
	Key   string
	Value string
}

// Record is a captured structured log entry.
type Record struct {
	Time    time.Time
	Level   slog.Level
	Message string
	Attrs   []Attr
}

// subBuffer is how many records a slow subscriber may fall behind before records
// are dropped for it (logging must never block on a subscriber).
const subBuffer = 256

// recentLimit is how many of the most-recent records the buffer retains and replays to
// a new subscriber, so a freshly-opened log stream shows recent history before live
// output (mirroring ksdk's util.LogBuffer ring, which the first kscale rewrite dropped).
// Kept below subBuffer so the whole backlog fits a new subscriber's channel without
// blocking.
const recentLimit = 200

// Buffer fans structured log records out to live subscribers AND retains the last
// recentLimit records, which it replays to each new subscriber. The zero value is ready
// to use; Default is the process-global instance the substrate captures into.
type Buffer struct {
	mu     sync.Mutex
	subs   map[*subscription]struct{}
	recent []*Record // ring (cap recentLimit) replayed on Subscribe
}

type subscription struct {
	ch chan *Record
}

// Default is the process-global log buffer (logs are inherently process-wide,
// like ksdk's GlobalBuffer).
var Default = &Buffer{}

// Subscribe registers a live subscriber, returning its channel and an unsubscribe
// func. The channel is closed by unsubscribe; records are dropped (never blocked)
// if the subscriber falls more than subBuffer behind.
func (b *Buffer) Subscribe() (<-chan *Record, func()) {
	s := &subscription{ch: make(chan *Record, subBuffer)}
	b.mu.Lock()
	// Replay the retained backlog first (recent history), THEN register for live records
	// — all under the lock, so a record arriving mid-Subscribe is either in the backlog
	// or delivered live, never both and never out of order.
	for _, r := range b.recent {
		select {
		case s.ch <- r:
		default:
		}
	}
	if b.subs == nil {
		b.subs = make(map[*subscription]struct{})
	}
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, s)
			b.mu.Unlock()
			close(s.ch)
		})
	}
}

// SubscriberCount reports the number of live subscribers.
func (b *Buffer) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// dispatch delivers a record to every subscriber without blocking (a full
// subscriber drops the record rather than stalling the logging goroutine).
func (b *Buffer) dispatch(r *Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Retain in the ring (drop the oldest past recentLimit) for replay to new subscribers.
	if len(b.recent) >= recentLimit {
		b.recent = append(b.recent[:0], b.recent[1:]...)
	}
	b.recent = append(b.recent, r)
	for s := range b.subs {
		select {
		case s.ch <- r:
		default:
		}
	}
}

// Handler is a slog.Handler that tees to an inner handler (e.g. a stderr
// TextHandler) and captures a structured copy into a Buffer.
type Handler struct {
	inner  slog.Handler
	buf    *Buffer
	groups []string
	attrs  []Attr
}

var _ slog.Handler = (*Handler)(nil)

// NewHandler wraps inner so records are both written to it and captured into buf.
func NewHandler(inner slog.Handler, buf *Buffer) *Handler {
	return &Handler{inner: inner, buf: buf}
}

func (h *Handler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(ctx, lvl)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	rec := &Record{Time: r.Time, Level: r.Level, Message: r.Message}
	rec.Attrs = append(rec.Attrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&rec.Attrs, h.groups, a)
		return true
	})
	h.buf.dispatch(rec)
	return h.inner.Handle(ctx, r)
}

func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	nh := h.clone()
	nh.inner = h.inner.WithAttrs(as)
	for _, a := range as {
		appendAttr(&nh.attrs, h.groups, a)
	}
	return nh
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := h.clone()
	nh.inner = h.inner.WithGroup(name)
	nh.groups = append(nh.groups, name)
	return nh
}

func (h *Handler) clone() *Handler {
	return &Handler{
		inner:  h.inner,
		buf:    h.buf,
		groups: append([]string(nil), h.groups...),
		attrs:  append([]Attr(nil), h.attrs...),
	}
}

// appendAttr flattens one attribute (recursing into groups with dotted keys) and
// renders its value to slog's canonical string form.
func appendAttr(dst *[]Attr, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		sub := a.Value.Group()
		if len(sub) == 0 {
			return
		}
		ng := groups
		if a.Key != "" {
			ng = append(append([]string(nil), groups...), a.Key)
		}
		for _, ga := range sub {
			appendAttr(dst, ng, ga)
		}
		return
	}
	key := a.Key
	if len(groups) > 0 {
		key = strings.Join(groups, ".") + "." + a.Key
	}
	*dst = append(*dst, Attr{Key: key, Value: a.Value.String()})
}

// NewLogger builds a slog.Logger whose records go to a stderr text handler and are
// also captured into Default — the standard logger for the dataplane agents and
// the control plane so their logs are streamable.
func NewLogger(stderrHandler slog.Handler) *slog.Logger {
	return slog.New(NewHandler(stderrHandler, Default))
}

// NewStderrLogger is the standard constructor: a stderr TextHandler gated by the shared
// dynamic LevelVar (initialized to init), teed into the streamable buffer. Use this so
// the process's level can be switched live via SetLevel (e.g. the logs set-level RPC).
//
// It also installs the logger as slog's package default, so the many package-level
// slog.Info/Warn/Error/Debug calls (l4lb driver, ca, popcache, geoloc, ...) ALSO flow
// through logbuf — captured for `logs stream` and gated by the LevelVar — instead of
// going to the stock stderr default that the dynamic level couldn't reach. One logger
// per process, so the global SetDefault is unambiguous.
func NewStderrLogger(w io.Writer, init slog.Level) *slog.Logger {
	LevelVar.Set(init)
	l := NewLogger(slog.NewTextHandler(w, &slog.HandlerOptions{Level: LevelVar}))
	slog.SetDefault(l)
	return l
}
