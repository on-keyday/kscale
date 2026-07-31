package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/kscale/httprps"
	"github.com/on-keyday/kscale/internal/safe"
	"github.com/on-keyday/kscale/popcache/cache"
	lbconnid "github.com/on-keyday/kscale/popcache/connid"
	"github.com/on-keyday/kscale/popcache/edge"
	"github.com/on-keyday/kscale/popcache/popmetrics"
	"github.com/on-keyday/kscale/popcache/types"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"
	"github.com/on-keyday/kscale/protobuf/wkt"
	"github.com/on-keyday/kscale/stat"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/http3/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
	"github.com/tetratelabs/wazero"
	"golang.org/x/net/http2"
	"golang.org/x/net/ipv4"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

type Config struct {
	NodeID             string
	SecureListenAddr   string
	InsecureListenAddr string
	HTTP3ListenAddr    string
	ServerID           uint32
	ServerCertFile     string
	ServerKeyFile      string
	OriginURL          string
	FileDir            *os.Root
}

// PacketConnListener optionally overrides how the HTTP/3 UDP listener is
// created (see Server.SetCustomStreamListener). If unset, the Server falls back
// to ListenObservedPacketConn — both paths are preserved.
type PacketConnListener func(network, address string, logger *slog.Logger) (net.PacketConn, error)

// Server is the framework-free popcache HTTP/HTTPS/HTTP3 cache server. It was
// re-homed from ksdk's agent-framework `serverAgent`: all the live config that
// used to arrive via the agent framework's command dispatch + config map is now
// applied through the exported Set*/Reload*/Sync* methods, and metrics that used
// to be pushed through a framework StatReporter are now stored locally for the
// owning agent to pull (Metrics) and serve (PromMetrics).
type Server struct {
	Config
	logger *slog.Logger

	originURL             atomic.Pointer[url.URL]
	wasmComputeDefaultMs  atomic.Uint32
	wasmComputeMaxMs      atomic.Uint32
	ec                    edge.EdgeComputer
	cache                 *cache.Cache
	sharedSecret          *lbconnid.QUICLBConnIDGenerator
	runningInstanceCancel context.CancelFunc

	connStat   popmetrics.PoPMetricsWithLock
	launchOnce sync.Once

	storedCert         atomic.Pointer[tls.Certificate]
	serverCertFilePath atomic.Value // string
	serverKeyFilePath  atomic.Value // string
	qlogEnabled        atomic.Bool

	// metrics sink — replaces the framework StatReporter. The latest stat
	// snapshots are stored here for the owning agent to pull via Metrics() and
	// stream over its StreamStats RPC; prometheus state is kept in promMetrics
	// (guarded by promMu) for the agent to serve at /metrics.
	promMu                 sync.Mutex
	promMetrics            *stat.PromMetrics
	latestPopcacheStat     atomic.Pointer[pbstat.PopcacheStat]
	latestPopcacheSpecStat atomic.Pointer[pbstat.PopcacheSpecStat]

	// customPacketListener optionally overrides the HTTP/3 UDP listener path.
	customPacketListener PacketConnListener
}

type wasmService struct {
	ec *Server
}

var _ pb.WasmServiceServer = (*wasmService)(nil)

func (s *wasmService) Register(ctx context.Context, args *pb.WasmServiceRegisterRequest) (*wkt.Empty, error) {
	wasmData, err := s.ec.FileDir.ReadFile(args.BinaryPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read wasm binary: %w", err)
	}
	err = s.ec.ec.Register(ctx, edge.ModuleSpec{
		ID:            args.Id,
		Method:        args.Method,
		Path:          args.Path,
		MatchType:     args.MatchType,
		FilePath:      args.BinaryPath,
		AllowedHosts:  args.AllowedHosts,
		ComputeBudget: time.Duration(args.ComputeBudgetMs) * time.Millisecond,
	}, wasmData)
	if err != nil {
		return nil, fmt.Errorf("failed to register wasm binary: %w", err)
	}
	return &wkt.Empty{}, nil
}

func (s *wasmService) Unregister(ctx context.Context, args *pb.WasmServiceUnregisterRequest) (*wkt.Empty, error) {
	err := s.ec.ec.Unregister(args.Id)
	if err != nil {
		return nil, fmt.Errorf("failed to unregister wasm binary: %w", err)
	}
	return &wkt.Empty{}, nil
}

func (s *wasmService) ListAttached(ctx context.Context, _ *wkt.Empty) (*pb.WasmServiceListAttachedResponse, error) {
	infos := s.ec.ec.ListRegistered()
	var resp pb.WasmServiceListAttachedResponse
	for _, info := range infos {
		resp.Info = append(resp.Info, &pb.WasmInfo{
			Id:              info.ID,
			BinaryPath:      info.FilePath,
			BinarySize:      uint64(info.FileSize),
			Method:          info.Method,
			Path:            info.Path,
			MatchType:       info.MatchType,
			AllowedHosts:    info.AllowedHosts,
			ComputeBudgetMs: uint32(info.ComputeBudget / time.Millisecond),
		})
	}
	return &resp, nil
}

// WasmService returns the WasmServiceServer implementation backed by this
// Server's edge computer, for the owning agent to register on its RPC manager.
func (s *Server) WasmService() pb.WasmServiceServer { return &wasmService{ec: s} }

// updateProm applies f to the Server's prometheus metrics under the lock.
func (s *Server) updateProm(f func(*stat.PromMetrics)) {
	s.promMu.Lock()
	defer s.promMu.Unlock()
	f(s.promMetrics)
}

// PromMetrics returns the Server's live prometheus metrics, for the owning agent
// to serve at /metrics. (Preserves the prometheus path that the old framework
// StatReporter.UpdatePromStat fed.)
func (s *Server) PromMetrics() *stat.PromMetrics { return s.promMetrics }

// Metrics returns the latest popcache stat snapshots, for the owning agent to
// stream over its StreamStats RPC. Either may be nil before the first tick.
func (s *Server) Metrics() (*pbstat.PopcacheStat, *pbstat.PopcacheSpecStat) {
	return s.latestPopcacheStat.Load(), s.latestPopcacheSpecStat.Load()
}

func (s *Server) sendMetrics(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	s.updateProm(func(m *stat.PromMetrics) {
		m.PopcacheEnabled = true
	})
	prev := stat.PopcacheStat{}
	prevSpec := stat.PopcacheSpecStat{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			metricsCopy := s.connStat.GetMetrics()
			originURL := s.originURL.Load()
			ps := stat.PopcacheStat{
				PopcacheStats: metricsCopy,
			}
			p := stat.PopcacheSpecStat{
				OriginServerAddress: originURL.String(),
			}
			if !ps.Equal(&prev) {
				s.latestPopcacheStat.Store(ps.ToProto())
				s.updateProm(func(m *stat.PromMetrics) {
					m.UpdatePopcacheStat(&ps)
				})
			}
			if !p.Equal(&prevSpec) {
				s.latestPopcacheSpecStat.Store(p.ToProto())
			}
			prev = ps
			prevSpec = p
		}
	}
}

func parseOriginURL(originStr string) (*url.URL, error) {
	originURL, err := url.Parse(originStr)
	if err != nil {
		return nil, err
	}
	if originURL.Scheme != "http" && originURL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported origin URL scheme %q", originURL.Scheme)
	}
	if originURL.Host == "" {
		return nil, fmt.Errorf("origin URL must have a host")
	}
	return originURL, nil
}

// NewServer builds a popcache Server. The cache/wazero/edge construction is
// identical to the old NewServerAgent; only the framework Agent return type is
// dropped in favour of the concrete *Server.
func NewServer(config Config, logger *slog.Logger) (*Server, error) {
	c := cache.NewCache()
	conf := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
	rt := wazero.NewRuntimeWithConfig(context.Background(), conf)
	// Load the geoip databases from the node-local file store, if present, and
	// hand the edge computer a GeoLocator for the module-facing geoloc_lookup.
	// Absent databases -> nil -> geoloc_lookup reports "not found".
	geo := loadGeoLocator(config.FileDir, logger.With("component", "geoloc"))
	ec, err := edge.NewEdgeComputing(rt, 100*time.Millisecond, logger.With("component", "edge_computer"), geo)
	if err != nil {
		return nil, fmt.Errorf("failed to create edge computing instance: %w", err)
	}
	originURL, err := parseOriginURL(config.OriginURL)
	if err != nil {
		return nil, fmt.Errorf("invalid origin URL %q: %w", config.OriginURL, err)
	}
	s := &Server{
		Config:       config,
		cache:        c,
		ec:           ec,
		sharedSecret: lbconnid.NewQUICLBConnIDGenerator(config.ServerID, []byte{}, 17),
		logger:       logger,
		promMetrics:  &stat.PromMetrics{},
	}
	s.originURL.Store(originURL)
	// Seed the cert/key path atomics so Set*/Reload*/startServer never observe an
	// unset value. (The old code seeded these inside initPortStateFromListenAddrs,
	// which ran on the framework init command before any setter; doing it here
	// avoids a later launchOnce clobbering a path set via SetCertPath/SetKeyPath.)
	s.serverCertFilePath.Store(config.ServerCertFile)
	s.serverKeyFilePath.Store(config.ServerKeyFile)
	// Expose the non-sensitive serving config (origin + listener ports) to wasm
	// modules via the get_popcache_config host callback.
	s.ec.SetPopcacheConfigProvider(s)
	return s, nil
}

// PopcacheConfig implements edge.PopcacheConfigProvider: the live serving config
// exposed to wasm modules. Origin is read from the atomic (it can change via
// ApplyConfig); listener ports are parsed from the fixed listen addresses. No
// cert/key paths are exposed.
func (s *Server) PopcacheConfig() *edge.PopcacheConfigInfo {
	info := &edge.PopcacheConfigInfo{
		HTTPPort:           portOf(s.Config.InsecureListenAddr),
		HTTPSPort:          portOf(s.Config.SecureListenAddr),
		HTTP3Port:          portOf(s.Config.HTTP3ListenAddr),
		WasmComputeDefault: time.Duration(s.wasmComputeDefaultMs.Load()) * time.Millisecond,
		WasmComputeMax:     time.Duration(s.wasmComputeMaxMs.Load()) * time.Millisecond,
	}
	if u := s.originURL.Load(); u != nil {
		info.Origin = u.String()
	}
	return info
}

// SetWasmComputeBudgets sets the node-wide default/ceiling for per-module wasm
// compute budgets (0 = built-in default), driven declaratively by the
// popcache_config reconcile via ApplyConfig.
func (s *Server) SetWasmComputeBudgets(defaultMs, maxMs uint32) {
	s.wasmComputeDefaultMs.Store(defaultMs)
	s.wasmComputeMaxMs.Store(maxMs)
}

// portOf extracts the numeric port from a listen address like ":443" or
// "0.0.0.0:8080"; returns 0 when the address has no parseable port.
func portOf(listenAddr string) uint16 {
	_, portStr, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return 0
	}
	p, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(p)
}

// SetCustomStreamListener overrides how the HTTP/3 UDP listener is created. If
// never set (nil), the normal ListenObservedPacketConn path is used.
func (s *Server) SetCustomStreamListener(l PacketConnListener) { s.customPacketListener = l }

func (s *Server) mainHandler(w http.ResponseWriter, r *http.Request) {
	originURL := s.originURL.Load()
	reqID, err := s.ec.StartRequest(r.Context(), r)
	var handleRequest func(*http.Request)
	var handleResponse func(*http.Response)
	routing := edge.Routing_Cont
	if err != nil {
		if !errors.Is(err, edge.ErrNoEdgeFunction) {
			s.logger.Error("Failed to start request", "error", err)
		}
		s.logger.Info("No edge function registered", "method", r.Method, "path", r.URL.Path)
		handleRequest = func(req *http.Request) {}
		handleResponse = func(resp *http.Response) {}
	} else {
		s.logger.Info("Edge function started", "method", r.Method, "path", r.URL.Path, "req_id", reqID)
		defer func() {
			if err := s.ec.FinishRequest(r.Context(), reqID); err != nil {
				s.logger.Error("Failed to finish request", "error", err)
			}
		}()
		handleRequest = func(req *http.Request) {
			req.Header.Set("Host", req.Host) // reset for edge function
			err := s.ec.ProcessRequest(r.Context(), reqID, s.ServerID, req)
			if err != nil {
				s.logger.Error("Failed to process request", "error", err)
			}
			cloned := req.Clone(r.Context())
			changeRoute := edge.Routing_Cont
			err = s.ec.ModifyRequest(r.Context(), reqID, func(d []edge.DiffData) error {
				for i := range d {
					if r := d[i].Routing(); r != nil {
						changeRoute = *r
						continue
					}
					err := edge.DefaultModifyRequest(&d[i], cloned)
					if err != nil {
						return err
					}
				}
				return nil
			}, true)
			if err != nil {
				return
			}
			*req = *cloned
			routing = changeRoute
		}
		handleResponse = func(resp *http.Response) {
			// first, apply the changes from on_request
			err := s.ec.ModifyResponse(r.Context(), reqID, func(d []edge.DiffData) error {
				for i := range d {
					if r := d[i].Routing(); r != nil {
						routing = *r
						continue
					}
					err := edge.DefaultModifyResponse(&d[i], resp)
					if err != nil {
						return err
					}
				}
				return nil
			}, true)
			if err != nil {
				s.logger.Error("Failed to modify response", "error", err)
				return
			}
			err = s.ec.ProcessResponse(r.Context(), reqID, resp)
			if err != nil {
				s.logger.Error("Failed to process response", "error", err)
			}
			// then, apply the changes from on_response
			err = s.ec.ModifyResponse(r.Context(), reqID, func(d []edge.DiffData) error {
				for i := range d {
					if r := d[i].Routing(); r != nil {
						routing = *r
						continue
					}
					err := edge.DefaultModifyResponse(&d[i], resp)
					if err != nil {
						return err
					}
				}
				return nil
			}, true)
			if err != nil {
				s.logger.Error("Failed to modify response", "error", err)
				return
			}
		}
	}
	doAbort := func() {
		s.logger.Info("Request is aborted by edge function, disconnect", "method", r.Method, "path", r.URL.Path)
		hijacked, ok := w.(http.Hijacker)
		if !ok {
			s.logger.Error("Response writer does not support hijacking, cannot abort connection")
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		conn, _, err := hijacked.Hijack()
		if err != nil {
			s.logger.Error("Failed to hijack connection", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		s.logger.Info("Hijacking connection", "remote_addr", conn.RemoteAddr())
		conn.Close() // Close the connection to abort

	}
	handleRequest(r)
	if routing == edge.Routing_Abort {
		doAbort()
		return
	}
	if routing == edge.Routing_Deny {
		respHdr := w.Header()
		respHdr.Set("Content-Type", "text/plain; charset=utf-8")
		resp := &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     respHdr,
			Body:       io.NopCloser(bytes.NewBufferString("Access denied by edge function")),
		}
		handleResponse(resp)
		w.Header().Set("X-KScale-PoPCache-Hit", "false")
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			s.logger.Error("Failed to write response body", "error", err)
		}
		return
	}
	isCacheable := r.Method == http.MethodGet || r.Method == http.MethodHead
	var cacheKey string
	if isCacheable && !(routing == edge.Routing_NoCache ||
		routing == edge.Routing_SkipCache) {
		key, resp, found := s.cache.HasCache(r)
		if found {
			s.logger.Info("Cache hit", "key", key)
			s.connStat.IncrementCacheHitsTotal()
			if routing == edge.Routing_SkipResponseExecIfCached {
				handleResponse(resp)
			}
			w.Header().Set("X-KScale-PoPCache-Hit", "true")
			w.WriteHeader(resp.StatusCode)
			if _, err := io.Copy(w, resp.Body); err != nil {
				s.logger.Error("Failed to write cached response body", "error", err)
			}
			return
		}
		cacheKey = key
	}
	s.logger.Info("Fetching from origin", "url", originURL.String()+r.URL.RequestURI())
	// Handle GET and HEAD requests
	reverseProxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetXForwarded()
			r.Out.Header.Set("X-KScale-PoPCache-NodeId", s.NodeID)
			r.SetURL(originURL)
		},
		ModifyResponse: func(resp *http.Response) error {
			handleResponse(resp)
			if routing == edge.Routing_Abort {
				doAbort()
				return nil
			}
			if routing != edge.Routing_NoCache {
				if cacheData, ok := s.cache.IsCacheable(resp); ok || routing == edge.Routing_ForceCache {
					if cacheKey == "" {
						cacheKey = s.cache.GenerateCacheKey(resp.Request)
					}
					s.cache.SetCache(cacheKey, r, resp, cacheData)
				}
			}
			s.connStat.IncrementCacheMissesTotal()
			resp.Header.Set("X-KScale-PoPCache-Hit", "false")
			return nil
		},
	}
	reverseProxy.ServeHTTP(w, r)
}

func (s *Server) setHandlers() http.Handler {
	logger := s.logger
	mux := http.NewServeMux()
	rps := httprps.NewMiddleware(mux, func(r *http.Request) {
		s.connStat.IncrementRequestsTotal()
		// currently collect version only
		switch r.ProtoMajor {
		case 1:
			switch r.ProtoMinor {
			case 1:
				if r.TLS != nil {
					s.connStat.IncrementPerProtocolTotalHttp11()
					logger.Info("HTTP/1.1 over TLS connection", "remote_address", r.RemoteAddr)
				} else {
					s.connStat.IncrementPerProtocolTotalHttp11Plain()
					logger.Info("HTTP/1.1 plain connection", "remote_address", r.RemoteAddr)
				}
			case 0:
				if r.TLS != nil {
					s.connStat.IncrementPerProtocolTotalHttp10()
					logger.Info("HTTP/1.0 over TLS connection", "remote_address", r.RemoteAddr)
				} else {
					s.connStat.IncrementPerProtocolTotalHttp10Plain()
					logger.Info("HTTP/1.0 plain connection", "remote_address", r.RemoteAddr)
				}
			default:
				logger.Info("Unknown HTTP/1.x connection", "version", r.Proto, "remote_address", r.RemoteAddr)
			}
		case 2:
			s.connStat.IncrementPerProtocolTotalHttp2()
			logger.Info("HTTP/2 connection", "remote_address", r.RemoteAddr)
		case 3:
			s.connStat.IncrementPerProtocolTotalHttp3()
			logger.Info("HTTP/3 connection", "remote_address", r.RemoteAddr)
		default:
			if r.TLS != nil {
				s.connStat.IncrementPerProtocolTotalOther()
				logger.Info("Unknown HTTP over TLS connection", "version", r.Proto, "remote_address", r.RemoteAddr)
			} else {
				s.connStat.IncrementPerProtocolTotalOtherPlain()
				logger.Info("Unknown HTTP version connection", "version", r.Proto, "remote_address", r.RemoteAddr)
			}
		}
		switch r.Method {
		case http.MethodGet:
			s.connStat.IncrementPerMethodTotalGet()
		case http.MethodHead:
			s.connStat.IncrementPerMethodTotalHead()
		case http.MethodPost:
			s.connStat.IncrementPerMethodTotalPost()
		case http.MethodPut:
			s.connStat.IncrementPerMethodTotalPut()
		case http.MethodPatch:
			s.connStat.IncrementPerMethodTotalPatch()
		case http.MethodDelete:
			s.connStat.IncrementPerMethodTotalDelete()
		case http.MethodOptions:
			s.connStat.IncrementPerMethodTotalOptions()
		case http.MethodTrace:
			s.connStat.IncrementPerMethodTotalTrace()
		case http.MethodConnect:
			s.connStat.IncrementPerMethodTotalConnect()
		default:
			s.connStat.IncrementPerMethodTotalOther()
		}
	}, func(r *http.Request) {
		s.connStat.IncrementRequestsEndTotal()
	})
	mux.HandleFunc("/statusz", func(w http.ResponseWriter, r *http.Request) {
		st := types.PoPStatus{
			Id:     s.NodeID,
			Uptime: stat.ServerUptime().Seconds(),
			Load:   rps.GetRPS(),
		}
		bs, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			logger.Error("Failed to marshal PoP status", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		_, _ = w.Write(bs)
	})
	mux.HandleFunc("/latencyz", func(w http.ResponseWriter, r *http.Request) {
		// return 204
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/streamz", func(w http.ResponseWriter, r *http.Request) {
		// 1分間ストリーミングでデータを送り続ける
		w.WriteHeader(http.StatusOK)
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		counter := 0
		flusher, ok := w.(http.Flusher)
		if !ok {
			logger.Error("Response writer does not support flushing")
		}
		flusher.Flush()
		for counter < 60 {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte("data chunk " + strconv.Itoa(counter) + "\n"))
				if ok {
					flusher.Flush()
				}
				counter++
			}
		}
		w.Write([]byte("end of stream\n"))
	})
	mux.HandleFunc("/ipz", func(w http.ResponseWriter, r *http.Request) {
		// return client IP address
		ip := r.RemoteAddr
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ip + "\n"))
	})

	mux.HandleFunc("/", s.mainHandler)

	return rps
}

func (s *Server) getConnStateLogger() func(conn net.Conn, state http.ConnState) {
	return func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			s.connStat.IncrementConnectTotal()
			s.logger.Info("New connection", "remote_address", conn.RemoteAddr(), "local_address", conn.LocalAddr())
		case http.StateHijacked:
			s.connStat.IncrementHijackedTotal()
			s.logger.Info("Connection hijacked", "remote_address", conn.RemoteAddr(), "local_address", conn.LocalAddr())
		case http.StateClosed:
			s.connStat.IncrementCloseTotal()
			s.logger.Info("Connection closed", "remote_address", conn.RemoteAddr(), "local_address", conn.LocalAddr())
		}
	}
}

func (s *Server) setupShutdown(ctx context.Context, shutdown interface{ Shutdown(context.Context) error }, gr *errgroup.Group) {
	gr.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("Failed to shutdown server", "error", err)
			return err
		}
		return nil
	})
}

// TODO: make this configurable
func (s *Server) setTimeout(srv *http.Server) {
	srv.ReadHeaderTimeout = 5 * time.Second // slowloris対策
	srv.ReadTimeout = 0
	srv.WriteTimeout = 0
	srv.IdleTimeout = 120 * time.Second
}

func (s *Server) setupHTTPServer(ctx context.Context, mux http.Handler, gr *errgroup.Group, cancel context.CancelFunc) {
	httpServ := &http.Server{
		Addr:      s.InsecureListenAddr,
		Handler:   mux,
		ConnState: s.getConnStateLogger(),
	}
	s.setTimeout(httpServ)
	gr.Go(func() error {
		defer cancel()
		if err := httpServ.ListenAndServe(); err != nil {
			if errors.Is(err, http.ErrServerClosed) {
				s.logger.Info("HTTP server closed")
				return nil
			}
			s.logger.Error("HTTP server error", "error", err)
			return err
		}
		return nil
	})
	s.setupShutdown(ctx, httpServ, gr)
}

type ObservedPacketConn struct {
	quic.OOBCapablePacketConn
	bt     *ipv4.PacketConn
	logger *slog.Logger
}

func (c *ObservedPacketConn) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (n, oobn int, err error) {
	//c.logger.Info("WritePacket", "to", addr.String(), "len", len(b), "oob_len", len(oob), "oob_bytes", oob)
	// 非対称ルーティング実現のために送信元ifindexを指定しているOOBを書き換える。
	// srcについてはそのままのほうが都合が良いので残す
	// see also https://github.com/quic-go/quic-go/blame/7659dd8e0fa06b41290ad29af323d93d673c6b36/sys_conn_oob.go#L280
	// if info.addr.Is4() {
	// 	ip := info.addr.As4()
	// 	// struct in_pktinfo {
	// 	// 	unsigned int   ipi_ifindex;  /* Interface index */
	// 	// 	struct in_addr ipi_spec_dst; /* Local address */
	// 	// 	struct in_addr ipi_addr;     /* Header Destination address */
	// 	// };
	// 	cm := ipv4.ControlMessage{
	// 		Src:     ip[:],
	// 		IfIndex: int(info.ifIndex),
	// 	}
	// 	return cm.Marshal()
	// } else if info.addr.Is6() {
	// 	ip := info.addr.As16()
	// 	// struct in6_pktinfo {
	// 	// 	struct in6_addr ipi6_addr;    /* src/dst IPv6 address */
	// 	// 	unsigned int    ipi6_ifindex; /* send/recv interface index */
	// 	// };
	// 	cm := ipv6.ControlMessage{
	// 		Src:     ip[:],
	// 		IfIndex: int(info.ifIndex),
	// 	}
	// 	return cm.Marshal()
	// }
	// 先頭に以下が16バイトある
	// struct cmsghdr {
	// 	size_t cmsg_len;    /* Data byte count, including header
	// 							(type is socklen_t in POSIX) */
	// 	int    cmsg_level;  /* Originating protocol */
	// 	int    cmsg_type;   /* Protocol-specific type */
	// /* followed by
	// 	unsigned char cmsg_data[]; */
	// };
	const cmsghdrLen = unix.SizeofCmsghdr
	if addr.IP.To4() != nil {
		c.logger.Debug("IPv4 packet detected, zeroing ifindex in OOB", "addr", addr.String())
		// 先頭4バイトを0にすることでifindexを0にする
		if len(oob) >= cmsghdrLen+4 {
			for i := 0; i < 4; i++ {
				oob[cmsghdrLen+i] = 0
			}
		}
	} else if addr.IP.To16() != nil {
		c.logger.Debug("IPv6 packet detected, zeroing ifindex in OOB", "addr", addr.String())
		// +16バイト目から4バイトを0にすることでifindexを0にする
		if len(oob) >= cmsghdrLen+16+4 {
			for i := 0; i < 4; i++ {
				oob[cmsghdrLen+16+i] = 0
			}
		}
	}
	return c.OOBCapablePacketConn.WriteMsgUDP(b, oob, addr)
}

func (c *ObservedPacketConn) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	n, err := c.bt.ReadBatch(ms, flags)
	for i := 0; i < n; i++ {
		c.logger.Debug("ReadPacket", "from", ms[i].Addr.String(), "len", ms[i].N)
	}
	return n, err
}

func ListenObservedPacketConn(network, address string, logger *slog.Logger) (*ObservedPacketConn, error) {
	pkt, err := net.ListenPacket(network, address)
	if err != nil {
		return nil, err
	}
	oobcap, ok := pkt.(quic.OOBCapablePacketConn)
	if !ok {
		return nil, fmt.Errorf("PacketConn %T does not implement OOBCapablePacketConn", pkt)
	}
	return &ObservedPacketConn{OOBCapablePacketConn: oobcap, bt: ipv4.NewPacketConn(pkt), logger: logger}, nil
}

func (s *Server) setupHTTP3Server(ctx context.Context, mux http.Handler, gr *errgroup.Group, tlsConf *tls.Config, cancel context.CancelFunc) error {
	srv := &http3.Server{
		Addr:    s.HTTP3ListenAddr,
		Handler: mux,
		Logger:  s.logger,
	}
	var pkt net.PacketConn
	var err error
	if s.customPacketListener != nil {
		pkt, err = s.customPacketListener("udp", s.HTTP3ListenAddr, s.logger)
	} else {
		pkt, err = ListenObservedPacketConn("udp", s.HTTP3ListenAddr, s.logger)
	}
	if err != nil {
		s.logger.Error("Failed to listen on UDP for HTTP/3", "address", s.HTTP3ListenAddr, "error", err)
		return err
	}
	tr := &quic.Transport{
		Conn:                  pkt,
		ConnectionIDLength:    17,
		ConnectionIDGenerator: s.sharedSecret,
	}
	var conf *quic.Config
	conf = &quic.Config{}
	if s.qlogEnabled.Load() {
		os.Setenv("QLOGDIR", "./logs/qlogs")    // set qlog dir TODO: make configurable
		err = os.MkdirAll("./logs/qlogs", 0755) // ensure dir exists
		if err != nil {
			s.logger.Error("Failed to create logs directory for qlog", "error", err)
			return err
		}
		conf.Tracer = func(ctx context.Context, isClient bool, connID quic.ConnectionID) qlogwriter.Trace {
			return qlog.DefaultConnectionTracer(ctx, isClient, connID)
		}
	}

	qlis, err := tr.Listen(http3.ConfigureTLSConfig(tlsConf), conf)
	if err != nil {
		s.logger.Error("Failed to start QUIC listener", "error", err)
		return err
	}

	s.logger.Info("Starting HTTP/3 server", "address", s.HTTP3ListenAddr)

	gr.Go(func() error {
		defer cancel()
		defer pkt.Close()
		defer tr.Close()
		defer qlis.Close()
		err := srv.ServeListener(qlis)
		if err != nil {
			if errors.Is(err, http.ErrServerClosed) {
				s.logger.Info("HTTP/3 server closed")
				return nil
			}
			s.logger.Error("Failed to serve QUIC listener", "error", err)
			return err
		}
		return nil
	})

	s.setupShutdown(ctx, srv, gr)
	return nil
}

func (s *Server) setupHTTPSServer(ctx context.Context, mux http.Handler, gr *errgroup.Group, tlsConf *tls.Config, cancel context.CancelFunc) error {
	tlsServ := &http.Server{
		Addr:      s.SecureListenAddr,
		TLSConfig: tlsConf,
		ConnState: s.getConnStateLogger(),
		Handler:   mux,
	}

	lis, err := net.Listen("tcp", s.SecureListenAddr)
	if err != nil {
		s.logger.Error("Failed to listen on TCP", "address", s.SecureListenAddr, "error", err)
		return err
	}

	lis = tls.NewListener(lis, tlsConf)

	err = http2.ConfigureServer(tlsServ, &http2.Server{})
	if err != nil {
		s.logger.Error("Failed to configure HTTP/2 server", "error", err)
		return err
	}

	s.logger.Info("Starting HTTPS server", "address", s.SecureListenAddr)

	gr.Go(func() error {
		defer cancel()
		if err := tlsServ.Serve(lis); err != nil {
			if errors.Is(err, http.ErrServerClosed) {
				s.logger.Info("HTTPS server closed")
				return nil
			}
			s.logger.Error("Failed to start HTTPS server", "error", err)
			return err
		}
		return nil
	})

	s.setupShutdown(ctx, tlsServ, gr)

	return nil
}

func (s *Server) loadKeyPair(certPath, keyPath string) (*tls.Certificate, error) {
	certData, err := s.FileDir.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read certificate file: %w", err)
	}
	keyData, err := s.FileDir.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}
	cert, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		return nil, fmt.Errorf("failed to load x509 key pair: %w", err)
	}
	return &cert, nil
}

// startServer launches the HTTP / HTTPS / HTTP3 listeners bound to a cancellable
// child of ctx, and records the cancel func as the running instance.
func (s *Server) startServer(ctx context.Context) (err error) {
	cancelCtx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()
	s.logger.Info("Starting HTTP server", "address", s.InsecureListenAddr)

	mux := s.setHandlers()

	gr := &errgroup.Group{}
	gr.SetLimit(-1)

	launchWait := func() {
		defer cancel()
		if err := gr.Wait(); err != nil {
			s.logger.Error("Server error", "error", err)
		}
	}

	s.setupHTTPServer(cancelCtx, mux, gr, cancel)
	certPath := s.serverCertFilePath.Load().(string)
	keyPath := s.serverKeyFilePath.Load().(string)

	if certPath == "" || keyPath == "" {
		go launchWait()
		s.runningInstanceCancel = cancel
		return nil
	}

	cert, err := s.loadKeyPair(certPath, keyPath)
	if err != nil {
		s.logger.Error("Failed to load TLS certificate and key", "error", err)
		return err
	}
	s.storedCert.Store(cert)

	tlsConf := &tls.Config{
		VerifyConnection: func(cs tls.ConnectionState) error {
			return nil
		},
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			s.logger.Info("Providing TLS certificate for client",
				"server_name", chi.ServerName, "remote_address", chi.Conn.RemoteAddr(),
				"signature_schemes", chi.SignatureSchemes,
				"cipher_suites", chi.CipherSuites,
				"supported_protos", chi.SupportedProtos,
				"supported_versions", chi.SupportedVersions,
				"supported_curves", chi.SupportedCurves,
				"supported_points", chi.SupportedPoints,
				"extensions", chi.Extensions,
			)
			cert := s.storedCert.Load()
			if cert == nil {
				return nil, fmt.Errorf("no TLS certificate available")
			}
			return cert, nil
		},
	}

	if err := s.setupHTTP3Server(cancelCtx, mux, gr, tlsConf, cancel); err != nil {
		return err
	}
	if err := s.setupHTTPSServer(cancelCtx, mux, gr, tlsConf, cancel); err != nil {
		return err
	}

	go launchWait()
	s.runningInstanceCancel = cancel
	return nil
}

func (s *Server) initPortStateFromListenAddrs() {
	if s.InsecureListenAddr != "" {
		_, portStr, err := net.SplitHostPort(s.InsecureListenAddr)
		if err == nil {
			port, err := strconv.ParseInt(portStr, 10, 16)
			if err == nil {
				s.connStat.SetHttpPort(uint16(port))
			}
		}
	}
	if s.SecureListenAddr != "" {
		_, portStr, err := net.SplitHostPort(s.SecureListenAddr)
		if err == nil {
			port, err := strconv.ParseInt(portStr, 10, 16)
			if err == nil {
				s.connStat.SetHttpsPort(uint16(port))
			}
		}
	}
	if s.HTTP3ListenAddr != "" {
		_, portStr, err := net.SplitHostPort(s.HTTP3ListenAddr)
		if err == nil {
			port, err := strconv.ParseInt(portStr, 10, 16)
			if err == nil {
				s.connStat.SetHttp3Port(uint16(port))
			}
		}
	}
}

func (s *Server) tryReloadTLSCert(fileName string) {
	keyPath := s.serverKeyFilePath.Load().(string)
	certPath := s.serverCertFilePath.Load().(string)
	if fileName == keyPath || fileName == certPath || fileName == "" /*manual reload*/ {
		cert, err := s.loadKeyPair(certPath, keyPath)
		if err != nil {
			s.logger.Error("Failed to load TLS certificate and key after file update", "error", err)
			return
		}
		s.storedCert.Store(cert)
		s.logger.Info("TLS certificate and key reloaded after file update", "file", fileName)
	}
}

// Start initialises port state once, launches the metrics collection goroutine
// once (bound to ctx), and starts the HTTP / HTTPS / HTTP3 listeners. It replaces
// the old framework "init" + "start-dataplane" command dispatch.
func (s *Server) Start(ctx context.Context) error {
	if s.runningInstanceCancel != nil {
		return fmt.Errorf("popcache server: server already running")
	}
	s.launchOnce.Do(func() {
		s.initPortStateFromListenAddrs()
		safe.Go(s.logger, "popcache-metrics", func() { s.sendMetrics(ctx) })
	})
	return s.startServer(ctx)
}

// Stop tears down the running listeners (idempotent). Replaces the old
// "stop-dataplane" command.
func (s *Server) Stop() {
	if s.runningInstanceCancel == nil {
		return
	}
	s.runningInstanceCancel()
	s.runningInstanceCancel = nil
}

// SetCertPath sets the TLS certificate file path (applied on next reload/start).
func (s *Server) SetCertPath(path string) {
	s.serverCertFilePath.Store(path)
	s.logger.Info("Set TLS cert path", "path", path)
}

// SetKeyPath sets the TLS key file path (applied on next reload/start).
func (s *Server) SetKeyPath(path string) {
	s.serverKeyFilePath.Store(path)
	s.logger.Info("Set TLS key path", "path", path)
}

// SetHttpPort updates the plaintext HTTP listen address and metrics port.
func (s *Server) SetHttpPort(port uint16) {
	s.InsecureListenAddr = fmt.Sprintf(":%d", port)
	s.connStat.SetHttpPort(port)
	s.logger.Info("Set HTTP port", "port", port)
}

// SetHttpsPort updates the HTTPS listen address and metrics port.
func (s *Server) SetHttpsPort(port uint16) {
	s.SecureListenAddr = fmt.Sprintf(":%d", port)
	s.connStat.SetHttpsPort(port)
	s.logger.Info("Set HTTPS port", "port", port)
}

// SetHttp3Port updates the HTTP/3 listen address and metrics port.
func (s *Server) SetHttp3Port(port uint16) {
	s.HTTP3ListenAddr = fmt.Sprintf(":%d", port)
	s.connStat.SetHttp3Port(port)
	s.logger.Info("Set HTTP3 port", "port", port)
}

// SetOrigin parses and atomically stores a new origin URL.
func (s *Server) SetOrigin(origin string) error {
	originURL, err := parseOriginURL(origin)
	if err != nil {
		return fmt.Errorf("popcache server: invalid origin URL %q: %w", origin, err)
	}
	s.originURL.Store(originURL)
	s.logger.Info("Set origin URL", "origin_url", origin)
	return nil
}

// SetQlogEnabled toggles HTTP/3 qlog tracing (takes effect on next start).
func (s *Server) SetQlogEnabled(enabled bool) {
	s.qlogEnabled.Store(enabled)
	s.logger.Info("Set qlog enabled", "enabled", enabled)
}

// ReloadTlsCert re-reads the configured cert/key from disk and swaps the live
// certificate atomically.
func (s *Server) ReloadTlsCert() error {
	certPath := s.serverCertFilePath.Load().(string)
	keyPath := s.serverKeyFilePath.Load().(string)
	cert, err := s.loadKeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("popcache server: failed to reload TLS cert: %w", err)
	}
	s.storedCert.Store(cert)
	s.logger.Info("TLS certificate and key reloaded")
	return nil
}

// OnFileDownloaded should be invoked by the owning agent when a watched file
// (e.g. a rotated cert or key) is updated; it reloads the TLS certificate if the
// updated file matches the configured cert/key path. Preserves the framework's
// RegisterOnFileDownloaded auto-reload hook.
func (s *Server) OnFileDownloaded(fileName string) {
	s.tryReloadTLSCert(fileName)
}

// SyncSecret rotates the QUIC-LB shared secret used by the connection-ID
// generator (the HTTP/3 listener's ConnectionIDGenerator).
func (s *Server) SyncSecret(secret []byte) error {
	if err := s.sharedSecret.RotateKey(secret); err != nil {
		return fmt.Errorf("popcache server: failed to rotate shared secret: %w", err)
	}
	s.logger.Info("Synchronized shared secret")
	return nil
}

// SetServerID updates the server ID baked into generated connection IDs.
func (s *Server) SetServerID(id uint32) {
	s.sharedSecret.SetServerID(id)
	s.logger.Info("Set server ID", "server_id", id)
}
