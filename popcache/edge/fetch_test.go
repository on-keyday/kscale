package edge

import (
	"net/url"
	"testing"
)

func TestFetchHostAllowed(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		rawURL  string
		want    bool
	}{
		{"empty list denies", nil, "http://example.com/", false},
		{"hostname any port", []string{"example.com"}, "http://example.com:8080/x", true},
		{"hostname default port", []string{"example.com"}, "https://example.com/", true},
		{"case-insensitive", []string{"Example.COM"}, "http://example.com/", true},
		{"other host denied", []string{"example.com"}, "http://evil.com/", false},
		{"subdomain not implied", []string{"example.com"}, "http://sub.example.com/", false},
		{"host:port exact match", []string{"example.com:8443"}, "https://example.com:8443/", true},
		{"host:port wrong port", []string{"example.com:8443"}, "https://example.com:9443/", false},
		{"host:port default https port", []string{"example.com:443"}, "https://example.com/", true},
		{"host:port default http port", []string{"example.com:80"}, "http://example.com/", true},
		{"ipv4 entry", []string{"127.0.0.1"}, "http://127.0.0.1:3000/", true},
		{"ipv6 bracketed entry with port", []string{"[::1]:8080"}, "http://[::1]:8080/", true},
		{"ipv6 bare entry any port", []string{"::1"}, "http://[::1]:9999/", true},
		{"blank entries skipped", []string{"", " ", "example.com"}, "http://example.com/", true},

		// Path-prefix entries (segment-boundary match on the cleaned path).
		{"path prefix allows itself", []string{"github.com/myorg"}, "https://github.com/myorg", true},
		{"path prefix allows subpath", []string{"github.com/myorg"}, "https://github.com/myorg/repo/pulls", true},
		{"path prefix denies sibling", []string{"github.com/myorg"}, "https://github.com/otherorg/repo", false},
		{"path prefix is segment-bounded", []string{"github.com/myorg"}, "https://github.com/myorgxyz", false},
		{"path prefix trailing slash same", []string{"github.com/myorg/"}, "https://github.com/myorg/repo", true},
		{"host trailing slash means any path", []string{"github.com/"}, "https://github.com/anything", true},
		{"dot segments cannot escape", []string{"github.com/myorg"}, "https://github.com/myorg/../otherorg", false},
		{"encoded slash cannot escape", []string{"github.com/myorg"}, "https://github.com/myorg%2F..%2Fotherorg", false},
		{"path entry with port", []string{"api.test:8443/v1"}, "https://api.test:8443/v1/users", true},
		{"path entry with wrong port", []string{"api.test:8443/v1"}, "https://api.test:9443/v1/users", false},

		// Scheme-pinned entries.
		{"scheme pinned https allows https", []string{"https://api.test"}, "https://api.test/x", true},
		{"scheme pinned https denies http", []string{"https://api.test"}, "http://api.test/x", false},
		{"scheme pinned with path", []string{"https://github.com/myorg"}, "https://github.com/myorg/r", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.rawURL)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.rawURL, err)
			}
			if got := fetchURLAllowed(tc.allowed, u); got != tc.want {
				t.Fatalf("fetchURLAllowed(%v, %s) = %v, want %v", tc.allowed, tc.rawURL, got, tc.want)
			}
		})
	}
}
