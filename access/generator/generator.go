package generator

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/template"

	"github.com/goccy/go-yaml"
)

type ActionArg struct {
	Name    string  `json:"name"`
	Type    string  `json:"type"`
	Default *string `json:"default,omitempty"`
	// ReadOnly marks a schema field that is computed/observed, not set by the
	// client: CRUD synthesis includes it in the object (get/list responses) but
	// excludes it from create's arguments (a minimal spec/status split).
	ReadOnly bool `json:"readonly,omitempty"`
	// Sensitive marks a secret-bearing field (e.g. a shared secret) that must
	// never become a policy attribute: the gate excludes it from $env.args so it
	// is not evaluated by — or logged in — the access policy audit. Access to the
	// resource is still gated (by authority/role); only the value is kept out.
	Sensitive bool `json:"sensitive,omitempty"`
}

type StringOrArray []string

func (s *StringOrArray) UnmarshalYAML(data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var single string
	if err := dec.Decode(&single); err == nil {
		*s = StringOrArray{single}
	} else {
		dec = yaml.NewDecoder(bytes.NewReader(data)) // reset decoder
		var arr []string
		if err := dec.Decode(&arr); err != nil {
			return err
		}
		*s = StringOrArray(arr)
	}
	return nil
}

type ActionResponse struct {
	Use          *string     `json:"use,omitempty"`
	ObjectFormat *string     `json:"object_format,omitempty"`
	CLIFormat    *string     `json:"cli_format,omitempty"`
	Fields       []ActionArg `json:"fields"`
	// Stream marks a server-streaming response: the generated RPC method becomes
	// `rpc {Action}(Args) returns (stream {Use})` and the RPC adapter/client use
	// the server-streaming path. It requires Use (the streamed element type).
	// Stream is an RPC-only concern: on the RawCommand command-tree path the
	// action stays no-response (the existing watcher/side-stream impl serves the
	// interactive path), so the generated config HandlerWrapper ignores it.
	Stream bool `json:"stream,omitempty"`
	// RpcOnly marks a unary response that exists ONLY on the typed-RPC surface:
	// the RPC method is `rpc {Action}(Args) returns ({Use})` served by a
	// hand-written direct handler (Handle(ctx,dto) (*Use,error)) that the
	// generated adapter calls instead of the config HandlerWrapper. On the
	// command-tree path the action stays no-response, so the existing
	// command-tree handler (e.g. dp_stats' text/FormatStats impl) is untouched.
	// Requires Use. Mutually exclusive with Stream.
	RpcOnly bool `json:"rpc_only,omitempty"`
}

// isCommandTreeResponse reports whether the action yields a unary response on
// the RawCommand command-tree path (HandlerWrapper). Server-streaming and
// rpc_only responses are RPC-only; the command tree treats them as no-response.
func (a *Action) isCommandTreeResponse() bool {
	return a.Response != nil && !a.Response.Stream && !a.Response.RpcOnly
}

// isStreamingResponse reports whether the action is a server-streaming RPC.
func (a *Action) isStreamingResponse() bool {
	return a.Response != nil && a.Response.Stream
}

// isRPCOnlyResponse reports whether the action is a unary RPC-only response
// (hand-written direct handler; command tree stays no-response).
func (a *Action) isRPCOnlyResponse() bool {
	return a.Response != nil && a.Response.RpcOnly
}

type Action struct {
	Name        string          `json:"name"`
	CommandName StringOrArray   `json:"command_name,omitempty"`
	Args        []ActionArg     `json:"args,omitempty"`
	Response    *ActionResponse `json:"response,omitempty"`
	// RpcSkip excludes this action from the generated typed admin RPC surface
	// (proto service, server adapter, client dispatch) while leaving it on the
	// RawCommand command tree. Used for actions that cannot be a unary RPC —
	// streaming actions (view_logs / realtime_stats / send_file …) or handlers
	// that are not config-DTO HandlerWrappers (raw agent.Agent Run). The action
	// still works via the legacy stream path
	// (ksdk notes/ai/2026_06_02_admin_shell_rpc_design.md Phase 2c).
	RpcSkip bool `json:"rpc_skip,omitempty"`
}

// rpcActions returns r's actions that participate in the generated admin RPC
// surface: rpc_service must be on and the action must not be rpc_skip'd.
func (r *ResourceTemplate) rpcActions() []Action {
	var out []Action
	for _, a := range r.Actions {
		if a.RpcSkip {
			continue
		}
		out = append(out, a)
	}
	return out
}

// generatesGate reports whether r gets the generated RPC surface (the ABAC gate
// adapter + client dispatch): a genuine resource (kind: resource) exposed over
// RPC. This is what makes `kind` load-bearing — tagging a resource enrolls it
// into generation, replacing a hand-maintained name allowlist. (Legacy
// unclassified facades, kind: "", are skipped; operation/query kinds will extend
// this predicate once their surfaces are generated.)
func (r *ResourceTemplate) generatesGate() bool {
	return r.Kind == "resource" && r.RpcService
}

// ResourceSchema declares a resource's shape for CRUD synthesis: its Fields and
// the Key field that identifies one instance.
type ResourceSchema struct {
	Key    string      `json:"key"`
	Fields []ActionArg `json:"fields"`
	// Verbs selects which standard verbs to synthesize (create/get/list/delete/
	// watch). Empty means the full set; a subset lets a resource expose only what
	// it backs (e.g. a Certificate that is list/get/delete with no create). list
	// implies get (the object shape is the get response). apply is added
	// separately when Declarative.
	Verbs []string `json:"verbs,omitempty"`
	// Declarative marks a resource whose desired state is set by an idempotent
	// upsert (k8s-style): CRUD synthesis adds an `apply` verb, and a reconcile
	// loop will key off this flag. Imperative resources (Declarative=false) use
	// only create (fail-if-exists).
	Declarative bool `json:"declarative,omitempty"`
	// Reconcile, on a declarative resource, describes how the control plane
	// converges the desired state to the dataplane: which dp kind to fan out to
	// and which southbound service RPC to call per desired item. GenerateReconcile
	// emits the controller from it.
	Reconcile *ReconcileSpec `json:"reconcile,omitempty"`
	// GeneratedStore opts a declarative resource into a generated backing store
	// (service/<pkg>/store_gen.go) — the map[key]*DTO upsert/list/get/delete +
	// OnChange/Desired/notify + Snapshot/Restore that were otherwise hand-written.
	// GenerateStore emits it; the hand-written handlers.go/persist.go are removed.
	// `sensitive` fields are blanked from list/get/apply responses by the generated
	// redacted() (kept full in Desired for reconcile). Resources with bespoke logic
	// (vip's observed status, secret's generated value) keep this false.
	GeneratedStore bool `json:"generated_store,omitempty"`
}

// ReconcileSpec is the southbound binding for a declarative resource's reconcile
// controller. The request type is derived as pb.<Service><Method>Request and is
// built per desired item with RequestField set to that item.
type ReconcileSpec struct {
	// Custom marks a resource whose reconcile loop is HAND-WRITTEN (reconcile/<r>.go),
	// not generated — for pushes the generated broadcast/per-node emitter can't express
	// (e.g. open_port: its []string "proto:port" ports need parsing into the southbound
	// []*PortInfo). The store still gets its reconcile surface (Desired/OnChange) because
	// Reconcile != nil; only GenerateReconcile skips it. DpType is the only field needed.
	Custom       bool   `json:"custom,omitempty"`
	DpType       string `json:"dp_type"`       // dataplane kind to fan out to (e.g. l4lb)
	Service      string `json:"service"`       // southbound pb service (e.g. DataplaneService)
	Method       string `json:"method"`        // southbound rpc (e.g. UpdateVip)
	RequestField string `json:"request_field"` // request field that receives each desired item
	// RequestBytes converts each desired item (a string) to []byte for the request
	// field (for byte-typed southbound fields like SyncSecret.shared_secret).
	RequestBytes bool `json:"request_bytes,omitempty"`
	// RequestFields is the multi-field alternative to RequestField: when set, the
	// desired element is the resource's object DTO (not a scalar key), and each
	// mapping copies a schema field into a request field — e.g. wasm_module's
	// {module_id,file_path,method,path} -> WasmService.Register{id,binary_path,
	// method,path}. RequestField/RequestBytes are ignored when this is set.
	RequestFields []ReconcileFieldMap `json:"request_fields,omitempty"`
	// PerNode makes the reconcile node-targeted instead of broadcast: the desired
	// element carries a node selector (NodeField) and each item is pushed ONLY to
	// that node (resolved via broker.Resolve), not fanned to every dp_type node.
	// For inherently per-node state like the XDP interface each node binds (node1
	// -> eth0, node2 -> enp3s0), which a global set cannot express. The desired
	// element is the object DTO (so the node + value travel together).
	PerNode bool `json:"per_node,omitempty"`
	// NodeField is the schema field holding the node selector (broker.Resolve form:
	// short name, dp_type/name, or full CommonName). Required when PerNode.
	NodeField string `json:"node_field,omitempty"`
	// StatusObservedFrom, when set, generates a status Observer: a dotted path into
	// the per-node reported pbstat.Stats whose StringList leaf holds this resource's
	// actual set (e.g. "CdnAppRealtime.Vip"). The Observer reports, per desired
	// item, the nodes whose latest stats list it — the resource's observed status,
	// closing the reconcile loop.
	StatusObservedFrom string `json:"status_observed_from,omitempty"`
	// Prune, when set (multi-field reconcile only), makes the loop converge on
	// deletion too: after pushing the desired set it lists what is actually on the
	// node and removes anything no longer desired. Without it the reconcile is
	// additive (a deleted item lingers on the node).
	Prune *ReconcilePrune `json:"prune,omitempty"`
}

// ReconcilePrune is the southbound binding for deletion: list the node's actual
// items (ListMethod -> the repeated ListField; each item's key is ItemKey) and
// remove (RemoveMethod with RemoveField set to the key) any whose key is not in
// the desired set. Keys are matched against the resource's schema key.
type ReconcilePrune struct {
	ListMethod   string `json:"list_method"`   // rpc returning the actual items (takes Empty)
	ListField    string `json:"list_field"`    // repeated field on the list response
	ItemKey      string `json:"item_key"`      // key field on each actual item
	RemoveMethod string `json:"remove_method"` // rpc removing one item
	RemoveField  string `json:"remove_field"`  // request field that receives the key
}

// ReconcileFieldMap copies one schema field (From) into one southbound request
// field (To) for a multi-field reconcile (see ReconcileSpec.RequestFields).
type ReconcileFieldMap struct {
	From string `json:"from"` // schema field name (source on the desired object DTO)
	To   string `json:"to"`   // request field name (destination on the southbound request)
}

// PeeringSpec is a membership-driven peering binding (NOT a declarative-store
// fanout): the control plane derives the SOURCE dataplane's ready nodes (those
// passing the stat membership gate — running + bound interface + non-zero
// ServerID) as DestEntries from the live stat cache, and pushes that set to every
// TARGET dataplane node via Service.Method (a repeated Destination field).
// PrependSelf puts the target node's OWN entry at index 0 — the l4lb eBPF dest
// table's special source-fill slot; reverse peering (popcache IPIP remotes) leaves
// it off. Generates one reconcile.Peering<Source><Target> controller each.
type PeeringSpec struct {
	Source       string `json:"source"`        // dp_type providing the set (e.g. popcache)
	Target       string `json:"target"`        // dp_type receiving the push (e.g. l4lb)
	Service      string `json:"service"`       // southbound pb service (e.g. DataplaneService)
	Method       string `json:"method"`        // southbound rpc (e.g. UpdateDestinations)
	RequestField string `json:"request_field"` // repeated Destination field on the request
	PrependSelf  bool   `json:"prepend_self,omitempty"`
}

// ExpandResourceSchemas is the pre-pass that turns a declared resource schema
// into the standard CRUD actions, so a kind: resource need only declare its
// shape (not each verb). It runs after loading resource.yaml and before any
// generator, mutating the in-memory Definition: every generator downstream then
// sees create/get/list/delete as if they had been hand-written. Drivers
// (cmd/gen, cmd/gen_access) must call this once after unmarshal.
func (d *Definition) ExpandResourceSchemas() {
	for i := range d.Resources {
		r := &d.Resources[i]
		if r.Schema == nil {
			continue
		}
		// Synthesized CRUD goes first; any hand-written actions (custom
		// operations on the resource) follow.
		r.Actions = append(synthesizeCRUD(r.Name, r.Schema), r.Actions...)
	}
}

// synthesizeCRUD builds the standard create/get/list/delete actions for a
// resource schema. The full object is the schema's fields; create takes only the
// settable (non-readonly) fields but returns the object; get takes the key and
// returns the object; delete takes and echoes the key; list returns the objects
// as `repeated <GetResponseDTO>` (the get response IS the object, reused as the
// list element — see the []obj: convention in goTypeToProtoType).
func synthesizeCRUD(resourceName string, s *ResourceSchema) []Action {
	verbs := s.Verbs
	if len(verbs) == 0 {
		verbs = []string{"create", "get", "list", "delete", "watch"}
	}
	// list needs the object shape, which is the get response.
	if contains(verbs, "list") && !contains(verbs, "get") {
		verbs = append(verbs, "get")
	}
	// A declarative resource always gets the idempotent upsert.
	if s.Declarative && !contains(verbs, "apply") {
		verbs = append(verbs, "apply")
	}
	actions := make([]Action, 0, len(verbs))
	for _, v := range verbs {
		actions = append(actions, buildVerb(resourceName, s, v))
	}
	return actions
}

// buildVerb constructs the synthesized Action for one standard verb. The object
// is the schema's fields; create/apply take the settable fields and return the
// object, get takes the key and returns the object, delete takes/echoes the key,
// list returns the objects (repeated get response), watch streams the object.
func buildVerb(resourceName string, s *ResourceSchema, verb string) Action {
	key := keyField(s)
	switch verb {
	case "create", "apply":
		return Action{Name: verb, CommandName: StringOrArray{verb}, Args: settableFields(s), Response: &ActionResponse{Fields: s.Fields}}
	case "get":
		return Action{Name: "get", CommandName: StringOrArray{"get"}, Args: []ActionArg{key}, Response: &ActionResponse{Fields: s.Fields}}
	case "delete":
		return Action{Name: "delete", CommandName: StringOrArray{"delete"}, Args: []ActionArg{key}, Response: &ActionResponse{Fields: []ActionArg{key}}}
	case "list":
		objectDTO := fmt.Sprintf("Resource%sActionGetResponseDTO", toCamelCase(resourceName))
		return Action{Name: "list", CommandName: StringOrArray{"list"}, Response: &ActionResponse{Fields: []ActionArg{{Name: "items", Type: "[]obj:" + objectDTO}}}}
	case "watch":
		return Action{Name: "watch", CommandName: StringOrArray{"watch"}, Response: &ActionResponse{Stream: true, Fields: s.Fields}}
	default:
		panic(fmt.Errorf("buildVerb: unknown verb %q for resource %q (want create/get/list/delete/watch/apply)", verb, resourceName))
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// settableFields returns the schema fields a client may set on create (all
// non-readonly fields).
func settableFields(s *ResourceSchema) []ActionArg {
	var out []ActionArg
	for _, f := range s.Fields {
		if !f.ReadOnly {
			out = append(out, f)
		}
	}
	return out
}

// keyField returns the schema field named by Key, panicking if absent (a schema
// authoring error worth failing generation over).
func keyField(s *ResourceSchema) ActionArg {
	for _, f := range s.Fields {
		if f.Name == s.Key {
			return f
		}
	}
	panic(fmt.Errorf("resource schema key %q is not among its fields", s.Key))
}

type ResourceTemplate struct {
	Name        string        `json:"name"`
	Actions     []Action      `json:"actions"`
	Attributes  []Attribute   `json:"attributes"`
	CommandName StringOrArray `json:"command_name,omitempty"`
	// Kind classifies the resource for the untangle taxonomy (kscale charter):
	//   resource  — a genuine declarative noun: standard CRUD verbs, authz, and
	//               (when it has desired state) a reconcile loop are generated.
	//   operation — an imperative RPC (k8s subresource verb, e.g. garbage_collect).
	//   query     — a read/stream RPC (e.g. list_connections, stats).
	// empty is the legacy facade grouping (unclassified, not generated). Today
	// kind drives the generated RPC surface (see generatesGate); the
	// operation/query kinds and kind-specific CRUD/Watch/reconcile generation
	// extend from here.
	Kind string `json:"kind,omitempty"`
	// Schema, when set on a kind: resource, declares the resource's shape (its
	// fields + identifying key) and lets ExpandResourceSchemas synthesize the
	// standard CRUD actions (create/get/list/delete) instead of hand-declaring
	// each verb. Synthesized actions are appended to any hand-written Actions
	// (so a resource can carry CRUD + extra operations).
	Schema *ResourceSchema `json:"schema,omitempty"`
	// RpcService が true の resource は GenerateProtoDefinition が
	// `service {Resource}Service { rpc {Action}(ArgsDTO) returns (ResponseDTO) }`
	// を access.proto に emit する (admin command の per-command typed RPC 化、
	// ksdk notes/ai/2026_06_02_admin_shell_rpc_design.md Phase 2)。
	RpcService bool `json:"rpc_service,omitempty"`
	// RpcAdapterDir is the repo-relative directory that GenerateRPCAdapters
	// writes this resource's server adapter (`{name}_rpc_gen.go`) into. The Go
	// package name is the directory's base element (e.g. agent/admin → admin).
	// Required when RpcService is true. The adapter bridges the generated
	// {Resource}Service proto server onto the hand-written {Resource}Handlers
	// wrappers, so it must live in the same package as those handlers
	// (ksdk notes/ai/2026_06_02_admin_shell_rpc_design.md Phase 2c).
	RpcAdapterDir string `json:"rpc_adapter_dir,omitempty"`
	// Monitor holds nodewatch tool hints keyed by action command_name (e.g.
	// list/get/diff): a one-line "what it returns / when to use it" description
	// that flows into ActionSpec.Description so the monitor agent's generated
	// read-tools carry intent guidance instead of a formulaic template. Optional;
	// actions with no entry fall back to the generic tool description.
	Monitor map[string]string `json:"monitor,omitempty"`
}

type AttributeType struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Attribute struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Environment struct {
	Attributes []Attribute `json:"attributes"`
}

type ParseMethod struct {
	Type     string `json:"type"`
	Format   string `json:"format,omitempty"`
	HasError bool   `json:"has_error,omitempty"`
}

type ResourceFile struct {
	Imports   []string           `json:"imports,omitempty"`
	Resources []ResourceTemplate `json:"resources"`
	Peering   []PeeringSpec      `json:"peering,omitempty"`
}

type Definition struct {
	ResourceFile
	Actions     []Action        `json:"actions"`
	Attributes  []AttributeType `json:"attributes"`
	Environment Environment     `json:"environment"`
	Policies    []Policy        `json:"policies"`
}

type Policy struct {
	Name      string     `json:"name"`
	Condition *string    `json:"condition,omitempty"`
	And       *[]*Policy `json:"and,omitempty"`
	Or        *[]*Policy `json:"or,omitempty"`
	Xor       *[]*Policy `json:"xor,omitempty"`
	Not       *Policy    `json:"not,omitempty"`
	Policy    string     `json:"policy"` // Allow or Deny
	Resource  string     `json:"resource,omitempty"`
	Action    []string   `json:"action,omitempty"`
	Role      []string   `json:"role,omitempty"`
}

type Writer struct {
	W io.Writer
}

func (w *Writer) Printf(format string, a ...any) (n int, err error) {
	return fmt.Fprintf(w.W, format, a...)
}

type indentWriter struct {
	io.Writer
	indent  string
	wasLine bool
}

func (iw *indentWriter) Write(p []byte) (n int, err error) {
	totalWritten := 0
	for i := 0; i < len(p); {
		if iw.wasLine {
			wn, err := iw.Writer.Write([]byte(iw.indent))
			totalWritten += wn
			if err != nil {
				return totalWritten, err
			}
			iw.wasLine = false
		}
		j := i
		for j < len(p) && p[j] != '\n' {
			j++
		}
		wn, err := iw.Writer.Write(p[i:j])
		totalWritten += wn
		if err != nil {
			return totalWritten, err
		}
		i = j
		if i < len(p) && p[i] == '\n' {
			wn, err := iw.Writer.Write([]byte{'\n'})
			totalWritten += wn
			if err != nil {
				return totalWritten, err
			}
			i++
			iw.wasLine = true
		}
	}
	return totalWritten, nil
}

func (w *Writer) Indented(fn func(*Writer)) {
	indentedWriter := &Writer{
		W: &indentWriter{w.W, "  ", true},
	}
	fn(indentedWriter)
}

type Input struct {
	Definition *Definition `json:"definition"`
}

func GenerateResource(w *Writer, r *ResourceTemplate) {
	w.Printf("access.NewResourceTemplate(%q,nil,[]access.Attribute{\n", r.Name)
	w.Indented(func(w *Writer) {
		for _, attr := range r.Attributes {
			w.Printf("access.NewAttribute(%q,%q),\n", attr.Name, attr.Value)
		}
	})
	w.Printf("},[]access.Action{\n")
	w.Indented(func(w *Writer) {
		for _, act := range r.Actions {
			w.Printf("Resource%sAction%s,\n", toCamelCase(r.Name), toCamelCase(act.Name))
		}
	})
	w.Printf("}),\n")
}

func (p *Policy) Validate(root bool) error {
	count := 0
	if p.Condition != nil {
		count++
	}
	if p.And != nil {
		count++
	}
	if p.Or != nil {
		count++
	}
	if p.Xor != nil {
		count++
	}
	if p.Not != nil {
		count++
	}
	if count > 1 {
		return fmt.Errorf("policy %q: only one of condition, and, or, xor can be set", p.Name)
	}
	if count == 0 {
		return fmt.Errorf("policy %q: no condition specified", p.Name)
	}
	if root {
		if p.Policy != "Allow" && p.Policy != "Deny" {
			return fmt.Errorf("policy %q: invalid policy value %q, must be 'Allow' or 'Deny'", p.Name, p.Policy)
		}
		if p.Resource == "" {
			return fmt.Errorf("policy %q: resource must be specified for root policies", p.Name)
		}
		if len(p.Action) == 0 {
			return fmt.Errorf("policy %q: at least one action must be specified for root policies", p.Name)
		}
		for _, act := range p.Action {
			if act == "" {
				return fmt.Errorf("policy %q: action names cannot be empty", p.Name)
			}
		}
	} else {
		if p.Policy != "" {
			return fmt.Errorf("policy %q: nested policies cannot have policy value set", p.Name)
		}
	}
	if p.And != nil {
		for _, sub := range *p.And {
			if err := sub.Validate(false); err != nil {
				return err
			}
		}
	}
	if p.Or != nil {
		for _, sub := range *p.Or {
			if err := sub.Validate(false); err != nil {
				return err
			}
		}
	}
	if p.Xor != nil {
		for _, sub := range *p.Xor {
			if err := sub.Validate(false); err != nil {
				return err
			}
		}
	}
	if p.Not != nil {
		if err := p.Not.Validate(false); err != nil {
			return err
		}
	}
	return nil
}

func GeneratePolicy(w *Writer, p *Policy) {
	if err := p.Validate(true); err != nil {
		panic(err)
	}
	generatePolicyUnchecked(w, p, true)
}

func generatePolicyUnchecked(w *Writer, p *Policy, root bool) {
	if root {
		w.Printf("policy.NewAndPolicy(%q,\n", p.Name)
		w.Indented(func(w *Writer) {
			w.Printf("policy.MustParsePolicy(\"ResourceIs%s\",\"$resource.name == `%s`\"),\n", toCamelCase(p.Resource), p.Resource)
			if len(p.Action) == 1 {
				w.Printf("policy.MustParsePolicy(\"ActionIs%s\",\"$action.name == `%s`\"),\n", toCamelCase(p.Action[0]), p.Action[0])
			} else {
				w.Printf("policy.NewOrPolicy(%q,\n", p.Name+"Actions")
				w.Indented(func(w *Writer) {
					for _, act := range p.Action {
						w.Printf("policy.MustParsePolicy(\"ActionIs%s\",\"$action.name == `%s`\"),\n", toCamelCase(act), act)
					}
				})
				w.Printf("),\n")
			}
			// main condition
			generatePolicyUnchecked(w, p, false)
			w.Printf(",\n")
		})
		w.Printf(")")
		return
	}
	if p.Condition != nil {
		w.Printf("policy.MustParsePolicy(%q,%q)", p.Name, *p.Condition)
	}
	if p.And != nil {
		w.Printf("policy.NewAndPolicy(%q,\n", p.Name)
		w.Indented(func(w *Writer) {
			for _, sub := range *p.And {
				generatePolicyUnchecked(w, sub, false)
				w.Printf(",\n")
			}
		})
		w.Printf(")")
	}
	if p.Or != nil {
		w.Printf("policy.NewOrPolicy(%q,\n", p.Name)
		w.Indented(func(w *Writer) {
			for _, sub := range *p.Or {
				generatePolicyUnchecked(w, sub, false)
				w.Printf(",\n")
			}
		})
		w.Printf(")")
	}
	if p.Xor != nil {
		w.Printf("policy.NewXorPolicy(%q,\n", p.Name)
		w.Indented(func(w *Writer) {
			for _, sub := range *p.Xor {
				generatePolicyUnchecked(w, sub, false)
				w.Printf(",\n")
			}
		})
		w.Printf(")")
	}
	if p.Not != nil {
		w.Printf("policy.NewNotPolicy(%q,\n", p.Name)
		w.Indented(func(w *Writer) {
			generatePolicyUnchecked(w, p.Not, false)
			w.Printf(",\n")
		})
		w.Printf(")")
	}
}

func toCamelCase(s string) string {
	result := ""
	capitalizeNext := true
	for _, ch := range s {
		if ch == '_' || ch == '-' || ch == ' ' {
			capitalizeNext = true
			continue
		}
		if capitalizeNext {
			if 'a' <= ch && ch <= 'z' {
				ch -= 'a' - 'A'
			}
			capitalizeNext = false
		}
		result += string(ch)
		// Match protoc-gen-go's GoCamelCase: a letter immediately following a digit
		// starts a new word and is uppercased ("l4lb" -> "L4Lb"). Without this, the
		// names we emit here diverge from the Go types protoc generates for the same
		// proto messages, producing "undefined: ResourceL4lb..." build errors.
		if '0' <= ch && ch <= '9' {
			capitalizeNext = true
		}
	}
	return result
}

func GenerateDefinition(w *Writer, d *Input) {
	w.Printf("// Code generated by ksdk/access/generator. DO NOT EDIT.\n\n")
	w.Printf("package predefined\n\n")
	w.Printf("import (\n")
	w.Printf("  \"github.com/on-keyday/kscale/access\"\n")
	w.Printf("  \"github.com/on-keyday/kscale/access/policy\"\n")
	w.Printf(")\n\n")
	w.Printf("func Resources() []access.ResourceTemplate {\n")
	w.Indented(func(w *Writer) {
		w.Printf("return []access.ResourceTemplate{\n")
		w.Indented(func(w *Writer) {
			for _, r := range d.Definition.Resources {
				GenerateResource(w, &r)
			}
		})
		w.Printf("}\n")
	})
	w.Printf("}\n")

	// DataplaneTypes: the distinct concrete reconcile dp_type values declared by
	// resources (excluding "*"). Generated so the set of dataplane kinds derives
	// from resource.yaml (kscale charter) rather than a hand-maintained switch —
	// a new dp_type is picked up automatically the moment a resource declares
	// reconcile.dp_type: X.
	dpTypeSet := map[string]struct{}{}
	for _, r := range d.Definition.Resources {
		if r.Schema != nil && r.Schema.Reconcile != nil {
			if t := r.Schema.Reconcile.DpType; t != "" && t != "*" {
				dpTypeSet[t] = struct{}{}
			}
		}
	}
	dpTypes := make([]string, 0, len(dpTypeSet))
	for t := range dpTypeSet {
		dpTypes = append(dpTypes, t)
	}
	sort.Strings(dpTypes)
	quoted := make([]string, len(dpTypes))
	for i, t := range dpTypes {
		quoted[i] = fmt.Sprintf("%q", t)
	}
	list := strings.Join(quoted, ", ")
	w.Printf("\n// DataplaneTypes are the distinct reconcile dp_type values declared in\n")
	w.Printf("// resource.yaml (excluding \"*\"). A new dp_type appears here automatically\n")
	w.Printf("// when a resource declares reconcile.dp_type.\n")
	w.Printf("func DataplaneTypes() []string {\n")
	w.Indented(func(w *Writer) { w.Printf("return []string{%s}\n", list) })
	w.Printf("}\n")
	w.Printf("\n// IsDataplaneType reports whether app is one of the generated dataplane dp_types.\n")
	w.Printf("func IsDataplaneType(app string) bool {\n")
	w.Indented(func(w *Writer) {
		if list == "" {
			w.Printf("return false\n")
			return
		}
		w.Printf("switch app {\n")
		w.Printf("case %s:\n", list)
		w.Indented(func(w *Writer) { w.Printf("return true\n") })
		w.Printf("}\n")
		w.Printf("return false\n")
	})
	w.Printf("}\n")

	// Define Constants for resource and action names
	var resourceActionMap = make(map[string]map[string]struct{})
	for _, r := range d.Definition.Resources {
		w.Printf("\n")
		w.Printf("const Resource%s = %q\n", toCamelCase(r.Name), r.Name)
		for _, a := range r.Actions {
			args := make([]string, 0, len(a.Args))
			for _, arg := range a.Args {
				args = append(args, arg.Name)
			}
			w.Printf("var Resource%sAction%s = access.NewAction(%q, %#v)\n", toCamelCase(r.Name), toCamelCase(a.Name), a.Name, args)
			if _, ok := resourceActionMap[r.Name]; !ok {
				resourceActionMap[r.Name] = make(map[string]struct{})
			}
			resourceActionMap[r.Name][a.Name] = struct{}{}
			for _, arg := range a.Args {
				w.Printf("const Resource%sAction%sArg%s = %q\n", toCamelCase(r.Name), toCamelCase(a.Name), toCamelCase(arg.Name), arg.Name)
			}
		}
	}

	w.Printf("\n")
	// first, validate all policies
	for _, p := range d.Definition.Policies {
		if err := p.Validate(true); err != nil {
			panic(err)
		}
		// Check for valid resource and action names
		if _, ok := resourceActionMap[p.Resource]; !ok {
			panic(fmt.Errorf("policy %q: unknown resource %q", p.Name, p.Resource))
		}
		for _, a := range p.Action {
			if _, ok := resourceActionMap[p.Resource][a]; !ok {
				panic(fmt.Errorf("policy %q: unknown action %q for resource %q", p.Name, a, p.Resource))
			}
		}
	}

	// then generate policy functions
	for _, p := range d.Definition.Policies {
		w.Printf("var Policy%s = ", toCamelCase(p.Name))
		generatePolicyUnchecked(w, &p, true)
		w.Printf("\n\n")
	}

	w.Printf("func AllowPolicies() []access.Policy {\n")
	w.Indented(func(w *Writer) {
		w.Printf("return []access.Policy{\n")
		w.Indented(func(w *Writer) {
			for _, p := range d.Definition.Policies {
				if p.Policy != "Allow" {
					continue
				}
				w.Printf("Policy%s,\n", toCamelCase(p.Name))
			}
		})
		w.Printf("}\n")
	})
	w.Printf("}\n")

	w.Printf("\n")
	w.Printf("func DenyPolicies() []access.Policy {\n")
	w.Indented(func(w *Writer) {
		w.Printf("return []access.Policy{\n")
		w.Indented(func(w *Writer) {
			for _, p := range d.Definition.Policies {
				if p.Policy != "Deny" {
					continue
				}
				w.Printf("Policy%s,\n", toCamelCase(p.Name))
			}
		})
		w.Printf("}\n")
	})
	w.Printf("}\n")

	type PolicyList struct {
		Deny  []*Policy
		Allow []*Policy
	}

	w.Printf("\n")
	w.Printf("var PolicyMap = map[string]map[string]*access.PolicyList{\n")
	w.Indented(func(w *Writer) {
		resourcePolicyMap := make(map[string]map[string]*PolicyList)
		actionOrderPerResource := make(map[string][]string)
		resourceOrder := make([]string, 0)
		for _, p := range d.Definition.Policies {
			if _, ok := resourcePolicyMap[p.Resource]; !ok {
				resourcePolicyMap[p.Resource] = make(map[string]*PolicyList)
				actionOrderPerResource[p.Resource] = make([]string, 0)
				resourceOrder = append(resourceOrder, p.Resource)
			}
			pl := resourcePolicyMap[p.Resource]
			for _, a := range p.Action {
				if _, ok := pl[a]; !ok {
					pl[a] = &PolicyList{}
					actionOrderPerResource[p.Resource] = append(actionOrderPerResource[p.Resource], a)
				}
				switch p.Policy {
				case "Allow":
					pl[a].Allow = append(pl[a].Allow, &p)
				case "Deny":
					pl[a].Deny = append(pl[a].Deny, &p)
				}
			}
		}
		for _, res := range resourceOrder {
			actMap := resourcePolicyMap[res]
			w.Printf("%q: map[string]*access.PolicyList{\n", res)
			w.Indented(func(w *Writer) {
				for _, act := range actionOrderPerResource[res] {
					pl := actMap[act]
					w.Printf("%q: &access.PolicyList{\n", act)
					w.Indented(func(w *Writer) {
						// Deny policies
						w.Printf("Deny: []access.Policy{\n")
						w.Indented(func(w *Writer) {
							for _, p := range pl.Deny {
								w.Printf("Policy%s,\n", toCamelCase(p.Name))
							}
						})
						w.Printf("},\n")
						// Allow policies
						w.Printf("Allow: []access.Policy{\n")
						w.Indented(func(w *Writer) {
							for _, p := range pl.Allow {
								w.Printf("Policy%s,\n", toCamelCase(p.Name))
							}
						})
						w.Printf("},\n")
					})
					w.Printf("},\n")
				}
			})
			w.Printf("},\n")
		}
	})
	w.Printf("}\n")
}

func GenerateConfigDefinition(w io.Writer, d *Input) {
	writer := &Writer{W: w}
	writer.Printf("// Code generated by ksdk/access/generator. DO NOT EDIT.\n\n")
	writer.Printf("package config\n\n")
	writer.Printf("import (\n")
	writer.Indented(func(w *Writer) {
		w.Printf("\"github.com/on-keyday/kscale/access/predefined\"\n")
		w.Printf("\"github.com/on-keyday/kscale/agent\"\n")
		w.Printf("\"fmt\"\n")
		w.Printf("\"encoding/json\"\n")
		w.Printf("\n\"net/netip\"\n")
		w.Printf("\"github.com/on-keyday/objtrsf/objproto\"\n")
		w.Printf("\"github.com/on-keyday/kscale/ca\"\n")
		w.Printf("pb \"github.com/on-keyday/kscale/protobuf/proto\"\n")
		w.Printf("pbaccess \"github.com/on-keyday/kscale/protobuf/proto/access\"\n")
		w.Printf("\"github.com/on-keyday/kscale/protobuf/wkt\"\n")
		w.Printf("\"time\"\n")
		w.Printf("\"strings\"\n")
		w.Printf("\"strconv\"\n")
	})
	writer.Printf(")\n\n")
	///*
	parseMethodMap := map[string]ParseMethod{
		"string": {
			Type:   "string",
			Format: "$arg",
		},
		"time.Duration": {
			Type:     "time.Duration",
			Format:   "time.ParseDuration($arg)",
			HasError: true,
		},
		"netip.Addr": {
			Type:     "netip.Addr",
			Format:   "netip.ParseAddr($arg)",
			HasError: true,
		},
		"objproto.ConnectionID": {
			Type:     "objproto.ConnectionID",
			Format:   "objproto.ParseConnectionID($arg,0)",
			HasError: true,
		},
		"*objproto.ConnectionID": {
			Type:     "*objproto.ConnectionID",
			Format:   "ParseOptionalConnectionID($arg)",
			HasError: true,
		},
		"[]string": {
			Type:     "[]string",
			Format:   "splitStrings($arg)",
			HasError: false,
		},
		"uint64": {
			Type:     "uint64",
			Format:   "strconv.ParseUint($arg,10,64)",
			HasError: true,
		},
		"uint32": {
			Type:     "uint32",
			Format:   "parseUint32($arg)",
			HasError: true,
		},
		"bool_enable": {
			Type:     "bool",
			Format:   "parseBoolEnable($arg)",
			HasError: true,
		},
		"[]*pb.PortInfo": {
			Type:     "[]*pb.PortInfo",
			Format:   "parsePortInfos($arg)",
			HasError: true,
		},
	}
	//*/

	for _, r := range d.Definition.Resources {
		for _, a := range r.Actions {
			for _, arg := range a.Args {
				pm, ok := parseMethodMap[arg.Type]
				if !ok {
					panic(fmt.Errorf("no parse method for type %q", arg.Type))
				}
				writer.Printf("func Resource%sAction%sArg%s(v %s) agent.ConfigField {\n", toCamelCase(r.Name), toCamelCase(a.Name), toCamelCase(arg.Name), pm.Type)
				writer.Printf("    return agent.ConfigField{\n")
				writer.Printf("        Key: predefined.Resource%sAction%sArg%s,\n", toCamelCase(r.Name), toCamelCase(a.Name), toCamelCase(arg.Name))
				switch arg.Type {
				case "*objproto.ConnectionID":
					writer.Printf("        Value: connIDToMap(v),\n")
				case "objproto.ConnectionID":
					writer.Printf("        Value: connIDToMap(&v),\n")
				case "bool_enable":
					writer.Printf("        Value: boolToEnable(v),\n")
				case "uint64":
					// to int64
					writer.Printf("        Value: int64(v),\n")
				case "uint32":
					// to int64
					writer.Printf("        Value: int64(v),\n")
				default:
					if strings.Contains(arg.Type, "pb.") {
						if strings.HasPrefix(arg.Type, "[]") {
							writer.Printf("        Value: pb.ArrayToPolicyArg(v),\n")
						} else {
							writer.Printf("        Value: pb.ToPolicyArg(v),\n")
						}
					} else {
						writer.Printf("        Value: v,\n")
					}
				}
				writer.Printf("    }\n")
				writer.Printf("}\n")
			}
			writer.Printf("\n")
			writer.Printf("func Resource%sAction%sArgs(", toCamelCase(r.Name), toCamelCase(a.Name))
			for i, arg := range a.Args {
				if i > 0 {
					writer.Printf(", ")
				}
				pm, ok := parseMethodMap[arg.Type]
				if !ok {
					panic(fmt.Errorf("no parse method for type %q", arg.Type))
				}
				writer.Printf("%s %s", arg.Name, pm.Type)
			}
			writer.Printf(") []agent.ConfigField {\n")
			writer.Printf("    return []agent.ConfigField{\n")
			for _, arg := range a.Args {
				writer.Printf("        Resource%sAction%sArg%s(%s),\n", toCamelCase(r.Name), toCamelCase(a.Name), toCamelCase(arg.Name), arg.Name)
			}
			writer.Printf("    }\n")
			writer.Printf("}\n\n")

			dtoType := fmt.Sprintf("Resource%sAction%sArgsDTO", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Printf("func (t *%s) Parse(args []string) error {\n", dtoType)
			requiredArgCount := 0
			hasError := false
			for _, arg := range a.Args {
				if arg.Default == nil {
					requiredArgCount++
				}
				pm, ok := parseMethodMap[arg.Type]
				if !ok {
					panic(fmt.Errorf("no parse method for type %q", arg.Type))
				}
				hasError = hasError || pm.HasError
			}
			writer.Indented(func(w *Writer) {
				if requiredArgCount == len(a.Args) && len(a.Args) > 0 {
					w.Printf("if len(args) != %d {\n", requiredArgCount)
					w.Indented(func(w *Writer) {
						args := make([]string, 0, len(a.Args))
						for _, arg := range a.Args {
							args = append(args, arg.Name)
						}
						w.Printf("return fmt.Errorf(\"expected %d args (%s) but got %%d args\",len(args))\n", requiredArgCount, strings.Join(args, ", "))
					})
					w.Printf("}\n")

				} else {
					argNames := make([]string, 0, len(a.Args))
					for _, arg := range a.Args {
						argNames = append(argNames, arg.Name)
					}
					if requiredArgCount > 0 {
						w.Printf("if len(args) < %d {\n", requiredArgCount)
						w.Indented(func(w *Writer) {
							w.Printf("return fmt.Errorf(\"expected least %d args(%s) but got %%d args\",len(args))\n", requiredArgCount, strings.Join(argNames, ", "))
						})
						w.Printf("}\n")
					}
					if len(a.Args) > 0 {
						w.Printf("if len(args) > %d {\n", len(a.Args))
						w.Indented(func(w *Writer) {
							w.Printf("return fmt.Errorf(\"expected at most %d args (%s) but got %%d args\",len(args))\n", len(a.Args), strings.Join(argNames, ", "))
						})
						w.Printf("}\n")
					}
				}
				if hasError {
					w.Printf("var err error\n")
				}
				for i, arg := range a.Args {
					pm, ok := parseMethodMap[arg.Type]
					if !ok {
						panic(fmt.Errorf("no parse method for type %q", arg.Type))
					}
					doParse := func(arg string) {
						parse := strings.Replace(pm.Format, "$arg", arg, -1)
						if pm.HasError {
							w.Printf("v%d, err = %s\n", i, parse)
							w.Printf("if err != nil {\n")
							w.Indented(func(w *Writer) {
								w.Printf("return fmt.Errorf(\"failed to parse arg %d: %%w\", err)\n", i)
							})
							w.Printf("}\n")
						} else {
							w.Printf("v%d = %s\n", i, parse)
						}
					}
					w.Printf("var v%d %s\n", i, pm.Type)
					if arg.Default != nil {
						w.Printf("if len(args) > %d {\n", i)
						w.Indented(func(w *Writer) {
							doParse(fmt.Sprintf("args[%d]", i))
						})
						w.Printf("} else {\n")
						w.Indented(func(w *Writer) {
							// set default value
							doParse(fmt.Sprintf("%q", *arg.Default))
						})
						w.Printf("}\n")
					} else {
						doParse(fmt.Sprintf("args[%d]", i))
					}
				}
				for i := range a.Args {
					ptr := ""
					w.Printf("t.%s = %sv%d\n", toCamelCase(a.Args[i].Name), ptr, i)
				}
				w.Printf("return nil\n")
			})
			writer.Printf("}\n\n")
		}
	}

	// command name constants
	for _, r := range d.Definition.Resources {
		if len(r.CommandName) == 1 {
			writer.Printf("const CommandResource%s = %q\n", toCamelCase(r.Name), r.CommandName[0])
		} else {
			for _, cmd := range r.CommandName {
				writer.Printf("const CommandResource%s%s = %q\n", toCamelCase(r.Name), toCamelCase(cmd), cmd)
			}
		}
		for _, a := range r.Actions {
			if len(a.CommandName) == 1 {
				writer.Printf("const CommandResource%sAction%s = %q\n", toCamelCase(r.Name), toCamelCase(a.Name), a.CommandName[0])
			} else {
				for _, cmd := range a.CommandName {
					writer.Printf("const CommandResource%sAction%s%s = %q\n", toCamelCase(r.Name), toCamelCase(a.Name), toCamelCase(cmd), cmd)
				}
			}
		}
	}

	// DTO objects for arguments and ToArgs functions
	for _, r := range d.Definition.Resources {
		for _, a := range r.Actions {
			// DTO struct
			writer.Printf("type Resource%sAction%sArgsDTO struct {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				for _, arg := range a.Args {
					pm, ok := parseMethodMap[arg.Type]
					if !ok {
						panic(fmt.Errorf("no parse method for type %q", arg.Type))
					}
					w.Printf("%s %s `json:\"%s\"`\n", toCamelCase(arg.Name), pm.Type, arg.Name)
				}
			})
			writer.Printf("}\n\n")
			// ToArgs function
			writer.Printf("func (dto *Resource%sAction%sArgsDTO) ToArgs() ([]string,error) {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("args := make([]string,0,%d)\n", len(a.Args))
				for _, arg := range a.Args {
					ptrDeref := ""
					switch arg.Type {
					case "string":
						w.Printf("args = append(args,%sdto.%s)\n", ptrDeref, toCamelCase(arg.Name))
					case "int":
						w.Printf("args = append(args,fmt.Sprintf(\"%%d\",%sdto.%s))\n", ptrDeref, toCamelCase(arg.Name))
					case "uint":
						w.Printf("args = append(args,fmt.Sprintf(\"%%d\",%sdto.%s))\n", ptrDeref, toCamelCase(arg.Name))
					case "bool":
						w.Printf("args = append(args,fmt.Sprintf(\"%%t\",%sdto.%s))\n", ptrDeref, toCamelCase(arg.Name))
					case "bool_enable":
						w.Printf("args = append(args,boolToEnable(%sdto.%s))\n", ptrDeref, toCamelCase(arg.Name))
					case "[]string":
						w.Printf("args = append(args,strings.Join(%sdto.%s,\",\"))\n", ptrDeref, toCamelCase(arg.Name))
					default:
						// for protobuf message types
						if strings.Contains(arg.Type, "pb.") {
							if strings.HasPrefix(arg.Type, "[]") {
								w.Printf("{\n")
								w.Printf("    var arr []string\n")
								w.Printf("    for _, p := range %sdto.%s {\n", ptrDeref, toCamelCase(arg.Name))
								w.Printf("        b := p.ToArg()\n")
								w.Printf("        arr = append(arr, string(b))\n")
								w.Printf("    }\n")
								w.Printf("    args = append(args, strings.Join(arr, \",\"))\n")
								w.Printf("}\n")
								continue
							}
							w.Printf("b := %sdto.%s.ToArg()\n", ptrDeref, toCamelCase(arg.Name))
							w.Printf("args = append(args, string(b))\n")
							continue
						}
						w.Printf("args = append(args,fmt.Sprintf(\"%%v\",%sdto.%s))\n", ptrDeref, toCamelCase(arg.Name))
					}
				}
				w.Printf("return args,nil\n")
			})
			writer.Printf("}\n\n")
			// ToProto: typed proto message from DTO. Unsupported fields are
			// skipped (matched against goTypeToProtoType in protogen.go).
			writer.Printf("func (dto *Resource%sAction%sArgsDTO) ToProto() *pbaccess.Resource%sAction%sArgsDTO {\n", toCamelCase(r.Name), toCamelCase(a.Name), toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("out := &pbaccess.Resource%sAction%sArgsDTO{}\n", toCamelCase(r.Name), toCamelCase(a.Name))
				for _, arg := range a.Args {
					emitDTOFieldToProto(w, arg)
				}
				w.Printf("return out\n")
			})
			writer.Printf("}\n\n")
			// FromProto: populate DTO from typed proto message. Mirror of
			// ToProto. Errors only for malformed parse-able fields.
			writer.Printf("func (dto *Resource%sAction%sArgsDTO) FromProto(src *pbaccess.Resource%sAction%sArgsDTO) error {\n", toCamelCase(r.Name), toCamelCase(a.Name), toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("if src == nil { return nil }\n")
				for _, arg := range a.Args {
					emitDTOFieldFromProto(w, arg)
				}
				w.Printf("return nil\n")
			})
			writer.Printf("}\n\n")
		}
	}
	// common DTO interface and wrapper

	writer.Printf("type InputDTO interface {\n")
	writer.Indented(func(w *Writer) {
		w.Printf("ToArgs() ([]string,error)\n")
	})
	writer.Printf("}\n\n")

	writer.Printf("type ResourceActionArgsDTO struct {\n")
	writer.Indented(func(w *Writer) {
		w.Printf("ResourceName string `json:\"resource_name\"`\n")
		w.Printf("ActionName   string `json:\"action_name\"`\n")
		w.Printf("Args         InputDTO `json:\"args\"`\n")
	})
	writer.Printf("}\n\n")

	writer.Printf("func (dto *ResourceActionArgsDTO) ToArgs() ([]string,error) {\n")
	writer.Indented(func(w *Writer) {
		w.Printf("return dto.Args.ToArgs()\n")
	})
	writer.Printf("}\n\n")

	writer.Printf("func (dto *ResourceActionArgsDTO) UnmarshalJSON(data []byte) error {\n")
	writer.Indented(func(w *Writer) {
		w.Printf("var aux struct {\n")
		w.Indented(func(w *Writer) {
			w.Printf("ResourceName string `json:\"resource_name\"`\n")
			w.Printf("ActionName   string `json:\"action_name\"`\n")
			w.Printf("Args         json.RawMessage `json:\"args\"`\n")
		})
		w.Printf("}\n")
		w.Printf("if err := json.Unmarshal(data, &aux); err != nil {\n")
		w.Indented(func(w *Writer) {
			w.Printf("return err\n")
		})
		w.Printf("}\n")
		w.Printf("dto.ResourceName = aux.ResourceName\n")
		w.Printf("dto.ActionName = aux.ActionName\n")
		w.Printf("argsDTO, err := NewResourceActionArgsDTO(aux.ResourceName, aux.ActionName)\n")
		w.Printf("if err != nil {\n")
		w.Indented(func(w *Writer) {
			w.Printf("return err\n")
		})
		w.Printf("}\n")
		w.Printf("if err := json.Unmarshal(aux.Args, argsDTO); err != nil {\n")
		w.Indented(func(w *Writer) {
			w.Printf("return err\n")
		})
		w.Printf("}\n")
		w.Printf("dto.Args = argsDTO\n")
		w.Printf("return nil\n")
	})
	writer.Printf("}\n\n")

	// DTO factory per resource action
	writer.Printf("func NewResourceActionArgsDTO(resourceName string, actionName string) (InputDTO, error) {\n")
	writer.Indented(func(w *Writer) {
		w.Printf("switch resourceName {\n")
		for _, r := range d.Definition.Resources {
			w.Printf("case %q:\n", r.Name)
			w.Indented(func(w *Writer) {
				w.Printf("switch actionName {\n")
				for _, a := range r.Actions {
					w.Printf("case %q:\n", a.Name)
					w.Indented(func(w *Writer) {
						w.Printf("return &Resource%sAction%sArgsDTO{},nil\n", toCamelCase(r.Name), toCamelCase(a.Name))
					})
				}
				w.Printf("default:\n")
				w.Indented(func(w *Writer) {
					w.Printf("return nil, fmt.Errorf(\"unknown action name %%q for resource %%q\", actionName, resourceName)\n")
				})
				w.Printf("}\n")
			})
		}
		w.Printf("default:\n")
		w.Indented(func(w *Writer) {
			w.Printf("return nil, fmt.Errorf(\"unknown resource name %%q\", resourceName)\n")
		})
		w.Printf("}\n")
	})
	writer.Printf("}\n")

	// response DTOs
	for _, r := range d.Definition.Resources {
		for _, a := range r.Actions {
			if a.Response == nil || a.Response.Use != nil {
				continue
			}
			writer.Printf("type Resource%sAction%sResponseDTO struct {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				for _, resp := range a.Response.Fields {
					w.Printf("%s %s `json:\"%s\"`\n", toCamelCase(resp.Name), resp.Type, resp.Name)
				}
			})
			writer.Printf("}\n\n")
			// ToProto for response DTO. Custom object_format DTOs still
			// emit a best-effort field-by-field conversion (ignores the
			// custom format — callers should treat the proto fields as
			// the canonical representation).
			writer.Printf("func (dto *Resource%sAction%sResponseDTO) ToProto() *pbaccess.Resource%sAction%sResponseDTO {\n", toCamelCase(r.Name), toCamelCase(a.Name), toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("out := &pbaccess.Resource%sAction%sResponseDTO{}\n", toCamelCase(r.Name), toCamelCase(a.Name))
				for _, resp := range a.Response.Fields {
					emitDTOFieldToProto(w, resp)
				}
				w.Printf("return out\n")
			})
			writer.Printf("}\n\n")
			// FromProto for response DTO.
			writer.Printf("func (dto *Resource%sAction%sResponseDTO) FromProto(src *pbaccess.Resource%sAction%sResponseDTO) error {\n", toCamelCase(r.Name), toCamelCase(a.Name), toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("if src == nil { return nil }\n")
				for _, resp := range a.Response.Fields {
					emitDTOFieldFromProto(w, resp)
				}
				w.Printf("return nil\n")
			})
			writer.Printf("}\n\n")
			// ProtoTypeName + EncodeAdminResponseBody for agent.ResponseDTO interface.
			writer.Printf("func (dto *Resource%sAction%sResponseDTO) ProtoTypeName() string {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("return %q\n", fmt.Sprintf("Resource%sAction%sResponseDTO", toCamelCase(r.Name), toCamelCase(a.Name)))
			})
			writer.Printf("}\n\n")
			writer.Printf("func (dto *Resource%sAction%sResponseDTO) EncodeAdminResponseBody() ([]byte, error) {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("return dto.ToProto().Append(nil)\n")
			})
			writer.Printf("}\n\n")
			// for cli output
			writer.Printf("func (dto *Resource%sAction%sResponseDTO) ToCLIOutput() string {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				if a.Response.CLIFormat != nil {
					// replace $self with dto.
					format := *a.Response.CLIFormat
					format = strings.ReplaceAll(format, "$self.", "dto.")
					w.Printf("// custom CLI format\n")
					w.Printf("%s\n", format)
					return
				}
				w.Printf("var sb strings.Builder\n")
				for _, resp := range a.Response.Fields {
					w.Printf("sb.WriteString(fmt.Sprintf(\"%s: %%v\\n\", dto.%s))\n", resp.Name, toCamelCase(resp.Name))
				}
				w.Printf("return sb.String()\n")
			})
			writer.Printf("}\n\n")
		}
	}

	// handler interfaces
	for _, r := range d.Definition.Resources {
		for _, a := range r.Actions {
			writer.Printf("type Resource%sAction%sHandler interface {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				// method signature. Streaming responses are RPC-only; the
				// command-tree HandlerWrapper stays no-response.
				if !a.isCommandTreeResponse() {
					w.Printf("Handle(ctx *agent.AgentContext, args *Resource%sAction%sArgsDTO) error\n", toCamelCase(r.Name), toCamelCase(a.Name))
				} else {
					responseDTO := "Resource" + toCamelCase(r.Name) + "Action" + toCamelCase(a.Name) + "ResponseDTO"
					if a.Response.Use != nil {
						responseDTO = *a.Response.Use
					}
					w.Printf("Handle(ctx *agent.AgentContext, args *Resource%sAction%sArgsDTO) (*%s,error)\n", toCamelCase(r.Name), toCamelCase(a.Name), responseDTO)
				}
			})
			writer.Printf("}\n\n")
			// input modifyer interface
			writer.Printf("type Resource%sAction%sInputModifier interface {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("ModifyInput(args *Resource%sAction%sArgsDTO) error\n", toCamelCase(r.Name), toCamelCase(a.Name))
			})
			writer.Printf("}\n\n")
			// add wrapper struct from ctx.Args to typed args
			writer.Printf("type Resource%sAction%sHandlerWrapper struct {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("Handler Resource%sAction%sHandler\n", toCamelCase(r.Name), toCamelCase(a.Name))
			})
			writer.Printf("}\n\n")
			writer.Printf("func (w *Resource%sAction%sHandlerWrapper) Name() string {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				// Resource  + Action name
				w.Printf("return predefined.Resource%s + predefined.Resource%sAction%s.Name()\n", toCamelCase(r.Name), toCamelCase(r.Name), toCamelCase(a.Name))
			})
			writer.Printf("}\n\n")
			// Help method for command line help
			writer.Printf("func (w *Resource%sAction%sHandlerWrapper) Help() string {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				// simple help(only arg names)
				w.Printf("return \"Resource: %s, Action: %s Args: [", r.Name, a.Name)
				for i, arg := range a.Args {
					if i > 0 {
						w.Printf(", ")
					}
					w.Printf("%s", arg.Name)
					if arg.Default != nil {
						w.Printf("=%s", *arg.Default)
					}
				}
				w.Printf("]\"\n")
			})
			writer.Printf("}\n\n")
			writer.Printf("func (w *Resource%sAction%sHandlerWrapper) Run(ctx *agent.AgentContext) error {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("argsDTO := &Resource%sAction%sArgsDTO{}\n", toCamelCase(r.Name), toCamelCase(a.Name))
				w.Printf("if err := argsDTO.Parse(ctx.Args); err != nil {\n")
				w.Indented(func(w *Writer) {
					w.Printf("return err\n")
				})
				w.Printf("}\n")
				w.Printf("return w.HandleDTO(ctx,argsDTO)\n")
			})
			writer.Printf("}\n\n")
			writer.Printf("func (dto *Resource%sAction%sArgsDTO) CanAccess(ctx *agent.AgentContext) (*agent.AgentContext, error) {\n", toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("evalCtx := ctx.WithEnvironmentArgs(Resource%sAction%sArgs(\n", toCamelCase(r.Name), toCamelCase(a.Name))
				w.Indented(func(w *Writer) {
					for _, arg := range a.Args {
						w.Printf("dto.%s,\n", toCamelCase(arg.Name))
					}
				})
				w.Printf(")).WithResource(predefined.ResourceMap()[predefined.Resource%s])\n", toCamelCase(r.Name))
				w.Printf("if err := evalCtx.CanAccess(predefined.Resource%sAction%s); err != nil {\n", toCamelCase(r.Name), toCamelCase(a.Name))
				w.Indented(func(w *Writer) {
					w.Printf("return nil, err\n")
				})
				w.Printf("}\n")
				w.Printf("return evalCtx, nil\n")
			})
			writer.Printf("}\n\n")
			// HandleDTO method
			writer.Printf("func (w *Resource%sAction%sHandlerWrapper) HandleDTO(ctx *agent.AgentContext, argsDTO *Resource%sAction%sArgsDTO) error {\n", toCamelCase(r.Name), toCamelCase(a.Name), toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				// if Handler implements InputModifier, call ModifyInput
				w.Printf("if modifier, ok := w.Handler.(Resource%sAction%sInputModifier); ok {\n", toCamelCase(r.Name), toCamelCase(a.Name))
				w.Indented(func(w *Writer) {
					w.Printf("if err := modifier.ModifyInput(argsDTO); err != nil {\n")
					w.Indented(func(w *Writer) {
						w.Printf("return err\n")
					})
					w.Printf("}\n")
				})
				w.Printf("}\n")
				// call actual handler
				// map arguments to EnvironmentArgs
				w.Printf("evalCtx, err := argsDTO.CanAccess(ctx)\n")
				w.Printf("if err != nil {\n")
				w.Indented(func(w *Writer) {
					w.Printf("return err\n")
				})
				w.Printf("}\n")
				// call handler
				w.Printf("childCtx := evalCtx.WithArgs(nil)\n")
				if !a.isCommandTreeResponse() {
					w.Printf("return w.Handler.Handle(childCtx, argsDTO)\n")
				} else {
					w.Printf("resp, err := w.Handler.Handle(childCtx, argsDTO)\n")
					w.Printf("if err != nil {\n")
					w.Indented(func(w *Writer) {
						w.Printf("return err\n")
					})
					w.Printf("}\n")
					w.Printf("if resp != nil {\n")
					w.Printf("ctx.Respond(resp)\n")
					w.Printf("}\n")
					w.Printf("return nil\n")
				}
			})
			writer.Printf("}\n\n")
			// factory function
			writer.Printf("func NewResource%sAction%sHandlerWrapper(handler Resource%sAction%sHandler) agent.Agent {\n", toCamelCase(r.Name), toCamelCase(a.Name), toCamelCase(r.Name), toCamelCase(a.Name))
			writer.Indented(func(w *Writer) {
				w.Printf("return &Resource%sAction%sHandlerWrapper{Handler: handler}\n", toCamelCase(r.Name), toCamelCase(a.Name))
			})
			writer.Printf("}\n\n")
		}
	}
}

func GenerateConfigTest(w io.Writer, d *Input) {
	// use golang template for test generation
	const testTemplate = `// Code generated by ksdk/access/generator. DO NOT EDIT.

package config_test

import (
	"testing"

	"github.com/on-keyday/kscale/config"
	"github.com/stretchr/testify/require"
	"github.com/on-keyday/objtrsf/objproto"
)

func TestResourceActionArgsDTOParsing(t *testing.T) {
	{{-  range $r := .Resources }}
		{{- range $a := .Actions }}
			t.Run("{{ $a.Name }}", func(t *testing.T) {
				dto := &config.Resource{{ ToCamelCase $r.Name }}Action{{ ToCamelCase $a.Name }}ArgsDTO{}
				args := []string{
					{{- range $a.Args }}
						{{- if eq .Type "time.Duration" }}
						"1h30m0s",
						{{- else if eq .Type "netip.Addr" }}
						"192.168.0.1",
						{{- else if eq .Type "objproto.ConnectionID" }}
						"udp:192.168.0.1:1234-3020",
						{{- else if eq .Type "*objproto.ConnectionID" }}
						"tcp:[2001:db8:85a3::8a2e:370:7334]:443-8080",
						{{- else if eq .Type "uint64" }}
						"30202",
						{{- else if eq .Type "uint32" }}
						"8080",
						{{- else if eq .Type "bool_enable" }}
						"enable",
						{{- else if eq .Type "[]string" }}
						"item1,item2,item3",
						{{- else if eq .Type "[]*pb.PortInfo" }}
						"tcp:80,tcp:443",
						{{- else }}
						"test-{{ .Name }}",
						{{- end }}
					{{- end }}
				}
				err := dto.Parse(args)
				require.NoError(t, err)
				argsOut, err := dto.ToArgs()
				require.NoError(t, err)
				require.Equal(t, args, argsOut)
			})
		{{- end }}
	{{- end }}
}

`
	tmpl, err := template.New("test").Funcs(template.FuncMap{
		"ToCamelCase": toCamelCase,
	}).Parse(testTemplate)
	if err != nil {
		panic(err)
	}
	err = tmpl.Execute(w, d.Definition)
	if err != nil {
		panic(err)
	}
}
