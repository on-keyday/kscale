package policy

import (
	"fmt"
	"strings"

	"github.com/on-keyday/kscale/access"
	"github.com/on-keyday/kscale/access/audit"
)

func init() {
	access.AttributeValueChecker = CheckAttributeType
}

type attributePolicy struct {
	name           string
	context        string
	originalName   string
	attributeNests []string
	compare        Condition
}

var _ access.Policy = (*attributePolicy)(nil)

func NewAttributePolicy(name, context, attribute string, compare *GenericCondition) access.Policy {
	nests := strings.Split(attribute, ".")
	return &attributePolicy{
		name:           name,
		context:        context,
		originalName:   attribute,
		attributeNests: nests,
		compare:        compare,
	}
}

func (p *attributePolicy) Name() string {
	return p.name
}

func (p *attributePolicy) String() string {
	if p.name == "" {
		return fmt.Sprintf("AttributePolicy(%s.%s %s)", p.context, p.originalName, p.compare.String())
	}
	return fmt.Sprintf("AttributePolicy(%s: %s.%s %s)", p.name, p.context, p.originalName, p.compare.String())
}

func Resolve(context string, name []string, ctx *access.PolicyContext) (any, error) {
	var attrs access.AttributeGetter
	switch context {
	case "user":
		attrs = ctx.User
	case "resource":
		attrs = ctx.Resource
	case "env":
		attrs = ctx.Environment
	case "action":
		attrs = ctx.Action
	default:
		return nil, fmt.Errorf("unknown attribute context: %s", context)
	}
	if len(name) == 0 {
		return nil, fmt.Errorf("no attribute specified")
	}
	var attr access.Attribute
	var ok bool
	current := attrs
	for i, nest := range name {
		if i == len(name)-1 {
			// last attribute
			break
		}
		attrg, ok := current.GetAttribute(nest)
		if !ok {
			return nil, fmt.Errorf("attribute not found: %s", strings.Join(name, "."))
		}
		nested, ok := attrg.(access.AttributeGetter)
		if !ok {
			return nil, fmt.Errorf("attribute is not nested: %s", strings.Join(name[:i+1], "."))
		}
		current = nested
	}
	attr, ok = current.GetAttribute(name[len(name)-1])
	if !ok {
		return nil, fmt.Errorf("attribute not found: %s", strings.Join(name, "."))
	}
	return attr.Value(), nil
}

type attributeResolver struct {
	ctx          *access.PolicyContext
	policyName   string
	auditResolve []*audit.ResolveRecord
	auditUse     []*audit.UsedValueRecord
	auditCompare []*audit.CompareResultRecord
}

func (r *attributeResolver) Resolve(name []string) (any, error) {
	if len(name) == 0 {
		return nil, fmt.Errorf("no attribute specified")
	}
	ctx := name[0]
	attrName := name[1:]
	resolved, err := Resolve(ctx, attrName, r.ctx)
	r.auditResolve = append(r.auditResolve, &audit.ResolveRecord{
		PolicyName: r.policyName,
		Context:    ctx,
		Attribute:  attrName,
		Resolved:   resolved,
		Err:        err,
	})
	if err != nil {
		return nil, err
	}
	return resolved, nil
}

func (r *attributeResolver) AuditValue(field string, value any) {
	r.auditUse = append(r.auditUse, &audit.UsedValueRecord{
		PolicyName: r.policyName,
		Field:      field,
		Value:      value,
	})
}

func (r *attributeResolver) AuditResult(condition Condition, target any, result bool, err error) {
	r.auditCompare = append(r.auditCompare, &audit.CompareResultRecord{
		Condition: condition.String(),
		Target:    target,
		Result:    result,
		Err:       err,
	})
}

func (p *attributePolicy) Satisfy(ctx *access.PolicyContext) *access.Decision {
	value, err := Resolve(p.context, p.attributeNests, ctx)
	ctx.Audit.RecordAttributeResolve(&audit.ResolveRecord{
		PolicyName: p.name,
		Context:    p.context,
		Attribute:  p.attributeNests,
		Resolved:   value,
		Err:        err,
	})
	if err != nil {
		return &access.Decision{
			Result: false,
			Reason: "failed to resolve attribute: " + err.Error(),
			Policy: p,
			Err:    err,
		}
	}
	resolver := &attributeResolver{ctx: ctx, policyName: p.name}
	result, err := p.compare.Compare(resolver, value)
	for _, record := range resolver.auditResolve {
		ctx.Audit.RecordAttributeResolve(record)
	}
	for _, record := range resolver.auditUse {
		ctx.Audit.RecordUsedValue(record)
	}
	for _, record := range resolver.auditCompare {
		ctx.Audit.RecordCompareResult(record)
	}
	if err != nil {
		return &access.Decision{
			Result: false,
			Reason: "error during attribute comparison: " + err.Error(),
			Policy: p,
			Err:    err,
		}
	}
	perm := &access.Decision{
		Result: result,
		Policy: p,
	}
	if result {
		perm.Reason = "attribute condition satisfied"
	} else {
		perm.Reason = "attribute condition not satisfied"
	}
	return perm
}
