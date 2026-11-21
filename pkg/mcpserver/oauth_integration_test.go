package mcpserver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/aliothmoon/mcplex/pkg/oauthserver"
	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
)

const (
	oauthPublicHost  = "mcp.example.com"
	oauthOwnerSecret = "owner-secret-for-tests-123456"
	oauthGoogleRedir = "https://oauth-redirect.googleusercontent.com/r/test-project"
	oauthVerifier    = "M25iVXpKU3puUjFaYWg3T1NDTDQtcW1ROUY5YXlwalNoc0hhakxifmZHag"
)

var oauthHiddenRe = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// tunnelClient sends every request with the public tunnel Host header, like cloudflared forwarding to a loopback bind.
type tunnelClient struct {
	t    *testing.T
	base string
	host string
}

func (c tunnelClient) do(method, path string, body io.Reader, hdr map[string]string) *http.Response {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, body)
	req.Host = c.host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp
}

func decodeJSONBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return m
}

func TestGeminiStyleOAuthFlowThroughTunnelHost(t *testing.T) {
	dataDir := t.TempDir()
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{DataDir: dataDir, DefaultCwd: t.TempDir()})
	defer eng.Close()
	oa, err := oauthserver.New(oauthserver.Config{PublicURL: "https://" + oauthPublicHost, MCPPath: "/mcp", OwnerSecret: oauthOwnerSecret, DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithOptions(eng, ServerOptions{ServerName: "mcplex", Tools: ToolSet{Read: true, Tasks: true, Docs: true}})
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{Addr: "127.0.0.1:8787", Token: oauthOwnerSecret, OAuth: oa}))
	defer ts.Close()
	c := tunnelClient{t: t, base: ts.URL, host: oauthPublicHost}
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`

	// 1. Unauthenticated MCP call -> 401 with RFC 9728 discovery hint.
	resp := c.do(http.MethodPost, "/mcp", strings.NewReader(initBody), jsonHdr)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("unauthenticated /mcp: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	// 2. Discovery through the tunnel host works; unknown hosts are still rejected (DNS rebinding guard).
	prm := decodeJSONBody(t, c.do(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil, nil))
	if prm["resource"] != "https://mcp.example.com/mcp" {
		t.Fatalf("PRM: %v", prm)
	}
	asResp := c.do(http.MethodGet, "/.well-known/oauth-authorization-server", nil, nil)
	if asResp.StatusCode != http.StatusOK {
		t.Fatalf("AS metadata status %d", asResp.StatusCode)
	}
	as := decodeJSONBody(t, asResp)
	if as["registration_endpoint"] != "https://mcp.example.com/oauth/register" {
		t.Fatalf("AS metadata: %v", as)
	}
	evil := tunnelClient{t: t, base: ts.URL, host: "evil.example"}
	if r := evil.do(http.MethodGet, "/.well-known/oauth-authorization-server", nil, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("evil host metadata: %d", r.StatusCode)
	}

	// 3. Dynamic client registration (Gemini omits the auth method -> client_secret_basic).
	reg := decodeJSONBody(t, c.do(http.MethodPost, "/oauth/register", strings.NewReader(`{"redirect_uris":["`+oauthGoogleRedir+`"],"client_name":"Gemini"}`), jsonHdr))
	clientID, _ := reg["client_id"].(string)
	clientSecret, _ := reg["client_secret"].(string)
	if clientID == "" || clientSecret == "" {
		t.Fatalf("registration: %v", reg)
	}

	// 4. Consent page + owner approval.
	sum := sha256.Sum256([]byte(oauthVerifier))
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {oauthGoogleRedir}, "state": {"gem-state"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"scope": {"openid profile email"}, "resource": {"https://mcp.example.com/mcp"},
	}
	page := c.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil, nil)
	pageBody, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(pageBody), "Gemini") {
		t.Fatalf("consent page: %d", page.StatusCode)
	}
	form := url.Values{}
	for _, m := range oauthHiddenRe.FindAllStringSubmatch(string(pageBody), -1) {
		form.Set(m[1], html.UnescapeString(m[2]))
	}
	form.Set("owner_secret", oauthOwnerSecret)
	form.Set("action", "approve")
	approve := c.do(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       "https://" + oauthPublicHost,
	})
	approve.Body.Close()
	loc, _ := url.Parse(approve.Header.Get("Location"))
	if approve.StatusCode != http.StatusFound || loc == nil || loc.Query().Get("code") == "" || loc.Query().Get("state") != "gem-state" {
		t.Fatalf("approve: %d %s", approve.StatusCode, approve.Header.Get("Location"))
	}

	// 5. Code exchange (server-to-server, HTTP Basic client auth).
	tokForm := url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "redirect_uri": {oauthGoogleRedir}, "code_verifier": {oauthVerifier}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/oauth/token", strings.NewReader(tokForm.Encode()))
	req.Host = oauthPublicHost
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)
	tokResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	tok := decodeJSONBody(t, tokResp)
	access, _ := tok["access_token"].(string)
	if tokResp.StatusCode != http.StatusOK || access == "" {
		t.Fatalf("token: %d %v", tokResp.StatusCode, tok)
	}

	// 6. MCP calls with the OAuth access token succeed and tools carry annotations.
	authHdr := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + access}
	initResp := c.do(http.MethodPost, "/mcp", strings.NewReader(initBody), authHdr)
	sid := initResp.Header.Get("Mcp-Session-Id")
	initResp.Body.Close()
	if initResp.StatusCode != http.StatusOK || sid == "" {
		t.Fatalf("initialize with OAuth token: %d", initResp.StatusCode)
	}
	authHdr["Mcp-Session-Id"] = sid
	list := decodeJSONBody(t, c.do(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`), authHdr))
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 5 {
		t.Fatalf("tools/list: %v", list)
	}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		ann, ok := tool["annotations"].(map[string]any)
		if !ok {
			t.Fatalf("tool %v lacks annotations", tool["name"])
		}
		ro, _ := ann["readOnlyHint"].(bool)
		de, _ := ann["destructiveHint"].(bool)
		switch tool["name"] {
		case "read", "docs":
			if !ro || de {
				t.Fatalf("%v should be read-only: %v", tool["name"], ann)
			}
		default:
			if ro || !de {
				t.Fatalf("%v should be destructive: %v", tool["name"], ann)
			}
		}
	}

	// 7. Static owner token still works; a bogus bearer gets invalid_token.
	if r := c.do(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"ping"}`), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + oauthOwnerSecret}); r.StatusCode != http.StatusOK {
		t.Fatalf("static token: %d", r.StatusCode)
	}
	bad := c.do(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"ping"}`), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer mcplex_at_bogus"})
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized || !strings.Contains(bad.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("bogus bearer: %d %q", bad.StatusCode, bad.Header.Get("WWW-Authenticate"))
	}
	// OAuth tokens do not open the event stream (owner token only).
	if r := c.do(http.MethodGet, "/api/events", nil, map[string]string{"Authorization": "Bearer " + access}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("OAuth token must not read /api/events: %d", r.StatusCode)
	}
}

// --public-url without --oauth only adds the tunnel hostname to the Host allowlist (bearer-token clients).
func TestAllowedHostsAdmitsTunnelHostname(t *testing.T) {
	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{DefaultCwd: t.TempDir()})
	defer eng.Close()
	srv := NewServerWithOptions(eng, ServerOptions{ServerName: "mcplex"})
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{Addr: "127.0.0.1:8787", Token: "tok", AllowedHosts: []string{"random-words.trycloudflare.com"}}))
	defer ts.Close()
	hdr := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer tok"}
	ok := tunnelClient{t: t, base: ts.URL, host: "random-words.trycloudflare.com"}
	if r := ok.do(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), hdr); r.StatusCode != http.StatusOK {
		t.Fatalf("allowed tunnel host: %d", r.StatusCode)
	}
	other := tunnelClient{t: t, base: ts.URL, host: "other.trycloudflare.com"}
	if r := other.do(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), hdr); r.StatusCode != http.StatusForbidden {
		t.Fatalf("other host: %d", r.StatusCode)
	}
}
