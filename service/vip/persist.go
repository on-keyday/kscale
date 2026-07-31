package vip

import (
	"encoding/json"
)

// Snapshot/Restore persist the desired VIP set (desiredstate.Store) as a
// map vip -> icmp_echo. Restore also accepts the legacy []string form (VIPs
// with no icmp_echo, defaulting false) so older snapshots still load.
func (s *Store) Snapshot() (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Copy under lock; json.Marshal of a map is key-sorted, so deterministic.
	out := make(map[string]bool, len(s.vips))
	for v, echo := range s.vips {
		out[v] = echo
	}
	return json.Marshal(out)
}

func (s *Store) Restore(raw json.RawMessage) error {
	m := map[string]bool{}
	if err := json.Unmarshal(raw, &m); err != nil {
		// Legacy format: a bare []string of VIPs (icmp_echo defaults false).
		var legacy []string
		if lerr := json.Unmarshal(raw, &legacy); lerr != nil {
			return err // report the map error, the expected current form
		}
		m = make(map[string]bool, len(legacy))
		for _, v := range legacy {
			m[v] = false
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vips = m
	return nil
}
