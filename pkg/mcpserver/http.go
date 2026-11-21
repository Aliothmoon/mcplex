package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"strings"
	"time"

	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
)

const maxHTTPBodyBytes = 16 * 1024 * 1024 // 16 MB

// HTTPOptions configures the MCP Streamable HTTP transport.
type HTTPOptions struct {
	Addr        string // e.g. "127.0.0.1:8787"
	Path        string // defaults to "/mcp"
	Token       string // optional Bearer token required on /mcp
	AllowOrigin string // optional CORS Access-Control-Allow-Origin value
}

// HTTPHandler returns an http.Handler implementing MCP Streamable HTTP over POST /mcp
// plus a lightweight GET /healthz probe.
func (s *Server) HTTPHandler(opts HTTPOptions) http.Handler {
	mcpPath := strings.TrimSpace(opts.Path)
	if mcpPath == "" {
		mcpPath = "/mcp"
	}
	if !strings.HasPrefix(mcpPath, "/") {
		mcpPath = "/" + mcpPath
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"server":  s.serverName(),
			"version": s.serverVersion(),
		})
	})

	mux.HandleFunc(mcpPath, func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" && !isAllowedOrigin(origin, r.Host, opts.AllowOrigin) {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"forbidden origin"}`, http.StatusForbidden)
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
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			if sid := strings.TrimSpace(r.Header.Get("Mcp-Session-Id")); sid != "" {
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !isJSONContentType(r.Header.Get("Content-Type")) {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"Content-Type must be application/json"}`, http.StatusUnsupportedMediaType)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHTTPBodyBytes))
		if err != nil {
			http.Error(w, `{"error":"failed reading request body"}`, http.StatusBadRequest)
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
			for _, req := range batch {
				if req.Method == "initialize" && sid == "" {
					sid = newSessionID()
					w.Header().Set("Mcp-Session-Id", sid)
				}
				reqCtx := ptcruntime.WithSessionID(r.Context(), sid)
				if resp := s.handleSingle(reqCtx, req); resp != nil {
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
		if req.Method == "initialize" {
			if sid == "" {
				sid = newSessionID()
			}
			w.Header().Set("Mcp-Session-Id", sid)
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
	addr := strings.TrimSpace(opts.Addr)
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.HTTPHandler(opts),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
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

// isAllowedOrigin enforces MCP Streamable HTTP Origin checks to prevent DNS-rebinding & browser CSRF attacks.
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
	// Same-origin loopback requests (e.g. local inspector UI on the exact same host:port) are permitted.
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
		return fmt.Sprintf("mcplex-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
