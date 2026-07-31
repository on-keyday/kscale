package policy_test

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/kscale/access/policy"
)

func TestCompareOpAliases(t *testing.T) {
	for alias, op := range policy.CompareOpAlias {
		if _, ok := policy.CompareOpFactory[op]; !ok {
			t.Errorf("alias %s points to unknown operation %s", alias, op)
		}
	}
}

func TestParseCompareArgCount(t *testing.T) {
	// zero arg to GenericCondition.Parse
	var nilGC *policy.GenericCondition = nil
	err := nilGC.Parse([]string{"UnknownOp"})
	if err == nil {
		t.Errorf("expected error for nil GenericCondition, got none")
	}
	gc := &policy.GenericCondition{}
	err = gc.Parse([]string{})
	if err == nil {
		t.Errorf("expected error for zero arguments, got none")
	}
	err = gc.Parse([]string{"UnknownOp"})
	if err == nil {
		t.Errorf("expected error for unknown operation, got none")
	}
	err = gc.Precompile()
	if err == nil {
		t.Errorf("expected error for uninitialized condition, got none")
	}
	for op := range policy.CompareOpFactory {
		argCount := policy.CompareOpArgCount[op]
		var args = []string{op}
		if argCount < 0 {
			args = append(args, "\"arg1\"", "\"arg2\"")
			gc := &policy.GenericCondition{}
			err := gc.Parse(args)
			if err != nil {
				t.Errorf("expected success for operation %s with variable args, got error: %v", op, err)
			}
			err = gc.ParseDirect([]any{op, []string{"arg1", "arg2"}})
			if err != nil {
				t.Errorf("expected success for direct parse operation %s with variable args, got error: %v", op, err)
			}
			err = gc.ParseDirect([]any{op, []string{}, []string{}})
			if err == nil {
				t.Errorf("expected error for direct parse operation %s with insufficient variable args, got none", op)
			}
			err = gc.UnmarshalFromMap(map[string]any{
				"op":                        op,
				policy.CompareOpArgs[op][0]: []string{"arg1", "arg2"},
			})
			if err != nil {
				t.Errorf("expected success for UnmarshalFromMap operation %s with variable args, got error: %v", op, err)
			}
			wrongArgs := []string{op, "arg1", "arg2"}
			err = gc.Parse(wrongArgs)
			if err == nil {
				t.Errorf("expected error for operation %s with wrong args, got none", op)
			}
			continue
		}
		if argCount > 0 {
			args = append(args, make([]string, argCount-1)...)
		} else {
			// add one extra argument to test too many args
			args = append(args, "extra")
		}
		gc := &policy.GenericCondition{}
		err := gc.Parse(args)
		if err == nil {
			t.Errorf("expected error for operation %s, got none", op)
		}
		var wrongArgs []string = []string{op}
		var wrongArgs2 []string = []string{op}
		var directArgs []any = []any{op}
		var wrongDirectArgs []any = []any{op}
		if argCount > 0 {
			// exact arg count
			directArgs = append(directArgs, make([]any, argCount)...)
			args = append([]string{op}, make([]string, argCount)...)
			wrongArgs = append([]string{op}, make([]string, argCount)...)
			wrongArgs2 = append([]string{op}, make([]string, argCount)...)
			wrongDirectArgs = append([]any{op}, make([]any, argCount)...)
			for i := 1; i <= argCount; i++ {
				if strings.Contains(op, "Integer") {
					args[i] = "123"
					wrongArgs[i] = "abc"
					if i == 1 && argCount > 1 {
						wrongArgs2[i] = "123"
					} else {
						wrongArgs2[i] = "123.45.67"
					}
					directArgs[i] = int64(123)
					if i == 1 && argCount > 1 {
						wrongDirectArgs[i] = int64(123)
					} else {
						wrongDirectArgs[i] = 123.45
					}
				} else if strings.Contains(op, "Numeric") {
					args[i] = "123.45"
					wrongArgs[i] = "abc"
					if i == 1 && argCount > 1 {
						wrongArgs2[i] = "123.45"
					} else {
						wrongArgs2[i] = "123.45.67"
					}
					directArgs[i] = 123.45
					if i == 1 && argCount > 1 {
						wrongDirectArgs[i] = 123.45
					} else {
						wrongDirectArgs[i] = int64(123)
					}
				} else if strings.Contains(op, "Duration") {
					args[i] = "\"1h30m\""
					wrongArgs[i] = "abc"
					if i == 1 && argCount > 1 {
						wrongArgs2[i] = "\"1h30m\""
					} else {
						wrongArgs2[i] = "invalid-duration"
					}
					directArgs[i] = "1h30m"
					if i == 1 && argCount > 1 {
						wrongDirectArgs[i] = "1h30m"
					} else {
						wrongDirectArgs[i] = "invalid-duration"
					}
				} else if strings.Contains(op, "Time") {
					args[i] = "\"2024-01-01T12:00:00Z\""
					wrongArgs[i] = "abc"
					if i == 1 && argCount > 1 {
						wrongArgs2[i] = "\"2024-01-01T12:00:00Z\""
					} else {
						wrongArgs2[i] = "invalid-time"
					}
					directArgs[i] = "2024-01-01T12:00:00Z"
					if i == 1 && argCount > 1 {
						wrongDirectArgs[i] = "2024-01-01T12:00:00Z"
					} else {
						wrongDirectArgs[i] = "invalid-time"
					}
				} else if op == "IPEquals" {
					args[i] = "\"192.168.1.1\""
					wrongArgs[i] = "\"192.168.1.\""
					wrongArgs2[i] = "\"256.256.256.256"
					directArgs[i] = "192.168.1.1"
					wrongDirectArgs[i] = "192.168.1."
				} else if op == "IPInCIDR" {
					args[i] = "\"192.168.1.0/24\""
					wrongArgs[i] = "\"192.168.1.0\""
					wrongArgs2[i] = "\"256.256.256.0/24"
					directArgs[i] = "192.168.1.0/24"
					wrongDirectArgs[i] = "256.256.256.0/24"
				} else {
					args[i] = "\"arg" + string(rune(i+'0')) + "\""
					wrongArgs[i] = "arg" + string(rune(i+'0'))
					wrongArgs2[i] = "\"arg" + string(rune(i+'0'))
					if strings.Contains(op, "Regex") {
						wrongArgs2[i] = "\"[unclosed\""
					}
					directArgs[i] = "arg" + string(rune(i+'0'))
					wrongDirectArgs[i] = 12345
				}
			}
		} else {
			// zero args
			args = []string{op}
			wrongArgs = []string{op, "extra"}
			wrongArgs2 = []string{op, "\"onlyonearg\""}
			directArgs = []any{op}
			wrongDirectArgs = []any{op, "extra"}
		}
		makeMap := func(strs []any) []map[string]any {
			partials := make([]map[string]any, 0)
			m := make(map[string]any)
			partials = append(partials, m)
			m = make(map[string]any)
			args := policy.CompareOpArgs[op]
			for i := 0; i < len(args); i++ {
				m[args[i]] = strs[i+1]
				partials = append(partials, m)
				p := m
				m = make(map[string]any)
				for k, v := range p {
					m[k] = v
				}
			}
			return partials
		}
		err = gc.Parse(args)
		if err != nil {
			t.Errorf("expected success for operation %s, got error: %v", op, err)
		}
		err = gc.Precompile()
		if err != nil {
			t.Errorf("expected success for precompile operation %s, got error: %v", op, err)
		}
		err = gc.ParseDirect(directArgs)
		if err != nil {
			t.Errorf("expected success for direct parse operation %s, got error: %v", op, err)
		}
		mappedDirectArgs := makeMap(directArgs)
		gc2 := &policy.GenericCondition{}
		err = gc2.UnmarshalFromMap(mappedDirectArgs[0])
		if err == nil {
			t.Errorf("expected error for UnmarshalFromMap without op operation %s, got none", op)
		}
		for i := 0; i < len(mappedDirectArgs); i++ {
			mappedDirectArgs[i]["op"] = op
			err = gc2.UnmarshalFromMap(mappedDirectArgs[i])
			if i == len(mappedDirectArgs)-1 {
				if err != nil {
					t.Errorf("expected success for UnmarshalFromMap with op operation %s, got error: %v", op, err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error for UnmarshalFromMap without op operation %s, got none", op)
				}
			}
		}
		err = gc.Parse(wrongArgs)
		if err == nil {
			t.Errorf("expected error for operation %s with wrong args, got none", op)
		}
		err = gc.Parse(wrongArgs2)
		if err == nil {
			t.Errorf("expected error for operation %s with wrong args type, got none", op)
		}
		err = gc.ParseDirect(wrongDirectArgs)
		if err == nil {
			t.Errorf("expected error for direct parse operation %s with wrong args, got none", op)
		}
		wrongMappedDirectArgs := makeMap(wrongDirectArgs)
		for i := 0; i < len(wrongMappedDirectArgs); i++ {
			wrongMappedDirectArgs[i]["op"] = op
			gc3 := &policy.GenericCondition{}
			err = gc3.UnmarshalFromMap(wrongMappedDirectArgs[i])
			if argCount == 0 {
				if err != nil {
					t.Errorf("expected success for UnmarshalFromMap operation %s with zero args, got error: %v", op, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("expected error for UnmarshalFromMap operation %s with wrong args, got none", op)
			}
		}
	}
}

type stubResolver struct{}

func (r *stubResolver) Resolve(name []string) (any, error) {
	return nil, fmt.Errorf("stub resolver cannot resolve any variable")
}

func (r *stubResolver) AuditValue(name string, value any)                                      {}
func (r *stubResolver) AuditResult(condition policy.Condition, target any, ok bool, err error) {}

func TestCompareOpCompare(t *testing.T) {
	gc := &policy.GenericCondition{}
	resolver := &stubResolver{}
	_, err := gc.Compare(resolver, "test")
	if err == nil {
		t.Errorf("expected error for uninitialized condition, got none")
	}
	for _, op := range policy.CompareOpList {
		argCount := policy.CompareOpArgCount[op]
		var args = []string{op}
		if argCount < 0 {
			args = append(args, "\"arg1\"", "\"arg2\"")
		} else if argCount > 0 {
			args = append(args, make([]string, argCount)...)
			for i := 1; i <= argCount; i++ {
				if strings.Contains(op, "Integer") {
					args[i] = "123"
				} else if strings.Contains(op, "Numeric") {
					args[i] = "123.45"
				} else if strings.Contains(op, "Duration") {
					args[i] = "\"1h30m\""
				} else if strings.Contains(op, "Time") {
					args[i] = "\"2024-01-01T12:00:00Z\""
				} else if op == "IPEquals" {
					args[i] = "\"192.168.1.1\""
				} else if op == "IPInCIDR" {
					args[i] = "\"192.168.1.0/24\""
				} else {
					args[i] = "\"testvalue\""
				}
			}
		}
		gc := &policy.GenericCondition{}
		err := gc.Parse(args)
		if err != nil {
			t.Errorf("failed to parse arguments for operation %s: %v", op, err)
			continue
		}
		_ = gc.String() // test String() method
		if strings.Contains(op, "Integer") {
			_, err = gc.Compare(resolver, "wrongtype")
			if err == nil {
				t.Errorf("expected type error for operation %s, got none", op)
			}
			_, err = gc.Compare(resolver, int64(123))
		} else if strings.Contains(op, "Numeric") {
			_, err = gc.Compare(resolver, "wrongtype")
			if err == nil {
				t.Errorf("expected type error for operation %s, got none", op)
			}
			_, err = gc.Compare(resolver, 123.45)
		} else if strings.Contains(op, "List") && op != "StringInList" {
			_, err = gc.Compare(resolver, "wrongtype")
			if err == nil {
				t.Errorf("expected type error for operation %s, got none", op)
			}
			_, err = gc.Compare(resolver, []string{"value1", "value2"})
		} else if strings.Contains(op, "Duration") {
			_, err = gc.Compare(resolver, "wrongtype")
			if err == nil {
				t.Errorf("expected type error for operation %s, got none", op)
			}
			_, err = gc.Compare(resolver, time.Duration(90*time.Minute))
			if err != nil {
				t.Errorf("failed to compare for operation %s: %v", op, err)
			}
		} else if strings.Contains(op, "Time") {
			_, err = gc.Compare(resolver, "wrongtype")
			if err == nil {
				t.Errorf("expected type error for operation %s, got none", op)
			}
			_, err = gc.Compare(resolver, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Errorf("failed to compare for operation %s: %v", op, err)
			}
		} else if strings.Contains(op, "IP") {
			_, err = gc.Compare(resolver, "wrongtype")
			if err == nil {
				t.Errorf("expected type error for operation %s, got none", op)
			}
			_, err = gc.Compare(resolver, 123)
			if err == nil {
				t.Errorf("expected type error for operation %s, got none", op)
			}
			_, err = gc.Compare(resolver, netip.MustParseAddr("192.168.0.1"))
		} else {
			if argCount != 0 {
				_, err = gc.Compare(resolver, 123)
				if err == nil {
					t.Errorf("expected type error for operation %s, got none", op)
				}
			}
			_, err = gc.Compare(resolver, "testtarget")
		}
		if err != nil {
			t.Errorf("failed to compare for operation %s: %v", op, err)
		}
	}
}
