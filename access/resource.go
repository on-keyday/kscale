package access

type resource struct {
	original ResourceTemplate
	name     string
	AttributeGetter
	actions []Action
}

var _ Resource = (*resource)(nil)

func (r *resource) Name() string {
	return r.name
}

func (r *resource) GetAttribute(name string) (Attribute, bool) {
	if name == "name" {
		return NewAttribute("name", r.name), true
	}
	return r.AttributeGetter.GetAttribute(name)
}

func (r *resource) Actions() []Action {
	return r.actions
}

func (r *resource) BaseTemplate() ResourceTemplate {
	return r.original
}

type resourceTemplate struct {
	parent  ResourceTemplate
	name    string
	actions []Action
	AttributeGetter
}

var _ ResourceTemplate = (*resourceTemplate)(nil)

func NewResourceTemplate(name string, parent ResourceTemplate, attrs []Attribute, actions []Action) ResourceTemplate {
	return &resourceTemplate{
		name:            name,
		parent:          parent,
		AttributeGetter: NewAttributeManager(name, attrs),
		actions:         actions,
	}
}

func (rt *resourceTemplate) GetAttribute(name string) (Attribute, bool) {
	if name == "name" {
		return NewAttribute("name", rt.name), true
	}
	return rt.AttributeGetter.GetAttribute(name)
}

func (rt *resourceTemplate) Name() string {
	return rt.name
}

func (rt *resourceTemplate) Parent() ResourceTemplate {
	return rt.parent
}

func (rt *resourceTemplate) Actions() []Action {
	return rt.actions
}

func (rt *resourceTemplate) BaseTemplate() ResourceTemplate {
	return rt.parent
}

func (rt *resourceTemplate) CreateInstance(name string, attrs []Attribute) Resource {
	return &resource{
		original:        rt,
		name:            name,
		AttributeGetter: NewAttributeManager(name, attrs),
		actions:         rt.actions,
	}
}
