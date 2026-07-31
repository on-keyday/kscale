package access

type user struct {
	authority Authority
	AttributeGetter
}

var _ UserContext = (*user)(nil)

func NewUserContext(authority Authority, attrs []Attribute) UserContext {
	return &user{
		authority:       authority,
		AttributeGetter: NewAttributeManager("user", attrs),
	}
}

func (u *user) GetAttribute(name string) (Attribute, bool) {
	if name == "authority" {
		return NewUserAuthorityAttribute(u.authority), true
	}
	return u.AttributeGetter.GetAttribute(name)
}

func (u *user) Authority() Authority {
	return u.authority
}

type userAuthorityAttribute struct {
	a Authority
}

func (aa *userAuthorityAttribute) Name() string {
	return "authority"
}

func (aa *userAuthorityAttribute) Value() any {
	return aa.a.FullName().String()
}

func (aa *userAuthorityAttribute) GetAttribute(name string) (Attribute, bool) {
	if name == "name" {
		return NewAttribute("name", aa.a.Name()), true
	}
	if name == "full_name" {
		// domain-last (DNS) form, e.g. "monitor.manager.ca.admin" — same ordering as the
		// cert CN and `authority list` output. Policies match it with `suffix`.
		return NewAttribute("full_name", aa.a.FullName().CommonName()), true
	}
	return nil, false
}

func NewUserAuthorityAttribute(a Authority) Attribute {
	return &userAuthorityAttribute{
		a: a,
	}
}

type authorityAttributeMapper struct {
	a Authority
}

func (aa *authorityAttributeMapper) Name() string {
	return "authority"
}

func (aa *authorityAttributeMapper) Value() any {
	return aa.a.FullName().CommonName()
}

func (aa *authorityAttributeMapper) GetAttribute(name string) (Attribute, bool) {
	if name == "" {
		// Leading-dot domain-last form for descendant checks: a child's full_name (DNS
		// order) ends with "." + this authority's full_name, so policies use
		// `$user.authority.full_name suffix $env.authority.<path>.` (was prefix + trailing
		// dot when full_name was domain-first).
		return NewAttribute("full_name_suffix", "."+aa.a.FullName().CommonName()), true
	}
	child, ok := aa.a.GetChild(name)
	if ok {
		return NewAuthorityAttributeMapper(child), true
	}
	return nil, false
}

func NewAuthorityAttributeMapper(a Authority) Attribute {
	return &authorityAttributeMapper{
		a: a,
	}
}
