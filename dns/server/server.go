package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"codeberg.org/miekg/dns"
	"github.com/on-keyday/kscale/dns/dnsmetrics"
)

type server struct {
	confLock     sync.RWMutex
	publicDomain string
	acmeToken    string
	currentV4VIP []netip.Addr
	logger       *slog.Logger
	portNumber   uint16
	canceller    context.CancelFunc
	mailAddress  string
	metrics      dnsmetrics.DNSMetricsMetricsInterface
	subdomain    map[string][]netip.Addr
}

type Server interface {
	Start(ctx context.Context) error
	Stop() error
	IsRunning() bool
	SetPort(port uint16) error
	SetAcmeToken(domain string, token string) error
	ClearAcmeToken(domain string, token string) error
	ResetAcmeToken(domain string) error
	SetV4VIP(vip []netip.Addr) error
	AddSubdomain(subdomain string, addr []netip.Addr) error
	RemoveSubdomain(subdomain string, addr []netip.Addr) error
	SetDomain(domain string) error
	SetMailAddress(mail string) error
	SetAPIToken(token string) error // if server implementation is external API-based DNS, set API token for it
	SetZoneID(zoneID string) error  // if server implementation is external API-based DNS, set zone ID for it
	SetMetricsCounter(m dnsmetrics.DNSMetricsMetricsInterface)
}

func (s *server) IsRunning() bool {
	s.confLock.RLock()
	defer s.confLock.RUnlock()
	return s.canceller != nil
}

func (s *server) SetMetricsCounter(m dnsmetrics.DNSMetricsMetricsInterface) {
	s.confLock.Lock()
	defer s.confLock.Unlock()
	s.metrics = m
}

func (s *server) SetAPIToken(token string) error {
	// no-op for built-in DNS server
	return fmt.Errorf("dns server: SetAPIToken is not supported for built-in DNS server")
}

func (s *server) SetZoneID(zoneID string) error {
	// no-op for built-in DNS server
	return fmt.Errorf("dns server: SetZoneID is not supported for built-in DNS server")
}

func (s *server) AddSubdomain(subdomain string, addrs []netip.Addr) error {
	s.confLock.Lock()
	defer s.confLock.Unlock()
	s.subdomain[subdomain] = addrs
	return nil
}

func (s *server) RemoveSubdomain(subdomain string, addrs []netip.Addr) error {
	s.confLock.Lock()
	defer s.confLock.Unlock()
	delete(s.subdomain, subdomain)
	return nil
}

func (s *server) SetMailAddress(mail string) error {
	s.confLock.Lock()
	defer s.confLock.Unlock()
	s.mailAddress = mail
	return nil
}

func (s *server) SetPort(port uint16) error {
	if port == 0 {
		return fmt.Errorf("dns server: invalid port number: %d", port)
	}
	s.confLock.Lock()
	defer s.confLock.Unlock()
	if s.canceller != nil {
		return fmt.Errorf("dns server: cannot change port while server is running")
	}
	s.portNumber = port
	return nil
}

func (s *server) SetAcmeToken(domain string, token string) error {
	s.confLock.Lock()
	defer s.confLock.Unlock()
	if domain != s.publicDomain {
		return fmt.Errorf("dns server: domain mismatch: got %s, want %s", domain, s.publicDomain)
	}
	s.acmeToken = token
	return nil
}

func (s *server) ClearAcmeToken(domain string, token string) error {
	s.confLock.Lock()
	defer s.confLock.Unlock()
	if domain != s.publicDomain {
		return fmt.Errorf("dns server: domain mismatch: got %s, want %s", domain, s.publicDomain)
	}
	if s.acmeToken != token {
		return fmt.Errorf("dns server: token mismatch")
	}
	s.acmeToken = ""
	return nil
}

func (s *server) ResetAcmeToken(domain string) error {
	s.confLock.Lock()
	defer s.confLock.Unlock()
	if domain != s.publicDomain {
		return fmt.Errorf("dns server: domain mismatch: got %s, want %s", domain, s.publicDomain)
	}
	s.acmeToken = ""
	return nil
}

func (s *server) SetV4VIP(vip []netip.Addr) error {
	for _, addr := range vip {
		if !addr.Is4() {
			return fmt.Errorf("dns server: only IPv4 addresses are supported as VIP")
		}
	}
	s.confLock.Lock()
	defer s.confLock.Unlock()
	s.currentV4VIP = vip
	return nil
}

func (s *server) SetDomain(domain string) error {
	s.confLock.Lock()
	defer s.confLock.Unlock()
	s.publicDomain = domain
	return nil
}

func NewServer(l *slog.Logger) Server {
	return &server{
		logger:     l,
		portNumber: 53,
		subdomain:  make(map[string][]netip.Addr),
	}
}

func dnsDomainName(domain string) string {
	return domain + "."
}

func acmeTXTRecordName(domain string) string {
	return "_acme-challenge." + dnsDomainName(domain)
}

var _ Server = (*server)(nil)

func (s *server) createResponse(req *dns.Msg, answer ...dns.RR) ([]byte, error) {
	response := &dns.Msg{}
	response.ID = req.ID
	response.Response = true
	response.Opcode = req.Opcode
	response.Authoritative = true
	response.RecursionAvailable = false
	response.Question = req.Question
	response.Answer = append(response.Answer, answer...)
	err := response.Pack()
	if err != nil {
		return nil, err
	}
	return response.Data, nil
}

func (s *server) handleSOARecord(r *dns.SOA, req *dns.Msg) ([]byte, error) {
	s.confLock.RLock()
	defer s.confLock.RUnlock()
	if r.Hdr.Name == dnsDomainName(s.publicDomain) {
		mail := s.mailAddress
		if mail == "" {
			mail = "hostmaster." + dnsDomainName(s.publicDomain)
		} else {
			mail = dnsDomainName(s.mailAddress)
			mail = strings.ReplaceAll(mail, "@", ".")
		}
		answer := &dns.SOA{
			Hdr: dns.Header{
				Name:  r.Hdr.Name,
				Class: dns.ClassINET,
				TTL:   60,
			},
			Ns:      "ns1." + dnsDomainName(s.publicDomain),
			Mbox:    mail,
			Serial:  uint32(time.Now().Unix()),
			Refresh: 3600,
			Retry:   600,
			Expire:  86400,
			Minttl:  60,
		}
		return s.createResponse(req, answer)
	}
	return s.handleUnknownRecord(req, fmt.Errorf("dns server: no SOA record set for domain %s", r.Hdr.Name))
}

func (s *server) handleNSRecord(r *dns.NS, req *dns.Msg) ([]byte, error) {
	s.confLock.RLock()
	defer s.confLock.RUnlock()
	if r.Hdr.Name == dnsDomainName(s.publicDomain) {
		answer := &dns.NS{
			Hdr: dns.Header{
				Name:  r.Hdr.Name,
				Class: dns.ClassINET,
				TTL:   60,
			},
			Ns: "ns1." + dnsDomainName(s.publicDomain),
		}
		return s.createResponse(req, answer)
	}
	return s.handleUnknownRecord(req, fmt.Errorf("dns server: no NS record set for domain %s", r.Hdr.Name))
}

func (s *server) handleARecord(r *dns.A, req *dns.Msg) ([]byte, error) {
	s.confLock.RLock()
	defer s.confLock.RUnlock()
	if r.Hdr.Name == dnsDomainName(s.publicDomain) {
		if len(s.currentV4VIP) > 0 {
			answers := []dns.RR{}
			for _, addr := range s.currentV4VIP {
				answer := &dns.A{
					Hdr: dns.Header{
						Name:  r.Hdr.Name,
						Class: dns.ClassINET,
						TTL:   60,
					},
					A: net.IP(addr.AsSlice()),
				}
				answers = append(answers, answer)
			}
			return s.createResponse(req, answers...)
		}
	} else if strings.HasSuffix(r.Hdr.Name, "."+dnsDomainName(s.publicDomain)) {
		subdomain := strings.TrimSuffix(r.Hdr.Name, "."+dnsDomainName(s.publicDomain))
		addrs, ok := s.subdomain[subdomain]
		if ok {
			answers := []dns.RR{}
			for _, addr := range addrs {
				if addr.Is4() {
					answer := &dns.A{
						Hdr: dns.Header{
							Name:  r.Hdr.Name,
							Class: dns.ClassINET,
							TTL:   60,
						},
						A: net.IP(addr.AsSlice()),
					}
					answers = append(answers, answer)
				}
			}
			if len(answers) > 0 {
				return s.createResponse(req, answers...)
			}
		}
	}
	return s.handleUnknownRecord(req, fmt.Errorf("dns server: no A record set for domain %s", r.Hdr.Name))
}

func (s *server) handleTXTRecord(r *dns.TXT, req *dns.Msg) ([]byte, error) {
	s.confLock.RLock()
	defer s.confLock.RUnlock()
	if r.Hdr.Name == acmeTXTRecordName(s.publicDomain) {
		if s.acmeToken != "" {
			answer := &dns.TXT{
				Hdr: dns.Header{
					Name:  r.Hdr.Name,
					Class: dns.ClassINET,
					TTL:   60,
				},
				Txt: []string{s.acmeToken},
			}
			return s.createResponse(req, answer)
		}
	}
	return s.handleUnknownRecord(req, fmt.Errorf("dns server: no TXT record set for domain %s", s.publicDomain))
}

func (s *server) handleUnknownRecord(req *dns.Msg, err error) ([]byte, error) {
	msg := &dns.Msg{}
	msg.ID = req.ID
	msg.Rcode = dns.RcodeRefused
	msg.Response = true
	msg.Authoritative = true
	msg.Opcode = req.Opcode
	msg.Truncated = true
	msg.Pack()
	return msg.Data, err
}

func (s *server) Start(ctx context.Context) error {
	s.confLock.Lock()
	defer s.confLock.Unlock()
	if s.canceller != nil {
		return fmt.Errorf("dns server: server is already running")
	}
	// bind to port and start listening for DNS requests would go here.
	list, err := net.ListenUDP("udp", &net.UDPAddr{Port: int(s.portNumber)})
	if err != nil {
		return fmt.Errorf("dns server: failed to start listening: %w", err)
	}
	s.logger.Info("dns server: started listening", "port", s.portNumber)
	cctx, cancel := context.WithCancel(ctx)
	s.canceller = func() {
		list.SetDeadline(time.Now()) // unblock ReadFrom
		list.Close()
		cancel()
	}
	go func() {
		defer list.Close()
		buf := make([]byte, 4096)
		for {
			select {
			case <-cctx.Done():
				s.logger.Info("dns server: server stopping")
				return
			default:
				n, addr, err := list.ReadFrom(buf)
				if err != nil {
					s.logger.Error("dns server: failed to read from connection", "error", err)
					select {
					case <-cctx.Done():
						return
					default:
					}
					continue
				}
				data := make([]byte, n)
				copy(data, buf[:n])
				go func(data []byte, addr net.Addr) {
					resp, err := s.ProcessRequest(data)
					if err != nil {
						s.logger.Error("dns server: failed to process request", "error", err)
						return
					}
					_, err = list.WriteTo(resp, addr)
					if err != nil {
						s.logger.Error("dns server: failed to write response", "error", err)
					}
				}(data, addr)
			}
		}
	}()
	return nil
}

func (s *server) Stop() error {
	s.confLock.Lock()
	defer s.confLock.Unlock()
	if s.canceller == nil {
		return fmt.Errorf("dns server: server is not running")
	}
	s.canceller()
	s.canceller = nil
	s.logger.Info("dns server: server stop requested")
	return nil
}

func (s *server) ProcessRequest(data []byte) ([]byte, error) {
	s.metrics.IncrementRequestsTotal()
	resp, err := s.processRequest(data)
	if err != nil {
		s.metrics.IncrementErrorsTotal()
		s.logger.Error("dns server: failed to process request", "error", err)
		if len(resp) == 0 {
			return nil, err
		}
	}
	s.metrics.IncrementResponsesTotal()
	return resp, nil
}

func (s *server) processRequest(data []byte) ([]byte, error) {
	msg := &dns.Msg{}
	msg.Data = data
	err := msg.Unpack()
	if err != nil {
		return nil, err
	}
	if msg.MsgHeader.Opcode != dns.OpcodeQuery {
		return nil, fmt.Errorf("dns server: only standard queries are supported")
	}
	if len(msg.Question) != 1 {
		return nil, fmt.Errorf("dns server: only one question is supported")
	}
	q := msg.Question[0]
	switch r := q.(type) {
	case *dns.SOA:
		return s.handleSOARecord(r, msg)
	case *dns.NS:
		return s.handleNSRecord(r, msg)
	case *dns.A:
		return s.handleARecord(r, msg)
	case *dns.TXT:
		return s.handleTXTRecord(r, msg)
	}
	return nil, fmt.Errorf("dns server: no matching record found for question: %+v", q)
}
