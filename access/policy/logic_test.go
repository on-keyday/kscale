package policy_test

import (
	"fmt"
	"testing"

	"github.com/on-keyday/kscale/access/policy"
)

func TestLogic(t *testing.T) {
	basic_constraint_check := policy.NewAndPolicy("basic_constraint_check",
		policy.MustParseAttributePolicy("resource_action_check", `$action.name in $resource.actions`),
		policy.MustParseAttributePolicy("user_action_check", `$action.name in $user.allowed_actions`),
		policy.MustParseAttributePolicy("user_resource_check", `$resource.name in $user.allowed_resource`),
	)
	hpa := policy.NewAndPolicy("high_privilege_access",
		policy.MustParseAttributePolicy("login_method", `$user.login_method == "mfa"`),
		policy.MustParseAttributePolicy("location_check", `$user.location == "JP"`),
		policy.MustParseAttributePolicy("credential_lifetime_check", `$user.credential_acquired_time.until_now duration_lte $env.system.max_privilege_credential_age`),
		policy.MustParseAttributePolicy("role_check", `$user.role == "admin"`),
		policy.MustParseAttributePolicy("age_check", `$user.age >= 18`),
		policy.MustParseAttributePolicy("time_check", "$env.time.hour int_between [9,16]"),
	)
	allow_secret_access := policy.NewAndPolicy("allow_secret_access",
		basic_constraint_check,
		hpa,
		policy.MustParseAttributePolicy("secret_access_level", `$resource.secret_level <= $user.clearance_level`),
		policy.MustParseAttributePolicy("read_access", `$action.name == "read_secret"`),
	)
	fmt.Println(allow_secret_access.String())
}
