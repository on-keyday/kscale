package main

import (
	"strings"
	"testing"

	"github.com/on-keyday/kscale/client"
)

// cliGlobalFlags mirrors the flags run() registers before the per-action arg
// flags. Keep in sync with run().
var cliGlobalFlags = map[string]bool{
	"addr": true, "data": true, "role": true, "apply": true, "apply-schema": true,
	"dump": true, "validate": true, "resource": true, "op": true, "arg": true,
	"ca-domain": true, "enroll-cn": true,
}

// TestNoResourceArgShadowsGlobalFlag: every resource arg must keep its typed
// --<arg> flag — a collision with a cli global degrades it to --arg name=value
// (and, before the guard, paniced the whole cli with "flag redefined": the
// identity flag was --common-name while node get / stats get / dp-file send /
// expected-node apply all take a common_name arg). If this fails, rename the
// GLOBAL flag — the plain kebab name belongs to the resource arg.
func TestNoResourceArgShadowsGlobalFlag(t *testing.T) {
	for _, r := range client.ResourceSpecs {
		for _, act := range r.Actions {
			for _, a := range act.Args {
				if kebab := strings.ReplaceAll(a.Name, "_", "-"); cliGlobalFlags[kebab] {
					t.Errorf("%s %s: arg %q collides with the cli's global --%s flag", r.Command, act.Command, a.Name, kebab)
				}
			}
		}
	}
}
