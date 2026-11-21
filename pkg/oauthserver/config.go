// Package oauthserver implements a minimal, standards-based OAuth 2.1 authorization server plus resource-server
// token verification so MCPlex can be exposed to OAuth-only MCP clients (e.g. Gemini custom apps) through a
// public HTTPS tunnel, following the MCP Authorization specification (2025-11-25):
//
//   - RFC 9728 Protected Resource Metadata (`/.well-known/oauth-protected-resource[/<mcp-path>]`)
//   - RFC 8414 Authorization Server Metadata (`/.well-known/oauth-authorization-server`)
//   - RFC 7591 Dynamic Client Registration (`/oauth/register`) with a redirect-URI allowlist
//   - Authorization code + mandatory PKCE S256 (`/oauth/authorize`), approved by the owner secret
//   - RFC 8707 resource-bound, opaque, hashed-at-rest access/refresh tokens (`/oauth/token`)
//   - RFC 7009 revocation (`/oauth/revoke`), RFC 9207 `iss` in authorization responses
//
// Refresh tokens rotate for public clients (OAuth 2.1 §4.3.1) and stay stable with a sliding expiry for
// confidential clients (Gemini "persistent linking" requirement).
package oauthserver

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ScopeMCP is the single scope granted to OAuth clients: full access to the MCP endpoint.
	ScopeMCP = "mcp"

	accessTokenTTL  = time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour
	authCodeTTL     = 60 * time.Second
	formTokenTTL    = 10 * time.Minute
	maxClients      = 200
	maxBodyBytes    = 64 * 1024
)

// GoogleRedirectPatterns are the redirect URIs Gemini / Google Connected Apps use for OAuth account linking.
var GoogleRedirectPatterns = []string{
	"https://oauth-redirect.googleusercontent.com/r/*",
	"https://oauth-redirect-sandbox.googleusercontent.com/r/*",
	"https://oauth-redirect-test.googleusercontent.com/r/*",
	"https://oauth-redirect.googleusercontent.com/a/*",
	"https://oauth-redirect-sandbox.googleusercontent.com/a/*",
	"https://oauth-redirect-test.googleusercontent.com/a/*",
}

// Config configures the OAuth server.
type Config struct {
	// PublicURL is the externally reachable HTTPS origin of this MCPlex instance (e.g. https://mcp.example.com).
	// It becomes the OAuth issuer; the protected resource is PublicURL + MCPPath.
	PublicURL string
	// MCPPath is the MCP Streamable HTTP endpoint path (default "/mcp").
	MCPPath string
	// OwnerSecret is the secret the owner types on the consent page to approve a client (MCPlex reuses --token).
	OwnerSecret string
	// DataDir stores persisted clients and grants under DataDir/oauth-server.
	DataDir string
	// RedirectAllow lists extra allowed redirect URI patterns. A trailing "*" is a prefix wildcard; the special
	// value "loopback" allows http://127.0.0.1|localhost|[::1] on any port (native clients such as Claude Code).
	// Google's redirect URIs are always allowed.
	RedirectAllow []string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// StateDir returns the directory holding persisted OAuth server state for a data dir.
func StateDir(dataDir string) string {
	return filepath.Join(dataDir, "oauth-server")
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// normalizePublicURL validates and canonicalizes the public origin.
func normalizePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("--public-url is required for OAuth (e.g. https://mcp.example.com)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid public URL %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if !(scheme == "http" && (strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()))) {
			return "", fmt.Errorf("public URL %q must use https (plain http is only allowed for localhost testing)", raw)
		}
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		return "", fmt.Errorf("public URL %q must be an origin without a path (the MCP endpoint path comes from --http-path)", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("public URL %q must not contain credentials, query or fragment", raw)
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

func normalizeMCPPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		p = "/mcp"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
