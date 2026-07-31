package edge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	pathlib "path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

type EdgeComputer interface {
	io.Closer
	Register(ctx context.Context, spec ModuleSpec, binary []byte) error
	Unregister(id uint32) error
	ListRegistered() []*ModuleInfo
	// SetPopcacheConfigProvider wires the live serving-config source used by the
	// module-facing get_popcache_config host callback.
	SetPopcacheConfigProvider(p PopcacheConfigProvider)
	StartRequest(ctx context.Context, p *http.Request) (uint64, error)
	ProcessRequest(ctx context.Context, reqID uint64, popID uint32, p *http.Request) error
	ProcessResponse(ctx context.Context, reqID uint64, p *http.Response) error
	FinishRequest(ctx context.Context, reqID uint64) error

	ModifyRequest(ctx context.Context, reqID uint64, modify func(d []DiffData) error, shouldClear bool) error
	ModifyResponse(ctx context.Context, reqID uint64, modify func(d []DiffData) error, shouldClear bool) error
}

func (r *edgeComputing) ModifyRequest(ctx context.Context, reqID uint64, modify func(d []DiffData) error, shouldClear bool) error {
	if modify == nil {
		return errors.New("modify function is nil")
	}
	handleCtx, err := r.getHandleContext(reqID)
	if err != nil {
		return fmt.Errorf("failed to get handle context for request ID %d: %w", reqID, err)
	}
	return handleCtx.withLock(func(h *HandleContext) error {
		err := modify(handleCtx.requestChangeSet)
		if err != nil {
			return fmt.Errorf("failed to modify request: %w", err)
		}
		if shouldClear {
			handleCtx.requestChangeSet = nil
		}
		return nil
	})
}

func (r *edgeComputing) ModifyResponse(ctx context.Context, reqID uint64, modify func(d []DiffData) error, shouldClear bool) error {
	if modify == nil {
		return errors.New("modify function is nil")
	}
	handleCtx, err := r.getHandleContext(reqID)
	if err != nil {
		return fmt.Errorf("failed to get handle context for request ID %d: %w", reqID, err)
	}
	return handleCtx.withLock(func(h *HandleContext) error {
		err := modify(handleCtx.responseChangeSet)
		if err != nil {
			return fmt.Errorf("failed to modify response: %w", err)
		}
		if shouldClear {
			handleCtx.responseChangeSet = nil
		}
		return nil
	})
}

func DefaultModifyRequest(d *DiffData, r *http.Request) error {
	if d == nil {
		return errors.New("diff data is nil")
	}
	if r == nil {
		return errors.New("request is nil")
	}
	switch d.DiffType {
	case DiffDataType_Header:
		hdr := d.Header()
		if hdr == nil {
			return errors.New("header is nil")
		}
		switch d.Kind {
		case DiffKind_Replace:
			var newHeader http.Header = make(http.Header)
			for _, field := range hdr.Fields {
				newHeader.Add(string(field.Key.Data), string(field.Value.Data))
			}
			r.Header = newHeader
		case DiffKind_Insert:
			for _, field := range hdr.Fields {
				r.Header.Add(string(field.Key.Data), string(field.Value.Data))
			}
		case DiffKind_Delete:
			for _, field := range hdr.Fields {
				r.Header.Del(string(field.Key.Data))
			}
		}
	case DiffDataType_Field:
		field := d.Field()
		if field == nil {
			return errors.New("field is nil")
		}
		switch d.Kind {
		case DiffKind_Replace:
			r.Header.Set(string(field.Key.Data), string(field.Value.Data))
		case DiffKind_Insert:
			r.Header.Add(string(field.Key.Data), string(field.Value.Data))
		case DiffKind_Delete:
			r.Header.Del(string(field.Key.Data))
		}
	case DiffDataType_Path:
		path := d.Path()
		if path == nil {
			return errors.New("path is nil")
		}
		switch d.Kind {
		case DiffKind_Replace:
			r.URL.Path = string(path.Path.Data)
		case DiffKind_Insert:
			r.URL.Path = pathlib.Join(r.URL.Path, string(path.Path.Data))
		case DiffKind_Delete:
			r.URL.Path = ""
		}
		if len(path.Query) > 0 {
			query := r.URL.Query()
			for _, field := range path.Query {
				switch d.Kind {
				case DiffKind_Replace:
					query.Set(string(field.Key.Data), string(field.Value.Data))
				case DiffKind_Insert:
					query.Add(string(field.Key.Data), string(field.Value.Data))
				case DiffKind_Delete:
					query.Del(string(field.Key.Data))
				}
			}
			r.URL.RawQuery = query.Encode()
		}
	case DiffDataType_Method:
		method := d.Method()
		if method == nil {
			return errors.New("method is nil")
		}
		methodName := method.Method.String()
		if method.Method == Method_OTHER {
			methodNameP := method.MethodName()
			if methodNameP == nil {
				return errors.New("method name is nil")
			}
			methodName = string(*methodNameP)
		}
		r.Method = string(methodName)
	case DiffDataType_Body:
		body := d.Body()
		if body == nil {
			return errors.New("body is nil")
		}
		if body.Offset != 0 {
			return fmt.Errorf("currently body offset is not supported: %d", body.Offset)
		}
		switch d.Kind {
		case DiffKind_Replace:
			if body.Body == nil {
				r.Body = nil
			} else {
				r.Body = io.NopCloser(bytes.NewReader(body.Body))
			}
		case DiffKind_Insert:
			if r.Body == nil {
				r.Body = io.NopCloser(bytes.NewReader(body.Body))
			} else {
				r.Body = io.NopCloser(io.MultiReader(r.Body, bytes.NewReader(body.Body)))
			}
		}
	case DiffDataType_Routing:
		// routing is not invalid but no need to handle in this function
	default:
		return fmt.Errorf("unsupported diff data type: %v", d.DiffType)
	}
	return nil
}

func DefaultModifyResponse(d *DiffData, resp *http.Response) error {
	if d == nil {
		return errors.New("diff data is nil")
	}
	if resp == nil {
		return errors.New("response is nil")
	}
	switch d.DiffType {
	case DiffDataType_Header:
		hdr := d.Header()
		if hdr == nil {
			return errors.New("header is nil")
		}
		switch d.Kind {
		case DiffKind_Replace:
			var newHeader http.Header = make(http.Header)
			for _, field := range hdr.Fields {
				newHeader.Add(string(field.Key.Data), string(field.Value.Data))
			}
			resp.Header = newHeader
		case DiffKind_Insert:
			for _, field := range hdr.Fields {
				resp.Header.Add(string(field.Key.Data), string(field.Value.Data))
			}
		case DiffKind_Delete:
			for _, field := range hdr.Fields {
				resp.Header.Del(string(field.Key.Data))
			}
		}
	case DiffDataType_Field:
		field := d.Field()
		if field == nil {
			return errors.New("field is nil")
		}
		switch d.Kind {
		case DiffKind_Replace:
			resp.Header.Set(string(field.Key.Data), string(field.Value.Data))
		case DiffKind_Insert:
			resp.Header.Add(string(field.Key.Data), string(field.Value.Data))
		case DiffKind_Delete:
			resp.Header.Del(string(field.Key.Data))
		}
	case DiffDataType_Status:
		status := d.Status()
		if status == nil {
			return errors.New("status is nil")
		}
		resp.StatusCode = int(*status)
	case DiffDataType_Body:
		body := d.Body()
		if body == nil {
			return errors.New("body is nil")
		}
		if body.Offset != 0 {
			return fmt.Errorf("currently body offset is not supported: %d", body.Offset)
		}
		switch d.Kind {
		case DiffKind_Replace:
			if body.Body == nil {
				resp.Body = nil
			} else {
				resp.Body = io.NopCloser(bytes.NewReader(body.Body))
			}
		case DiffKind_Insert:
			if resp.Body == nil {
				resp.Body = io.NopCloser(bytes.NewReader(body.Body))
			} else {
				resp.Body = io.NopCloser(io.MultiReader(resp.Body, bytes.NewReader(body.Body)))
			}
		}
	case DiffDataType_Routing:
		// routing is not invalid but no need to handle in this function
	default:
		return fmt.Errorf("unsupported diff data type: %v", d.DiffType)
	}
	return nil
}

var _ EdgeComputer = (*edgeComputing)(nil)

type HandleContext struct {
	m   sync.Mutex
	mod api.Module
	// moduleID is the wasm_module id whose route matched this request (recorded
	// at StartRequest). NOT RequestInfo.PopID — that is the SERVER's id, which
	// the popcache server passes to ProcessRequest. The fetch host call keys the
	// module's allowlist off this.
	moduleID uint32
	// watchdog is the compute clock of the hook currently executing (set by
	// executeWasm around the wasm call); blocking host calls pause it.
	watchdog *computeWatchdog
	req               *RequestInfo
	requestChangeSet  []DiffData
	resp              *ResponseInfo
	responseChangeSet []DiffData

	// Raw request/response, stashed for lazy body pull (get_request_body /
	// get_response_body). reqBody/respBody cache the buffered body after the
	// first pull; the *Loaded flags gate the one-time read.
	rawReq       *http.Request
	rawResp      *http.Response
	reqBody      []byte
	reqBodyDone  bool
	respBody     []byte
	respBodyDone bool

	// Outbound fetch results (host call "fetch"), keyed by fetch id; held until
	// the request finishes. fetchCount enforces the per-request fetch budget.
	fetches     map[uint32]*fetchResult
	lastFetchID uint32
	fetchCount  int

	// optimized for caching
	bufferPointer uint32
	bufferSize    uint32
}

// Match types for a module's path. Empty is treated as exact.
const (
	MatchTypeExact  = "exact"
	MatchTypePrefix = "prefix"
)

func isPrefixMatch(matchType string) bool { return matchType == MatchTypePrefix }

// wildcardMethod is the method token that matches any request method. A module
// registered with it (or with an empty method) is keyed under this sentinel and
// only consulted when no method-exact entry matches the same path.
const wildcardMethod = "*"

// routeKey is the composite (method, path) map key. method is either an exact
// (uppercased) HTTP method or wildcardMethod; path is an exact request path or a
// normalized prefix.
func routeKey(method, path string) string { return method + " " + path }

// parseMethods splits a module's method spec into the method tokens it should be
// keyed under. A comma lists several methods ("GET,HEAD"); "*" or an empty spec
// means any method. Tokens are uppercased and de-duplicated. The bool reports
// whether the spec is a wildcard, in which case the returned slice is nil.
func parseMethods(method string) (methods []string, wildcard bool) {
	seen := map[string]bool{}
	for _, tok := range strings.Split(method, ",") {
		tok = strings.ToUpper(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		if tok == wildcardMethod {
			return nil, true
		}
		if !seen[tok] {
			seen[tok] = true
			methods = append(methods, tok)
		}
	}
	if len(methods) == 0 {
		return nil, true // empty spec == match any method
	}
	return methods, false
}

// normalizePrefix strips trailing slashes so "/api/" and "/api" register the
// same prefix. Empty (or all-slash) normalizes to the root "/" catch-all.
func normalizePrefix(path string) string {
	p := strings.TrimRight(path, "/")
	if p == "" {
		return "/"
	}
	return p
}

type RoutingRegistry struct {
	registerRW sync.RWMutex
	compiled   map[uint32]wazero.CompiledModule
	// methodPathToID holds exact-match modules keyed by routeKey(method, path).
	methodPathToID map[string]uint32
	// prefixPathToID holds prefix-match modules keyed by
	// routeKey(method, normalizePrefix(path)). Lookup walks request-path
	// segments longest-first, so the longest registered prefix wins.
	prefixPathToID map[string]uint32
	idToMethodPath map[uint32]*ModuleInfo
}

// indexKeys returns every map key under which info is (or should be) stored —
// one per method it serves (or a single wildcard key) — and whether those keys
// live in the prefix index.
func indexKeys(info *ModuleInfo) (keys []string, prefix bool) {
	path := info.Path
	prefix = isPrefixMatch(info.MatchType)
	if prefix {
		path = normalizePrefix(path)
	}
	methods, wildcard := parseMethods(info.Method)
	if wildcard {
		return []string{routeKey(wildcardMethod, path)}, prefix
	}
	keys = make([]string, 0, len(methods))
	for _, m := range methods {
		keys = append(keys, routeKey(m, path))
	}
	return keys, prefix
}

// deleteKeys removes info's keys from whichever index (prefix or exact) holds it.
func (r *RoutingRegistry) deleteKeys(info *ModuleInfo) {
	keys, prefix := indexKeys(info)
	m := r.methodPathToID
	if prefix {
		m = r.prefixPathToID
	}
	for _, k := range keys {
		delete(m, k)
	}
}

func (r *RoutingRegistry) Register(spec ModuleSpec, fileSize int64, mod wazero.CompiledModule) error {
	r.registerRW.Lock()
	defer r.registerRW.Unlock()
	if old, ok := r.compiled[spec.ID]; ok {
		// Idempotent re-register: the declarative reconcile re-pushes the desired
		// set, so registering an existing id replaces it rather than erroring.
		old.Close(context.Background())
		if prev, ok := r.idToMethodPath[spec.ID]; ok {
			r.deleteKeys(prev)
		}
	}
	r.compiled[spec.ID] = mod
	info := &ModuleInfo{
		ModuleSpec: spec,
		FileSize:   fileSize,
	}
	keys, prefix := indexKeys(info)
	m := r.methodPathToID
	if prefix {
		m = r.prefixPathToID
	}
	for _, k := range keys {
		m[k] = spec.ID
	}
	r.idToMethodPath[spec.ID] = info
	return nil
}

// Current returns a copy of the registered module info for id, if any.
func (r *RoutingRegistry) Current(id uint32) (ModuleInfo, bool) {
	r.registerRW.RLock()
	defer r.registerRW.RUnlock()
	info, ok := r.idToMethodPath[id]
	if !ok {
		return ModuleInfo{}, false
	}
	return *info, true
}

func (r *RoutingRegistry) Unregister(id uint32) error {
	r.registerRW.Lock()
	defer r.registerRW.Unlock()
	c, ok := r.compiled[id]
	if !ok {
		return fmt.Errorf("no compiled instance found for ID %d", id)
	}
	defer c.Close(context.Background())
	delete(r.compiled, id)
	if info, ok := r.idToMethodPath[id]; ok {
		r.deleteKeys(info)
	}
	delete(r.idToMethodPath, id)
	return nil
}

// LookupByMethodPath resolves the module handling (method, path). Exact matches
// win over any prefix; among prefixes the longest (most path segments) wins.
func (r *RoutingRegistry) LookupByMethodPath(method, path string) (wazero.CompiledModule, uint32, bool) {
	r.registerRW.RLock()
	defer r.registerRW.RUnlock()
	id, ok := r.lookupID(method, path)
	if !ok {
		return nil, 0, false
	}
	mod, ok := r.compiled[id]
	return mod, id, ok
}

// lookupID resolves (method, path) to a module id. Path specificity is the
// primary axis (exact path beats any prefix); within a path candidate the
// method-exact entry beats the wildcard one. Caller must hold the read lock.
func (r *RoutingRegistry) lookupID(method, path string) (uint32, bool) {
	if id, ok := matchMethod(r.methodPathToID, method, path); ok {
		return id, true
	}
	return r.longestPrefixID(method, path)
}

// matchMethod looks up (method, path) in m, preferring the method-exact entry
// over the wildcard ("*") one at the same path.
func matchMethod(m map[string]uint32, method, path string) (uint32, bool) {
	if id, ok := m[routeKey(method, path)]; ok {
		return id, true
	}
	if id, ok := m[routeKey(wildcardMethod, path)]; ok {
		return id, true
	}
	return 0, false
}

// longestPrefixID walks the request path from longest to shortest at "/"
// segment boundaries, returning the id of the longest registered prefix. Cutting
// only at boundaries means prefix "/api" matches "/api" and "/api/..." but never
// "/apixyz". At each candidate a method-exact prefix beats a wildcard one.
// Caller must hold the read lock.
func (r *RoutingRegistry) longestPrefixID(method, path string) (uint32, bool) {
	cand := path
	for {
		if id, ok := matchMethod(r.prefixPathToID, method, cand); ok {
			return id, true
		}
		if cand == "/" {
			return 0, false
		}
		if i := strings.LastIndexByte(cand, '/'); i > 0 {
			cand = cand[:i]
		} else {
			cand = "/" // fall through to the root catch-all, then stop
		}
	}
}

// ModuleSpec is the desired registration of one wasm module — everything the
// control plane declares about it (routing, fetch allowlist, compute budget).
type ModuleSpec struct {
	ID        uint32
	Method    string
	Path      string
	MatchType string
	FilePath  string
	// AllowedHosts gates the module's outbound fetch host call (default-deny:
	// empty = fetch disabled). Entries are [scheme://]host[:port][/path-prefix]
	// (see parseFetchAllowRule).
	AllowedHosts []string
	// ComputeBudget is the module's per-hook wasm compute budget (time spent
	// actually executing wasm; paused host calls don't count). 0 = the node
	// default; always clamped to the node ceiling
	// (PopcacheConfigInfo.WasmComputeBudget).
	ComputeBudget time.Duration
}

func (s *ModuleSpec) equal(o *ModuleSpec) bool {
	return s.ID == o.ID && s.Method == o.Method && s.Path == o.Path &&
		s.MatchType == o.MatchType && s.FilePath == o.FilePath &&
		slices.Equal(s.AllowedHosts, o.AllowedHosts) &&
		s.ComputeBudget == o.ComputeBudget
}

type ModuleInfo struct {
	ModuleSpec
	FileSize int64
}

func (r *RoutingRegistry) ListRegistered() []*ModuleInfo {
	r.registerRW.RLock()
	defer r.registerRW.RUnlock()
	modules := make([]*ModuleInfo, 0, len(r.idToMethodPath))
	for _, info := range r.idToMethodPath {
		modules = append(modules, info)
	}
	return modules
}

func NewRoutingRegistry() *RoutingRegistry {
	return &RoutingRegistry{
		compiled:       make(map[uint32]wazero.CompiledModule),
		methodPathToID: make(map[string]uint32),
		prefixPathToID: make(map[string]uint32),
		idToMethodPath: make(map[uint32]*ModuleInfo),
	}
}

type edgeComputing struct {
	rt wazero.Runtime

	handlerRW     sync.RWMutex
	atomicCounter atomic.Uint64
	handleContext map[uint64]*HandleContext

	computingTimeout time.Duration

	routing *RoutingRegistry

	geo GeoLocator
	cfg PopcacheConfigProvider

	// fetchTransport carries the modules' outbound fetch host calls (shared so
	// connection pooling works across requests; per-call http.Clients add only
	// the per-module redirect check on top).
	fetchTransport http.RoundTripper

	logger *slog.Logger
}

// GeoInfo is the geo-location of an IP as returned to a wasm module.
type GeoInfo struct {
	ASN     uint32
	ASNOrg  string
	Country string // ISO 3166-1 alpha-2
	City    string
}

// GeoLocator resolves an IP to its geo-location. Injected into the edge
// computer so it stays decoupled from the geoip database implementation; a nil
// GeoLocator makes the module-facing geoloc_lookup report "not found".
type GeoLocator interface {
	Lookup(ip netip.Addr) (*GeoInfo, error)
}

// PopcacheConfigInfo is this node's serving config as exposed to a wasm module
// (the non-sensitive subset: origin + listener ports; a port of 0 means that
// listener is not configured / not parseable). No cert/key paths are exposed.
type PopcacheConfigInfo struct {
	Origin    string
	HTTPPort  uint16
	HTTPSPort uint16
	HTTP3Port uint16
	// WasmComputeDefault/-Max drive per-module compute-budget resolution (see
	// computeBudgetFor): default applies to modules that declare no budget
	// (0 = built-in), max is the ceiling declared budgets are clamped to
	// (0 = the default is also the ceiling). Not exposed to guests.
	WasmComputeDefault time.Duration
	WasmComputeMax     time.Duration
}

// PopcacheConfigProvider supplies the live serving config for the module-facing
// get_popcache_config host callback. Injected (via SetPopcacheConfigProvider) so
// the edge computer stays decoupled from the popcache Server; a nil provider
// makes get_popcache_config report "unavailable" (0 bytes).
type PopcacheConfigProvider interface {
	PopcacheConfig() *PopcacheConfigInfo
}

func NewEdgeComputing(rt wazero.Runtime, timeout time.Duration, logger *slog.Logger, geo GeoLocator) (EdgeComputer, error) {
	c := &edgeComputing{
		rt:               rt,
		handleContext:    make(map[uint64]*HandleContext),
		computingTimeout: timeout,
		routing:          NewRoutingRegistry(),
		geo:              geo,
		fetchTransport:   http.DefaultTransport,
		logger:           logger,
	}
	if err := c.initRequestHandler(); err != nil {
		return nil, fmt.Errorf("failed to initialize request handler: %w", err)
	}
	return c, nil
}

func (r *edgeComputing) SetPopcacheConfigProvider(p PopcacheConfigProvider) { r.cfg = p }

func (r *edgeComputing) Close() error {
	return r.rt.Close(context.Background())
}

type HandlerFunc func(c context.Context, mod api.Module, offset, size uint32) uint32

var wellKnownMethods map[string]Method

func init() {
	wellKnownMethods = make(map[string]Method, int(Method_WELL_KNOWN_MAX))
	for i := 0; i < int(Method_WELL_KNOWN_MAX); i++ {
		method := Method(i)
		wellKnownMethods[method.String()] = method
	}
}

func getRequestInfo(popID uint32, reqID uint64, r *http.Request) (*RequestInfo, error) {
	info := &RequestInfo{
		PopID: popID,
		ReqID: reqID,
	}
	remoteAddrParsed, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse remote address %s: %w", r.RemoteAddr, err)
	}
	switch r.ProtoMajor {
	case 1:
		if r.TLS == nil {
			info.Protocol.Protocol = Protocol_Http1Plain
		} else {
			info.Protocol.Protocol = Protocol_Http1
		}
	case 2:
		info.Protocol.Protocol = Protocol_H2
	case 3:
		info.Protocol.Protocol = Protocol_H3
	default:
		info.Protocol.Protocol = Protocol_Other
		if !info.Protocol.SetProtocolName([]byte(r.Proto)) {
			return nil, fmt.Errorf("failed to set protocol name for %s", r.Proto)
		}
	}
	if !setAddress(&info.Remote.Addr, remoteAddrParsed.Addr()) {
		return nil, fmt.Errorf("unsupported remote address type: %s", remoteAddrParsed.Addr().String())
	}
	info.Remote.Port = remoteAddrParsed.Port()
	// Local (server-side) address the request landed on, when the http.Server
	// recorded it in the request context. Default to the unspecified address so
	// the wire Address union is always valid even for synthetic requests.
	setAddress(&info.Local.Addr, netip.IPv4Unspecified())
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		lap, err := netip.ParseAddrPort(la.String())
		if err != nil {
			return nil, fmt.Errorf("failed to parse local address %s: %w", la.String(), err)
		}
		if !setAddress(&info.Local.Addr, lap.Addr()) {
			return nil, fmt.Errorf("unsupported local address type: %s", lap.Addr().String())
		}
		info.Local.Port = lap.Port()
	}
	if m, ok := wellKnownMethods[r.Method]; ok {
		info.Method.Method = m
	} else {
		info.Method.Method = Method_OTHER
		if !info.Method.SetMethodName([]byte(r.Method)) {
			return nil, fmt.Errorf("failed to set method name for %s", r.Method)
		}
	}
	var headers = make([]Field, 0, len(r.Header))
	for key, h := range r.Header {
		for _, value := range h {
			var field Field
			if !field.Key.SetData([]byte(key)) {
				return nil, fmt.Errorf("failed to set header key %s", key)
			}
			if !field.Value.SetData([]byte(value)) {
				return nil, fmt.Errorf("failed to set header value for %s", key)
			}
			headers = append(headers, field)
		}
	}
	if !info.Header.SetFields(headers) {
		return nil, fmt.Errorf("failed to set headers for %s", r.URL.Path)
	}
	if !info.Path.Path.SetData([]byte(r.URL.Path)) {
		return nil, fmt.Errorf("failed to set path for %s", r.URL.Path)
	}
	q := r.URL.Query()
	if len(q) > 0 {
		query := make([]Field, 0, len(q))
		for key, v := range q {
			for _, value := range v {
				var f Field
				if !f.Key.SetData([]byte(key)) {
					return nil, fmt.Errorf("failed to set query key %s", key)
				}
				if !f.Value.SetData([]byte(value)) {
					return nil, fmt.Errorf("failed to set query value for %s", key)
				}
				query = append(query, f)
			}
		}
		if !info.Path.SetQuery(query) {
			return nil, fmt.Errorf("failed to set query fields for %s", r.URL.Path)
		}
	}
	if r.TLS != nil {
		info.SetHasTls(true)
		var tlsInfo TlsInfo
		tlsInfo.Version = r.TLS.Version
		tlsInfo.CipherSuite = r.TLS.CipherSuite
		if !tlsInfo.Sni.SetData([]byte(r.TLS.ServerName)) {
			return nil, fmt.Errorf("failed to set TLS SNI %q", r.TLS.ServerName)
		}
		if !tlsInfo.Alpn.SetData([]byte(r.TLS.NegotiatedProtocol)) {
			return nil, fmt.Errorf("failed to set TLS ALPN %q", r.TLS.NegotiatedProtocol)
		}
		if len(r.TLS.PeerCertificates) > 0 {
			if !tlsInfo.SetClientCert(r.TLS.PeerCertificates[0].Raw) {
				return nil, fmt.Errorf("failed to set TLS client cert")
			}
		}
		if !info.SetTls(tlsInfo) {
			return nil, fmt.Errorf("failed to set TLS info")
		}
	}
	return info, nil
}

func getResponseInfo(p *http.Response) (*ResponseInfo, error) {
	info := &ResponseInfo{
		Status: uint16(p.StatusCode),
	}
	var headers = make([]Field, 0, len(p.Header))
	for key, h := range p.Header {
		for _, value := range h {
			var field Field
			if !field.Key.SetData([]byte(key)) {
				return nil, fmt.Errorf("failed to set response header key %s", key)
			}
			if !field.Value.SetData([]byte(value)) {
				return nil, fmt.Errorf("failed to set response header value for %s", key)
			}
			headers = append(headers, field)
		}
	}
	if !info.Header.SetFields(headers) {
		return nil, fmt.Errorf("failed to set response headers for status code %d", p.StatusCode)
	}
	return info, nil
}

func (r *edgeComputing) initRequestHandler() error {
	_, err := wasi_snapshot_preview1.Instantiate(context.Background(), r.rt)
	if err != nil {
		return fmt.Errorf("failed to instantiate wasi_snapshot_preview1: %w", err)
	}
	kscale := r.rt.NewHostModuleBuilder("kscale")
	kscale.NewFunctionBuilder().WithFunc(r.getRequestInfo).Export("get_request_info")
	kscale.NewFunctionBuilder().WithFunc(r.getResponseInfo).Export("get_response_info")
	kscale.NewFunctionBuilder().WithFunc(r.logOutput).Export("log_output")
	kscale.NewFunctionBuilder().WithFunc(r.saveBufferPointer).Export("save_buffer_pointer")
	kscale.NewFunctionBuilder().WithFunc(r.getBufferPointer).Export("get_buffer_pointer")
	kscale.NewFunctionBuilder().WithFunc(r.changeRequest).Export("change_request_info")
	kscale.NewFunctionBuilder().WithFunc(r.changeResponse).Export("change_response_info")
	kscale.NewFunctionBuilder().WithFunc(r.geolocLookup).Export("geoloc_lookup")
	kscale.NewFunctionBuilder().WithFunc(r.getPopcacheConfig).Export("get_popcache_config")
	kscale.NewFunctionBuilder().WithFunc(r.getRequestBody).Export("get_request_body")
	kscale.NewFunctionBuilder().WithFunc(r.getResponseBody).Export("get_response_body")
	kscale.NewFunctionBuilder().WithFunc(r.hostFetch).Export("fetch")
	kscale.NewFunctionBuilder().WithFunc(r.getFetchResponse).Export("get_fetch_response")
	kscale.NewFunctionBuilder().WithFunc(r.getFetchBody).Export("get_fetch_body")
	_, err = kscale.Instantiate(context.Background())
	if err != nil {
		return fmt.Errorf("failed to instantiate kscale module: %w", err)
	}
	return nil
}

// geolocLookup is the "kscale.geoloc_lookup" host function: decode a GeolocRequest
// from [inPtr,inSize), resolve it via the injected GeoLocator, and encode a
// GeolocResponse into [outPtr,outSize). Returns the number of bytes written, or
// 0 when no locator is configured, the lookup found nothing / failed, or the
// output buffer is too small.
func (r *edgeComputing) geolocLookup(c context.Context, mod api.Module, inPtr, inSize, outPtr, outSize uint32) uint32 {
	if r.geo == nil {
		return 0
	}
	in, ok := mod.Memory().Read(inPtr, inSize)
	if !ok {
		r.logger.Error("geoloc_lookup: failed to read request buffer", "ptr", inPtr, "size", inSize)
		return 0
	}
	var req GeolocRequest
	if err := req.DecodeExact(in); err != nil {
		r.logger.Error("geoloc_lookup: failed to decode request", "error", err)
		return 0
	}
	ip, ok := addressToAddr(&req.Ip)
	if !ok {
		return 0
	}
	info, err := r.geo.Lookup(ip)
	if err != nil || info == nil {
		return 0
	}
	var resp GeolocResponse
	resp.Asn = info.ASN
	if !resp.AsnOrg.SetData([]byte(info.ASNOrg)) ||
		!resp.Country.SetData([]byte(info.Country)) ||
		!resp.City.SetData([]byte(info.City)) {
		return 0
	}
	out, ok := mod.Memory().Read(outPtr, outSize)
	if !ok {
		r.logger.Error("geoloc_lookup: failed to read output buffer", "ptr", outPtr, "size", outSize)
		return 0
	}
	written, err := resp.Encode(out)
	if err != nil {
		r.logger.Error("geoloc_lookup: failed to encode response (buffer too small?)", "error", err)
		return 0
	}
	return uint32(len(written))
}

// getPopcacheConfig is the "kscale.get_popcache_config" host function: encode this
// node's serving config (origin + listener ports) into [outPtr,outSize). Returns
// the number of bytes written, or 0 when no provider is configured or the output
// buffer is too small. Input-free (unlike geoloc_lookup) — the config is node
// state, not a per-request query.
func (r *edgeComputing) getPopcacheConfig(c context.Context, mod api.Module, outPtr, outSize uint32) uint32 {
	if r.cfg == nil {
		return 0
	}
	info := r.cfg.PopcacheConfig()
	if info == nil {
		return 0
	}
	var resp PopcacheConfig
	if !resp.Origin.SetData([]byte(info.Origin)) {
		return 0
	}
	resp.HttpPort = info.HTTPPort
	resp.HttpsPort = info.HTTPSPort
	resp.Http3Port = info.HTTP3Port
	out, ok := mod.Memory().Read(outPtr, outSize)
	if !ok {
		r.logger.Error("get_popcache_config: failed to read output buffer", "ptr", outPtr, "size", outSize)
		return 0
	}
	written, err := resp.Encode(out)
	if err != nil {
		r.logger.Error("get_popcache_config: failed to encode response (buffer too small?)", "error", err)
		return 0
	}
	return uint32(len(written))
}

// setAddress writes a netip.Addr into a wire Address (is_v6 discriminant + bytes).
func setAddress(a *Address, addr netip.Addr) bool {
	if addr.Is4() {
		a.SetIsV6(false)
		return a.SetAddrV4(addr.As4())
	}
	if addr.Is6() {
		a.SetIsV6(true)
		return a.SetAddrV6(addr.As16())
	}
	return false
}

// maxPullBodyBytes caps how much of a request/response body the host will buffer
// for a wasm module's lazy body pull.
const maxPullBodyBytes = 1 << 20 // 1 MiB

// loadBody reads up to maxPullBodyBytes from body exactly once, returning the
// buffered bytes and a ReadCloser that replays the FULL original stream so the
// request/response can still be forwarded. ok=false (bytes nil) means the body
// exceeded the cap or errored — the replay reader still carries everything, but
// oversized bodies are not exposed to modules in this version.
func loadBody(body io.ReadCloser) (buf []byte, restored io.ReadCloser, ok bool) {
	if body == nil {
		return nil, nil, true
	}
	read, err := io.ReadAll(io.LimitReader(body, maxPullBodyBytes+1))
	if err != nil || len(read) > maxPullBodyBytes {
		// Replay the prefix we consumed plus whatever remains unread.
		return nil, io.NopCloser(io.MultiReader(bytes.NewReader(read), body)), false
	}
	body.Close()
	return read, io.NopCloser(bytes.NewReader(read)), true
}

// writeBody copies min(outSize, len(body)) bytes of body into the module's
// [outPtr,outSize) buffer and returns the body's total length, so the module can
// re-call with a larger buffer when the first was too small.
func writeBody(mod api.Module, outPtr, outSize uint32, body []byte) uint32 {
	total := len(body)
	w := uint32(total)
	if w > outSize {
		w = outSize
	}
	if w > 0 {
		out, ok := mod.Memory().Read(outPtr, w)
		if !ok {
			return 0
		}
		copy(out, body[:w])
	}
	return uint32(total)
}

// handleFromCtx resolves the HandleContext for the in-flight request carried in
// the wasm call context.
func (r *edgeComputing) handleFromCtx(c context.Context) (*HandleContext, bool) {
	key, ok := c.Value(requestIDKey{}).(uint64)
	if !ok {
		return nil, false
	}
	h, err := r.getHandleContext(key)
	if err != nil {
		return nil, false
	}
	return h, true
}

// getRequestBody is the "kscale.get_request_body" host function: lazily buffer the
// request body (once, capped) and copy it into the module buffer.
func (r *edgeComputing) getRequestBody(c context.Context, mod api.Module, outPtr, outSize uint32) uint32 {
	handleCtx, ok := r.handleFromCtx(c)
	if !ok {
		return 0
	}
	var total uint32
	handleCtx.withLock(func(h *HandleContext) error {
		if !h.reqBodyDone {
			if h.rawReq != nil {
				buf, restored, _ := loadBody(h.rawReq.Body)
				h.reqBody = buf
				if restored != nil {
					h.rawReq.Body = restored
				}
			}
			h.reqBodyDone = true
		}
		total = writeBody(mod, outPtr, outSize, h.reqBody)
		return nil
	})
	return total
}

// getResponseBody is the "kscale.get_response_body" host function: lazily buffer
// the response body (once, capped) and copy it into the module buffer.
func (r *edgeComputing) getResponseBody(c context.Context, mod api.Module, outPtr, outSize uint32) uint32 {
	handleCtx, ok := r.handleFromCtx(c)
	if !ok {
		return 0
	}
	var total uint32
	handleCtx.withLock(func(h *HandleContext) error {
		if !h.respBodyDone {
			if h.rawResp != nil {
				buf, restored, _ := loadBody(h.rawResp.Body)
				h.respBody = buf
				if restored != nil {
					h.rawResp.Body = restored
				}
			}
			h.respBodyDone = true
		}
		total = writeBody(mod, outPtr, outSize, h.respBody)
		return nil
	})
	return total
}

// addressToAddr converts a wire Address into a netip.Addr.
func addressToAddr(a *Address) (netip.Addr, bool) {
	if a.IsV6() {
		p := a.AddrV6()
		if p == nil {
			return netip.Addr{}, false
		}
		return netip.AddrFrom16(*p), true
	}
	p := a.AddrV4()
	if p == nil {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4(*p), true
}

const hookOnRequest = "on_request"
const hookOnResponse = "on_response"
const hookOnFinish = "on_finish"

var hooks = []string{hookOnRequest, hookOnResponse, hookOnFinish}

func (r *edgeComputing) Register(ctx context.Context, spec ModuleSpec, binary []byte) error {
	// Idempotent no-op when the desired module is already registered unchanged —
	// the reconcile re-pushes the full desired set on every converge, so skip the
	// recompile in that common case.
	if cur, ok := r.routing.Current(spec.ID); ok &&
		cur.ModuleSpec.equal(&spec) && cur.FileSize == int64(len(binary)) {
		return nil
	}
	mod, err := r.rt.CompileModule(ctx, binary)
	if err != nil {
		return err
	}
	exported := mod.ExportedFunctions()
	hasLeastOne := false
	for _, hook := range hooks {
		if f, ok := exported[hook]; ok {
			params := f.ParamTypes()
			if len(params) != 0 {
				mod.Close(ctx)
				return fmt.Errorf("function %s must not have parameters", hook)
			}
			// return values are ignored, so we don't check them.
			hasLeastOne = true
		}
	}
	if !hasLeastOne {
		mod.Close(ctx)
		return fmt.Errorf("module must export at least one of the following functions: %v", hooks)
	}
	if err := r.routing.Register(spec, int64(len(binary)), mod); err != nil {
		mod.Close(ctx)
		return fmt.Errorf("failed to register edge function %s %s with ID %d: %w", spec.Method, spec.Path, spec.ID, err)
	}
	return nil
}

func (r *edgeComputing) Unregister(id uint32) error {
	if err := r.routing.Unregister(id); err != nil {
		return fmt.Errorf("failed to unregister edge function with ID %d: %w", id, err)
	}
	return nil
}

func (r *edgeComputing) ListRegistered() []*ModuleInfo {
	l := r.routing.ListRegistered()
	slices.SortFunc(l, func(a, b *ModuleInfo) int {
		if a.ID < b.ID {
			return -1
		} else if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return l
}

type requestIDKey struct{}

func (r *edgeComputing) saveBufferPointer(ctx context.Context, _ api.Module, pointer, size uint32) {
	key, ok := ctx.Value(requestIDKey{}).(uint64)
	if !ok {
		r.logger.Error("No request ID found in context")
		return
	}
	handleCtx, err := r.getHandleContext(key)
	if err != nil {
		r.logger.Error("Failed to get handle context for request ID", "requestID", key, "error", err)
		return
	}
	err = handleCtx.withLock(func(h *HandleContext) error {
		handleCtx.bufferPointer = pointer
		handleCtx.bufferSize = size
		return nil
	})
	if err != nil {
		r.logger.Error("Failed to get lock for request ID", "requestID", key, "error", err)
		return
	}
}

func (r *edgeComputing) getBufferPointer(ctx context.Context, mod api.Module, pointerToPointer uint32, pointerToSize uint32) {
	key, ok := ctx.Value(requestIDKey{}).(uint64)
	if !ok {
		r.logger.Error("No request ID found in context")
		return
	}
	handleCtx, err := r.getHandleContext(key)
	if err != nil {
		r.logger.Error("Failed to get handle context for request ID", "requestID", key, "error", err)
		return
	}
	err = handleCtx.withLock(func(h *HandleContext) error {
		mod.Memory().WriteUint32Le(pointerToPointer, handleCtx.bufferPointer)
		mod.Memory().WriteUint32Le(pointerToSize, handleCtx.bufferSize)
		return nil
	})
	if err != nil {
		r.logger.Error("Failed to get lock for request ID", "requestID", key, "error", err)
		return
	}
}

func (r *edgeComputing) logOutput(c context.Context, mod api.Module, logLevel, pointer, size uint32) uint32 {
	level := LogLevel(logLevel)
	data, ok := mod.Memory().Read(pointer, size)
	if !ok {
		r.logger.Error("Failed to read log data from memory", "pointer", pointer, "size", size)
		return 0
	}
	message := string(data)
	switch level {
	case LogLevel_TRACE:
		r.logger.Debug(message)
	case LogLevel_DEBUG:
		r.logger.Debug(message)
	case LogLevel_INFO:
		r.logger.Info(message)
	case LogLevel_WARN:
		r.logger.Warn(message)
	case LogLevel_ERROR:
		r.logger.Error(message)
	default:
		r.logger.Info(message)
	}
	return uint32(len(data))
}

// encodableInfo is a request/response info that serializes itself into a
// caller-provided fixed buffer, returning the written prefix (or a bounds
// error). Satisfied by the generated *RequestInfo / *ResponseInfo.
type encodableInfo interface {
	Encode([]byte) ([]byte, error)
}

func (r *edgeComputing) passInfoToWasm(c context.Context, mod api.Module, pointer, size uint32, getInfo func(uint64, *HandleContext) (encodableInfo, error)) uint32 {
	key, ok := c.Value(requestIDKey{}).(uint64)
	if !ok {
		r.logger.Error("No request ID found in context")
		return 0
	}
	handleCtx, err := r.getHandleContext(key)
	if err != nil {
		r.logger.Error("Failed to get handle context for request ID", "requestID", key, "error", err)
		return 0
	}
	buf, ok := mod.Memory().Read(pointer, size)
	if !ok {
		r.logger.Error("Failed to read buffer from memory", "pointer", pointer, "size", size)
		return 0
	}
	var offset uint32
	err = handleCtx.withLock(func(h *HandleContext) error {
		info, err := getInfo(key, handleCtx)
		if err != nil {
			return fmt.Errorf("failed to get request info for request ID %d: %v", key, err)
		}
		// Encode straight into the wasm-provided fixed buffer; returns the
		// written prefix, or a bounds error if the info doesn't fit.
		written, err := info.Encode(buf)
		if err != nil {
			return fmt.Errorf("failed to encode request info for request ID %d: %v", key, err)
		}
		offset = uint32(len(written))
		return nil
	})
	if err != nil {
		r.logger.Error("Failed to get lock for request ID", "requestID", key, "error", err)
		return 0
	}
	return offset
}

func (r *edgeComputing) getRequestInfo(c context.Context, mod api.Module, pointer, size uint32) uint32 {
	return r.passInfoToWasm(c, mod, pointer, size, func(reqID uint64, handleCtx *HandleContext) (encodableInfo, error) {
		if handleCtx.req == nil {
			return nil, fmt.Errorf("no request info found for request ID %d", reqID)
		}
		return handleCtx.req, nil
	})
}

func (r *edgeComputing) getResponseInfo(c context.Context, mod api.Module, pointer, size uint32) uint32 {
	return r.passInfoToWasm(c, mod, pointer, size, func(reqID uint64, handleCtx *HandleContext) (encodableInfo, error) {
		if handleCtx.resp == nil {
			return nil, fmt.Errorf("no response info found for request ID %d", reqID)
		}
		return handleCtx.resp, nil
	})
}

func (r *edgeComputing) addDiffFromWasm(c context.Context, mod api.Module, pointer, size uint32, addDiff func(*HandleContext, []DiffData)) uint32 {
	key, ok := c.Value(requestIDKey{}).(uint64)
	if !ok {
		r.logger.Error("No request ID found in context")
		return 0
	}
	handleCtx, err := r.getHandleContext(key)
	if err != nil {
		r.logger.Error("Failed to get handle context for request ID", "requestID", key, "error", err)
		return 0
	}
	buf, ok := mod.Memory().Read(pointer, size)
	if !ok {
		r.logger.Error("Failed to read buffer from memory", "pointer", pointer, "size", size)
		return 0
	}
	// mod.Memory().Read returns a view aliasing wasm linear memory, and the
	// zero-copy DecodeExact leaves the resulting DiffData pointing into it. The
	// changeset is stashed and applied later, by which time wasm may have reused
	// this region — so copy it into a host-owned buffer once and decode that.
	// (One alloc + direct-slice DecodeExact whose DiffData alias the owned copy;
	// faster than DecodeExactCopy, which reads via io.Reader and allocates each
	// field.)
	owned := make([]byte, len(buf))
	copy(owned, buf)
	cc := &ChangeSet{}
	if err := cc.DecodeExact(owned); err != nil {
		r.logger.Error("Failed to decode change set for request ID", "requestID", key, "error", err)
		return 0
	}
	err = handleCtx.withLock(func(h *HandleContext) error {
		addDiff(h, cc.Diff)
		return nil
	})
	if err != nil {
		r.logger.Error("Failed to get lock for request ID", "requestID", key, "error", err)
		return 0
	}
	return 1
}

func (r *edgeComputing) changeRequest(ctx context.Context, mod api.Module, pointer, size uint32) uint32 {
	return r.addDiffFromWasm(ctx, mod, pointer, size, func(h *HandleContext, diff []DiffData) {
		h.requestChangeSet = append(h.requestChangeSet, diff...)
	})
}

func (r *edgeComputing) changeResponse(ctx context.Context, mod api.Module, pointer, size uint32) uint32 {
	return r.addDiffFromWasm(ctx, mod, pointer, size, func(h *HandleContext, diff []DiffData) {
		h.responseChangeSet = append(h.responseChangeSet, diff...)
	})
}

var ErrNoEdgeFunction = errors.New("no edge function registered for this request")

func (r *edgeComputing) StartRequest(ctx context.Context, p *http.Request) (uint64, error) {
	compiled, moduleID, ok := r.routing.LookupByMethodPath(p.Method, p.URL.Path)
	if !ok {
		return 0, fmt.Errorf("%w: %s %s", ErrNoEdgeFunction, p.Method, p.URL.Path)
	}
	reqID := r.atomicCounter.Add(1)
	conf := wazero.NewModuleConfig().WithName("")
	mod, err := r.rt.InstantiateModule(ctx, compiled, conf)
	if err != nil {
		return 0, fmt.Errorf("failed to instantiate module for %s %s: %w", p.Method, p.URL.Path, err)
	}
	handleCtx := &HandleContext{
		mod:      mod,
		moduleID: moduleID,
	}
	r.handlerRW.Lock()
	if _, exists := r.handleContext[reqID]; exists {
		r.handlerRW.Unlock()
		return 0, fmt.Errorf("request ID %d already exists", reqID)
	}
	r.handleContext[reqID] = handleCtx
	r.handlerRW.Unlock()

	return reqID, nil
}

func (r *edgeComputing) getHandleContext(reqID uint64) (*HandleContext, error) {
	r.handlerRW.RLock()
	defer r.handlerRW.RUnlock()
	handleCtx, exists := r.handleContext[reqID]
	if !exists {
		return nil, fmt.Errorf("no handle context found for request ID %d", reqID)
	}
	return handleCtx, nil
}

func (r *edgeComputing) executeWasm(ctx context.Context, mod api.Module, reqID uint64, fname string) error {
	f := mod.ExportedFunction(fname)
	if f == nil {
		return nil
	}
	handleCtx, err := r.getHandleContext(reqID)
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, requestIDKey{}, reqID)
	var moduleID uint32
	handleCtx.withLock(func(h *HandleContext) error { moduleID = h.moduleID; return nil })
	// The compute budget is a stoppable clock, not a plain context deadline:
	// our own timer cancels the ctx (which CloseOnContextDone turns into a
	// module kill) after the budget of RUNNING time. Blocking host calls
	// (fetch) pause the clock while the module sits inside them — it executes
	// no wasm then, so network latency must not count as compute.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wd := newComputeWatchdog(r.computeBudgetFor(moduleID), cancel)
	defer wd.stop()
	handleCtx.withLock(func(h *HandleContext) error { h.watchdog = wd; return nil })
	defer handleCtx.withLock(func(h *HandleContext) error { h.watchdog = nil; return nil })
	if _, err := f.Call(ctx); err != nil {
		return fmt.Errorf("failed to call %s function: %w", fname, err)
	}
	return nil
}

// computeBudgetFor resolves one hook execution's compute budget:
//
//	budget = the module's declared ComputeBudget, else the node default
//	         (popcache_config.wasm_compute_default_ms, else the built-in
//	         constructor value), clamped to the node ceiling
//	         (popcache_config.wasm_compute_max_ms, else that same default).
//
// So an undeclared module follows the node default, and a module can only
// raise itself above it when the node explicitly grants headroom via max.
func (r *edgeComputing) computeBudgetFor(moduleID uint32) time.Duration {
	def := r.computingTimeout
	ceiling := time.Duration(0)
	if r.cfg != nil {
		if c := r.cfg.PopcacheConfig(); c != nil {
			if c.WasmComputeDefault > 0 {
				def = c.WasmComputeDefault
			}
			ceiling = c.WasmComputeMax
		}
	}
	if ceiling <= 0 {
		ceiling = def
	}
	budget := def
	if m, ok := r.routing.Current(moduleID); ok && m.ComputeBudget > 0 {
		budget = m.ComputeBudget
	}
	if budget > ceiling {
		budget = ceiling
	}
	return budget
}

// computeWatchdog enforces the wasm compute budget as a pausable clock. It
// cancels the hook context once the module has RUN for the budget; pause/resume
// bracket blocking host calls so their wall time is excluded.
type computeWatchdog struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	timer     *time.Timer
	remaining time.Duration
	started   time.Time // zero while paused
}

func newComputeWatchdog(budget time.Duration, cancel context.CancelFunc) *computeWatchdog {
	return &computeWatchdog{
		cancel:    cancel,
		timer:     time.AfterFunc(budget, cancel),
		remaining: budget,
		started:   time.Now(),
	}
}

func (w *computeWatchdog) pause() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started.IsZero() {
		return
	}
	w.timer.Stop()
	w.remaining -= time.Since(w.started)
	w.started = time.Time{}
}

func (w *computeWatchdog) resume() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started.IsZero() {
		return
	}
	if w.remaining <= 0 {
		w.cancel()
		return
	}
	w.started = time.Now()
	w.timer.Reset(w.remaining)
}

func (w *computeWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer.Stop()
}

func (c *HandleContext) withLock(task func(h *HandleContext) error) error {
	c.m.Lock()
	defer c.m.Unlock()
	if err := task(c); err != nil {
		return err
	}
	return nil
}

func (c *HandleContext) getModule(reqID uint64, task func(h *HandleContext) error) (api.Module, error) {
	var mod api.Module
	err := c.withLock(func(h *HandleContext) error {
		if h.mod == nil {
			return fmt.Errorf("module is not set for request ID %d", reqID)
		}
		mod = h.mod
		if task != nil {
			if err := task(h); err != nil {
				return err
			}
		}
		return nil
	})
	return mod, err
}

func (r *edgeComputing) ProcessRequest(ctx context.Context, reqID uint64, popID uint32, p *http.Request) error {
	handleCtx, err := r.getHandleContext(reqID)
	if err != nil {
		return err
	}
	info, err := getRequestInfo(popID, reqID, p)
	if err != nil {
		return fmt.Errorf("failed to get request info: %w", err)
	}
	mod, err := handleCtx.getModule(reqID, func(h *HandleContext) error {
		if h.req != nil {
			return fmt.Errorf("request already processed for request ID %d", reqID)
		}
		h.req = info
		h.rawReq = p
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to process request for request ID %d: %w", reqID, err)
	}
	return r.executeWasm(ctx, mod, reqID, hookOnRequest)
}

func (r *edgeComputing) ProcessResponse(ctx context.Context, reqID uint64, resp *http.Response) error {
	handleCtx, err := r.getHandleContext(reqID)
	if err != nil {
		return err
	}
	respInfo, err := getResponseInfo(resp)
	if err != nil {
		return fmt.Errorf("failed to get response info: %w", err)
	}
	mod, err := handleCtx.getModule(reqID, func(h *HandleContext) error {
		if h.resp != nil {
			return fmt.Errorf("response already processed for request ID %d", reqID)
		}
		h.resp = respInfo
		h.rawResp = resp
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to process request for request ID %d: %w", reqID, err)
	}
	return r.executeWasm(ctx, mod, reqID, hookOnResponse)
}

func (r *edgeComputing) FinishRequest(ctx context.Context, reqID uint64) error {
	handleCtx, err := r.getHandleContext(reqID)
	if err != nil {
		return err
	}
	defer func() {
		r.handlerRW.Lock()
		defer r.handlerRW.Unlock()
		delete(r.handleContext, reqID)
	}()
	mod, err := handleCtx.getModule(reqID, func(h *HandleContext) error {
		if h.req == nil {
			return fmt.Errorf("request not processed for request ID %d", reqID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to get module for request ID %d: %w", reqID, err)
	}
	err = r.executeWasm(ctx, mod, reqID, hookOnFinish)
	err2 := mod.Close(ctx)
	return errors.Join(err, err2)
}
