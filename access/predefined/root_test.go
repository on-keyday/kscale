package predefined_test

import (
	"fmt"
	"testing"

	"github.com/on-keyday/kscale/access/predefined"
)

func TestRoot(t *testing.T) {
	_ = predefined.Resources()
	pol := predefined.AllowPolicies()
	for _, p := range pol {
		fmt.Println(p.String())
	}
	pol = predefined.DenyPolicies()
	for _, p := range pol {
		fmt.Println(p.String())
	}
}
