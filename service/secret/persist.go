package secret

import "encoding/json"

// persistedSecret is the on-disk form of an entry: the unexported value/length need
// exported fields to marshal. The desiredstate file is encrypted, so the generated
// secret value is safe to persist here (and MUST be — re-applying would mint a new
// value rather than restore the one the dataplane already trusts).
//
// Value is []byte (base64 in JSON) because the secret is raw HKDF output, not UTF-8:
// persisting it as a JSON string let json.Marshal replace invalid sequences with
// U+FFFD, silently changing the key's length across a control-plane restart — the
// popcache "new shared key must be 16 bytes long" reconcile failure.
type persistedSecret struct {
	Value  []byte `json:"value"`
	Length uint32 `json:"length"`
}

// UnmarshalJSON accepts both the base64 []byte form and the legacy raw-string form.
// A legacy value is kept verbatim so Restore never fails the whole desired-state
// load, but it is almost certainly already U+FFFD-mangled — the operator re-applies
// the secret to mint a clean one.
func (p *persistedSecret) UnmarshalJSON(data []byte) error {
	var v struct {
		Value  json.RawMessage `json:"value"`
		Length uint32          `json:"length"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	p.Length = v.Length
	var b []byte
	if err := json.Unmarshal(v.Value, &b); err == nil {
		p.Value = b
		return nil
	}
	var s string
	if err := json.Unmarshal(v.Value, &s); err != nil {
		return err
	}
	p.Value = []byte(s)
	return nil
}

// Snapshot/Restore persist the generated secrets (desiredstate.Store).
func (s *Store) Snapshot() (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]persistedSecret, len(s.byName))
	for k, e := range s.byName {
		m[k] = persistedSecret{Value: []byte(e.value), Length: e.length}
	}
	return json.Marshal(m)
}

func (s *Store) Restore(raw json.RawMessage) error {
	var m map[string]persistedSecret
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byName = make(map[string]entry, len(m))
	for k, p := range m {
		s.byName[k] = entry{value: string(p.Value), length: p.Length}
	}
	return nil
}
