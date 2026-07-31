package client

import "testing"

// validateArgValue must accept exactly what the generated dispatch's binders
// accept — for []string that is parseStringList (comma-separated OR JSON array),
// not JSON-only: the TUI validates on submit with this and its form hint
// advertises the comma form.
func TestValidateArgValueStringList(t *testing.T) {
	arg := ArgSpec{Name: "ports", Type: "[]string"}
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"comma-separated", "tcp:443,udp:443,icmp:echo", false},
		{"single element", "tcp:443", false},
		{"json array", `["tcp:443","udp:443"]`, false},
		{"malformed json errors, no fallback", `["a",`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArgValue(arg, tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateArgValue(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}
}
