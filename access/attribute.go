package access

import (
	"fmt"
	"strconv"
)

type attribute struct {
	key   string
	value any
}

var _ Attribute = (*attribute)(nil)

var AttributeValueChecker func(any)

func NewAttribute(key string, value any) Attribute {
	if AttributeValueChecker != nil {
		AttributeValueChecker(value)
	}
	return &attribute{
		key:   key,
		value: value,
	}
}

func (a *attribute) Name() string {
	return a.key
}

func (a *attribute) Value() any {
	return a.value
}

type dynamicAttribute struct {
	key    string
	getter func() any
}

var _ Attribute = (*dynamicAttribute)(nil)

func NewDynamicAttribute(key string, getter func() any) Attribute {
	return &dynamicAttribute{
		key:    key,
		getter: getter,
	}
}

func NewTypedDynamicAttribute[T any](key string, getter func() T) Attribute {
	if AttributeValueChecker != nil {
		AttributeValueChecker(getter())
	}
	return &dynamicAttribute{
		key: key,
		getter: func() any {
			return getter()
		},
	}
}

func (a *dynamicAttribute) Name() string {
	return a.key
}

func (a *dynamicAttribute) Value() any {
	return a.getter()
}

type attributeManager struct {
	name       string
	attributes map[string]Attribute
}

var _ AttributeGetter = (*attributeManager)(nil)

func NewAttributeManager(name string, attrs []Attribute) AttributeGetter {
	am := &attributeManager{
		name:       name,
		attributes: make(map[string]Attribute),
	}
	for _, attr := range attrs {
		am.attributes[attr.Name()] = attr
	}
	return am
}

func (am *attributeManager) Name() string {
	return am.name
}

func (am *attributeManager) Value() any {
	return nil
}

func (am *attributeManager) ListAttributes() []Attribute {
	result := make([]Attribute, 0, len(am.attributes))
	for _, attr := range am.attributes {
		result = append(result, attr)
	}
	return result
}

func (am *attributeManager) GetAttribute(name string) (Attribute, bool) {
	attr, ok := am.attributes[name]
	return attr, ok
}

type overriddenAttributeGetter struct {
	inner    AttributeGetter
	override map[string]Attribute
}

var _ AttributeGetter = (*overriddenAttributeGetter)(nil)

func NewOverriddenAttributeGetter(inner AttributeGetter, override map[string]Attribute) AttributeGetter {
	return &overriddenAttributeGetter{
		inner:    inner,
		override: override,
	}
}

func (o *overriddenAttributeGetter) Name() string {
	return o.inner.Name()
}

func (o *overriddenAttributeGetter) Value() any {
	return o.inner.Value()
}

func (o *overriddenAttributeGetter) GetAttribute(name string) (Attribute, bool) {
	if attr, ok := o.override[name]; ok {
		return attr, true
	}
	return o.inner.GetAttribute(name)
}

type StringArrayAttribute struct {
	name   string
	values []string
	conv   map[string]func(any) (any, error)
}

var _ Attribute = (*StringArrayAttribute)(nil)

func NewStringArrayAttribute(name string, values []string, conv map[string]func(any) (any, error)) Attribute {
	return &StringArrayAttribute{
		name:   name,
		values: values,
		conv:   conv,
	}
}

func (a *StringArrayAttribute) Name() string {
	return a.name
}

func (a *StringArrayAttribute) Value() any {
	return a.values
}

func (a *StringArrayAttribute) GetAttribute(name string) (Attribute, bool) {
	if name == "len" {
		return NewAttribute(fmt.Sprintf("%s.len", a.name), int64(len(a.values))), true
	}
	// parse index
	var index uint64
	var err error
	if index, err = strconv.ParseUint(name, 10, 0); err != nil {
		return nil, false
	}
	if index >= uint64(len(a.values)) {
		return nil, false
	}
	return NewConvertibleAttribute(NewAttribute(fmt.Sprintf("%s.%d", a.name, index), a.values[index]), a.conv), true
}

type convertibleAttribute struct {
	Attribute
	convertr map[string]func(any) (any, error)
}

var _ Attribute = (*convertibleAttribute)(nil)

func NewConvertibleAttribute(attr Attribute, conv map[string]func(any) (any, error)) Attribute {
	return &convertibleAttribute{
		Attribute: attr,
		convertr:  conv,
	}
}

func (a *convertibleAttribute) Name() string {
	return a.Attribute.Name()
}

func (a *convertibleAttribute) Value() any {
	return a.Attribute.Value()
}

func (a *convertibleAttribute) GetAttribute(name string) (Attribute, bool) {
	if conv, ok := a.convertr[name]; ok {
		convertedValue, err := conv(a.Value())
		if err != nil {
			return nil, false
		}
		return NewAttribute(fmt.Sprintf("%s.%s", a.Name(), name), convertedValue), true
	}
	return nil, false
}

type mapAttribute struct {
	name  string
	value map[string]any
}

var _ Attribute = (*mapAttribute)(nil)

func NewMapAttribute(name string, value map[string]any) Attribute {
	return &mapAttribute{
		name:  name,
		value: value,
	}
}

func (a *mapAttribute) Name() string {
	return a.name
}

func (a *mapAttribute) Value() any {
	result := make(map[string]any)
	for k, v := range a.value {
		result[k] = v
	}
	return result
}

func (a *mapAttribute) GetAttribute(name string) (Attribute, bool) {
	attr, ok := a.value[name]
	if nested, ok := attr.(map[string]any); ok {
		return NewMapAttribute(fmt.Sprintf("%s.%s", a.name, name), nested), true
	}
	return NewAttribute(name, attr), ok
}
