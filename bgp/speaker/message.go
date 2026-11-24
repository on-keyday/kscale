// Package speaker is a minimal BGP-4 speaker that announces and withdraws
// kscale's own IPv4 prefixes to one peer. Spike: one peer, IPv4 unicast,
// 2-octet AS, routes received from the peer are logged and dropped.
package speaker

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"

	"github.com/on-keyday/kscale/bgp/wire"
)

const headerLen = 19

var marker = [16]uint8{
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
}

// packet wraps a message body into a BGPPacket with the header filled in.
func packet(typ wire.BGPType, bodyLen int, set func(*wire.BGPPacket) bool) ([]byte, error) {
	if headerLen+bodyLen > 4096 {
		return nil, fmt.Errorf("bgp: message of %d bytes exceeds 4096", headerLen+bodyLen)
	}
	p := wire.BGPPacket{Header: wire.BGPHeader{Marker: marker, Length: uint16(headerLen + bodyLen), Type: typ}}
	if set != nil && !set(&p) {
		return nil, fmt.Errorf("bgp: cannot set body for %v", typ)
	}
	return p.Append(nil)
}

func encodeOpen(localAS, hold uint16, routerID netip.Addr) ([]byte, error) {
	if !routerID.Is4() {
		return nil, errors.New("bgp: router id must be IPv4")
	}
	mp := wire.Capability{Code: wire.CapabilityCode_Multiprotocol, Len: 4}
	mp.SetMultiProtocol(wire.MultiProtocolCapability{Afi: uint16(wire.AFI_IPv4), Safi: uint8(wire.SAFI_Unicast)})
	caps := []wire.Capability{mp}
	capsLen := 0
	for i := range caps {
		b, err := caps[i].Append(nil)
		if err != nil {
			return nil, err
		}
		capsLen += len(b)
	}
	opt := wire.BGPOption{Type: wire.BGPOptionType_Capability, Len: uint8(capsLen)}
	opt.SetCapabilities(caps)
	optBytes, err := opt.Append(nil)
	if err != nil {
		return nil, err
	}
	id := routerID.As4()
	o := wire.Open{
		Version: 4,
		As:      localAS,
		Hold:    hold,
		Id:      uint32(id[0])<<24 | uint32(id[1])<<16 | uint32(id[2])<<8 | uint32(id[3]),
		Optlen:  uint8(len(optBytes)),
		Options: []wire.BGPOption{opt},
	}
	body, err := o.Append(nil)
	if err != nil {
		return nil, err
	}
	return packet(wire.BGPType_Open, len(body), func(p *wire.BGPPacket) bool { return p.SetOpen(o) })
}

func encodeKeepalive() ([]byte, error) {
	return packet(wire.BGPType_Keepalive, 0, nil)
}

func encodeNotification(code, subcode uint8, data []byte) ([]byte, error) {
	n := wire.Notification{Code: code, Subcode: subcode, Data: data}
	return packet(wire.BGPType_Notification, 2+len(data), func(p *wire.BGPPacket) bool { return p.SetNotification(n) })
}

func toPrefix(p netip.Prefix) (wire.Prefix, error) {
	if !p.Addr().Is4() {
		return wire.Prefix{}, fmt.Errorf("bgp: %v is not IPv4", p)
	}
	p = p.Masked()
	a := p.Addr().As4()
	n := (p.Bits() + 7) / 8
	return wire.Prefix{PrefixLen: uint8(p.Bits()), Prefix: append([]uint8(nil), a[:n]...)}, nil
}

func fromPrefix(w wire.Prefix) (netip.Prefix, error) {
	if w.PrefixLen > 32 {
		return netip.Prefix{}, fmt.Errorf("bgp: IPv4 prefix length %d", w.PrefixLen)
	}
	var a [4]byte
	copy(a[:], w.Prefix)
	return netip.AddrFrom4(a).Prefix(int(w.PrefixLen))
}

func prefixesLen(ps []wire.Prefix) int {
	n := 0
	for _, p := range ps {
		n += 1 + len(p.Prefix)
	}
	return n
}

// attr builds a path attribute whose payload is encoded by enc (to measure it)
// and stored by set.
func attr(code wire.AttributeCode, optional, transitive bool, payloadLen int, set func(*wire.PathAttribute) bool) (wire.PathAttribute, error) {
	var a wire.PathAttribute
	a.Type.Code = code
	a.Type.SetOptional(optional)
	a.Type.SetTransitive(transitive)
	a.Type.SetExtended(payloadLen > 255)
	a.SetLen(uint16(payloadLen))
	if set != nil && !set(&a) {
		return a, fmt.Errorf("bgp: cannot set attribute %v", code)
	}
	return a, nil
}

// updateAttrs are the attributes for routes kscale originates.
type updateAttrs struct {
	localAS uint16
	ibgp    bool
	nextHop netip.Addr
}

func (u updateAttrs) build() ([]wire.PathAttribute, error) {
	if !u.nextHop.Is4() {
		return nil, errors.New("bgp: next hop must be IPv4")
	}
	origin, err := attr(wire.AttributeCode_ORIGIN, false, true, 1, func(a *wire.PathAttribute) bool {
		return a.SetOrigin(wire.Origin_IGP)
	})
	if err != nil {
		return nil, err
	}
	// eBGP: prepend our AS. iBGP: an empty AS_PATH (RFC 4271 5.1.2).
	var path wire.AsPath
	if !u.ibgp {
		var seg wire.AsPathSegment
		seg.Type = wire.AsPathSegmentType_AS_SEQUENCE
		seg.SetData([]uint16{u.localAS})
		path.Segments = []wire.AsPathSegment{seg}
	}
	pathBytes, err := path.Append(nil)
	if err != nil {
		return nil, err
	}
	asPath, err := attr(wire.AttributeCode_AS_PATH, false, true, len(pathBytes), func(a *wire.PathAttribute) bool {
		return a.SetAsPath(path)
	})
	if err != nil {
		return nil, err
	}
	nh, err := attr(wire.AttributeCode_NEXT_HOP, false, true, 4, func(a *wire.PathAttribute) bool {
		return a.SetNextHop(u.nextHop.As4())
	})
	if err != nil {
		return nil, err
	}
	attrs := []wire.PathAttribute{origin, asPath, nh}
	if u.ibgp {
		lp, err := attr(wire.AttributeCode_LOCAL_PREF, false, true, 4, func(a *wire.PathAttribute) bool {
			return a.SetLocalPref(100)
		})
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, lp)
	}
	return attrs, nil
}

// encodeUpdate builds one UPDATE that withdraws `withdraw` and announces
// `announce` with attrs. Either may be empty; attrs are omitted when nothing is
// announced (RFC 4271 4.3).
func encodeUpdate(withdraw, announce []netip.Prefix, attrs updateAttrs) ([]byte, error) {
	var u wire.Update
	for _, p := range withdraw {
		w, err := toPrefix(p)
		if err != nil {
			return nil, err
		}
		u.WithdrawnRoutes.Data = append(u.WithdrawnRoutes.Data, w)
	}
	u.WithdrawnRoutes.Len = uint16(prefixesLen(u.WithdrawnRoutes.Data))
	if len(announce) > 0 {
		as, err := attrs.build()
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		for i := range as {
			if err := as[i].Write(&buf); err != nil {
				return nil, err
			}
		}
		u.PathAttr = wire.PathAttributes{Len: uint16(buf.Len()), Data: as}
		for _, p := range announce {
			w, err := toPrefix(p)
			if err != nil {
				return nil, err
			}
			u.Nlri = append(u.Nlri, w)
		}
	}
	body, err := u.Append(nil)
	if err != nil {
		return nil, err
	}
	return packet(wire.BGPType_Update, len(body), func(p *wire.BGPPacket) bool { return p.SetUpdate(u) })
}
