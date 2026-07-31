package server

import (
	"testing"

	geoip2 "github.com/oschwald/geoip2-golang/v2"
	"github.com/on-keyday/kscale/geoloc"
)

func TestGeoInfoFrom(t *testing.T) {
	loc := &geoloc.GeoLocation{
		ASN: &geoip2.ASN{
			AutonomousSystemNumber:       64500,
			AutonomousSystemOrganization: "TestNet",
		},
		City: &geoip2.City{
			Country: geoip2.CountryRecord{ISOCode: "JP"},
			City:    geoip2.CityRecord{Names: geoip2.Names{English: "Tokyo"}},
		},
	}
	got := geoInfoFrom(loc)
	if got.ASN != 64500 || got.ASNOrg != "TestNet" || got.Country != "JP" || got.City != "Tokyo" {
		t.Fatalf("geoInfoFrom = %+v, want {ASN:64500 ASNOrg:TestNet Country:JP City:Tokyo}", got)
	}
}

func TestGeoInfoFromPartial(t *testing.T) {
	// A location with no ASN/City records maps to zero values, not a panic.
	got := geoInfoFrom(&geoloc.GeoLocation{})
	if got.ASN != 0 || got.ASNOrg != "" || got.Country != "" || got.City != "" {
		t.Fatalf("geoInfoFrom(empty) = %+v, want all-zero", got)
	}
}
