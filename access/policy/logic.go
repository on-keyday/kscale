package policy

import (
	"fmt"

	"github.com/on-keyday/kscale/access"
)

type policySetBase struct {
	name     string
	policies []access.Policy
}

func (p *policySetBase) Name() string {
	return p.name
}

func (p *policySetBase) AddPolicy(pol access.Policy) error {
	for _, existing := range p.policies {
		if existing.Name() == pol.Name() {
			return nil
		}
	}
	p.policies = append(p.policies, pol)
	return nil
}

func (p *policySetBase) ListPolicies() []access.Policy {
	return p.policies
}

func (p *policySetBase) RemovePolicy(name string) {
	for i, pol := range p.policies {
		if pol.Name() == name {
			p.policies = append(p.policies[:i], p.policies[i+1:]...)
			return
		}
	}
}

func (p *policySetBase) String() string {
	result := "PolicySet(" + p.name + "): ["
	for i, pol := range p.policies {
		if i > 0 {
			result += ", "
		}
		result += pol.String()
	}
	result += "]"
	return result
}

type andPolicy struct {
	policySetBase
}

func NewAndPolicy(name string, policies ...access.Policy) access.Policy {
	return &andPolicy{
		policySetBase: policySetBase{
			name:     name,
			policies: policies,
		},
	}
}

func (p *andPolicy) Satisfy(ctx *access.PolicyContext) *access.Decision {
	var derivedPerms []*access.Decision
	for _, pol := range p.policies {
		perm := pol.Satisfy(ctx)
		if !perm.Result {
			return &access.Decision{
				Result:  false,
				Reason:  "at least one policy not satisfied",
				Policy:  p,
				Derived: append(derivedPerms, perm),
			}
		}
		derivedPerms = append(derivedPerms, perm)
	}
	return &access.Decision{
		Result:  true,
		Reason:  "all policies satisfied",
		Policy:  p,
		Derived: derivedPerms,
	}
}

func (p *andPolicy) String() string {
	return fmt.Sprintf("And%s", p.policySetBase.String())
}

type orPolicy struct {
	policySetBase
}

func NewOrPolicy(name string, policies ...access.Policy) access.Policy {
	return &orPolicy{
		policySetBase: policySetBase{
			name:     name,
			policies: policies,
		},
	}
}

func (p *orPolicy) Satisfy(ctx *access.PolicyContext) *access.Decision {
	var derivedPerms []*access.Decision
	for _, pol := range p.policies {
		perm := pol.Satisfy(ctx)
		derivedPerms = append(derivedPerms, perm)
		if perm.Result {
			return &access.Decision{
				Result:  true,
				Reason:  "at least one policy satisfied",
				Policy:  p,
				Derived: derivedPerms,
			}
		}
	}
	return &access.Decision{
		Result:  false,
		Reason:  "no policies satisfied",
		Policy:  p,
		Derived: derivedPerms,
	}
}

func (p *orPolicy) String() string {
	return fmt.Sprintf("Or%s", p.policySetBase.String())
}

type xorPolicy struct {
	policySetBase
}

func NewXorPolicy(name string, policies ...access.Policy) access.Policy {
	return &xorPolicy{
		policySetBase: policySetBase{
			name:     name,
			policies: policies,
		},
	}
}

func (p *xorPolicy) Satisfy(ctx *access.PolicyContext) *access.Decision {
	var derivedPerms []*access.Decision
	satisfiedCount := 0
	for _, pol := range p.policies {
		perm := pol.Satisfy(ctx)
		derivedPerms = append(derivedPerms, perm)
		if perm.Result {
			satisfiedCount++
		}
	}
	if satisfiedCount == 1 {
		return &access.Decision{
			Result:  true,
			Reason:  "exactly one policy satisfied",
			Policy:  p,
			Derived: derivedPerms,
		}
	}
	return &access.Decision{
		Result:  false,
		Reason:  "zero or multiple policies satisfied",
		Policy:  p,
		Derived: derivedPerms,
	}
}

func (p *xorPolicy) String() string {
	return fmt.Sprintf("Xor%s", p.policySetBase.String())
}

type notPolicy struct {
	policy access.Policy
	name   string
}

func NewNotPolicy(name string, policy access.Policy) access.Policy {
	return &notPolicy{
		policy: policy,
		name:   name,
	}
}

func (p *notPolicy) Name() string {
	return p.name
}

func (p *notPolicy) Satisfy(ctx *access.PolicyContext) *access.Decision {
	perm := p.policy.Satisfy(ctx)
	if perm.Result {
		return &access.Decision{
			Result:  false,
			Reason:  "inner policy satisfied",
			Policy:  p,
			Derived: []*access.Decision{perm},
		}
	}
	return &access.Decision{
		Result:  true,
		Reason:  "inner policy not satisfied",
		Policy:  p,
		Derived: []*access.Decision{perm},
	}
}

func (p *notPolicy) String() string {
	return fmt.Sprintf("Not(%s)", p.policy.String())
}
