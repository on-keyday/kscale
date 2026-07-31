package server

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"

	"github.com/cloudflare/cloudflare-go"
	"github.com/on-keyday/kscale/dns/dnsmetrics"
)

type cloudflareServer struct {
	apiToken    string
	zoneID      string
	domain      string
	domainAddrs []netip.Addr
	lock        sync.RWMutex
}

func NewCloudflareDNS() Server {
	return &cloudflareServer{}
}

func (c *cloudflareServer) listRecords(ctx context.Context) (*cloudflare.API, string, []cloudflare.DNSRecord, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	if c.apiToken == "" {
		return nil, "", nil, fmt.Errorf("cloudflare dns server: api token is not set")
	}
	if c.zoneID == "" {
		return nil, "", nil, fmt.Errorf("cloudflare dns server: zone ID is not set")
	}
	if c.domain == "" {
		return nil, "", nil, fmt.Errorf("cloudflare dns server: domain is not set")
	}
	api, err := cloudflare.NewWithAPIToken(c.apiToken)
	if err != nil {
		return nil, "", nil, fmt.Errorf("cloudflare dns server: failed to create API client: %w", err)
	}
	zoneID := cloudflare.ZoneIdentifier(c.zoneID)
	zone, err := api.ZoneDetails(ctx, c.zoneID)
	if err != nil {
		return nil, "", nil, fmt.Errorf("cloudflare dns server: failed to get zone details: %w", err)
	}
	// domain must be subdomain of zone
	if !strings.HasSuffix(c.domain, "."+zone.Name) && c.domain != "."+zone.Name {
		return nil, "", nil, fmt.Errorf("cloudflare dns server: domain %s is not a subdomain of zone %s", c.domain, zone.Name)
	}
	records, _, err := api.ListDNSRecords(ctx, zoneID, cloudflare.ListDNSRecordsParams{})
	if err != nil {
		return nil, "", nil, fmt.Errorf("cloudflare dns server: failed to list DNS records: %w", err)
	}
	handledRecords := []cloudflare.DNSRecord{}
	// only handle ends with the domain
	for _, record := range records {
		if strings.HasSuffix(record.Name, c.domain) {
			handledRecords = append(handledRecords, record)
		}
	}
	return api, zone.Name, handledRecords, nil
}

func (c *cloudflareServer) Start(ctx context.Context) error {
	api, _, records, err := c.listRecords(ctx)
	if err != nil {
		return err
	}
	// create non-existing records
	for _, addr := range c.domainAddrs {
		found := false
		for _, record := range records {
			if record.Content == addr.String() {
				found = true
				break
			}
		}
		if found {
			continue
		}
		newRecord := cloudflare.CreateDNSRecordParams{
			Type:    "A",
			Name:    c.domain,
			Content: addr.String(),
			TTL:     300,
			Proxied: cloudflare.BoolPtr(false), // CDNを通すか (true/false)
			Comment: "Created via kscale",
		}
		_, err := api.CreateDNSRecord(ctx, cloudflare.ZoneIdentifier(c.zoneID), newRecord)
		if err != nil {
			return fmt.Errorf("cloudflare dns server: failed to create initial DNS record: %w", err)
		}
	}
	// remove stale records
	for _, record := range records {
		found := false
		for _, addr := range c.domainAddrs {
			if record.Content == addr.String() {
				found = true
				break
			}
		}
		if !found {
			err := api.DeleteDNSRecord(ctx, cloudflare.ZoneIdentifier(c.zoneID), record.ID)
			if err != nil {
				return fmt.Errorf("cloudflare dns server: failed to delete stale DNS record: %w", err)
			}
		}
	}
	return nil
}

func (c *cloudflareServer) Stop() error {
	api, _, records, err := c.listRecords(context.Background())
	if err != nil {
		return err
	}
	// remove all managed records
	for _, record := range records {
		if strings.HasSuffix(record.Name, c.domain) {
			err := api.DeleteDNSRecord(context.Background(), cloudflare.ZoneIdentifier(c.zoneID), record.ID)
			if err != nil {
				return fmt.Errorf("cloudflare dns server: failed to delete DNS record: %w", err)
			}
		}
	}
	return nil
}

func (c *cloudflareServer) IsRunning() bool {
	return false
}

// SetPort is a no-op for the Cloudflare backend: it's an API client, not a local
// listener, so a "listen port" is meaningless. Silently ignore it rather than failing the
// whole ApplyConfig/Start (a port in dns_config is only for the built-in server).
func (c *cloudflareServer) SetPort(port uint16) error {
	return nil
}

func (c *cloudflareServer) SetAcmeToken(domain string, token string) error {
	api, rootDomain, _, err := c.listRecords(context.Background())
	if err != nil {
		return err
	}
	// check domain is subdomain of managed domain
	if !strings.HasSuffix(domain, "."+rootDomain) || domain == "."+rootDomain {
		return fmt.Errorf("cloudflare dns server: acme domain %s is not a subdomain of managed domain %s", domain, c.domain)
	}
	c.lock.RLock()
	curDomain := c.domain
	c.lock.RUnlock()
	if !strings.HasSuffix(domain, "."+curDomain) && domain != curDomain {
		c.lock.RUnlock()
		return fmt.Errorf("cloudflare dns server: acme domain %s is not a subdomain of managed domain %s", domain, c.domain)
	}
	acmeRecordName := "_acme-challenge." + domain
	// add record for token
	newRecord := cloudflare.CreateDNSRecordParams{
		Type:    "TXT",
		Name:    acmeRecordName,
		Content: token,
		TTL:     120,
		Proxied: cloudflare.BoolPtr(false),
		Comment: "ACME challenge record created via kscale",
	}
	_, err = api.CreateDNSRecord(context.Background(), cloudflare.ZoneIdentifier(c.zoneID), newRecord)
	if err != nil {
		return fmt.Errorf("cloudflare dns server: failed to create ACME challenge DNS record: %w", err)
	}
	return nil
}

func (c *cloudflareServer) ClearAcmeToken(domain string, token string) error {
	api, rootDomain, records, err := c.listRecords(context.Background())
	if err != nil {
		return err
	}
	// check domain is subdomain of managed domain
	if !strings.HasSuffix(domain, "."+rootDomain) || domain == "."+rootDomain {
		return fmt.Errorf("cloudflare dns server: acme domain %s is not a subdomain of managed domain %s", domain, c.domain)
	}
	c.lock.RLock()
	curDomain := c.domain
	c.lock.RUnlock()
	if !strings.HasSuffix(domain, "."+curDomain) && domain != curDomain {
		return fmt.Errorf("cloudflare dns server: acme domain %s is not a subdomain of managed domain %s", domain, c.domain)
	}
	acmeRecordName := "_acme-challenge." + domain
	// find and delete record for token
	for _, record := range records {
		if record.Type == "TXT" && record.Name == acmeRecordName && record.Content == token {
			err := api.DeleteDNSRecord(context.Background(), cloudflare.ZoneIdentifier(c.zoneID), record.ID)
			if err != nil {
				return fmt.Errorf("cloudflare dns server: failed to delete ACME challenge DNS record: %w", err)
			}
			return nil
		}
	}
	// no existance is not error
	return nil
}

func (c *cloudflareServer) ResetAcmeToken(domain string) error {
	api, rootDomain, records, err := c.listRecords(context.Background())
	if err != nil {
		return err
	}
	// check domain is subdomain of managed domain
	if !strings.HasSuffix(domain, "."+rootDomain) || domain == "."+rootDomain {
		return fmt.Errorf("cloudflare dns server: acme domain %s is not a subdomain of managed domain %s", domain, c.domain)
	}
	c.lock.RLock()
	curDomain := c.domain
	c.lock.RUnlock()
	if !strings.HasSuffix(domain, "."+curDomain) && domain != curDomain {
		return fmt.Errorf("cloudflare dns server: acme domain %s is not a subdomain of managed domain %s", domain, c.domain)
	}
	acmeRecordName := "_acme-challenge." + domain
	// find and delete record
	for _, record := range records {
		if record.Type == "TXT" && record.Name == acmeRecordName {
			err := api.DeleteDNSRecord(context.Background(), cloudflare.ZoneIdentifier(c.zoneID), record.ID)
			if err != nil {
				return fmt.Errorf("cloudflare dns server: failed to delete ACME challenge DNS record: %w", err)
			}
		}
	}
	return nil
}

func (c *cloudflareServer) SetV4VIP(vip []netip.Addr) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.domainAddrs = vip
	return nil
}

func (c *cloudflareServer) AddSubdomain(subdomain string, addrs []netip.Addr) error {
	return fmt.Errorf("dns server: AddSubdomain is not supported for Cloudflare DNS")
}

func (c *cloudflareServer) RemoveSubdomain(subdomain string, addrs []netip.Addr) error {
	return fmt.Errorf("dns server: RemoveSubdomain is not supported for Cloudflare DNS")
}

func (c *cloudflareServer) SetDomain(domain string) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.domain = domain
	return nil
}

func (c *cloudflareServer) SetZoneID(zoneID string) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.zoneID = zoneID
	return nil
}

func (c *cloudflareServer) SetMailAddress(mail string) error {
	return fmt.Errorf("dns server: SetMailAddress is not supported for Cloudflare DNS")
}

func (c *cloudflareServer) SetAPIToken(token string) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.apiToken = token
	return nil
}

func (c *cloudflareServer) SetMetricsCounter(m dnsmetrics.DNSMetricsMetricsInterface) {
}
