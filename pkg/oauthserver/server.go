package oauthserver

import (
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server is the OAuth authorization server + resource-server verifier for one public MCPlex endpoint.
type Server struct {
	cfg      Config
	issuer   string // public origin, e.g. https://mcp.example.com
	resource string // issuer + MCP path, e.g. https://mcp.example.com/mcp
	mcpPath  string
	rules    []redirectRule
	store    *store
	formKey  []byte

	codesMu sync.Mutex
	codes   map[string]*authCode // key: sha256(code)

	loginLimiter    *failureLimiter
	registerLimiter *rateCounter
}

type authCode struct {
	ClientID         string
	RedirectURI      string
	RedirectExplicit bool
	Challenge        string
	Scope            string
	Resource         string
	Expires          time.Time
	Used             bool
	GrantID          string
}

// New builds an OAuth server from cfg, loading persisted clients and grants.
func New(cfg Config) (*Server, error) {
	issuer, err := normalizePublicURL(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(cfg.OwnerSecret)) == 0 {
		return nil, errOwnerSecretRequired
	}
	rules, err := parseRedirectRules(cfg.RedirectAllow)
	if err != nil {
		return nil, err
	}
	st, err := openStore(cfg.DataDir, true)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	mcpPath := normalizeMCPPath(cfg.MCPPath)
	return &Server{
		cfg:             cfg,
		issuer:          issuer,
		resource:        issuer + mcpPath,
		mcpPath:         mcpPath,
		rules:           rules,
		store:           st,
		formKey:         key,
		codes:           map[string]*authCode{},
		loginLimiter:    newFailureLimiter(),
		registerLimiter: newRateCounter(30, time.Hour),
	}, nil
}

// Issuer returns the OAuth issuer (public origin).
func (s *Server) Issuer() string { return s.issuer }

// Resource returns the canonical protected resource URI (public origin + MCP path).
func (s *Server) Resource() string { return s.resource }

// PublicHost returns the host[:port] of the public URL (for Host-header allowlisting).
func (s *Server) PublicHost() string {
	return strings.TrimPrefix(strings.TrimPrefix(s.issuer, "https://"), "http://")
}

// ResourceMetadataURL is advertised in WWW-Authenticate on 401 responses from the MCP endpoint.
func (s *Server) ResourceMetadataURL() string {
	return s.issuer + "/.well-known/oauth-protected-resource" + s.mcpPath
}

// WWWAuthenticate returns the RFC 6750 / RFC 9728 challenge for an unauthenticated or invalid-token request.
func (s *Server) WWWAuthenticate(invalidToken bool) string {
	v := `Bearer resource_metadata="` + s.ResourceMetadataURL() + `", scope="` + ScopeMCP + `"`
	if invalidToken {
		v += `, error="invalid_token", error_description="The access token is missing, expired or revoked"`
	}
	return v
}

// Routes returns the OAuth/metadata endpoints keyed by exact path.
func (s *Server) Routes() map[string]http.HandlerFunc {
	prm := s.handleProtectedResourceMetadata
	return map[string]http.HandlerFunc{
		"/.well-known/oauth-protected-resource":             prm,
		"/.well-known/oauth-protected-resource" + s.mcpPath: prm,
		"/.well-known/oauth-authorization-server":           s.handleAuthorizationServerMetadata,
		"/oauth/register":  s.handleRegister,
		"/oauth/authorize": s.handleAuthorize,
		"/oauth/token":     s.handleToken,
		"/oauth/revoke":    s.handleRevoke,
	}
}

func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.resource,
		"authorization_servers":    []string{s.issuer},
		"scopes_supported":         []string{ScopeMCP},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "MCPlex",
	})
}

func (s *Server) handleAuthorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	authMethods := []string{"client_secret_basic", "client_secret_post", "none"}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         s.issuer,
		"authorization_endpoint":                         s.issuer + "/oauth/authorize",
		"token_endpoint":                                 s.issuer + "/oauth/token",
		"registration_endpoint":                          s.issuer + "/oauth/register",
		"revocation_endpoint":                            s.issuer + "/oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          authMethods,
		"revocation_endpoint_auth_methods_supported":     authMethods,
		"scopes_supported":                               []string{ScopeMCP},
		"authorization_response_iss_parameter_supported": true,
	})
}

type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name"`
	Scope                   string   `json:"scope"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	now := s.cfg.now()
	if !s.registerLimiter.allow(requestKey(r), now) {
		writeOAuthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many client registrations; retry later")
		return
	}
	var req registrationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "request body must be a JSON client metadata object")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !s.redirectAllowed(u) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri is not allowed by this server: "+u)
			return
		}
	}
	method := strings.TrimSpace(req.TokenEndpointAuthMethod)
	if method == "" {
		method = "client_secret_basic" // RFC 7591 §2 default
	}
	switch method {
	case "none", "client_secret_basic", "client_secret_post":
	default:
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method: "+method)
		return
	}
	grantTypes := req.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code"}
	}
	for _, g := range grantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant_type: "+g)
			return
		}
	}
	responseTypes := req.ResponseTypes
	if len(responseTypes) == 0 {
		responseTypes = []string{"code"}
	}
	for _, rt := range responseTypes {
		if rt != "code" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported response_type: "+rt)
			return
		}
	}

	client := &clientRecord{
		ID:           randomToken("mcplex_ci_", 16),
		Name:         sanitizeDisplay(req.ClientName, 120),
		RedirectURIs: append([]string(nil), req.RedirectURIs...),
		AuthMethod:   method,
		CreatedAt:    now,
		LastUsedAt:   now,
	}
	var secret string
	if method != "none" {
		secret = randomToken("mcplex_cs_", 32)
		client.SecretHash = hashSecret(secret)
	}
	full := false
	err := s.store.update(now, func(st *persistedState) (bool, error) {
		if !evictClientsLocked(st, maxClients) {
			full = true
			return false, nil
		}
		st.Clients[client.ID] = client
		return true, nil
	})
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to persist client")
		return
	}
	if full {
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "client limit reached; revoke unused clients with `mcplex oauth revoke`")
		return
	}
	resp := map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        now.Unix(),
		"redirect_uris":              client.RedirectURIs,
		"token_endpoint_auth_method": method,
		"grant_types":                grantTypes,
		"response_types":             responseTypes,
		"scope":                      ScopeMCP,
	}
	if client.Name != "" {
		resp["client_name"] = client.Name
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, resp)
}

// requestKey identifies the caller for rate limiting. Behind cloudflared every request arrives from loopback,
// so the Cloudflare-provided client IP is trusted only when the TCP peer is local.
func requestKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" && net.ParseIP(cf) != nil {
			return cf
		}
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	writeJSON(w, status, body)
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
}

// sanitizeDisplay strips control characters and bounds length for values echoed on the consent page / CLI.
func sanitizeDisplay(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return b.String()
}
