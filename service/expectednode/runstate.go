// runstate.go — the hand-written desired-run surface of the generated
// expected_node store. desired_run/run_generation are readonly schema fields
// (excluded from apply, preserved across inventory re-applies); the ONLY writer
// is SetDesiredRun, called by dataplane_node start/stop — which are sugar over
// this desired state, converged by reconcile/node_run.go.
package expectednode

import (
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

// SetDesiredRun records the declared run-state ("running"/"stopped") for each
// node in nodes (CommonName -> dp_type), creating the inventory entry when
// absent (dp_type is only used then — an existing entry keeps its declared
// type). Every call bumps run_generation even when desired_run is unchanged:
// the bump is the operator's explicit "try again" signal, clearing a reconcile
// hold. One notify() fires for the whole batch.
func (h *Handlers) SetDesiredRun(nodes map[string]string, run string) {
	if len(nodes) == 0 {
		return
	}
	h.mu.Lock()
	for cn, dpType := range nodes {
		obj := &pbaccess.ResourceExpectedNodeActionGetResponseDTO{
			CommonName: cn,
			DpType:     dpType,
		}
		if prev := h.store[cn]; prev != nil {
			// Copy-on-write: readers (Get/List/Desired) hold pointers into the
			// store, so the existing DTO is never mutated in place.
			*obj = *prev
		}
		obj.DesiredRun = run
		obj.RunGeneration++
		h.store[cn] = obj
	}
	h.mu.Unlock()
	h.notify()
}
