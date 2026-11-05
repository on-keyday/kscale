package hpack

import (
	"bytes"
	"testing"

	xhpack "golang.org/x/net/http2/hpack"
)

// Header blocks as a gRPC server sends them on one connection: response
// headers and then trailers for several RPCs. x/net's encoder indexes these
// into its dynamic table, so from the second RPC on the trailers arrive as
// dynamic-table references (with more than one entry in the table). This is how
// containerd's CRI errors reached the workload agent: with the dynamic table
// indexed oldest-first, grpc-status decoded as another entry's value and
// grpc-message as empty.
var interopBlocks = [][]KeyValue{
	{{":status", "200"}, {"content-type", "application/grpc"}},
	{{"grpc-status", "2"}, {"grpc-message", "failed to inject CDI devices: unresolvable CDI devices kscale.test/null=nope"}},
	{{":status", "200"}, {"content-type", "application/grpc"}},
	{{"grpc-status", "2"}, {"grpc-message", "failed to inject CDI devices: unresolvable CDI devices kscale.test/null=nope"}},
	{{"grpc-status", "0"}, {"grpc-message", ""}},
	{{"grpc-status", "2"}, {"grpc-message", "another error"}},
}

func TestDecodeInteropWithXNet(t *testing.T) {
	var buf bytes.Buffer
	enc := xhpack.NewEncoder(&buf)
	table := NewTable(4096)
	for i, block := range interopBlocks {
		buf.Reset()
		for _, kv := range block {
			if err := enc.WriteField(xhpack.HeaderField{Name: kv.Key, Value: kv.Value}); err != nil {
				t.Fatal(err)
			}
		}
		var got []KeyValue
		if err := DecodeFields(table, buf.Bytes(), func(_ FieldType, k, v string) {
			got = append(got, KeyValue{Key: k, Value: v})
		}); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		if len(got) != len(block) {
			t.Fatalf("block %d: got %+v, want %+v", i, got, block)
		}
		for j := range block {
			if got[j] != block[j] {
				t.Errorf("block %d field %d: got %q:%q, want %q:%q", i, j, got[j].Key, got[j].Value, block[j].Key, block[j].Value)
			}
		}
	}
}

func TestEncodeInteropWithXNet(t *testing.T) {
	table := NewTable(4096)
	dec := xhpack.NewDecoder(4096, nil)
	insert := func(KeyValue, bool) FieldType { return FieldType_IndexLiteralInsert }
	for i, block := range interopBlocks {
		var out []byte
		for _, kv := range block {
			var err error
			if out, err = AppendField(out, kv, BestEffort, table, insert); err != nil {
				t.Fatal(err)
			}
		}
		got, err := dec.DecodeFull(out)
		if err != nil {
			t.Fatalf("block %d: x/net decode: %v", i, err)
		}
		if len(got) != len(block) {
			t.Fatalf("block %d: got %+v, want %+v", i, got, block)
		}
		for j := range block {
			if got[j].Name != block[j].Key || got[j].Value != block[j].Value {
				t.Errorf("block %d field %d: got %q:%q, want %q:%q", i, j, got[j].Name, got[j].Value, block[j].Key, block[j].Value)
			}
		}
	}
}
