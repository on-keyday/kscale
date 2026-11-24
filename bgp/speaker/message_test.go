package speaker

import (
	"bytes"
	"testing"

	"github.com/on-keyday/kscale/bgp/wire"
)

func TestKeepaliveEncodesHeaderOnly(t *testing.T) {
	b, err := packet(wire.BGPType_Keepalive, 0, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := append(bytes.Repeat([]byte{0xff}, 16), 0x00, 0x13, 0x04)
	if !bytes.Equal(b, want) {
		t.Fatalf("got % x want % x", b, want)
	}
}

func TestMarkerWrittenFromConstant(t *testing.T) {
	p := wire.BGPPacket{Header: wire.BGPHeader{Length: 19, Type: wire.BGPType_Keepalive}}
	b, err := p.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Equal(b[:16], bytes.Repeat([]byte{0xff}, 16)) {
		t.Fatalf("marker not written from the constant: % x", b[:16])
	}
}

func TestBadMarkerRejected(t *testing.T) {
	in := append(append([]byte{0x00}, bytes.Repeat([]byte{0xff}, 15)...), 0x00, 0x13, 0x04)
	var p wire.BGPPacket
	err := p.Read(bytes.NewReader(in))
	t.Logf("bad marker -> err=%v", err)
	if err == nil {
		t.Fatal("bad marker accepted")
	}
}

func TestUnknownTypeRejected(t *testing.T) {
	in := append(bytes.Repeat([]byte{0xff}, 16), 0x00, 0x13, 0x05)
	var p wire.BGPPacket
	err := p.Read(bytes.NewReader(in))
	t.Logf("type 5 -> err=%v", err)
	if err == nil {
		t.Fatal("unknown type accepted")
	}
}
