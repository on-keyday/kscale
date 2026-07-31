package iface

import "encoding/json"

// Snapshot/Restore persist the per-node desired interface map (desiredstate.Store).
func (s *Store) Snapshot() (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Marshal(s.byNode)
}

func (s *Store) Restore(raw json.RawMessage) error {
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if m == nil {
		m = map[string]string{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byNode = m
	return nil
}
