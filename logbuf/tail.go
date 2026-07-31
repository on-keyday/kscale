package logbuf

import (
	"log/slog"
	"strings"
)

// Tail returns the newest matching records from the retained ring, oldest first
// (at most n; n <= 0 or > recentLimit means the whole ring). minLevel filters to
// records at or above it when hasLevel is set; contains filters on the message and
// the rendered attrs. The returned slice is a copy — safe to use after unlock.
func (b *Buffer) Tail(minLevel slog.Level, hasLevel bool, contains string, n int) []*Record {
	b.mu.Lock()
	snapshot := append([]*Record(nil), b.recent...)
	b.mu.Unlock()

	matched := snapshot[:0]
	for _, r := range snapshot {
		if hasLevel && r.Level < minLevel {
			continue
		}
		if contains != "" && !recordContains(r, contains) {
			continue
		}
		matched = append(matched, r)
	}
	if n > 0 && len(matched) > n {
		matched = matched[len(matched)-n:]
	}
	return matched
}

// recordContains reports whether the substring appears in the record's message or
// any attr key/value.
func recordContains(r *Record, sub string) bool {
	if strings.Contains(r.Message, sub) {
		return true
	}
	for _, a := range r.Attrs {
		if strings.Contains(a.Key, sub) || strings.Contains(a.Value, sub) {
			return true
		}
	}
	return false
}
