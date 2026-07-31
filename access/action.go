package access

type action struct {
	name string
	args []string
}

var _ Action = (*action)(nil)

func NewAction(name string, args []string) Action {
	return &action{
		name: name,
		args: args,
	}
}

func (a *action) Name() string {
	return a.name
}

func (a *action) Value() any {
	return a.name
}

func (a *action) Args() []string {
	return a.args
}

func (a *action) GetAttribute(name string) (Attribute, bool) {
	if name == "name" {
		return NewAttribute("name", a.name), true
	}
	if name == "args" {
		return NewStringArrayAttribute("args", a.args, map[string]func(any) (any, error){}), true
	}
	return nil, false
}
