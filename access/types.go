package access

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/on-keyday/kscale/access/audit"
)

// Not restrict in interface level
// but restrict in implementation level

type AttributeGetter interface {
	Attribute
	GetAttribute(name string) (Attribute, bool)
}

// UserContext represents the user making the access request
type UserContext interface {
	AttributeGetter
	Authority() Authority
}

// Environment of access request
type Environment interface {
	AttributeGetter
}

// Action to be performed on resource
type Action interface {
	Name() string
	Args() []string
	AttributeGetter
}

// Context for policy decision
type PolicyContext struct {
	Context     context.Context // for API compatibility
	User        UserContext
	Environment Environment
	Resource    Resource
	Action      Action
	Audit       audit.PolicyAudit
}

// Decision result of policy decision
type Decision struct {
	Result  bool
	Reason  string
	Err     error // error during evaluation
	Policy  Policy
	Derived []*Decision
}

func (d *Decision) String() string {
	b := strings.Builder{}
	d.stringInternal(0, &b)
	return b.String()
}

func (d *Decision) writeIndent(indent int, b *strings.Builder) {
	for i := 0; i < indent; i++ {
		b.WriteString("  ")
	}
}

func (d *Decision) stringInternal(indent int, b *strings.Builder) {
	d.writeIndent(indent, b)
	b.WriteString(fmt.Sprintf("Policy: %s, Decision: %v, Reason: %s\n", d.Policy.Name(), d.Result, d.Reason))
	if d.Err != nil {
		d.writeIndent(indent, b)
		b.WriteString(fmt.Sprintf("Error: %v\n", d.Err))
	}
	if indent == 0 {
		b.WriteString(fmt.Sprintf("Policy Detail: %s\n", d.Policy.String()))
	}
	if len(d.Derived) == 0 {
		return
	}
	d.writeIndent(indent, b)
	b.WriteString("Derived From:\n")
	for i, derived := range d.Derived {
		d.writeIndent(indent+1, b)
		b.WriteString(fmt.Sprintf("(%d):\n", i+1))
		derived.stringInternal(indent+2, b)
	}
}

// Policy to make access decision
type Policy interface {
	Name() string
	Satisfy(ctx *PolicyContext) *Decision
	fmt.Stringer
}

// PolicySet is a collection of policies.
// itself is a policy.
// handled as AND, OR, or XOR by underlying implementation.
type PolicySet interface {
	Policy
	AddPolicy(p Policy) error
	ListPolicies() []Policy
	RemovePolicy(name string)
}

type FullName struct {
	Names []string
}

func (fn *FullName) CommonName() string {
	result := ""
	for i := len(fn.Names) - 1; i >= 0; i-- {
		if i < len(fn.Names)-1 {
			result += "."
		}
		result += fn.Names[i]
	}
	return result
}

func ParseCommonNameToFullNameStrict(commonName string, domain string) (*FullName, error) {
	commonName = strings.ToLower(commonName)
	domain = strings.ToLower(domain)
	if !strings.HasSuffix(commonName, "."+domain) {
		return nil, fmt.Errorf("invalid common name: %s", commonName)
	}
	commonName = strings.TrimSuffix(commonName, "."+domain)
	if commonName == "" {
		return nil, fmt.Errorf("invalid common name: %s", commonName)
	}
	parts := strings.Split(commonName, ".")
	if len(parts) == 0 {
		return nil, fmt.Errorf("invalid common name: %s", commonName)
	}
	slices.Reverse(parts)
	return &FullName{
		Names: parts,
	}, nil
}

func ParseCommonNameToFullName(commonName string, domain string) *FullName {
	commonName = strings.ToLower(commonName)
	domain = strings.ToLower(domain)
	commonName = strings.TrimSuffix(commonName, "."+domain)
	parts := strings.Split(commonName, ".")
	slices.Reverse(parts)
	return &FullName{
		Names: parts,
	}
}

func NewFullName(name []string) *FullName {
	return &FullName{
		Names: name,
	}
}

func (fn *FullName) String() string {
	result := ""
	for i, name := range fn.Names {
		if i > 0 {
			result += "."
		}
		result += name
	}
	return result
}

// Users authority hierarchy
type Authority interface {
	Name() string
	FullName() *FullName
	Children() []Authority
	GetChild(name string) (Authority, bool)
	Parent() Authority
	CreateChild(name string) (Authority, error)
	CreateLeafChild(name string) (Authority, error)
	IsLeaf() bool
	RemoveChild(name string) error
}

type RootAuthority interface {
	Authority
	GetDescendant(fullName *FullName) (Authority, bool)
	CreateDescendant(fullName *FullName) (Authority, error)
	CreateLeafDescendant(fullName *FullName, makeParent bool) (Authority, error)
	RemoveDescendant(fullName *FullName) error
}

// Attribute is a key-value pair associated with user, resource, or environment
type Attribute interface {
	Name() string
	Value() any
}

// Resource to be accessed
// its has own attributes and allowed actions
type Resource interface {
	Name() string
	AttributeGetter
	Actions() []Action
	BaseTemplate() ResourceTemplate
}

// ResourceTemplate is a template for resources
// it can create resource instances by specifying name and attributes
// itself is also a Resource
// ResourceTemplate is defined by system, while Resource is created based on ResourceTemplate
type ResourceTemplate interface {
	Resource
	CreateInstance(name string, attrs []Attribute) Resource
}
