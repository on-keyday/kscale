package policy

import "testing"

func TestParser(t *testing.T) {
	p := NewParser("$target str_eq \"value\"")
	target, cond, err := p.ParseSingleCondition()
	if err != nil {
		t.Fatalf("failed to parse condition: %v", err)
	}
	if target != "target" {
		t.Fatalf("expected target 'target', got '%s'", target)
	}
	if cond == nil {
		t.Fatal("expected condition, got nil")
	}
	if cond.Op != OpStringEquals {
		t.Fatalf("expected OpStringEquals, got %v", cond.Op)
	}
	p = NewParser("$num == 42")
	target, cond, err = p.ParseSingleCondition()
	if err != nil {
		t.Fatalf("failed to parse condition: %v", err)
	}
	if target != "num" {
		t.Fatalf("expected target 'num', got '%s'", target)
	}
	if cond == nil {
		t.Fatal("expected condition, got nil")
	}
	if cond.Op != OpIntegerEquals {
		t.Fatalf("expected OpIntegerEquals, got %v", cond.Op)
	}
	intEq, ok := cond.Condition.(*ConditionIntegerEquals)
	if !ok {
		t.Fatalf("expected ConditionIntegerEquals, got %T", cond.Condition)
	}
	if intEq.Value.Value != 42 {
		t.Fatalf("expected value 42, got %d", intEq.Value.Value)
	}
	p = NewParser("$flt == 3.14")
	target, cond, err = p.ParseSingleCondition()
	if err != nil {
		t.Fatalf("failed to parse condition: %v", err)
	}
	if target != "flt" {
		t.Fatalf("expected target 'flt', got '%s'", target)
	}
	if cond == nil {
		t.Fatal("expected condition, got nil")
	}
	if cond.Op != OpNumericEquals {
		t.Fatalf("expected OpNumericEquals, got %v", cond.Op)
	}
	numEq, ok := cond.Condition.(*ConditionNumericEquals)
	if !ok {
		t.Fatalf("expected ConditionNumericEquals, got %T", cond.Condition)
	}
	if numEq.Value.Value != 3.14 {
		t.Fatalf("expected value 3.14, got %f", numEq.Value.Value)
	}
	p = NewParser("$user.item in [\"a\",\"b\",\"c\"]")
	target, cond, err = p.ParseSingleCondition()
	if err != nil {
		t.Fatalf("failed to parse condition: %v", err)
	}
	if target != "user.item" {
		t.Fatalf("expected target 'user.item', got '%s'", target)
	}
	if cond == nil {
		t.Fatal("expected condition, got nil")
	}
	if cond.Op != OpStringInList {
		t.Fatalf("expected OpStringInList, got %v", cond.Op)
	}
	strListCond, ok := cond.Condition.(*ConditionStringInList)
	if !ok {
		t.Fatalf("expected ConditionStringInList, got %T", cond.Condition)
	}
	expectedList := []string{"a", "b", "c"}
	if len(strListCond.AllowedList.Value) != len(expectedList) {
		t.Fatalf("expected list length %d, got %d", len(expectedList), len(strListCond.AllowedList.Value))
	}
	for i, v := range expectedList {
		if strListCond.AllowedList.Value[i] != v {
			t.Fatalf("expected list value %s at index %d, got %s", v, i, strListCond.AllowedList.Value[i])
		}
	}
	p = NewParser("$item startswith \"pre\"")
	target, cond, err = p.ParseSingleCondition()
	if err != nil {
		t.Fatalf("failed to parse condition: %v", err)
	}
	if target != "item" {
		t.Fatalf("expected target 'item', got '%s'", target)
	}
	if cond == nil {
		t.Fatal("expected condition, got nil")
	}
	if cond.Op != OpStringPrefix {
		t.Fatalf("expected OpStringPrefix, got %v", cond.Op)
	}
	prefixCond, ok := cond.Condition.(*ConditionStringPrefix)
	if !ok {
		t.Fatalf("expected ConditionStringPrefix, got %T", cond.Condition)
	}
	if prefixCond.Prefix.Value != "pre" {
		t.Fatalf("expected prefix 'pre', got '%s'", prefixCond.Prefix.Value)
	}
}
