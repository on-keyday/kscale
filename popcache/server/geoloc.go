package server

import (
	"log/slog"
	"net/netip"
	"os"

	"github.com/on-keyday/kscale/geoloc"
	"github.com/on-keyday/kscale/popcache/edge"
)

// Conventional paths of the geoip databases within the node-local file store.
const (
	geolocASNPath  = "geoloc/GeoLite2-ASN.mmdb"
	geolocCityPath = "geoloc/GeoLite2-City.mmdb"
)

// loadGeoLocator loads the geoip databases from the node-local file store and
// returns an edge.GeoLocator, or nil when the files are absent or fail to load
// (the module-facing geoloc_lookup then reports "not found"). popcache is a
// pure consumer: fetching the mmdb files and placing them into the file store
// is a separate concern (control-plane / ops), not popcache's job.
func loadGeoLocator(fileDir *os.Root, logger *slog.Logger) edge.GeoLocator {
	if fileDir == nil {
		return nil
	}
	asn, err := fileDir.ReadFile(geolocASNPath)
	if err != nil {
		logger.Info("geoloc disabled: ASN database not available", "path", geolocASNPath, "error", err)
		return nil
	}
	city, err := fileDir.ReadFile(geolocCityPath)
	if err != nil {
		logger.Info("geoloc disabled: City database not available", "path", geolocCityPath, "error", err)
		return nil
	}
	geo, err := geoloc.LoadGeoLocation(asn, city)
	if err != nil {
		logger.Warn("geoloc disabled: failed to load databases", "error", err)
		return nil
	}
	logger.Info("geoloc enabled", "asn", geolocASNPath, "city", geolocCityPath)
	return geoLocatorAdapter{geo: geo}
}

// geoLocatorAdapter adapts geoloc.GeoLocationInfo to the edge.GeoLocator the
// edge computer consumes.
type geoLocatorAdapter struct {
	geo *geoloc.GeoLocationInfo
}

func (a geoLocatorAdapter) Lookup(ip netip.Addr) (*edge.GeoInfo, error) {
	loc, err := a.geo.GeoLocation(ip)
	if err != nil {
		return nil, err
	}
	return geoInfoFrom(loc), nil
}

// geoInfoFrom maps a geoloc.GeoLocation to the edge.GeoInfo carried over the
// wasm ABI (ASN number/org, country ISO code, English city name).
func geoInfoFrom(loc *geoloc.GeoLocation) *edge.GeoInfo {
	info := &edge.GeoInfo{}
	if loc.ASN != nil {
		info.ASN = uint32(loc.ASN.AutonomousSystemNumber)
		info.ASNOrg = loc.ASN.AutonomousSystemOrganization
	}
	if loc.City != nil {
		info.Country = loc.City.Country.ISOCode
		info.City = loc.City.City.Names.English
	}
	return info
}
