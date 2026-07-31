package edge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	pathlib "path"
	"strings"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// Outbound fetch host call ("kscale.fetch"): a module issues an HTTP(S)
// sub-request, gated by its registration's AllowedHosts (default-deny). The
// host executes the request synchronously, buffers the response, and hands back
// a fetch id; the module then pulls the outcome via get_fetch_response
// (encoded FetchResponseInfo) and get_fetch_body (same pull convention as
// get_{request,response}_body). Results live in the HandleContext until the
// request finishes.
const (
	// maxFetchBodyBytes caps how much of a fetched response body the host will
	// buffer. Over-cap is reported as a transport error, not a truncation, so
	// the module never operates on partial data.
	maxFetchBodyBytes = 4 << 20 // 4 MiB
	// maxFetchesPerRequest bounds how many fetches one in-flight request may
	// issue (the computing timeout is the other bound; this stops tight loops
	// from hammering an allowlisted host within that window).
	maxFetchesPerRequest = 16
	defaultFetchTimeout  = 5 * time.Second
	maxFetchTimeout      = 10 * time.Second
	maxFetchRedirects    = 10
	// maxFetchErrorLen bounds the error string carried in FetchResponseInfo
	// (its wire type is a u16-length String).
	maxFetchErrorLen = 512
)

// fetchResult is one completed fetch, held on the HandleContext: the
// pre-encoded FetchResponseInfo plus the buffered body (nil on error).
type fetchResult struct {
	info []byte
	body []byte
}

// fetchAllowRule is one parsed allowed_hosts entry:
//
//	[scheme://]host[:port][/path-prefix]
//
// host is required; everything else narrows the rule. A host-only entry allows
// any port/path (the original form). A path-prefix entry allows only URLs
// whose (cleaned) path equals the prefix or sits under it on a segment
// boundary — "github.com/myorg" covers /myorg and /myorg/..., not /myorgxyz.
type fetchAllowRule struct {
	scheme string // "" = any (fetch only permits http/https anyway)
	host   string // lowercase hostname, required
	port   string // "" = any port
	prefix string // cleaned path prefix, "" = any path
}

// parseFetchAllowRule parses one allowed_hosts entry; ok=false for blank or
// hostless entries (which then allow nothing).
func parseFetchAllowRule(entry string) (fetchAllowRule, bool) {
	e := strings.TrimSpace(entry)
	var r fetchAllowRule
	if scheme, rest, ok := strings.Cut(e, "://"); ok {
		r.scheme = strings.ToLower(scheme)
		e = rest
	}
	hostport := e
	if i := strings.IndexByte(e, '/'); i >= 0 {
		hostport = e[:i]
		r.prefix = cleanFetchPath(e[i:])
		if r.prefix == "/" {
			r.prefix = "" // trailing "host/" means any path
		}
	}
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		r.host, r.port = strings.ToLower(h), p
	} else {
		// No port. Also accept a bracketed IPv6 literal ("[::1]") the way the
		// with-port form does — url.Hostname() reports it unbracketed.
		r.host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]"))
	}
	return r, r.host != ""
}

func (r fetchAllowRule) matches(u *url.URL) bool {
	if r.scheme != "" && r.scheme != u.Scheme {
		return false
	}
	if r.host != strings.ToLower(u.Hostname()) {
		return false
	}
	if r.port != "" {
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		if r.port != port {
			return false
		}
	}
	if r.prefix != "" {
		p := cleanFetchPath(u.Path)
		if p != r.prefix && !strings.HasPrefix(p, r.prefix+"/") {
			return false
		}
	}
	return true
}

// cleanFetchPath canonicalizes a URL path for prefix matching: rooted and
// dot-segment-free, so "/a/../b" and the %2F-decoded forms cannot escape an
// allowed prefix (url.Parse already percent-decodes Path).
func cleanFetchPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return pathlib.Clean(p)
}

// fetchURLAllowed reports whether u is covered by one of the module's
// allowed_hosts entries. Matching is case-insensitive on the host, exact on a
// pinned port/scheme, and segment-boundary on a path prefix; no wildcards. An
// empty list allows nothing (default-deny).
func fetchURLAllowed(allowed []string, u *url.URL) bool {
	for _, entry := range allowed {
		if r, ok := parseFetchAllowRule(entry); ok && r.matches(u) {
			return true
		}
	}
	return false
}

// encodeFetchError builds the encoded FetchResponseInfo for a transport-level
// failure (status 0 + error string).
func encodeFetchError(msg string) []byte {
	if len(msg) > maxFetchErrorLen {
		msg = msg[:maxFetchErrorLen]
	}
	info := &FetchResponseInfo{Status: 0}
	var s String
	if !s.SetData([]byte(msg)) {
		s.SetData([]byte("fetch failed"))
	}
	info.SetError(s)
	out, err := info.Append(nil)
	if err != nil {
		// Cannot happen for a bounded error string; return an empty info so the
		// module still sees status 0.
		out, _ = (&FetchResponseInfo{Status: 0}).Append(nil)
	}
	return out
}

// encodeFetchInfo builds the encoded FetchResponseInfo for an HTTP response
// (status + headers; the body is pulled separately via get_fetch_body).
func encodeFetchInfo(resp *http.Response) ([]byte, error) {
	info := &FetchResponseInfo{Status: uint16(resp.StatusCode)}
	fields := make([]Field, 0, len(resp.Header))
	for key, vs := range resp.Header {
		for _, value := range vs {
			var f Field
			if !f.Key.SetData([]byte(key)) || !f.Value.SetData([]byte(value)) {
				return nil, fmt.Errorf("header %q does not fit the wire format", key)
			}
			fields = append(fields, f)
		}
	}
	var hdr Header
	if !hdr.SetFields(fields) {
		return nil, errors.New("too many response headers for the wire format")
	}
	if !info.SetHeader(hdr) {
		return nil, errors.New("failed to set response header")
	}
	return info.Append(nil)
}

// hostFetch is the "kscale.fetch" host function. Returns the fetch id (>=1), or 0
// when the request is rejected outright: bad encoding, no in-flight request,
// scheme not http/https, host not in the module's allowed_hosts, or the
// per-request fetch budget is spent. Transport failures are NOT rejections —
// they return an id whose FetchResponseInfo carries status 0 + error.
func (r *edgeComputing) hostFetch(c context.Context, mod api.Module, reqPtr, reqSize uint32) uint32 {
	handleCtx, ok := r.handleFromCtx(c)
	if !ok {
		return 0
	}
	buf, ok := mod.Memory().Read(reqPtr, reqSize)
	if !ok {
		r.logger.Error("fetch: failed to read request buffer", "ptr", reqPtr, "size", reqSize)
		return 0
	}
	freq := &FetchRequest{}
	if err := freq.DecodeExact(buf); err != nil {
		r.logger.Error("fetch: failed to decode request", "error", err)
		return 0
	}

	// Resolve the executing module (its id was recorded at StartRequest; the
	// registry entry carries the allowlist) and take one unit of the per-request
	// fetch budget. NOT h.req.PopID — that is the server's id, not the module's.
	var moduleID uint32
	budgetOK := false
	handleCtx.withLock(func(h *HandleContext) error {
		moduleID = h.moduleID
		if h.fetchCount < maxFetchesPerRequest {
			budgetOK = true
			h.fetchCount++
		}
		return nil
	})
	if !budgetOK {
		r.logger.Error("fetch: per-request fetch budget spent", "moduleID", moduleID, "budget", maxFetchesPerRequest)
		return 0
	}
	modInfo, ok := r.routing.Current(moduleID)
	if !ok {
		// Only possible when the module was unregistered (resource deleted /
		// pruned) while this request was in flight: fail closed, its allowlist
		// is gone.
		r.logger.Error("fetch: executing module is no longer registered", "moduleID", moduleID)
		return 0
	}

	target, err := url.Parse(string(freq.Url.Data))
	if err != nil || target.Host == "" {
		r.logger.Error("fetch: bad url", "url", string(freq.Url.Data), "error", err)
		return 0
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		r.logger.Error("fetch: scheme not allowed", "scheme", target.Scheme)
		return 0
	}
	if !fetchURLAllowed(modInfo.AllowedHosts, target) {
		r.logger.Error("fetch: url not in allowed_hosts", "moduleID", moduleID, "url", target.Redacted())
		return 0
	}

	// Stop the compute clock while blocked in network IO: the module executes
	// no wasm during the host call, so only its own fetch timeout applies.
	var wd *computeWatchdog
	handleCtx.withLock(func(h *HandleContext) error { wd = h.watchdog; return nil })
	res := func() *fetchResult {
		if wd != nil {
			wd.pause()
			defer wd.resume()
		}
		return r.doFetch(c, freq, target, modInfo.AllowedHosts)
	}()

	var id uint32
	handleCtx.withLock(func(h *HandleContext) error {
		if h.fetches == nil {
			h.fetches = map[uint32]*fetchResult{}
		}
		h.lastFetchID++
		id = h.lastFetchID
		h.fetches[id] = res
		return nil
	})
	return id
}

// doFetch executes the allowlist-vetted request and buffers the outcome. All
// failures from here on are transport errors (fetchResult with status 0), not
// rejections.
func (r *edgeComputing) doFetch(c context.Context, freq *FetchRequest, target *url.URL, allowed []string) *fetchResult {
	methodName := freq.Method.Method.String()
	if freq.Method.Method == Method_OTHER {
		p := freq.Method.MethodName()
		if p == nil {
			return &fetchResult{info: encodeFetchError("method name is missing")}
		}
		methodName = string(*p)
	}

	timeout := defaultFetchTimeout
	if freq.TimeoutMs != 0 {
		timeout = time.Duration(freq.TimeoutMs) * time.Millisecond
	}
	if timeout > maxFetchTimeout {
		timeout = maxFetchTimeout
	}
	// c is the hook-execution context (bounded by computingTimeout), so the
	// effective deadline is the earlier of the two.
	ctx, cancel := context.WithTimeout(c, timeout)
	defer cancel()

	var body io.Reader
	if len(freq.Body) > 0 {
		// Own the bytes: the decoded request aliases wasm linear memory.
		body = bytes.NewReader(append([]byte(nil), freq.Body...))
	}
	hreq, err := http.NewRequestWithContext(ctx, methodName, target.String(), body)
	if err != nil {
		return &fetchResult{info: encodeFetchError(err.Error())}
	}
	for _, f := range freq.Header.Fields {
		key, value := string(f.Key.Data), string(f.Value.Data)
		if strings.EqualFold(key, "Host") {
			hreq.Host = value
			continue
		}
		hreq.Header.Add(key, value)
	}

	client := &http.Client{
		Transport: r.fetchTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxFetchRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect scheme %q not allowed", req.URL.Scheme)
			}
			if !fetchURLAllowed(allowed, req.URL) {
				return fmt.Errorf("redirect target %q not in allowed_hosts", req.URL.Host)
			}
			return nil
		},
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return &fetchResult{info: encodeFetchError(err.Error())}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBodyBytes+1))
	if err != nil {
		return &fetchResult{info: encodeFetchError(err.Error())}
	}
	if len(data) > maxFetchBodyBytes {
		return &fetchResult{info: encodeFetchError(fmt.Sprintf("response body exceeds %d bytes", maxFetchBodyBytes))}
	}
	info, err := encodeFetchInfo(resp)
	if err != nil {
		return &fetchResult{info: encodeFetchError(err.Error())}
	}
	return &fetchResult{info: info, body: data}
}

// getFetchResponse is the "kscale.get_fetch_response" host function: copy the
// encoded FetchResponseInfo for fetch id into the module buffer (pull
// convention: writes min(outSize, total), returns total; 0 = unknown id).
func (r *edgeComputing) getFetchResponse(c context.Context, mod api.Module, id, outPtr, outSize uint32) uint32 {
	return r.pullFetch(c, mod, id, outPtr, outSize, func(res *fetchResult) []byte { return res.info })
}

// getFetchBody is the "kscale.get_fetch_body" host function: copy the buffered
// response body for fetch id into the module buffer (same pull convention).
func (r *edgeComputing) getFetchBody(c context.Context, mod api.Module, id, outPtr, outSize uint32) uint32 {
	return r.pullFetch(c, mod, id, outPtr, outSize, func(res *fetchResult) []byte { return res.body })
}

func (r *edgeComputing) pullFetch(c context.Context, mod api.Module, id, outPtr, outSize uint32, pick func(*fetchResult) []byte) uint32 {
	handleCtx, ok := r.handleFromCtx(c)
	if !ok {
		return 0
	}
	var total uint32
	handleCtx.withLock(func(h *HandleContext) error {
		res, ok := h.fetches[id]
		if !ok {
			return nil
		}
		total = writeBody(mod, outPtr, outSize, pick(res))
		return nil
	})
	return total
}
