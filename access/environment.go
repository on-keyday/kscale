package access

type envrionment struct {
	AttributeGetter
}

var _ Environment = (*envrionment)(nil)

func NewEnvironment(attrs []Attribute) Environment {
	return &envrionment{
		AttributeGetter: NewAttributeManager("env", attrs),
	}
}
