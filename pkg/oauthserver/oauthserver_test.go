package oauthserver

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
	"sync"
	"testing"
	"time"
)

const (
	testSecret   = "owner-secret-0123456789"
	googleRedir  = "https://oauth-redirect.googleusercontent.com/r/my-gcp-project"
	testVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk_extra_entropy"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	t     *testing.T
	srv   *Server
	ts    *httptest.Server
	clock *testClock
	data  string
}

func newHarness(t *testing.T, extraRedirects ...string) *harness {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	data := t.TempDir()
	srv, err := New(Config{
		PublicURL:     "https://mcp.example.com",
		MCPPath:       "/mcp",
		OwnerSecret:   testSecret,
		DataDir:       data,
		RedirectAllow: extraRedirects,
		Now:           clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for p, h := range srv.Routes() {
		mux.HandleFunc(p, h)
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &harness{t: t, srv: srv, ts: ts, clock: clock, data: data}
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (h *harness) getJSON(path string) (int, map[string]any) {
	h.t.Helper()
	resp, err := http.Get(h.ts.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func (h *harness) register(body map[string]any) (int, map[string]any) {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(h.ts.URL+"/oauth/register", "application/json", strings.NewReader(string(raw)))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func challengeFor(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizeParams(clientID, redirect, state string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"state":                 {state},
		"code_challenge":        {challengeFor(testVerifier)},
		"code_challenge_method": {"S256"},
		"scope":                 {"openid profile email"},
		"resource":              {"https://mcp.example.com/mcp"},
	}
}

var hiddenRe = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// consent loads the consent page and returns its hidden form fields.
func (h *harness) consent(params url.Values) url.Values {
	h.t.Helper()
	resp, err := noRedirect.Get(h.ts.URL + "/oauth/authorize?" + params.Encode())
	if err != nil {
		h.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("consent page status %d: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		h.t.Fatal("consent page must not be frameable")
	}
	form := url.Values{}
	for _, m := range hiddenRe.FindAllStringSubmatch(string(body), -1) {
		form.Set(m[1], html.UnescapeString(m[2]))
	}
	if form.Get("form_token") == "" {
		h.t.Fatalf("no form_token in consent page: %s", body)
	}
	return form
}

func (h *harness) decide(form url.Values, secret, action string) *http.Response {
	h.t.Helper()
	f := url.Values{}
	for k, v := range form {
		f[k] = v
	}
	f.Set("owner_secret", secret)
	f.Set("action", action)
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/oauth/authorize", strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirect.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) token(form url.Values, basicID, basicSecret string) (int, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicID != "" {
		req.SetBasicAuth(url.QueryEscape(basicID), url.QueryEscape(basicSecret))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		h.t.Fatalf("token responses must be no-store, got %q", cc)
	}
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// linkConfidential runs the full Gemini-style flow (DCR without auth method -> client_secret_basic) and returns
// client credentials and the first token response.
func (h *harness) linkConfidential() (clientID, clientSecret string, tok map[string]any) {
	h.t.Helper()
	status, reg := h.register(map[string]any{"redirect_uris": []string{googleRedir}, "client_name": "Gemini"})
	if status != http.StatusCreated {
		h.t.Fatalf("register: %d %v", status, reg)
	}
	clientID, _ = reg["client_id"].(string)
	clientSecret, _ = reg["client_secret"].(string)
	if clientID == "" || clientSecret == "" || reg["token_endpoint_auth_method"] != "client_secret_basic" {
		h.t.Fatalf("unexpected registration response: %v", reg)
	}
	form := h.consent(authorizeParams(clientID, googleRedir, "st-123"))
	resp := h.decide(form, testSecret, "approve")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		h.t.Fatalf("approve status %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), googleRedir+"?") || loc.Query().Get("state") != "st-123" || loc.Query().Get("iss") != "https://mcp.example.com" {
		h.t.Fatalf("bad redirect: %s", loc)
	}
	code := loc.Query().Get("code")
	status, tok = h.token(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {googleRedir},
		"code_verifier": {testVerifier},
		"resource":      {"https://mcp.example.com/mcp"},
	}, clientID, clientSecret)
	if status != http.StatusOK {
		h.t.Fatalf("code exchange: %d %v", status, tok)
	}
	tok["_code"] = code
	return clientID, clientSecret, tok
}

func TestMetadataMatchesGeminiRequirements(t *testing.T) {
	h := newHarness(t)
	status, as := h.getJSON("/.well-known/oauth-authorization-server")
	if status != http.StatusOK {
		t.Fatalf("AS metadata status %d", status)
	}
	for _, k := range []string{"issuer", "authorization_endpoint", "token_endpoint", "registration_endpoint", "revocation_endpoint"} {
		if s, _ := as[k].(string); !strings.HasPrefix(s, "https://mcp.example.com") {
			t.Fatalf("%s = %v", k, as[k])
		}
	}
	if as["issuer"] != "https://mcp.example.com" {
		t.Fatalf("issuer = %v", as["issuer"])
	}
	has := func(key, want string) bool {
		list, _ := as[key].([]any)
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}
	if !has("response_types_supported", "code") || !has("code_challenge_methods_supported", "S256") || !has("grant_types_supported", "refresh_token") {
		t.Fatalf("missing required metadata: %v", as)
	}
	if has("code_challenge_methods_supported", "plain") {
		t.Fatal("plain PKCE must not be advertised")
	}
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		status, prm := h.getJSON(p)
		if status != http.StatusOK || prm["resource"] != "https://mcp.example.com/mcp" {
			t.Fatalf("PRM %s: %d %v", p, status, prm)
		}
		if as, _ := prm["authorization_servers"].([]any); len(as) != 1 || as[0] != "https://mcp.example.com" {
			t.Fatalf("PRM authorization_servers = %v", prm["authorization_servers"])
		}
	}
	if !strings.Contains(h.srv.WWWAuthenticate(true), `resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("challenge = %s", h.srv.WWWAuthenticate(true))
	}
}

func TestRegistrationRedirectAllowlist(t *testing.T) {
	h := newHarness(t)
	for _, bad := range []string{
		"https://evil.example/cb",
		"https://oauth-redirect.googleusercontent.com.evil.example/r/x",
		"https://oauth-redirect.googleusercontent.com/x/../r/x",
		"http://oauth-redirect.googleusercontent.com/r/x",
		"http://127.0.0.1:5555/callback",
		"https://oauth-redirect.googleusercontent.com/r/x#frag",
	} {
		if status, body := h.register(map[string]any{"redirect_uris": []string{bad}}); status != http.StatusBadRequest || body["error"] != "invalid_redirect_uri" {
			t.Fatalf("redirect %q accepted: %d %v", bad, status, body)
		}
	}
	for _, bad := range []map[string]any{
		{"redirect_uris": []string{googleRedir}, "grant_types": []string{"client_credentials"}},
		{"redirect_uris": []string{googleRedir}, "response_types": []string{"token"}},
		{"redirect_uris": []string{googleRedir}, "token_endpoint_auth_method": "private_key_jwt"},
	} {
		if status, _ := h.register(bad); status != http.StatusBadRequest {
			t.Fatalf("bad metadata accepted: %v", bad)
		}
	}

	hl := newHarness(t, "loopback")
	if status, body := hl.register(map[string]any{"redirect_uris": []string{"http://127.0.0.1:5555/callback"}, "token_endpoint_auth_method": "none"}); status != http.StatusCreated || body["client_secret"] != nil {
		t.Fatalf("loopback public client: %d %v", status, body)
	}
}

func TestFullConfidentialFlowRefreshAndRevoke(t *testing.T) {
	h := newHarness(t)
	clientID, clientSecret, tok := h.linkConfidential()
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	if tok["token_type"] != "Bearer" || tok["scope"] != ScopeMCP || access == "" || refresh == "" {
		t.Fatalf("token response: %v", tok)
	}
	if !h.srv.VerifyAccessToken(access) {
		t.Fatal("fresh access token rejected")
	}
	if h.srv.VerifyAccessToken("mcplex_at_forged") || h.srv.VerifyAccessToken(refresh) {
		t.Fatal("forged / refresh token accepted as access token")
	}

	// Confidential clients keep the same refresh token (Gemini persistent linking) and get a new access token.
	h.clock.add(2 * time.Hour)
	if h.srv.VerifyAccessToken(access) {
		t.Fatal("expired access token accepted")
	}
	status, tok2 := h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}, clientID, clientSecret)
	if status != http.StatusOK || tok2["refresh_token"] != refresh || tok2["access_token"] == access {
		t.Fatalf("refresh: %d %v", status, tok2)
	}
	access2 := tok2["access_token"].(string)
	if !h.srv.VerifyAccessToken(access2) {
		t.Fatal("refreshed access token rejected")
	}
	// Wrong client secret fails.
	if status, body := h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}, clientID, "wrong"); status != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("wrong secret: %d %v", status, body)
	}
	// client_secret_post also works for a confidential client.
	if status, _ := h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}, "client_secret": {clientSecret}}, "", ""); status != http.StatusOK {
		t.Fatalf("client_secret_post refresh: %d", status)
	}

	// Persistence: a new server instance on the same data dir accepts the token.
	srv2, err := New(Config{PublicURL: "https://mcp.example.com", OwnerSecret: testSecret, DataDir: h.data, Now: h.clock.now})
	if err != nil || !srv2.VerifyAccessToken(access2) {
		t.Fatalf("token not persisted across restart: %v", err)
	}
	// A different public URL (resource) must not accept it (audience binding).
	srv3, _ := New(Config{PublicURL: "https://other.example.com", OwnerSecret: testSecret, DataDir: h.data, Now: h.clock.now})
	if srv3.VerifyAccessToken(access2) {
		t.Fatal("token accepted by a different resource")
	}

	// Revocation (Gemini unlink) invalidates the whole grant.
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/oauth/revoke", strings.NewReader(url.Values{"token": {refresh}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %v %v", err, resp)
	}
	resp.Body.Close()
	if h.srv.VerifyAccessToken(access2) {
		t.Fatal("access token still valid after revocation")
	}
	if status, _ := h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}, clientID, clientSecret); status != http.StatusBadRequest {
		t.Fatalf("revoked refresh token still works: %d", status)
	}
}

func TestAuthorizationCodeIsSingleUseAndPKCEBound(t *testing.T) {
	h := newHarness(t)
	clientID, clientSecret, tok := h.linkConfidential()
	access := tok["access_token"].(string)
	code := tok["_code"].(string)
	// Reusing the code fails AND revokes what it produced.
	status, body := h.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {googleRedir}, "code_verifier": {testVerifier}}, clientID, clientSecret)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("code reuse: %d %v", status, body)
	}
	if h.srv.VerifyAccessToken(access) {
		t.Fatal("tokens from a replayed code must be revoked")
	}

	// Wrong PKCE verifier.
	form := h.consent(authorizeParams(clientID, googleRedir, "s2"))
	resp := h.decide(form, testSecret, "approve")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp.Body.Close()
	status, body = h.token(url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "redirect_uri": {googleRedir}, "code_verifier": {strings.Repeat("a", 50)}}, clientID, clientSecret)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("bad verifier: %d %v", status, body)
	}

	// Expired code.
	form = h.consent(authorizeParams(clientID, googleRedir, "s3"))
	resp = h.decide(form, testSecret, "approve")
	loc, _ = url.Parse(resp.Header.Get("Location"))
	resp.Body.Close()
	h.clock.add(2 * authCodeTTL)
	if status, _ := h.token(url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "redirect_uri": {googleRedir}, "code_verifier": {testVerifier}}, clientID, clientSecret); status != http.StatusBadRequest {
		t.Fatalf("expired code accepted: %d", status)
	}
}

func TestAuthorizeRequestValidation(t *testing.T) {
	h := newHarness(t)
	_, reg := h.register(map[string]any{"redirect_uris": []string{googleRedir}})
	clientID := reg["client_id"].(string)

	get := func(p url.Values) *http.Response {
		resp, err := noRedirect.Get(h.ts.URL + "/oauth/authorize?" + p.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	// Unknown client / unregistered redirect -> error page, never a redirect.
	p := authorizeParams("nope", googleRedir, "x")
	if resp := get(p); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown client: %d", resp.StatusCode)
	}
	p = authorizeParams(clientID, "https://oauth-redirect.googleusercontent.com/r/other", "x")
	if resp := get(p); resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Fatalf("unregistered redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Missing PKCE / plain PKCE / wrong resource -> redirect with error.
	for name, mutate := range map[string]func(url.Values){
		"no pkce":     func(v url.Values) { v.Del("code_challenge") },
		"plain pkce":  func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"bad type":    func(v url.Values) { v.Set("response_type", "token") },
		"bad target":  func(v url.Values) { v.Set("resource", "https://evil.example/mcp") },
		"short pkce":  func(v url.Values) { v.Set("code_challenge", "abc") },
		"no response": func(v url.Values) { v.Del("response_type") },
	} {
		p := authorizeParams(clientID, googleRedir, "st")
		mutate(p)
		resp := get(p)
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusFound || loc == nil || loc.Query().Get("error") == "" || loc.Query().Get("state") != "st" {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// Deny -> access_denied.
	form := h.consent(authorizeParams(clientID, googleRedir, "d"))
	resp := h.decide(form, "", "deny")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp.Body.Close()
	if loc.Query().Get("error") != "access_denied" {
		t.Fatalf("deny: %s", loc)
	}
	// Tampered form (different state than the signed one) is rejected.
	form = h.consent(authorizeParams(clientID, googleRedir, "orig"))
	form.Set("state", "tampered")
	if resp := h.decide(form, testSecret, "approve"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tampered form accepted: %d", resp.StatusCode)
	}
	// Cross-origin POST is rejected.
	form = h.consent(authorizeParams(clientID, googleRedir, "o"))
	form.Set("owner_secret", testSecret)
	form.Set("action", "approve")
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	if resp, err := noRedirect.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin approve: %v %v", err, resp)
	}
}

func TestOwnerSecretLockout(t *testing.T) {
	h := newHarness(t)
	_, reg := h.register(map[string]any{"redirect_uris": []string{googleRedir}})
	clientID := reg["client_id"].(string)
	form := h.consent(authorizeParams(clientID, googleRedir, "l"))
	for i := 0; i < perKeyMaxFails; i++ {
		resp := h.decide(form, "wrong-guess", "approve")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	resp := h.decide(form, testSecret, "approve")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected lockout even with the right secret, got %d", resp.StatusCode)
	}
	h.clock.add(perKeyWindow + time.Minute)
	form = h.consent(authorizeParams(clientID, googleRedir, "l2"))
	resp = h.decide(form, testSecret, "approve")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("lockout did not expire: %d", resp.StatusCode)
	}
}

func TestPublicClientRotatesRefreshAndDetectsReplay(t *testing.T) {
	h := newHarness(t, "loopback")
	redir := "http://127.0.0.1:4545/callback"
	_, reg := h.register(map[string]any{"redirect_uris": []string{redir}, "token_endpoint_auth_method": "none"})
	clientID := reg["client_id"].(string)
	form := h.consent(authorizeParams(clientID, redir, "p"))
	resp := h.decide(form, testSecret, "approve")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp.Body.Close()
	status, tok := h.token(url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "redirect_uri": {redir}, "code_verifier": {testVerifier}, "client_id": {clientID}}, "", "")
	if status != http.StatusOK {
		t.Fatalf("public exchange: %d %v", status, tok)
	}
	r1 := tok["refresh_token"].(string)
	status, tok2 := h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {r1}, "client_id": {clientID}}, "", "")
	if status != http.StatusOK || tok2["refresh_token"] == r1 {
		t.Fatalf("public refresh must rotate: %d %v", status, tok2)
	}
	access2 := tok2["access_token"].(string)
	// Replaying the old refresh token revokes the grant.
	if status, _ := h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {r1}, "client_id": {clientID}}, "", ""); status != http.StatusBadRequest {
		t.Fatalf("replay accepted: %d", status)
	}
	if h.srv.VerifyAccessToken(access2) {
		t.Fatal("grant must be revoked after refresh token replay")
	}
	// A public client must not authenticate with a secret.
	if status, _ := h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}, "client_id": {clientID}, "client_secret": {"s"}}, "", ""); status != http.StatusUnauthorized {
		t.Fatalf("public client with secret: %d", status)
	}
}

func TestCLIRevokeAppliesToRunningServer(t *testing.T) {
	h := newHarness(t)
	clientID, _, tok := h.linkConfidential()
	access := tok["access_token"].(string)
	list, err := ListClients(h.data)
	if err != nil || len(list) != 1 || list[0].ID != clientID || list[0].Grants != 1 || list[0].Name != "Gemini" {
		t.Fatalf("ListClients: %v %+v", err, list)
	}
	time.Sleep(20 * time.Millisecond) // distinct mtime on coarse filesystems
	if n, err := RevokeClients(h.data, []string{clientID}); err != nil || n != 1 {
		t.Fatalf("RevokeClients: %d %v", n, err)
	}
	if h.srv.VerifyAccessToken(access) {
		t.Fatal("running server still accepts a token of a revoked client")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	data := t.TempDir()
	for _, bad := range []Config{
		{PublicURL: "", OwnerSecret: "x", DataDir: data},
		{PublicURL: "http://mcp.example.com", OwnerSecret: "x", DataDir: data},
		{PublicURL: "https://mcp.example.com/prefix", OwnerSecret: "x", DataDir: data},
		{PublicURL: "https://mcp.example.com", OwnerSecret: "", DataDir: data},
		{PublicURL: "https://mcp.example.com", OwnerSecret: "x", DataDir: data, RedirectAllow: []string{"https://a.example*"}},
	} {
		if _, err := New(bad); err == nil {
			t.Fatalf("config accepted: %+v", bad)
		}
	}
	if s, err := New(Config{PublicURL: "http://127.0.0.1:8787/", OwnerSecret: "x", DataDir: data}); err != nil || s.Resource() != "http://127.0.0.1:8787/mcp" {
		t.Fatalf("localhost http public URL: %v", err)
	}
}
