package access

import (
	"context"
	"errors"
	"fmt"

	"github.com/on-keyday/kscale/access/audit"
)

type AccessController interface {
	CanAccess(p *PolicyContext) error
}

type PolicyList struct {
	Deny  []Policy
	Allow []Policy
}

type accessController struct {
	policyMap map[string]map[string]*PolicyList
}

func NewAccessController(policyMap map[string]map[string]*PolicyList) AccessController {
	return &accessController{
		policyMap: policyMap,
	}
}

type DenyError struct {
	Cause *Decision
}

func (de *DenyError) Error() string {
	return fmt.Sprintf("access denied by policy %q: %s", de.Cause.Policy.Name(), de.Cause.Reason)
}

func IsDenyError(err error) bool {
	var denyErr *DenyError
	return errors.As(err, &denyErr)
}

func IsDefaultDenyError(err error) bool {
	var denyErr *DenyError
	if errors.As(err, &denyErr) {
		_, ok := denyErr.Cause.Policy.(*defaultDenyPolicy)
		return ok
	}
	return false
}

type defaultDenyPolicy struct{}

func (p *defaultDenyPolicy) Name() string {
	return "DefaultDeny"
}

func (p *defaultDenyPolicy) Satisfy(ctx *PolicyContext) *Decision {
	return &Decision{
		Result: false,
		Reason: "default deny policy",
		Policy: p,
	}
}

func (p *defaultDenyPolicy) String() string {
	return "DefaultDeny"
}

func (ac *accessController) CanAccess(p *PolicyContext) error {
	if p.Resource == nil || p.Action == nil {
		d := &DenyError{Cause: &Decision{
			Result: true,
			Reason: "resource or action is nil",
			Policy: &defaultDenyPolicy{},
		}}
		p.Audit.RecordPolicyDecision(audit.DecisionTypeDeny, d.Cause)
		return d
	}
	resourcePolicies, ok := ac.policyMap[p.Resource.Name()]
	if !ok {
		d := &DenyError{Cause: &Decision{
			Result: true,
			Reason: fmt.Sprintf("no policies for resource %q", p.Resource.Name()),
			Policy: &defaultDenyPolicy{},
		}}
		p.Audit.RecordPolicyDecision(audit.DecisionTypeDeny, d.Cause)
		return d
	}
	policyList, ok := resourcePolicies[p.Action.Name()]
	if !ok {
		d := &DenyError{Cause: &Decision{
			Result: true,
			Reason: fmt.Sprintf("no policies for action %q on resource %q", p.Action.Name(), p.Resource.Name()),
			Policy: &defaultDenyPolicy{},
		}}
		p.Audit.RecordPolicyDecision(audit.DecisionTypeDeny, d.Cause)
		return d
	}
	for _, policy := range policyList.Deny {
		p.Audit.RecordPolicyStart(audit.DecisionTypeDeny, &audit.PolicyStartRecord{PolicyName: policy.Name()})
		decision := policy.Satisfy(p)
		p.Audit.RecordPolicyDecision(audit.DecisionTypeDeny, decision)
		if decision.Result {
			return &DenyError{Cause: decision}
		}
	}
	for _, policy := range policyList.Allow {
		p.Audit.RecordPolicyStart(audit.DecisionTypeAllow, &audit.PolicyStartRecord{PolicyName: policy.Name()})
		decision := policy.Satisfy(p)
		p.Audit.RecordPolicyDecision(audit.DecisionTypeAllow, decision)
		if decision.Result {
			return nil
		}
	}
	d := &DenyError{Cause: &Decision{
		Result: true,
		Reason: "no allow policy matched",
		Policy: &defaultDenyPolicy{},
	}}
	p.Audit.RecordPolicyDecision(audit.DecisionTypeDeny, d.Cause)
	return d
}

type ContextCollector struct {
	User        UserContext
	Environment Environment
	Resource    Resource
	Action      Action
}

func NewContextCollector() *ContextCollector {
	return &ContextCollector{}
}

func (cc *ContextCollector) CollectEnvironment(e Environment) *ContextCollector {
	return &ContextCollector{
		User:        cc.User,
		Environment: e,
		Resource:    cc.Resource,
		Action:      cc.Action,
	}
}

func (cc *ContextCollector) CollectUser(u UserContext) *ContextCollector {
	return &ContextCollector{
		User:        u,
		Environment: cc.Environment,
		Resource:    cc.Resource,
		Action:      cc.Action,
	}
}

func (cc *ContextCollector) CollectResource(r Resource) *ContextCollector {
	return &ContextCollector{
		User:        cc.User,
		Environment: cc.Environment,
		Resource:    r,
		Action:      cc.Action,
	}
}

func (cc *ContextCollector) CollectAction(a Action) *ContextCollector {
	return &ContextCollector{
		User:        cc.User,
		Environment: cc.Environment,
		Resource:    cc.Resource,
		Action:      a,
	}
}

func (cc *ContextCollector) Build(ctx context.Context, audit audit.Audit) *PolicyContext {
	return &PolicyContext{
		Context:     ctx,
		User:        cc.User,
		Environment: cc.Environment,
		Resource:    cc.Resource,
		Action:      cc.Action,
		Audit:       audit,
	}
}
