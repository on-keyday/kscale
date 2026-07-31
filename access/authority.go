package access

import (
	"fmt"
	"sync"
)

type leafAuthority struct {
	name   string
	parent Authority
}

var _ Authority = (*leafAuthority)(nil)

func (a *leafAuthority) Name() string {
	return a.name
}

func (a *leafAuthority) Parent() Authority {
	return a.parent
}

func (a *leafAuthority) Children() []Authority {
	return []Authority{}
}

func (a *leafAuthority) GetChild(name string) (Authority, bool) {
	return nil, false
}

func (a *leafAuthority) RemoveChild(name string) error {
	return fmt.Errorf("leaf authority has no children to remove: %s", a.name)
}

func (a *leafAuthority) FullName() *FullName {
	if a.parent == nil {
		return &FullName{Names: []string{a.name}}
	}
	parentFullName := a.parent.FullName()
	parentFullName.Names = append(parentFullName.Names, a.name)
	return parentFullName
}

type ErrCannotCreateChildAuthority struct {
	name string
}

func (e ErrCannotCreateChildAuthority) Error() string {
	return "cannot create child authority for leaf authority: " + e.name
}

func (a *leafAuthority) CreateChild(name string) (Authority, error) {
	return nil, ErrCannotCreateChildAuthority{name: a.name}
}

func (a *leafAuthority) CreateLeafChild(name string) (Authority, error) {
	return nil, ErrCannotCreateChildAuthority{name: a.name}
}

func (a *leafAuthority) IsLeaf() bool {
	return true
}

type intermediateAuthority struct {
	name          string
	parent        Authority
	authorityLock sync.RWMutex
	children      map[string]Authority
}

var _ Authority = (*intermediateAuthority)(nil)

func (a *intermediateAuthority) Name() string {
	return a.name
}

func (a *intermediateAuthority) Parent() Authority {
	return a.parent
}

func (a *intermediateAuthority) GetChild(name string) (Authority, bool) {
	a.authorityLock.RLock()
	defer a.authorityLock.RUnlock()
	child, ok := a.children[name]
	return child, ok
}

func (a *intermediateAuthority) Children() []Authority {
	a.authorityLock.RLock()
	defer a.authorityLock.RUnlock()
	children := make([]Authority, 0, len(a.children))
	for _, child := range a.children {
		children = append(children, child)
	}
	return children
}

func (a *intermediateAuthority) RemoveChild(name string) error {
	a.authorityLock.Lock()
	defer a.authorityLock.Unlock()
	if _, exists := a.children[name]; !exists {
		return fmt.Errorf("child authority not found: %s", name)
	}
	if len(a.children[name].Children()) > 0 {
		return fmt.Errorf("cannot remove authority with existing children: %s", name)
	}
	delete(a.children, name)
	return nil
}

func (a *intermediateAuthority) FullName() *FullName {
	if a.parent == nil {
		return &FullName{Names: []string{a.name}}
	}
	parentFullName := a.parent.FullName()
	parentFullName.Names = append(parentFullName.Names, a.name)
	return parentFullName
}

func (a *intermediateAuthority) IsLeaf() bool {
	return false
}

type ErrDuplicateAuthorityName struct {
	name string
}

func (e ErrDuplicateAuthorityName) Error() string {
	return "duplicate authority name: " + e.name
}

func (a *intermediateAuthority) CreateChild(name string) (Authority, error) {
	a.authorityLock.Lock()
	defer a.authorityLock.Unlock()
	if a.children == nil {
		a.children = make(map[string]Authority)
	}
	if _, exists := a.children[name]; exists {
		return nil, ErrDuplicateAuthorityName{name: name}
	}
	child := &intermediateAuthority{
		name:   name,
		parent: a,
	}
	a.children[name] = child
	return child, nil
}

func (a *intermediateAuthority) CreateLeafChild(name string) (Authority, error) {
	a.authorityLock.Lock()
	defer a.authorityLock.Unlock()
	if a.children == nil {
		a.children = make(map[string]Authority)
	}
	if _, exists := a.children[name]; exists {
		return nil, ErrDuplicateAuthorityName{name: name}
	}
	child := &leafAuthority{
		name:   name,
		parent: a,
	}
	a.children[name] = child
	return child, nil
}

type rootAuthority struct {
	intermediateAuthority
}

var _ RootAuthority = (*rootAuthority)(nil)

func NewRootAuthority(name string) RootAuthority {
	return &rootAuthority{
		intermediateAuthority: intermediateAuthority{
			name:     name,
			parent:   nil,
			children: make(map[string]Authority),
		},
	}
}

func (r *rootAuthority) GetDescendant(fullName *FullName) (Authority, bool) {
	current := Authority(r)
	for _, name := range fullName.Names {
		child, ok := current.GetChild(name)
		if !ok {
			return nil, false
		}
		current = child
	}
	return current, true
}

func (r *rootAuthority) CreateDescendant(fullName *FullName) (Authority, error) {
	current := Authority(r)
	for _, name := range fullName.Names {
		child, ok := current.GetChild(name)
		if !ok {
			var err error
			child, err = current.CreateChild(name)
			if err != nil {
				return nil, err
			}
		}
		current = child
	}
	return current, nil
}

func (r *rootAuthority) CreateLeafDescendant(fullName *FullName, makeParent bool) (Authority, error) {
	current := Authority(r)
	for i, name := range fullName.Names {
		child, ok := current.GetChild(name)
		if !ok {
			var err error
			if i == len(fullName.Names)-1 {
				child, err = current.CreateLeafChild(name)
				if err != nil {
					return nil, err
				}
			} else {
				if !makeParent {
					return nil, ErrCannotCreateChildAuthority{name: current.Name()}
				}
				child, err = current.CreateChild(name)
				if err != nil {
					return nil, err
				}
			}
		}
		current = child
	}
	return current, nil
}

func (r *rootAuthority) RemoveDescendant(fullName *FullName) error {
	if len(fullName.Names) == 0 {
		return fmt.Errorf("cannot remove root authority")
	}
	parentNames := fullName.Names[:len(fullName.Names)-1]
	childName := fullName.Names[len(fullName.Names)-1]
	parentAuthority, ok := r.GetDescendant(&FullName{Names: parentNames})
	if !ok {
		return fmt.Errorf("parent authority not found: %s", parentNames)
	}
	return parentAuthority.RemoveChild(childName)
}
