package reconcile

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status records the outcome of the LAST reconcile push of each declarative resource to
// each node. The reconcile loops (and the synchronous apply path) write to it; the `node`
// resource surfaces it (get/list), apply reads it back for immediate feedback, and
// `node start` gates on it. Without this, a failed push was only ever logged — invisible
// to the operator, who'd then start a misconfigured node none the wiser.
type Status struct {
	mu sync.Mutex
	m  map[statusKey]Entry
}

type statusKey struct{ resource, node string }

// Entry is the last push outcome for one (resource, node).
type Entry struct {
	OK      bool
	Message string // the error, when !OK
	Time    time.Time
}

func NewStatus() *Status { return &Status{m: map[statusKey]Entry{}} }

// Record stores the outcome of pushing resource's config to node (err == nil means OK).
// now is passed in so callers can stamp a real time (the generated reconcile loops pass
// time.Now()).
func (s *Status) Record(resource, node string, now time.Time, err error) {
	e := Entry{OK: err == nil, Time: now}
	if err != nil {
		e.Message = err.Error()
	}
	s.mu.Lock()
	s.m[statusKey{resource, node}] = e
	s.mu.Unlock()
}

// ClearNode drops all status entries for a node — called when it disconnects, so its
// last (now stale) reconcile results don't linger in ResourceError / ErrorsForNode.
func (s *Status) ClearNode(node string) {
	s.mu.Lock()
	for k := range s.m {
		if k.node == node {
			delete(s.m, k)
		}
	}
	s.mu.Unlock()
}

// ErrorsForNode returns "<resource>: <message>" for every resource currently in error on
// node (the last push failed), sorted. Empty when the node is clean.
func (s *Status) ErrorsForNode(node string) []string {
	return s.ErrorsForNodeExcept(node, "")
}

// ErrorsForNodeExcept is ErrorsForNode minus one resource's own entries — for a
// consumer that both writes status and gates on the OTHERS' status (node_run
// refuses to start a node whose config failed to reconcile, but must not block
// on its own previous start failure, which its backoff already handles).
func (s *Status) ErrorsForNodeExcept(node, except string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, e := range s.m {
		if k.node == node && !e.OK && k.resource != except {
			out = append(out, k.resource+": "+e.Message)
		}
	}
	sort.Strings(out)
	return out
}

// NodeError returns one (resource, node)'s current error, nil when its last push
// succeeded or was never attempted — per-node synchronous feedback for an
// operation that just changed that node's desired state.
func (s *Status) NodeError(resource, node string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[statusKey{resource, node}]; ok && !e.OK {
		return fmt.Errorf("%s", e.Message)
	}
	return nil
}

// ResourceError combines the current per-node failures of one resource into a single
// error (nil when every node's last push of it succeeded), for synchronous apply feedback.
func (s *Status) ResourceError(resource string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []string
	for k, e := range s.m {
		if k.resource == resource && !e.OK {
			parts = append(parts, k.node+": "+e.Message)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	sort.Strings(parts)
	return fmt.Errorf("push failed on %d node(s): %s", len(parts), strings.Join(parts, "; "))
}
