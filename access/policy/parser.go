package policy

import (
	"fmt"
	"strconv"
	"strings"
	"unsafe"

	"github.com/on-keyday/kscale/access"
)

type Parser struct {
	data         []byte // do not modify after creating Parser
	pos          int
	prevExpected string
}

func NewParser(input string) *Parser {
	data := []byte(input)
	return &Parser{
		data: data,
		pos:  0,
	}
}

func (p *Parser) Expect(expected string) bool {
	for i := 0; i < len(expected); i++ {
		if p.pos+i >= len(p.data) || p.data[p.pos+i] != expected[i] {
			return false
		}
	}
	p.pos += len(expected)
	p.prevExpected = expected
	return true
}

// Match checks if the next bytes match the expected string without advancing the position.
func (p *Parser) Match(expected string) bool {
	start := p.pos
	if !p.Expect(expected) {
		p.pos = start
		return false
	}
	p.pos = start
	return true
}

func (p *Parser) EOF() bool {
	return p.pos >= len(p.data)
}

var ErrParseString = &ParseError{"failed to parse string"}

type ParseError struct {
	Msg string
}

func (e *ParseError) Error() string {
	return e.Msg
}

func (p *Parser) ParseString(prefix string) (string, error) {
	start := p.pos
	if !p.Expect(prefix) {
		return "", ErrParseString
	}
	for !p.EOF() {
		if p.data[p.pos] == prefix[0] {
			break
		} else if p.data[p.pos] == '\\' {
			// skip escaped character
			p.pos += 2
			continue
		}
		p.pos++
	}
	if !p.Expect(prefix) {
		p.pos = start // reset position on failure
		return "", ErrParseString
	}
	slice := p.data[start:p.pos]
	str := unsafe.String(unsafe.SliceData(slice), len(slice))
	_, err := strconv.Unquote(str)
	if err != nil {
		p.pos = start // reset position on failure
		return "", fmt.Errorf("failed to unquote string: %w", err)
	}
	return str, err
}

func (p *Parser) ParseNumeric() (string, error) {
	start := p.pos
	requireNoEOF := false
	if p.Expect("-") || p.Expect("+") {
		requireNoEOF = true
	}
	radixRange := func(x byte) bool {
		return (x >= '0' && x <= '9')
	}
	maybeFloat := true
	prefloatPoint := p.pos
	if p.Expect("0x") || p.Expect("0X") {
		requireNoEOF = true
		radixRange = func(x byte) bool {
			return (x >= '0' && x <= '9') || (x >= 'a' && x <= 'f') || (x >= 'A' && x <= 'F')
		}
		maybeFloat = false
	} else if p.Expect("0b") || p.Expect("0B") {
		requireNoEOF = true
		radixRange = func(x byte) bool {
			return x == '0' || x == '1'
		}
		maybeFloat = false
	} else if p.Expect("0o") || p.Expect("0O") {
		requireNoEOF = true
		radixRange = func(x byte) bool {
			return x >= '0' && x <= '7'
		}
		maybeFloat = false
	}
	for !p.EOF() {
		c := p.data[p.pos]
		if radixRange(c) {
			p.pos++
		} else {
			break
		}
		requireNoEOF = false
	}
	if requireNoEOF {
		p.pos = start
		return "", &ParseError{"unexpected end of input while parsing numeric"}
	}
	omitedZero := prefloatPoint == p.pos
	if maybeFloat && p.Expect(".") {
		digitFound := false
		for !p.EOF() {
			c := p.data[p.pos]
			if c >= '0' && c <= '9' {
				p.pos++
				digitFound = true
			} else {
				break
			}
		}
		if omitedZero && !digitFound {
			p.pos = start
			return "", &ParseError{"invalid numeric format"}
		}
		// exponent part
		if p.Expect("e") || p.Expect("E") {
			if p.Expect("-") || p.Expect("+") {
				// optional sign
			}
			expDigitFound := false
			for !p.EOF() {
				c := p.data[p.pos]
				if c >= '0' && c <= '9' {
					p.pos++
					expDigitFound = true
				} else {
					break
				}
			}
			if !expDigitFound {
				p.pos = start
				return "", &ParseError{"invalid numeric format in exponent"}
			}
		}
	}
	slice := p.data[start:p.pos]
	str := unsafe.String(unsafe.SliceData(slice), len(slice))
	return str, nil
}

func (p *Parser) ParseIdentifier(includeDollar bool) (string, error) {
	start := p.pos
	if p.EOF() {
		return "", &ParseError{"unexpected end of input while parsing identifier"}
	}
	c := p.data[p.pos]
	if includeDollar && c == '$' {
		p.pos++
		if p.EOF() {
			return "", &ParseError{"unexpected end of input after '$'"}
		}
		c = p.data[p.pos]
	}
	if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
		p.pos++
	} else {
		return "", &ParseError{"invalid start of identifier"}
	}
	requireMore := false
	for !p.EOF() {
		c = p.data[p.pos]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			p.pos++
		} else if c == '.' {
			if requireMore {
				return "", &ParseError{"unexpected twice '.' in identifier"}
			}
			p.pos++
			requireMore = true
		} else {
			break
		}
		requireMore = false
	}
	if requireMore {
		return "", &ParseError{"identifier cannot end with '.'"}
	}
	slice := p.data[start:p.pos]
	str := unsafe.String(unsafe.SliceData(slice), len(slice))
	return str, nil
}

func (p *Parser) SkipWhitespace() {
	for !p.EOF() {
		c := p.data[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++
		} else {
			break
		}
	}
}

func wrapList(s string, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	return []string{s}, nil
}

func (p *Parser) ParseBrackets() ([]string, error) {
	if !p.Expect("[") {
		return nil, &ParseError{"expected '['"}
	}
	var results []string
	for {
		p.SkipWhitespace()
		if p.Expect("]") {
			break
		}
		vals, err := p.ParsePrimitive()
		if err != nil {
			return nil, err
		}
		results = append(results, vals...)
		p.SkipWhitespace()
		p.Expect(",") // optional comma
	}
	return results, nil
}

func (p *Parser) ParsePrimitive() ([]string, error) {
	if p.Match("\"") {
		return wrapList(p.ParseString("\""))
	}
	if p.Match("`") {
		return wrapList(p.ParseString("`"))
	}
	if p.Match("-") || (p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9') {
		return wrapList(p.ParseNumeric())
	}
	if p.Match("$") {
		return wrapList(p.ParseIdentifier(true))
	}
	if p.Match("[") {
		return p.ParseBrackets()
	}
	return nil, &ParseError{"failed to parse primitive value"}
}

/*
type EvalContext struct {
	Variables map[string]string
}

func (p *EvalContext) Resolve(txt string) (string, error) {
	if len(txt) == 0 {
		return "", fmt.Errorf("empty string")
	}
	if txt[0] == '"' {
		return txt, nil
	}
	if (txt[0] >= '0' && txt[0] <= '9') || txt[0] == '-' || txt[0] == '+' {
		return txt, nil
	}
	if p.Variables == nil {
		return "", fmt.Errorf("no variables defined")
	}
	val, ok := p.Variables[txt]
	if !ok {
		return "", fmt.Errorf("undefined variable: %s", txt)
	}
	return val, nil
}

func (p *EvalContext) ResolveList(txts []string) ([]string, error) {
	var results []string
	for _, txt := range txts {
		val, err := p.Resolve(txt)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve variable: %s", txt)
		}
		results = append(results, val)
	}
	return results, nil
}
*/

func isNumericFloat(s string) bool {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") ||
		strings.HasPrefix(s, "0b") || strings.HasPrefix(s, "0B") ||
		strings.HasPrefix(s, "0o") || strings.HasPrefix(s, "0O") {
		return false
	}
	if len(s) == 0 {
		return false
	}
	if s[0] == '-' || s[0] == '+' {
		s = s[1:]
	}
	if s[0] == '.' {
		return true
	}
	if s[0] >= '0' && s[0] <= '9' {
		// continue
	} else {
		return false
	}
	// check for decimal point or exponent
	if strings.ContainsAny(s, ".eE") {
		return true
	}
	return false
}

func isIntegerLike(s string) bool {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") ||
		strings.HasPrefix(s, "0b") || strings.HasPrefix(s, "0B") ||
		strings.HasPrefix(s, "0o") || strings.HasPrefix(s, "0O") {
		return true
	}
	if _, err := strconv.ParseInt(s, 0, 64); err == nil {
		return true
	}
	return false
}

// mangling is best effort to determine type-specific operator from generic ones
// use strict operators when possible
func mangleOp(op string, target []string) string {
	if len(target) != 1 {
		return op
	}
	if op == "==" {
		if isNumericFloat(target[0]) {
			return "num_eq"
		}
		if isIntegerLike(target[0]) {
			return "int_eq"
		}
		return "str_eq"
	}
	if op == "!=" {
		if isNumericFloat(target[0]) {
			return "num_neq"
		}
		if isIntegerLike(target[0]) {
			return "int_neq"
		}
		return "str_neq"
	}
	if op == "<" {
		if isNumericFloat(target[0]) {
			return "num_lt"
		}
		return "int_lt"
	}
	if op == "<=" {
		if isNumericFloat(target[0]) {
			return "num_lte"
		}
		return "int_lte"
	}
	if op == ">" {
		if isNumericFloat(target[0]) {
			return "num_gt"
		}
		return "int_gt"
	}
	if op == ">=" {
		if isNumericFloat(target[0]) {
			return "num_gte"
		}
		return "int_gte"
	}
	return op
}

func (p *Parser) ParseSingleCondition() (string, *GenericCondition, error) {
	p.SkipWhitespace()
	primitive, err := p.ParsePrimitive()
	if err != nil {
		return "", nil, err
	}
	if len(primitive) != 1 {
		return "", nil, &ParseError{"expected single primitive value for condition"}
	}
	if len(primitive[0]) == 0 || primitive[0][0] != '$' {
		return "", nil, &ParseError{"expected variable reference"}
	}
	input := primitive[0][1:] // remove leading $
	p.SkipWhitespace()
	var strOp string
	if p.Expect("==") || p.Expect("!=") ||
		p.Expect("<=") || p.Expect(">=") ||
		p.Expect("<") || p.Expect(">") {
		strOp = p.prevExpected
	} else {
		strOp, err = p.ParseIdentifier(false)
		if err != nil {
			return "", nil, err
		}
	}
	p.SkipWhitespace()
	target, err := p.ParsePrimitive()
	if err != nil {
		return "", nil, err
	}
	strOp = mangleOp(strOp, target)
	g := &GenericCondition{}
	err = g.Parse(append([]string{strOp}, target...))
	if err != nil {
		return "", nil, err
	}
	return input, g, nil
}

// helpers for auto-generated parse functions

func parseString(input string) (string, error) {
	return strconv.Unquote(input)
}

func parseFloat64(input string) (float64, error) {
	return strconv.ParseFloat(input, 64)
}

func parseInt64(input string) (int64, error) {
	return strconv.ParseInt(input, 0, 64)
}

func parseStringList(input []string) ([]string, error) {
	for i, s := range input {
		unquoted, err := strconv.Unquote(s)
		if err != nil {
			return nil, fmt.Errorf("failed to unquote string at index %d: %w", i, err)
		}
		input[i] = unquoted
	}
	return input, nil
}

func (p *Parser) ParsePureAttributePolicy(name string) (access.Policy, error) {
	target, cond, err := p.ParseSingleCondition()
	if err != nil {
		return nil, err
	}
	contextName := strings.SplitN(target, ".", 2)
	if len(contextName) < 2 {
		return nil, fmt.Errorf("invalid target format: %s", target)
	}
	if contextName[0] != "user" && contextName[0] != "resource" && contextName[0] != "env" && contextName[0] != "action" {
		return nil, fmt.Errorf("invalid context in target: %s", contextName[0])
	}
	return NewAttributePolicy(name, contextName[0], contextName[1], cond), nil
}

func (p *Parser) ParsePrimitivePolicy(name string) (access.Policy, error) {
	if p.Expect("(") {
		pol, err := p.ParsePolicy(name)
		if err != nil {
			return nil, err
		}
		p.SkipWhitespace()
		if !p.Expect(")") {
			return nil, &ParseError{"expected ')'"}
		}
		return pol, nil
	}
	return p.ParsePureAttributePolicy(name)
}

func mangleNameOfChildren(pol []access.Policy) []access.Policy {
	var results []access.Policy
	for _, p := range pol {
		switch child := p.(type) {
		case *andPolicy:
			child.name = fmt.Sprintf("(child of %s)", p.Name())
			results = append(results, child)
		case *orPolicy:
			child.name = fmt.Sprintf("(child of %s)", p.Name())
			results = append(results, child)
		case *xorPolicy:
			child.name = fmt.Sprintf("(child of %s)", p.Name())
			results = append(results, child)
		case *attributePolicy:
			child.name = fmt.Sprintf("(child of %s)", p.Name())
			results = append(results, child)
		default:
			results = append(results, p)
		}
	}
	return results
}

func (p *Parser) ParseNotPolicy(name string) (access.Policy, error) {
	p.SkipWhitespace()
	if p.Expect("not") || p.Expect("!") {
		p.SkipWhitespace()
		primitive, err := p.ParsePrimitivePolicy(name)
		if err != nil {
			return nil, err
		}
		return NewNotPolicy(name, primitive), nil
	}
	return p.ParsePrimitivePolicy(name)
}

func (p *Parser) ParseAndPolicy(name string) (access.Policy, error) {
	a, err := p.ParseNotPolicy(name)
	if err != nil {
		return nil, err
	}
	p.SkipWhitespace()
	andPolicies := []access.Policy{a}
	for p.Expect("and") || p.Expect("&&") {
		p.SkipWhitespace()
		a, err := p.ParseNotPolicy(name)
		if err != nil {
			return nil, err
		}
		andPolicies = append(andPolicies, a)
		p.SkipWhitespace()
	}
	if len(andPolicies) == 1 {
		return andPolicies[0], nil
	}
	return NewAndPolicy(name, mangleNameOfChildren(andPolicies)...), nil
}

func (p *Parser) ParseOrPolicy(name string) (access.Policy, error) {
	a, err := p.ParseAndPolicy(name)
	if err != nil {
		return nil, err
	}
	p.SkipWhitespace()
	orPolicies := []access.Policy{a}
	for p.Expect("or") || p.Expect("||") {
		p.SkipWhitespace()
		a, err := p.ParseAndPolicy(name)
		if err != nil {
			return nil, err
		}
		orPolicies = append(orPolicies, a)
		p.SkipWhitespace()
	}
	if len(orPolicies) == 1 {
		return orPolicies[0], nil
	}
	return NewOrPolicy(name, mangleNameOfChildren(orPolicies)...), nil
}

func (p *Parser) ParseXorPolicy(name string) (access.Policy, error) {
	a, err := p.ParseOrPolicy(name)
	if err != nil {
		return nil, err
	}
	p.SkipWhitespace()
	xorPolicies := []access.Policy{a}
	for p.Expect("xor") {
		p.SkipWhitespace()
		a, err := p.ParseOrPolicy(name)
		if err != nil {
			return nil, err
		}
		xorPolicies = append(xorPolicies, a)
		p.SkipWhitespace()
	}
	if len(xorPolicies) == 1 {
		return xorPolicies[0], nil
	}
	return NewXorPolicy(name, mangleNameOfChildren(xorPolicies)...), nil
}

func (p *Parser) ParsePolicy(name string) (access.Policy, error) {
	p.SkipWhitespace()
	return p.ParseXorPolicy(name)
}

func MustParseAttributePolicy(name string, input string) access.Policy {
	p := NewParser(input)
	pol, err := p.ParsePureAttributePolicy(name)
	if err != nil {
		panic(fmt.Sprintf("failed to parse attribute policy: %v", err))
	}
	return pol
}

func MustParsePolicy(name string, input string) access.Policy {
	p := NewParser(input)
	pol, err := p.ParsePolicy(name)
	if err != nil {
		panic(fmt.Sprintf("failed to parse policy: %v", err))
	}
	return pol
}
