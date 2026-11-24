package speaker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/on-keyday/kscale/bgp/wire"
)

type Config struct {
	Peer     string // host:port of the peer; the speaker always dials
	LocalAS  uint16
	PeerAS   uint16
	RouterID netip.Addr
	NextHop  netip.Addr // next hop put on every announced prefix
	HoldTime uint16     // proposed hold time in seconds; 0 means 90
	Logger   *slog.Logger
}

// Speaker keeps one session to Config.Peer and makes the peer's view of our
// announcements match the set given through Announce and Withdraw.
type Speaker struct {
	cfg     Config
	log     *slog.Logger
	mu      sync.Mutex
	want    map[netip.Prefix]struct{}
	changed chan struct{}
}

func New(cfg Config) *Speaker {
	if cfg.HoldTime == 0 {
		cfg.HoldTime = 90
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Speaker{cfg: cfg, log: log, want: map[netip.Prefix]struct{}{}, changed: make(chan struct{}, 1)}
}

func (s *Speaker) Announce(p netip.Prefix) { s.set(p.Masked(), true) }
func (s *Speaker) Withdraw(p netip.Prefix) { s.set(p.Masked(), false) }

func (s *Speaker) set(p netip.Prefix, on bool) {
	s.mu.Lock()
	if on {
		s.want[p] = struct{}{}
	} else {
		delete(s.want, p)
	}
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *Speaker) wanted() map[netip.Prefix]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[netip.Prefix]struct{}, len(s.want))
	for p := range s.want {
		m[p] = struct{}{}
	}
	return m
}

// Run keeps a session up until ctx is done, reconnecting with backoff.
func (s *Speaker) Run(ctx context.Context) error {
	backoff := 5 * time.Second
	for {
		err := s.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.log.Warn("bgp session ended", "peer", s.cfg.Peer, "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

// NotificationError is a NOTIFICATION, sent by us or received from the peer.
type NotificationError struct {
	Code, Subcode uint8
	Data          []byte
	FromPeer      bool
}

func (e *NotificationError) Error() string {
	who := "sent"
	if e.FromPeer {
		who = "received"
	}
	return fmt.Sprintf("bgp notification %s: code %d subcode %d data %x", who, e.Code, e.Subcode, e.Data)
}

type readResult struct {
	pkt wire.BGPPacket
	err error
}

// readPacket reads one message. The generated reader leaves the body of an
// unknown message type unread, so the type is checked here and the session
// ends on one (the stream would be out of sync).
func readPacket(r io.Reader) (wire.BGPPacket, error) {
	var p wire.BGPPacket
	if err := p.Read(r); err != nil {
		return p, err
	}
	if p.Header.Marker != marker {
		return p, &NotificationError{Code: 1, Subcode: 1} // connection not synchronized
	}
	switch p.Header.Type {
	case wire.BGPType_Open, wire.BGPType_Update, wire.BGPType_Notification, wire.BGPType_Keepalive:
		return p, nil
	}
	return p, &NotificationError{Code: 1, Subcode: 3, Data: []byte{uint8(p.Header.Type)}} // bad message type
}

type conn struct {
	c   net.Conn
	log *slog.Logger
}

func (c *conn) send(b []byte, err error) error {
	if err != nil {
		return err
	}
	c.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = c.c.Write(b)
	return err
}

// notify sends a NOTIFICATION for err when err is one we generated, then
// returns err.
func (c *conn) notify(err error) error {
	var ne *NotificationError
	if errors.As(err, &ne) && !ne.FromPeer {
		if serr := c.send(encodeNotification(ne.Code, ne.Subcode, ne.Data)); serr != nil {
			c.log.Warn("bgp: sending notification failed", "err", serr)
		}
	}
	return err
}

func (s *Speaker) session(ctx context.Context) error {
	d := net.Dialer{Timeout: 10 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", s.cfg.Peer)
	if err != nil {
		return err
	}
	defer nc.Close()
	c := &conn{c: nc, log: s.log}
	s.log.Info("bgp connected", "peer", s.cfg.Peer, "local", nc.LocalAddr())

	msgs := make(chan readResult, 8)
	go func() {
		for {
			p, err := readPacket(nc)
			msgs <- readResult{p, err}
			if err != nil {
				return
			}
		}
	}()
	next := func(timeout time.Duration) (wire.BGPPacket, error) {
		select {
		case <-ctx.Done():
			c.send(encodeNotification(6, 2, nil)) // cease, administrative shutdown
			return wire.BGPPacket{}, ctx.Err()
		case <-time.After(timeout):
			return wire.BGPPacket{}, &NotificationError{Code: 4} // hold timer expired
		case r := <-msgs:
			if r.err != nil {
				return r.pkt, r.err
			}
			if r.pkt.Header.Type == wire.BGPType_Notification {
				n := r.pkt.Notification()
				return r.pkt, &NotificationError{Code: n.Code, Subcode: n.Subcode, Data: n.Data, FromPeer: true}
			}
			return r.pkt, nil
		}
	}

	// OpenSent
	if err := c.send(encodeOpen(s.cfg.LocalAS, s.cfg.HoldTime, s.cfg.RouterID)); err != nil {
		return err
	}
	p, err := next(4 * time.Minute)
	if err != nil {
		return c.notify(err)
	}
	if p.Header.Type != wire.BGPType_Open {
		return c.notify(&NotificationError{Code: 5}) // FSM error
	}
	o := p.Open()
	s.log.Info("bgp open received", "version", o.Version, "as", o.As, "hold", o.Hold,
		"id", netip.AddrFrom4([4]byte{byte(o.Id >> 24), byte(o.Id >> 16), byte(o.Id >> 8), byte(o.Id)}),
		"capabilities", describeCapabilities(o.Options))
	if o.Version != 4 {
		return c.notify(&NotificationError{Code: 2, Subcode: 1, Data: []byte{0, 4}})
	}
	if o.As != s.cfg.PeerAS {
		return c.notify(&NotificationError{Code: 2, Subcode: 2})
	}
	if o.Hold == 1 || o.Hold == 2 {
		return c.notify(&NotificationError{Code: 2, Subcode: 6})
	}
	hold := min(o.Hold, s.cfg.HoldTime)

	// OpenConfirm
	if err := c.send(encodeKeepalive()); err != nil {
		return err
	}
	holdDur := time.Duration(hold) * time.Second
	if hold == 0 {
		holdDur = 24 * time.Hour
	}
	p, err = next(holdDur)
	if err != nil {
		return c.notify(err)
	}
	if p.Header.Type != wire.BGPType_Keepalive {
		return c.notify(&NotificationError{Code: 5})
	}
	s.log.Info("bgp established", "peer", s.cfg.Peer, "hold", hold)

	// Established
	attrs := updateAttrs{localAS: s.cfg.LocalAS, ibgp: s.cfg.LocalAS == s.cfg.PeerAS, nextHop: s.cfg.NextHop}
	advertised := map[netip.Prefix]struct{}{}
	sync := func() error {
		want := s.wanted()
		var add, del []netip.Prefix
		for p := range want {
			if _, ok := advertised[p]; !ok {
				add = append(add, p)
			}
		}
		for p := range advertised {
			if _, ok := want[p]; !ok {
				del = append(del, p)
			}
		}
		if len(add) == 0 && len(del) == 0 {
			return nil
		}
		slices.SortFunc(add, netip.Prefix.Compare)
		slices.SortFunc(del, netip.Prefix.Compare)
		if err := c.send(encodeUpdate(del, add, attrs)); err != nil {
			return err
		}
		s.log.Info("bgp update sent", "announce", add, "withdraw", del)
		advertised = want
		return nil
	}
	if err := sync(); err != nil {
		return err
	}
	var keepalive <-chan time.Time
	if hold > 0 {
		t := time.NewTicker(holdDur / 3)
		defer t.Stop()
		keepalive = t.C
	}
	holdTimer := time.NewTimer(holdDur)
	defer holdTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			c.send(encodeNotification(6, 2, nil))
			return ctx.Err()
		case <-keepalive:
			if err := c.send(encodeKeepalive()); err != nil {
				return err
			}
		case <-holdTimer.C:
			return c.notify(&NotificationError{Code: 4})
		case <-s.changed:
			if err := sync(); err != nil {
				return err
			}
		case r := <-msgs:
			if r.err != nil {
				return c.notify(r.err)
			}
			holdTimer.Reset(holdDur)
			switch r.pkt.Header.Type {
			case wire.BGPType_Keepalive:
			case wire.BGPType_Update:
				logUpdate(s.log, r.pkt.Update())
			case wire.BGPType_Notification:
				n := r.pkt.Notification()
				return &NotificationError{Code: n.Code, Subcode: n.Subcode, Data: n.Data, FromPeer: true}
			default:
				return c.notify(&NotificationError{Code: 5})
			}
		}
	}
}

func describeCapabilities(opts []wire.BGPOption) []string {
	var out []string
	for i := range opts {
		caps := opts[i].Capabilities()
		if caps == nil {
			out = append(out, fmt.Sprintf("opt%d", opts[i].Type))
			continue
		}
		for j := range *caps {
			c := &(*caps)[j]
			if mp := c.MultiProtocol(); mp != nil {
				out = append(out, fmt.Sprintf("mp(afi=%d,safi=%d)", mp.Afi, mp.Safi))
			} else {
				out = append(out, fmt.Sprintf("%v(len=%d)", c.Code, c.Len))
			}
		}
	}
	return out
}

// logUpdate logs what the peer sent. Routes from the peer are not used.
func logUpdate(log *slog.Logger, u *wire.Update) {
	var nlri, wd []netip.Prefix
	for _, p := range u.Nlri {
		if pp, err := fromPrefix(p); err == nil {
			nlri = append(nlri, pp)
		}
	}
	for _, p := range u.WithdrawnRoutes.Data {
		if pp, err := fromPrefix(p); err == nil {
			wd = append(wd, pp)
		}
	}
	log.Info("bgp update received (ignored)", "nlri", nlri, "withdrawn", wd, "attrs", len(u.PathAttr.Data))
}
