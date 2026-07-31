package policy

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Condition interface {
	Name() string
	Compare(resolver Resolver, target any) (bool, error)
	json.Unmarshaler
	UnmarshalFromMap(data map[string]any) error
	ParseDirect(args []any) error
	Precompile() error
	Parse(args []string) error
	fmt.Stringer
}

type GenericCondition struct {
	Op        string    `json:"op"`
	Condition Condition `json:"condition"`
}

var _ Condition = (*GenericCondition)(nil)

func (gc *GenericCondition) UnmarshalJSON(data []byte) error {
	if gc == nil {
		return fmt.Errorf("nil GenericCondition")
	}
	type RawGenericCondition struct {
		Op         string          `json:"op"`
		RawMessage json.RawMessage `json:"condition"`
	}
	var raw RawGenericCondition
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	factory, ok := CompareOpFactory[raw.Op]
	if !ok {
		return fmt.Errorf("unknown compare operation: %s", raw.Op)
	}
	cond := factory()
	if err := json.Unmarshal(raw.RawMessage, cond); err != nil {
		return err
	}
	gc.Condition = cond
	return nil
}

func (gc *GenericCondition) UnmarshalFromMap(data map[string]any) error {
	if gc == nil {
		return fmt.Errorf("nil GenericCondition")
	}
	opAny, ok := data["op"]
	if !ok {
		return fmt.Errorf("missing 'op' field in condition")
	}
	op, ok := opAny.(string)
	if !ok {
		return fmt.Errorf("'op' field must be a string")
	}
	factory, ok := CompareOpFactory[op]
	if !ok {
		return fmt.Errorf("unknown compare operation: %s", op)
	}
	cond := factory()
	if err := cond.UnmarshalFromMap(data); err != nil {
		return err
	}
	gc.Op = op
	gc.Condition = cond
	return nil
}

func (gc *GenericCondition) Compare(resolver Resolver, target any) (bool, error) {
	if gc == nil || gc.Condition == nil {
		return false, fmt.Errorf("condition is not initialized")
	}
	return gc.Condition.Compare(resolver, target)
}

func (gc *GenericCondition) Precompile() error {
	if gc == nil || gc.Condition == nil {
		return fmt.Errorf("condition is not initialized")
	}
	return gc.Condition.Precompile()
}

func (gc *GenericCondition) Parse(args []string) error {
	if gc == nil {
		return fmt.Errorf("condition is not initialized")
	}
	if len(args) == 0 {
		return fmt.Errorf("expected at least 1 argument, got 0")
	}
	op := args[0]
	if aliasOp, ok := CompareOpAlias[op]; ok {
		op = aliasOp
	}
	factory, ok := CompareOpFactory[op]
	if !ok {
		return fmt.Errorf("unknown compare operation: %s", op)
	}
	cond := factory()
	if err := cond.Parse(args[1:]); err != nil {
		return err
	}
	gc.Op = op
	gc.Condition = cond
	return nil
}

func (gc *GenericCondition) ParseDirect(args []any) error {
	if gc == nil {
		return fmt.Errorf("condition is not initialized")
	}
	if len(args) == 0 {
		return fmt.Errorf("expected at least 1 argument, got 0")
	}
	opAny := args[0]
	op, ok := opAny.(string)
	if !ok {
		return fmt.Errorf("first argument must be a string representing operation")
	}
	if aliasOp, ok := CompareOpAlias[op]; ok {
		op = aliasOp
	}
	factory, ok := CompareOpFactory[op]
	if !ok {
		return fmt.Errorf("unknown compare operation: %s", op)
	}
	cond := factory()
	if err := cond.ParseDirect(args[1:]); err != nil {
		return err
	}
	gc.Op = op
	gc.Condition = cond
	return nil
}

func (gc *GenericCondition) Name() string {
	if gc == nil || gc.Condition == nil {
		return "uninitialized"
	}
	return gc.Op
}

func (gc *GenericCondition) String() string {
	if gc == nil || gc.Condition == nil {
		return "uninitialized condition"
	}
	if gc.Op == gc.Condition.Name() {
		return gc.Condition.String()
	}
	return fmt.Sprintf("!!%s != %s", gc.Op, gc.Condition.String())
}

type MaybeVariable[T any, U any] struct {
	Value            T
	PrecompiledValue U
	Precompiled      bool
	Variable         []string
}

func (mv *MaybeVariable[T, U]) IsVariable() bool {
	return len(mv.Variable) > 0
}

func (mv *MaybeVariable[T, U]) UnmarshalJSON(data []byte) error {
	type VariableObj struct {
		Var string `json:"var"`
	}
	var varObj VariableObj
	if err := json.Unmarshal(data, &varObj); err == nil && varObj.Var != "" {
		mv.Variable = strings.Split(varObj.Var, ".")
		return nil
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	mv.Value = value
	return nil
}

func (mv *MaybeVariable[T, U]) ParseStringList(parse func([]string) (T, error), inputs []string) error {
	if len(inputs) >= 1 && strings.HasPrefix(inputs[0], "$") {
		if len(inputs) != 1 {
			return fmt.Errorf("variable input should be a single variable name, got %d inputs", len(inputs))
		}
		mv.Variable = strings.Split(inputs[0][1:], ".")
		return nil
	}
	value, err := parse(inputs)
	if err != nil {
		return err
	}
	mv.Value = value
	return nil
}

func (mv *MaybeVariable[T, U]) ParseString(parse func(string) (T, error), input string) error {
	if len(input) >= 2 && input[0] == '$' {
		mv.Variable = strings.Split(input[1:], ".")
		return nil
	}
	value, err := parse(input)
	if err != nil {
		return err
	}
	mv.Value = value
	return nil
}

func (mv *MaybeVariable[T, U]) String() string {
	if mv.IsVariable() {
		return fmt.Sprintf("Variable(%s)", strings.Join(mv.Variable, "."))
	}
	return fmt.Sprintf("Value(%v)", mv.Value)
}

type Resolver interface {
	Resolve(variable []string) (any, error)
	AuditValue(name string, value any)
	AuditResult(condition Condition, target any, ok bool, err error)
}

type ResolverFunc func(variable string) (any, error)

func (rf ResolverFunc) Resolve(variable string) (any, error) {
	return rf(variable)
}

func mayUnwrap(x any) any {
	if uw, ok := x.(interface{ UnwrapValue() any }); ok {
		return uw.UnwrapValue()
	}
	return x
}

func (mv *MaybeVariable[T, U]) GetValue(resolver Resolver, field string) (T, error) {
	if mv.IsVariable() {
		x, err := resolver.Resolve(mv.Variable)
		if err != nil {
			var zero T
			return zero, fmt.Errorf("failed to resolve variable %s: %w", mv.Variable, err)
		}
		x = mayUnwrap(x)
		val, ok := x.(T)
		if !ok {
			var zero T
			return zero, fmt.Errorf("variable %s has invalid type: expected %T, got %T", mv.Variable, zero, x)
		}
		resolver.AuditValue(field, val)
		return val, nil
	}
	resolver.AuditValue(field, mv.Value)
	return mv.Value, nil
}

func (mv *MaybeVariable[T, U]) Precompile(precompileFunc func(T) (U, error)) error {
	if mv.IsVariable() {
		// cannot precompile variable
		return nil
	}
	// anyway, overwrite precompiled value
	val, err := precompileFunc(mv.Value)
	if err != nil {
		return fmt.Errorf("failed to precompile value: %w", err)
	}
	mv.PrecompiledValue = val
	mv.Precompiled = true
	return nil
}

func (mv *MaybeVariable[T, U]) GetPrecompiledValue(resolver Resolver, field string, precompileFunc func(T) (U, error)) (U, error) {
	if mv.IsVariable() {
		x, err := resolver.Resolve(mv.Variable)
		if err != nil {
			var zero U
			return zero, fmt.Errorf("failed to resolve variable %s: %w", mv.Variable, err)
		}
		x = mayUnwrap(x)
		// fast fetch precompiled value
		u, ok := x.(U)
		if ok {
			return u, nil
		}
		// otherwise, try to compile
		t, ok := x.(T)
		if !ok {
			var zero U
			return zero, fmt.Errorf("variable %s has invalid type: expected %T, got %T", mv.Variable, *new(T), x)
		}
		val, err := precompileFunc(t)
		if err != nil {
			var zero U
			return zero, fmt.Errorf("failed to precompile variable %s: %w", mv.Variable, err)
		}
		resolver.AuditValue(field, val)
		return val, nil
	}
	if mv.Precompiled {
		resolver.AuditValue(field, mv.PrecompiledValue)
		return mv.PrecompiledValue, nil
	}
	val, err := precompileFunc(mv.Value)
	if err != nil {
		var zero U
		return zero, fmt.Errorf("failed to precompile value: %w", err)
	}
	resolver.AuditValue(field, val)
	return val, nil
}

func (mv *MaybeVariable[T, U]) ParseAny(input any) error {
	input = mayUnwrap(input)
	v, ok := input.(T)
	if ok {
		mv.Value = v
		return nil
	}
	m, ok := input.(map[string]any)
	if !ok {
		return fmt.Errorf("expected %T or map[string]any, got %T", *new(T), input)
	}
	varObj, ok := m["var"]
	if !ok {
		return fmt.Errorf("expected 'var' key in map")
	}
	varName, ok := varObj.(string)
	if !ok {
		return fmt.Errorf("'var' value must be a string")
	}
	mv.Variable = strings.Split(varName, ".")
	return nil
}
