package hpack

import (
	"encoding/hex"
	"testing"
)

// TestDynamicTableSizeUpdatePrefix reproduces the intermittent TestStd failure
// ("unknown field type"): the Go net/http client sometimes prefixes its request
// header block with a Dynamic Table Size Update instruction (001xxxxx). This is
// the exact 36-byte header block captured from a failing run — a size update to
// 4096 followed by :authority/:method/:path/:scheme/accept-encoding/user-agent.
func TestDynamicTableSizeUpdatePrefix(t *testing.T) {
	block, err := hex.DecodeString(
		"3fe11f41882f91d35d055c87a782848650839bd9ab7a8dc475a74a6b589418b525812e0f")
	if err != nil {
		t.Fatal(err)
	}
	table := NewTable(4096)
	var got []KeyValue
	derr := DecodeFields(table, block, func(_ FieldType, key, value string) {
		got = append(got, KeyValue{Key: key, Value: value})
	})
	if derr != nil {
		t.Fatalf("DecodeFields: %v", derr)
	}
	want := []KeyValue{
		{":authority", "example.com"},
		{":method", "GET"},
		{":path", "/"},
		{":scheme", "http"},
		{"accept-encoding", "gzip"},
		{"user-agent", "Go-http-client/2.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d fields, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Key != want[i].Key || got[i].Value != want[i].Value {
			t.Errorf("field %d = %q:%q, want %q:%q", i, got[i].Key, got[i].Value, want[i].Key, want[i].Value)
		}
	}
}
