package client

import (
	"reflect"
	"testing"
)

func TestParseStringList(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"empty is empty list", "", nil, false},
		{"blank is empty list", "  ", nil, false},
		{"single element", "a.example.com", []string{"a.example.com"}, false},
		{"comma-separated", "a,b:8443,c", []string{"a", "b:8443", "c"}, false},
		{"comma-separated trims", " a , b ", []string{"a", "b"}, false},
		{"empty elements dropped", "a,,b,", []string{"a", "b"}, false},
		{"json array", `["a","b,with,commas"]`, []string{"a", "b,with,commas"}, false},
		{"json empty array", `[]`, []string{}, false},
		{"json with leading space", ` ["a"]`, []string{"a"}, false},
		{"malformed json errors, no fallback", `["a",`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStringList(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseStringList(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseStringList(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}
