package rpcparse

import (
	"fmt"
	"strings"
)

type RPCField struct {
	Name        string
	Type        RPCType
	Annotations RPCAnnotations
}

type RPCTypeAlias struct {
	Name        string
	Type        RPCType
	Annotations RPCAnnotations
}

type RPCSignature struct {
	Method string
	Args   RPCStruct
	Return RPCStruct
}

type RPCGroup struct {
	Name        string
	Signatures  []RPCSignature
	Annotations RPCAnnotations
}

type RPCAnnotation struct {
	Key   string
	Value []string
}

type RPCAnnotations struct {
	Annotations   []RPCAnnotation
	AnnotationMap map[string][]string
}

func (r *RPCAnnotations) Get(key string) ([]string, bool) {
	if r.AnnotationMap == nil {
		return nil, false
	}
	values, ok := r.AnnotationMap[key]
	return values, ok
}

func (r *RPCAnnotations) Append(ann *RPCAnnotation) {
	r.Annotations = append(r.Annotations, *ann)
	if r.AnnotationMap == nil {
		r.AnnotationMap = make(map[string][]string)
	}
	r.AnnotationMap[ann.Key] = ann.Value
}

type RPCStruct struct {
	Name              string
	TypeParameters    []*RPCField
	Fields            []*RPCField
	Annotations       RPCAnnotations
	IsRequestResponse bool
}

func (r *RPCStruct) IsTypeParam(name string) *RPCField {
	for _, p := range r.TypeParameters {
		if p.Name == name {
			return p
		}
	}
	return nil
}

type RPCFile struct {
	Groups       []*RPCGroup
	GroupMap     map[string]*RPCGroup
	Structs      []*RPCStruct
	StructMap    map[string]*RPCStruct
	TypeAliases  []*RPCTypeAlias
	TypeAliasMap map[string]*RPCTypeAlias
}

type RPCParser struct {
	buf []byte
	pos int
}

func (r *RPCParser) Expect(s string) bool {
	if len(r.buf)-r.pos < len(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if r.buf[r.pos+i] != s[i] {
			return false
		}
	}
	r.pos += len(s)
	return true
}

func (r *RPCParser) Peek(s string) bool {
	start := r.pos
	if r.Expect(s) {
		r.pos = start
		return true
	}
	r.pos = start
	return false
}

func (r *RPCParser) skipWhitespace() {
	for r.pos < len(r.buf) {
		if r.buf[r.pos] == ' ' || r.buf[r.pos] == '\t' || r.buf[r.pos] == '\n' || r.buf[r.pos] == '\r' {
			r.pos++
			continue
		}
		// skip comments
		if r.buf[r.pos] == '/' && r.pos+1 < len(r.buf) && r.buf[r.pos+1] == '/' {
			r.pos += 2
			for r.pos < len(r.buf) && r.buf[r.pos] != '\n' {
				r.pos++
			}
			continue
		}
		break
	}
}

type RPCType interface {
	rpcType()
	fmt.Stringer
	StringInstance() string
	Base() string
	Args() []RPCType

	ToInstance(typMap map[string]RPCType) RPCType
}

type RPCArrayType struct {
	ElementType RPCType
}

func (r *RPCArrayType) rpcType() {}

func (r *RPCArrayType) String() string {
	return "[]" + r.ElementType.String()
}

func (r *RPCArrayType) StringInstance() string {
	return "ArrayOf" + r.ElementType.StringInstance()
}

func (r *RPCArrayType) Base() string {
	return r.ElementType.Base()
}
func (r *RPCArrayType) Args() []RPCType {
	return r.ElementType.Args()
}

func (r *RPCArrayType) ToInstance(typMap map[string]RPCType) RPCType {
	return &RPCArrayType{
		ElementType: r.ElementType.ToInstance(typMap),
	}
}

type RPCInstanceType struct {
	BaseName string
	TypeArgs []RPCType
}

func (r *RPCInstanceType) rpcType() {}

func (r *RPCInstanceType) String() string {
	var args []string
	for _, t := range r.TypeArgs {
		args = append(args, t.StringInstance())
	}
	// because instaniated by code generator, use replaced name instead of generic formation
	return fmt.Sprintf("%s_%s", r.BaseName, strings.Join(args, "_"))
}

func (r *RPCInstanceType) StringInstance() string {
	return r.String()
}

func (r *RPCInstanceType) Base() string {
	return r.BaseName
}

func (r *RPCInstanceType) Args() []RPCType {
	return r.TypeArgs
}

func (r *RPCInstanceType) ToInstance(typMap map[string]RPCType) RPCType {
	var instantiatedArgs []RPCType
	for _, t := range r.TypeArgs {
		instantiatedArgs = append(instantiatedArgs, t.ToInstance(typMap))
	}
	return &RPCInstanceType{
		BaseName: r.BaseName,
		TypeArgs: instantiatedArgs,
	}
}

type RPCBasicType struct {
	Name string
}

func (r *RPCBasicType) rpcType() {}

func (r *RPCBasicType) String() string {
	return r.Name
}

func (r *RPCBasicType) StringInstance() string {
	return r.Name
}

func (r *RPCBasicType) Base() string {
	return r.Name
}

func (r *RPCBasicType) IsGeneric() bool {
	return false
}

func (r *RPCBasicType) Args() []RPCType {
	return nil
}

func (r *RPCBasicType) ToInstance(typMap map[string]RPCType) RPCType {
	if inst, ok := typMap[r.Name]; ok {
		return inst
	}
	return r
}

func (r *RPCParser) parseType() (RPCType, bool) {
	start := r.pos
	if r.Expect("[]") {
		elemType, ok := r.parseType()
		if !ok {
			return nil, false
		}
		return &RPCArrayType{ElementType: elemType}, true
	}
	name, ok := r.parseIdentifier()
	if !ok {
		return nil, false
	}
	if r.Expect("[") {
		var typeArgs []RPCType
		for {
			r.skipWhitespace()
			argType, ok := r.parseType()
			if !ok {
				return nil, false
			}
			typeArgs = append(typeArgs, argType)
			r.skipWhitespace()
			if r.Expect("]") {
				break
			}
			if !r.Expect(",") {
				return nil, false
			}
		}
		return &RPCInstanceType{BaseName: name, TypeArgs: typeArgs}, true
	}
	r.pos = start + len(name)
	return &RPCBasicType{Name: name}, true
}

func (r *RPCParser) parseIdentifier() (string, bool) {
	start := r.pos
	for r.pos < len(r.buf) {
		c := r.buf[r.pos]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			r.pos++
			continue
		}
		break
	}
	if start == r.pos {
		return "", false
	}
	return string(r.buf[start:r.pos]), true
}

func (r *RPCParser) parseString() (string, bool) {
	isRawStr := r.Expect("`")
	isCharStr := isRawStr || r.Expect("'")
	isStr := isCharStr || r.Expect("\"")
	if !isStr {
		return "", false
	}
	endChar := byte('"')
	if isRawStr {
		endChar = '`'
	} else if isCharStr {
		endChar = '\''
	}
	start := r.pos
	for r.pos < len(r.buf) {
		if r.buf[r.pos] == endChar {
			s := string(r.buf[start:r.pos])
			r.pos++
			return s, true
		} else if r.buf[r.pos] == '\\' && !isRawStr {
			// skip escaped character
			r.pos += 2
			continue
		}
		r.pos++
	}
	return "", false
}

func (r *RPCParser) parseAnnotation() (*RPCAnnotation, bool) {
	var ann RPCAnnotation
	if !r.Expect("@") {
		return nil, false
	}
	key, ok := r.parseIdentifier()
	if !ok {
		return nil, false
	}
	ann.Key = key
	r.skipWhitespace()
	if !r.Expect("(") { // no value, its ok
		return &ann, true
	}
	r.skipWhitespace()
	var values []string
	for {
		if r.Expect(")") {
			break
		}
		value, ok := r.parseString()
		if !ok {
			return nil, false
		}
		values = append(values, value)
		r.skipWhitespace()
	}
	ann.Value = values
	return &ann, true
}

func (r *RPCParser) ParseRPCField() (*RPCField, bool) {
	var arg RPCField
	name, ok := r.parseIdentifier()
	if !ok {
		return nil, false
	}
	arg.Name = name
	r.skipWhitespace()
	arg.Type, ok = r.parseType()
	if !ok {
		return nil, false
	}
	r.skipWhitespace()
	arg.Annotations = r.parseAnnotations()
	return &arg, true
}

func (r *RPCParser) ParseArgs() ([]*RPCField, bool) {
	var args []*RPCField
	r.skipWhitespace()
	if !r.Expect("(") {
		return args, false
	}
	r.skipWhitespace()
	for {
		if r.Expect(")") {
			break
		}
		arg, ok := r.ParseRPCField()
		if !ok {
			return args, false
		}
		args = append(args, arg)
		r.skipWhitespace()
		if r.Expect(",") {
			r.skipWhitespace()
			continue
		}
	}
	return args, true
}

func (r *RPCParser) parseAnnotations() RPCAnnotations {
	var anns RPCAnnotations
	for {
		r.skipWhitespace()
		if r.Peek("@") {
			ann, ok := r.parseAnnotation()
			if !ok {
				break
			}
			anns.Append(ann)
		} else {
			break
		}
	}
	return anns
}

func (r *RPCParser) ParseRPCSignature() (RPCSignature, bool) {
	var sig RPCSignature
	if !r.Expect("rpc ") {
		return sig, false
	}
	method, ok := r.parseIdentifier()
	if !ok {
		return sig, false
	}
	sig.Method = method
	sig.Args.Fields, ok = r.ParseArgs()
	if !ok {
		return sig, false
	}
	sig.Args.IsRequestResponse = true
	sig.Args.Annotations = r.parseAnnotations()
	if !r.Expect("->") {
		return sig, false
	}
	r.skipWhitespace()
	sig.Return.Fields, ok = r.ParseArgs()
	if !ok {
		return sig, false
	}
	sig.Args.IsRequestResponse = true
	sig.Return.Annotations = r.parseAnnotations()
	if !r.Expect(";") {
		return sig, false
	}
	return sig, true
}

func (r *RPCParser) ParseRPCGroup() (RPCGroup, bool) {
	var group RPCGroup
	if !r.Expect("service ") {
		return group, false
	}
	name, ok := r.parseIdentifier()
	if !ok {
		return group, false
	}
	group.Name = name
	r.skipWhitespace()
	if !r.Expect("{") {
		return group, false
	}
	r.skipWhitespace()
	for {
		if r.Expect("}") {
			break
		}
		if r.Peek("@") {
			ann, ok := r.parseAnnotation()
			if !ok {
				return group, false
			}
			group.Annotations.Append(ann)
			r.skipWhitespace()
			if r.Expect(";") {
				r.skipWhitespace()
			}
			continue
		}
		sig, ok := r.ParseRPCSignature()
		if !ok {
			return group, false
		}
		// for each args and return, create struct names
		sig.Args.Name = group.Name + sig.Method + "Request"
		sig.Return.Name = group.Name + sig.Method + "Response"
		group.Signatures = append(group.Signatures, sig)
		r.skipWhitespace()
	}
	return group, true
}

func (r *RPCParser) ParseRPCStruct() (*RPCStruct, bool) {
	var strct RPCStruct
	if !r.Expect("struct ") {
		return nil, false
	}
	name, ok := r.parseIdentifier()
	if !ok {
		return nil, false
	}
	strct.Name = name
	r.skipWhitespace()
	if r.Peek("(") {
		typeParams, ok := r.ParseArgs()
		if !ok {
			return nil, false
		}
		strct.TypeParameters = typeParams
		r.skipWhitespace()
	}
	if !r.Expect("{") {
		return nil, false
	}
	r.skipWhitespace()
	for {
		if r.Expect("}") {
			break
		}
		if r.Peek("@") {
			ann, ok := r.parseAnnotation()
			if !ok {
				return nil, false
			}
			strct.Annotations.Append(ann)
			r.skipWhitespace()
			if r.Expect(";") {
				r.skipWhitespace()
			}
			continue
		}
		field, ok := r.ParseRPCField()
		if !ok {
			return nil, false
		}
		strct.Fields = append(strct.Fields, field)
		r.skipWhitespace()
		if r.Expect(";") {
			r.skipWhitespace()
		}
	}
	return &strct, true
}

func (r *RPCParser) ParseRPCTypeAlias() (*RPCTypeAlias, bool) {
	var alias RPCTypeAlias
	if !r.Expect("type ") {
		return nil, false
	}
	name, ok := r.parseIdentifier()
	if !ok {
		return nil, false
	}
	alias.Name = name
	r.skipWhitespace()
	if !r.Expect("=") {
		return nil, false
	}
	r.skipWhitespace()
	typ, ok := r.parseType()
	if !ok {
		return nil, false
	}
	alias.Type = typ
	r.skipWhitespace()
	if !r.Expect(";") {
		return nil, false
	}
	return &alias, true
}

func NewRPCParser(buf []byte) *RPCParser {
	return &RPCParser{buf: buf}
}

func Parse(buf []byte) (*RPCFile, bool) {
	var file RPCFile
	file.GroupMap = make(map[string]*RPCGroup)
	file.StructMap = make(map[string]*RPCStruct)
	file.TypeAliasMap = make(map[string]*RPCTypeAlias)
	parser := NewRPCParser(buf)
	parser.skipWhitespace()
	for parser.pos < len(parser.buf) {
		if parser.Peek("struct ") {
			strct, ok := parser.ParseRPCStruct()
			if !ok {
				return nil, false
			}
			file.Structs = append(file.Structs, strct)
			file.StructMap[strct.Name] = strct
			parser.skipWhitespace()
			continue
		}
		if parser.Peek("type ") {
			alias, ok := parser.ParseRPCTypeAlias()
			if !ok {
				return nil, false
			}
			file.TypeAliases = append(file.TypeAliases, alias)
			file.TypeAliasMap[alias.Name] = alias
			parser.skipWhitespace()
			continue
		}
		group, ok := parser.ParseRPCGroup()
		if !ok {
			return nil, false
		}
		file.Groups = append(file.Groups, &group)
		file.GroupMap[group.Name] = &group
		parser.skipWhitespace()
	}
	return &file, true
}

func InstantiateType(file *RPCFile, typ RPCType) {
	if args := typ.Args(); len(args) > 0 {
		// check if already instantiated
		if _, found := file.StructMap[typ.String()]; found {
			return
		}
		baseStruct, found := file.StructMap[typ.Base()]
		if found {
			Instantiate(file, baseStruct, args)
		}
	}
}

// recursivly instantiate a generic struct with type arguments
func Instantiate(file *RPCFile, strcut_ *RPCStruct, typArgs []RPCType) {
	if len(strcut_.TypeParameters) != len(typArgs) {
		return
	}
	instanceType := (&RPCInstanceType{BaseName: strcut_.Name, TypeArgs: typArgs}).String()
	result := &RPCStruct{
		Name:              instanceType,
		Annotations:       strcut_.Annotations,
		IsRequestResponse: strcut_.IsRequestResponse,
	}
	file.Structs = append(file.Structs, result)
	file.StructMap[instanceType] = result
	typeParamMap := make(map[string]RPCType)
	for i, p := range strcut_.TypeParameters {
		typeParamMap[p.Name] = typArgs[i]
	}
	var instantiatedFields []*RPCField
	for _, field := range strcut_.Fields {
		fieldType := field.Type.ToInstance(typeParamMap)
		instantiatedFields = append(instantiatedFields, &RPCField{
			Name:        field.Name,
			Type:        fieldType,
			Annotations: field.Annotations,
		})
		InstantiateType(file, fieldType)
	}
	result.Fields = instantiatedFields
}

func InstantiateFile(file *RPCFile) {
	for _, strct := range file.Structs {
		if len(strct.TypeParameters) != 0 { // generic struct, skip until instantiated
			continue
		}
		for _, field := range strct.Fields {
			InstantiateType(file, field.Type)
		}
	}
	for _, group := range file.Groups {
		for _, sig := range group.Signatures {
			// check args
			for _, field := range sig.Args.Fields {
				InstantiateType(file, field.Type)
			}
			// check return
			for _, field := range sig.Return.Fields {
				InstantiateType(file, field.Type)
			}
		}
	}
	// type aliases instantiation
	for _, alias := range file.TypeAliases {
		InstantiateType(file, alias.Type)
	}
}
