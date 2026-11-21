package oauthserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// authzRequest is a validated authorization request.
type authzRequest struct {
	Client           clientRecord
	RedirectURI      string
	RedirectExplicit bool
	State            string
	Challenge        string
	Scope            string // granted scope
	Resource         string // canonical resource the token will be bound to
}

// authzError is either shown on an error page (client/redirect could not be trusted) or redirected back.
type authzError struct {
	page        bool
	status      int
	code        string
	description string
}

func (e *authzError) Error() string { return e.code + ": " + e.description }

func (s *Server) parseAuthzRequest(form url.Values) (*authzRequest, *authzError) {
	clientID := form.Get("client_id")
	var client *clientRecord
	_ = s.store.view(func(st *persistedState) {
		if c := st.Clients[clientID]; c != nil {
			cp := *c
			cp.RedirectURIs = append([]string(nil), c.RedirectURIs...)
			client = &cp
		}
	})
	if clientID == "" || client == nil {
		return nil, &authzError{page: true, status: http.StatusBadRequest, code: "invalid_client", description: "Unknown client_id. The client must register first (dynamic client registration)."}
	}
	redirect := form.Get("redirect_uri")
	explicit := redirect != ""
	if !explicit {
		if len(client.RedirectURIs) != 1 {
			return nil, &authzError{page: true, status: http.StatusBadRequest, code: "invalid_request", description: "redirect_uri is required when the client registered several redirect URIs."}
		}
		redirect = client.RedirectURIs[0]
	}
	registered := false
	for _, u := range client.RedirectURIs {
		if u == redirect {
			registered = true
			break
		}
	}
	if !registered || !s.redirectAllowed(redirect) {
		return nil, &authzError{page: true, status: http.StatusBadRequest, code: "invalid_request", description: "redirect_uri does not exactly match a URI registered for this client."}
	}

	req := &authzRequest{Client: *client, RedirectURI: redirect, RedirectExplicit: explicit, State: form.Get("state")}
	if rt := form.Get("response_type"); rt != "code" {
		return req, &authzError{code: "unsupported_response_type", description: "only response_type=code is supported"}
	}
	req.Challenge = form.Get("code_challenge")
	if req.Challenge == "" {
		return req, &authzError{code: "invalid_request", description: "PKCE code_challenge is required"}
	}
	if m := form.Get("code_challenge_method"); m != "S256" {
		return req, &authzError{code: "invalid_request", description: "code_challenge_method must be S256"}
	}
	if l := len(req.Challenge); l < 43 || l > 128 {
		return req, &authzError{code: "invalid_request", description: "malformed code_challenge"}
	}
	for _, res := range form["resource"] {
		if !s.resourceMatches(res) {
			return req, &authzError{code: "invalid_target", description: "this server only issues tokens for " + s.resource}
		}
	}
	req.Resource = s.resource
	// Unknown scopes (e.g. "openid profile email") are ignored rather than rejected: RFC 6749 lets the server
	// grant a different scope, which the token response reports.
	req.Scope = ScopeMCP
	return req, nil
}

// resourceMatches accepts the canonical resource URI (with or without trailing slash) or the bare origin.
func (s *Server) resourceMatches(res string) bool {
	res = strings.TrimSpace(res)
	trimmed := strings.TrimRight(res, "/")
	return trimmed == s.resource || trimmed == s.issuer
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	setPageSecurityHeaders(w)
	switch r.Method {
	case http.MethodGet:
		req, aerr := s.parseAuthzRequest(r.URL.Query())
		if aerr != nil {
			s.failAuthz(w, r, req, aerr)
			return
		}
		s.renderConsent(w, http.StatusOK, req, "")
	case http.MethodPost:
		s.handleAuthorizeDecision(w, r)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (s *Server) handleAuthorizeDecision(w http.ResponseWriter, r *http.Request) {
	// The consent form is same-origin; reject cross-site posts outright (defense in depth on top of the form token).
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" && !strings.EqualFold(strings.TrimRight(origin, "/"), s.issuer) {
		renderErrorPage(w, http.StatusForbidden, "Cross-origin authorization request rejected.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		renderErrorPage(w, http.StatusBadRequest, "Malformed form submission.")
		return
	}
	req, aerr := s.parseAuthzRequest(r.PostForm)
	if aerr != nil {
		s.failAuthz(w, r, req, aerr)
		return
	}
	now := s.cfg.now()
	if !s.verifyFormToken(r.PostForm.Get("form_token"), req, now) {
		renderErrorPage(w, http.StatusBadRequest, "This authorization page expired. Start the connection again from your MCP client.")
		return
	}
	if r.PostForm.Get("action") != "approve" {
		s.redirectWithError(w, r, req, "access_denied", "the owner denied the request")
		return
	}
	key := requestKey(r)
	if locked, wait := s.loginLimiter.locked(key, now); locked {
		s.renderConsent(w, http.StatusTooManyRequests, req, fmt.Sprintf("Too many wrong attempts. Try again in %d minutes.", int(wait.Minutes())+1))
		return
	}
	given := r.PostForm.Get("owner_secret")
	if subtle.ConstantTimeCompare([]byte(hashSecret(given)), []byte(hashSecret(s.cfg.OwnerSecret))) != 1 {
		s.loginLimiter.fail(key, now)
		s.renderConsent(w, http.StatusUnauthorized, req, "Wrong owner secret.")
		return
	}
	s.loginLimiter.success(key)

	code := randomToken("mcplex_ac_", 32)
	s.codesMu.Lock()
	for h, c := range s.codes {
		if now.After(c.Expires.Add(authCodeTTL)) {
			delete(s.codes, h)
		}
	}
	s.codes[hashSecret(code)] = &authCode{
		ClientID:         req.Client.ID,
		RedirectURI:      req.RedirectURI,
		RedirectExplicit: req.RedirectExplicit,
		Challenge:        req.Challenge,
		Scope:            req.Scope,
		Resource:         req.Resource,
		Expires:          now.Add(authCodeTTL),
	}
	s.codesMu.Unlock()

	params := url.Values{"code": {code}, "iss": {s.issuer}}
	if req.State != "" {
		params.Set("state", req.State)
	}
	http.Redirect(w, r, appendQuery(req.RedirectURI, params), http.StatusFound)
}

func (s *Server) failAuthz(w http.ResponseWriter, r *http.Request, req *authzRequest, aerr *authzError) {
	if aerr.page || req == nil {
		status := aerr.status
		if status == 0 {
			status = http.StatusBadRequest
		}
		renderErrorPage(w, status, aerr.description)
		return
	}
	s.redirectWithError(w, r, req, aerr.code, aerr.description)
}

func (s *Server) redirectWithError(w http.ResponseWriter, r *http.Request, req *authzRequest, code, desc string) {
	params := url.Values{"error": {code}, "iss": {s.issuer}}
	if desc != "" {
		params.Set("error_description", desc)
	}
	if req.State != "" {
		params.Set("state", req.State)
	}
	http.Redirect(w, r, appendQuery(req.RedirectURI, params), http.StatusFound)
}

func appendQuery(base string, params url.Values) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// form tokens bind the consent form to the validated request parameters and expire after formTokenTTL.
func (s *Server) formTokenMAC(req *authzRequest, exp int64) string {
	mac := hmac.New(sha256.New, s.formKey)
	fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n%s\n%d", req.Client.ID, req.RedirectURI, req.Challenge, req.State, req.Resource, exp)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) newFormToken(req *authzRequest, now time.Time) string {
	exp := now.Add(formTokenTTL).Unix()
	return strconv.FormatInt(exp, 10) + "." + s.formTokenMAC(req, exp)
}

func (s *Server) verifyFormToken(tok string, req *authzRequest, now time.Time) bool {
	expStr, mac, ok := strings.Cut(tok, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || now.Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(s.formTokenMAC(req, exp)))
}

func setPageSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	// No form-action directive: browsers apply it to the post-submit redirect to the client's redirect_uri.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
}

var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorize access to MCPlex</title>
<style>
body{font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;background:#0f1115;color:#e6e6e6;display:flex;justify-content:center;padding:40px 16px;margin:0}
.card{max-width:460px;width:100%;background:#181b22;border:1px solid #2a2f3a;border-radius:12px;padding:28px}
h1{font-size:20px;margin:0 0 16px}.muted{color:#9aa3b2;font-size:14px}.warn{background:#3a2a12;border:1px solid #6b4a16;border-radius:8px;padding:10px 12px;font-size:14px;margin:16px 0}
.err{background:#3a1616;border:1px solid #7a2626;border-radius:8px;padding:10px 12px;font-size:14px;margin:12px 0}
dl{display:grid;grid-template-columns:auto 1fr;gap:6px 12px;font-size:14px;margin:16px 0}dt{color:#9aa3b2}dd{margin:0;word-break:break-all}
input[type=password]{width:100%;box-sizing:border-box;padding:10px;border-radius:8px;border:1px solid #2a2f3a;background:#0f1115;color:#e6e6e6;font-size:15px;margin:6px 0 16px}
.row{display:flex;gap:10px}button{flex:1;padding:10px;border-radius:8px;border:0;font-size:15px;cursor:pointer}
.approve{background:#3b82f6;color:#fff}.deny{background:#2a2f3a;color:#e6e6e6}
</style></head><body><div class="card">
<h1>Authorize access to MCPlex</h1>
<p class="muted">An application wants to connect to this MCPlex server.</p>
<dl><dt>Application</dt><dd>{{if .ClientName}}{{.ClientName}}{{else}}(unnamed client){{end}}</dd>
<dt>Returns to</dt><dd>{{.RedirectHost}}</dd><dt>Server</dt><dd>{{.Resource}}</dd><dt>Access</dt><dd>full MCP access (scope "mcp")</dd></dl>
<div class="warn">Approving lets this application run MCPlex tools on this machine, including shell commands through <code>exec</code>. Only approve applications you started connecting yourself.</div>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<form method="post" action="/oauth/authorize" autocomplete="off">
{{range $k, $v := .Hidden}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<label class="muted" for="owner_secret">Owner secret (the MCPlex --token)</label>
<input type="password" id="owner_secret" name="owner_secret" autofocus>
<div class="row"><button class="deny" type="submit" name="action" value="deny">Deny</button>
<button class="approve" type="submit" name="action" value="approve">Approve</button></div>
</form></div></body></html>`))

var errorTmpl = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>MCPlex authorization error</title>
<style>body{font-family:system-ui,sans-serif;background:#0f1115;color:#e6e6e6;padding:40px}</style></head>
<body><h1>Authorization error</h1><p>{{.}}</p></body></html>`))

func (s *Server) renderConsent(w http.ResponseWriter, status int, req *authzRequest, errMsg string) {
	redirectHost := req.RedirectURI
	if u, err := url.Parse(req.RedirectURI); err == nil {
		redirectHost = u.Scheme + "://" + u.Host
	}
	hidden := map[string]string{
		"response_type":         "code",
		"client_id":             req.Client.ID,
		"code_challenge":        req.Challenge,
		"code_challenge_method": "S256",
		"resource":              req.Resource,
		"scope":                 req.Scope,
		"form_token":            s.newFormToken(req, s.cfg.now()),
	}
	if req.RedirectExplicit {
		hidden["redirect_uri"] = req.RedirectURI
	}
	if req.State != "" {
		hidden["state"] = req.State
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = consentTmpl.Execute(w, map[string]any{
		"ClientName":   req.Client.Name,
		"RedirectHost": redirectHost,
		"Resource":     s.resource,
		"Error":        errMsg,
		"Hidden":       hidden,
	})
}

func renderErrorPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = errorTmpl.Execute(w, msg)
}
