package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aliothmoon/mcplex/pkg/events"
	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
)

const (
	maxHTTPBodyBytes      = 16 * 1024 * 1024 // 16 MB
	defaultSessionIdleTTL = 30 * time.Minute
	maxTrackedSessions    = 1024
)

// HTTPOptions configures the MCP Streamable HTTP transport.
type HTTPOptions struct {
	Addr                 string        // e.g. "127.0.0.1:8787"
	Path                 string        // defaults to "/mcp"
	Token                string        // optional Bearer token required on /mcp, /events, /api/events
	AllowOrigin          string        // optional CORS Access-Control-Allow-Origin value
	AllowInsecureNoToken bool          // explicitly permit non-loopback bind or wildcard CORS without Token
	SessionIdleTTL       time.Duration // idle session TTL before auto-reaping tasks/spills (0 -> 30m, <0 disables)
}

// ValidateHTTPOptions enforces safe-by-default authentication invariants before exposing HTTP endpoints.
func ValidateHTTPOptions(opts HTTPOptions) error {
	addr := strings.TrimSpace(opts.Addr)
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	if strings.TrimSpace(opts.Token) == "" && !opts.AllowInsecureNoToken {
		if strings.TrimSpace(opts.AllowOrigin) == "*" {
			return errors.New(`refusing to serve HTTP with --cors-origin "*" without --token (any webpage could invoke exec); pass --token <secret> or --insecure-no-token`)
		}
		if !IsLoopbackBindAddr(addr) {
			return fmt.Errorf("refusing to bind non-loopback HTTP address %q without --token; bind to 127.0.0.1, pass --token <secret>, or pass --insecure-no-token", addr)
		}
	}
	return nil
}

// IsLoopbackBindAddr reports whether an HTTP listen address (`host:port`) binds strictly to loopback.
func IsLoopbackBindAddr(addr string) bool {
	h := extractHostname(addr)
	return isLoopbackHost(h)
}

type trackedHTTPSession struct {
	lastSeen time.Time
	inFlight int
}

// maxEndedSessionTombstones bounds how many terminated session IDs are remembered for 404 responses.
const maxEndedSessionTombstones = 4096

type httpSessionRegistry struct {
	mu        sync.Mutex
	sessions  map[string]*trackedHTTPSession
	lastSweep time.Time
	// ended remembers sessions terminated by idle reaping or DELETE so later requests carrying that
	// Mcp-Session-Id get HTTP 404 (MCP Streamable HTTP spec) and the client re-initializes, instead of
	// silently continuing in a fresh session that has lost its workspace, spills and background tasks.
	ended      map[string]struct{}
	endedOrder []string
}

func newHTTPSessionRegistry() *httpSessionRegistry {
	return &httpSessionRegistry{
		sessions: map[string]*trackedHTTPSession{},
		ended:    map[string]struct{}{},
	}
}

func (r *httpSessionRegistry) markEndedLocked(sid string) {
	if sid == "" {
		return
	}
	if _, ok := r.ended[sid]; ok {
		return
	}
	if len(r.endedOrder) >= maxEndedSessionTombstones {
		oldest := r.endedOrder[0]
		r.endedOrder = r.endedOrder[1:]
		delete(r.ended, oldest)
	}
	r.ended[sid] = struct{}{}
	r.endedOrder = append(r.endedOrder, sid)
}

// isEnded reports whether sid was terminated by this server (idle reaping or DELETE).
func (r *httpSessionRegistry) isEnded(sid string) bool {
	if sid == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.ended[sid]
	return ok
}

func (r *httpSessionRegistry) register(sid string, now time.Time) {
	if sid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sessions) >= maxTrackedSessions && r.sessions[sid] == nil {
		var oldestID string
		var oldestAt time.Time
		for k, st := range r.sessions {
			if st.inFlight == 0 && (oldestID == "" || st.lastSeen.Before(oldestAt)) {
				oldestID = k
				oldestAt = st.lastSeen
			}
		}
		if oldestID != "" {
			delete(r.sessions, oldestID)
		}
	}
	r.sessions[sid] = &trackedHTTPSession{lastSeen: now}
}

func (r *httpSessionRegistry) begin(sid string, now time.Time) {
	if sid == "" || len(sid) > 128 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[sid]
	if !ok {
		if len(r.sessions) >= maxTrackedSessions {
			return
		}
		st = &trackedHTTPSession{}
		r.sessions[sid] = st
	}
	st.lastSeen = now
	st.inFlight++
}

func (r *httpSessionRegistry) end(sid string, now time.Time) {
	if sid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.sessions[sid]; ok {
		if st.inFlight > 0 {
			st.inFlight--
		}
		st.lastSeen = now
	}
}

func (r *httpSessionRegistry) remove(sid string) {
	if sid == "" {
		return
	}
	r.mu.Lock()
	delete(r.sessions, sid)
	r.markEndedLocked(sid)
	r.mu.Unlock()
}

func (r *httpSessionRegistry) collectExpired(now time.Time, ttl time.Duration, force bool) []string {
	if ttl <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !force && !r.lastSweep.IsZero() && now.Sub(r.lastSweep) < min(ttl/2, 250*time.Millisecond) {
		return nil
	}
	r.lastSweep = now
	var expired []string
	for sid, st := range r.sessions {
		if st.inFlight == 0 && now.Sub(st.lastSeen) > ttl {
			expired = append(expired, sid)
			delete(r.sessions, sid)
			r.markEndedLocked(sid)
		}
	}
	return expired
}

func batchHasInitialize(batch []jsonRPCRequest) bool {
	for _, req := range batch {
		if req.Method == "initialize" {
			return true
		}
	}
	return false
}

// writeSessionEnded answers a request for a terminated session with HTTP 404, which MCP Streamable HTTP
// clients treat as "start a new session with initialize".
func writeSessionEnded(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      json.RawMessage("null"),
		Error:   &rpcError{Code: -32001, Message: "Session not found: it was closed or expired after being idle; send initialize to start a new session"},
	})
}

func writeJSONHTTPError(w http.ResponseWriter, status int, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
}

func redactSessionID(sid string) string {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sid))
	return "s_" + hex.EncodeToString(sum[:4])
}

func filterAndRedactEvents(list []events.Event, filter string) []events.Event {
	f := strings.TrimSpace(filter)
	out := make([]events.Event, 0, len(list))
	for _, ev := range list {
		redacted := redactSessionID(ev.SessionID)
		if f != "" && ev.SessionID != f && redacted != f {
			continue
		}
		ev.SessionID = redacted
		out = append(out, ev)
	}
	return out
}

// HTTPHandler returns an http.Handler implementing MCP Streamable HTTP over POST /mcp
// plus GET /healthz, GET /api/events, and GET /events (SSE).
func (s *Server) HTTPHandler(opts HTTPOptions) http.Handler {
	return s.httpHandlerWithRegistry(opts, newHTTPSessionRegistry())
}

func (s *Server) httpHandlerWithRegistry(opts HTTPOptions, reg *httpSessionRegistry) http.Handler {
	mcpPath := strings.TrimSpace(opts.Path)
	if mcpPath == "" {
		mcpPath = "/mcp"
	}
	if !strings.HasPrefix(mcpPath, "/") {
		mcpPath = "/" + mcpPath
	}
	idleTTL := opts.SessionIdleTTL
	if idleTTL == 0 {
		idleTTL = defaultSessionIdleTTL
	}

	reapIdle := func(force bool) {
		if idleTTL <= 0 {
			return
		}
		for _, expSID := range reg.collectExpired(time.Now(), idleTTL, force) {
			s.cancelSessionInFlight(expSID)
			if s.Engine != nil {
				s.Engine.CloseSession(expSID)
			}
		}
	}

	// The Host allowlist defends loopback binds against DNS rebinding. A non-loopback bind is either protected by
	// the bearer token or was explicitly exposed with --insecure-no-token; in both cases LAN/container clients
	// legitimately send non-loopback Host headers, so the allowlist would only lock them out.
	loopbackBind := strings.TrimSpace(opts.Addr) == "" || IsLoopbackBindAddr(opts.Addr)
	enforceHostGuard := loopbackBind || (strings.TrimSpace(opts.Token) == "" && !opts.AllowInsecureNoToken)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if enforceHostGuard && !isAllowedHostHeader(r.Host, opts.Addr) {
			writeJSONHTTPError(w, http.StatusForbidden, "forbidden host")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"server":  s.serverName(),
			"version": s.serverVersion(),
		})
	})

	checkAuthAndOrigin := func(w http.ResponseWriter, r *http.Request) bool {
		if enforceHostGuard && !isAllowedHostHeader(r.Host, opts.Addr) {
			writeJSONHTTPError(w, http.StatusForbidden, "forbidden host")
			return false
		}
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" && !isAllowedOrigin(origin, r.Host, opts.AllowOrigin) {
			writeJSONHTTPError(w, http.StatusForbidden, "forbidden origin")
			return false
		}
		if opts.AllowOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", opts.AllowOrigin)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id, Last-Event-ID, X-MCPlex-Instance")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, X-MCPlex-Instance")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return false
		}
		if opts.Token != "" && !validBearerToken(r.Header.Get("Authorization"), opts.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcplex"`)
			writeJSONHTTPError(w, http.StatusUnauthorized, "unauthorized")
			return false
		}
		return true
	}

	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuthAndOrigin(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET, OPTIONS")
			writeJSONHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if s.Engine == nil || s.Engine.EventHub() == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"events":[]}`))
			return
		}
		hub := s.Engine.EventHub()
		w.Header().Set("X-MCPlex-Instance", hub.InstanceID())
		sessionFilter := strings.TrimSpace(r.URL.Query().Get("session"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		afterSeq, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		reqInst := strings.TrimSpace(r.Header.Get("X-MCPlex-Instance"))
		if (reqInst != "" && reqInst != hub.InstanceID()) || afterSeq > hub.CurrentSeq() {
			afterSeq = 0
		}
		hubFilter := sessionFilter
		if strings.HasPrefix(sessionFilter, "s_") {
			hubFilter = ""
		}
		list := filterAndRedactEvents(hub.Recent(hubFilter, limit, afterSeq), sessionFilter)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"instanceId": hub.InstanceID(),
			"events":     list,
		})
	})

	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuthAndOrigin(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET, OPTIONS")
			writeJSONHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok || s.Engine == nil || s.Engine.EventHub() == nil {
			writeJSONHTTPError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		hub := s.Engine.EventHub()

		sessionFilter := strings.TrimSpace(r.URL.Query().Get("session"))
		replay := 200
		if raw := strings.TrimSpace(r.URL.Query().Get("replay")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
				replay = n
			}
		}
		var afterSeq int64
		if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
			afterSeq, _ = strconv.ParseInt(raw, 10, 64)
		} else if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
			afterSeq, _ = strconv.ParseInt(raw, 10, 64)
		}
		reqInst := strings.TrimSpace(r.Header.Get("X-MCPlex-Instance"))
		if (reqInst != "" && reqInst != hub.InstanceID()) || afterSeq > hub.CurrentSeq() {
			afterSeq = 0
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("X-MCPlex-Instance", hub.InstanceID())

		hubFilter := sessionFilter
		if strings.HasPrefix(sessionFilter, "s_") {
			hubFilter = ""
		}
		ch, history, cancel := hub.Subscribe(hubFilter, replay, afterSeq)
		defer cancel()

		writeEvent := func(ev events.Event) bool {
			redacted := redactSessionID(ev.SessionID)
			if sessionFilter != "" && ev.SessionID != sessionFilter && redacted != sessionFilter {
				return true
			}
			ev.SessionID = redacted
			raw, err := json.Marshal(ev)
			if err != nil {
				return true
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: mcplex\ndata: %s\n\n", ev.Seq, raw); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}

		for _, ev := range history {
			if !writeEvent(ev) {
				return
			}
		}
		_, _ = fmt.Fprintf(w, ": connected instance=%s\n\n", hub.InstanceID())
		flusher.Flush()

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev, open := <-ch:
				if !open {
					return
				}
				if !writeEvent(ev) {
					return
				}
			case <-ticker.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, r *http.Request) {
		reapIdle(false)
		if enforceHostGuard && !isAllowedHostHeader(r.Host, opts.Addr) {
			writeJSONHTTPError(w, http.StatusForbidden, "forbidden host")
			return
		}
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" && !isAllowedOrigin(origin, r.Host, opts.AllowOrigin) {
			writeJSONHTTPError(w, http.StatusForbidden, "forbidden origin")
			return
		}
		if opts.AllowOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", opts.AllowOrigin)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id, MCP-Protocol-Version")
			w.Header().Set("Access-Control-Allow-Methods", "POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if opts.Token != "" && !validBearerToken(r.Header.Get("Authorization"), opts.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcplex"`)
			writeJSONHTTPError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if r.Method == http.MethodDelete {
			if sid := strings.TrimSpace(r.Header.Get("Mcp-Session-Id")); sid != "" {
				reg.remove(sid)
				s.cancelSessionInFlight(sid)
				if s.Engine != nil {
					s.Engine.CloseSession(sid)
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST, DELETE, OPTIONS")
			writeJSONHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !isJSONContentType(r.Header.Get("Content-Type")) {
			writeJSONHTTPError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHTTPBodyBytes))
		if err != nil {
			writeJSONHTTPError(w, http.StatusBadRequest, "failed reading request body")
			return
		}
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &rpcError{Code: -32700, Message: "Parse error: empty body"},
			})
			return
		}

		if trimmed[0] == '[' {
			var batch []jsonRPCRequest
			if err := json.Unmarshal(trimmed, &batch); err != nil || len(batch) == 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(jsonRPCResponse{
					JSONRPC: "2.0",
					ID:      json.RawMessage("null"),
					Error:   &rpcError{Code: -32600, Message: "Invalid Request"},
				})
				return
			}
			var replies []jsonRPCResponse
			sid := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
			if reg.isEnded(sid) && !batchHasInitialize(batch) {
				writeSessionEnded(w)
				return
			}
			for _, req := range batch {
				if req.Method == "initialize" {
					sid = newSessionID()
					reg.register(sid, time.Now())
					w.Header().Set("Mcp-Session-Id", sid)
				}
				if sid != "" {
					reg.begin(sid, time.Now())
				}
				reqCtx := ptcruntime.WithSessionID(r.Context(), sid)
				resp := s.handleSingle(reqCtx, req)
				if sid != "" {
					reg.end(sid, time.Now())
				}
				if resp != nil {
					replies = append(replies, *resp)
				}
			}
			if len(replies) == 0 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(replies)
			return
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(trimmed, &req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &rpcError{Code: -32700, Message: "Parse error"},
			})
			return
		}
		sid := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
		if req.Method != "initialize" && reg.isEnded(sid) {
			writeSessionEnded(w)
			return
		}
		if req.Method == "initialize" {
			sid = newSessionID()
			reg.register(sid, time.Now())
			w.Header().Set("Mcp-Session-Id", sid)
		}
		if sid != "" {
			reg.begin(sid, time.Now())
			// Evaluate time.Now() when the request FINISHES: `defer reg.end(sid, time.Now())` would record the
			// start time, so a long call (task wait, slow exec) could be reaped right after completing.
			defer func() { reg.end(sid, time.Now()) }()
		}
		resp := s.handleSingle(ptcruntime.WithSessionID(r.Context(), sid), req)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(*resp)
	})

	return mux
}

// ServeHTTP runs the MCP Streamable HTTP server until ctx is canceled.
func (s *Server) ServeHTTP(ctx context.Context, opts HTTPOptions) error {
	return s.ListenAndServeHTTP(ctx, opts, nil)
}

// ListenAndServeHTTP validates security options, binds the TCP listener first, invokes onBound
// with the actual listening address, runs background idle-session reaping, and serves HTTP until ctx is canceled.
func (s *Server) ListenAndServeHTTP(ctx context.Context, opts HTTPOptions, onBound func(addr string)) error {
	if err := ValidateHTTPOptions(opts); err != nil {
		return err
	}
	addr := strings.TrimSpace(opts.Addr)
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if onBound != nil {
		onBound(ln.Addr().String())
	}

	reg := newHTTPSessionRegistry()
	idleTTL := opts.SessionIdleTTL
	if idleTTL == 0 {
		idleTTL = defaultSessionIdleTTL
	}
	if idleTTL > 0 {
		sweepEvery := min(idleTTL/2, 1*time.Minute)
		if sweepEvery < 50*time.Millisecond {
			sweepEvery = 50 * time.Millisecond
		}
		go func() {
			t := time.NewTicker(sweepEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					for _, expSID := range reg.collectExpired(time.Now(), idleTTL, true) {
						s.cancelSessionInFlight(expSID)
						if s.Engine != nil {
							s.Engine.CloseSession(expSID)
						}
					}
				}
			}
		}()
	}

	srv := &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           s.httpHandlerWithRegistry(opts, reg),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			_ = srv.Close()
		}
		return nil
	case err := <-errCh:
		return err
	}
}

func isJSONContentType(header string) bool {
	if strings.TrimSpace(header) == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "application/json")
}

func extractHostname(hostPort string) string {
	hp := strings.TrimSpace(hostPort)
	if hp == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(hp); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(hp, "[]")
}

func isAllowedHostHeader(reqHost, configuredAddr string) bool {
	h := extractHostname(reqHost)
	if h == "" {
		return false
	}
	if isLoopbackHost(h) {
		return true
	}
	cfgHost := extractHostname(configuredAddr)
	if cfgHost != "" && cfgHost != "0.0.0.0" && cfgHost != "::" && strings.EqualFold(h, cfgHost) {
		return true
	}
	return false
}

func isAllowedOrigin(origin, reqHost, allowOrigin string) bool {
	ao := strings.TrimSpace(allowOrigin)
	if ao == "*" {
		return true
	}
	if ao != "" && strings.EqualFold(origin, ao) {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, reqHost) && isLoopbackHost(u.Hostname()) {
		return true
	}
	return false
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func validBearerToken(header, expected string) bool {
	if expected == "" {
		return true
	}
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(header[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("sess_%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
