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
	"net/http"
	"strings"
	"time"
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
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST, DELETE, OPTIONS")
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
			for _, req := range batch {
				if req.Method == "initialize" && r.Header.Get("Mcp-Session-Id") == "" {
					w.Header().Set("Mcp-Session-Id", newSessionID())
				}
				if resp := s.handleSingle(r.Context(), req); resp != nil {
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
		if req.Method == "initialize" {
			sid := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
			if sid == "" {
				sid = newSessionID()
			}
			w.Header().Set("Mcp-Session-Id", sid)
		}
		resp := s.handleSingle(r.Context(), req)
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
