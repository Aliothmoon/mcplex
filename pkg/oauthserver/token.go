package oauthserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type oauthError struct {
	status int
	code   string
	desc   string
	basic  bool // client tried HTTP Basic auth -> add WWW-Authenticate on 401
}

func (s *Server) writeTokenError(w http.ResponseWriter, e *oauthError) {
	if e.status == http.StatusUnauthorized && e.basic {
		w.Header().Set("WWW-Authenticate", `Basic realm="mcplex-oauth"`)
	}
	writeOAuthError(w, e.status, e.code, e.desc)
}

// authenticateClient implements client_secret_basic, client_secret_post and none (public clients).
// Must be called inside a store callback.
func authenticateClient(r *http.Request, st *persistedState) (*clientRecord, *oauthError) {
	id, secret, basic := r.BasicAuth()
	if basic {
		// RFC 6749 §2.3.1: Basic credentials are form-urlencoded before base64.
		if v, err := url.QueryUnescape(id); err == nil {
			id = v
		}
		if v, err := url.QueryUnescape(secret); err == nil {
			secret = v
		}
		if formID := r.PostForm.Get("client_id"); formID != "" && formID != id {
			return nil, &oauthError{status: http.StatusBadRequest, code: "invalid_request", desc: "client_id mismatch"}
		}
	} else {
		id = r.PostForm.Get("client_id")
		secret = r.PostForm.Get("client_secret")
	}
	invalid := &oauthError{status: http.StatusUnauthorized, code: "invalid_client", desc: "client authentication failed", basic: basic}
	if id == "" {
		return nil, invalid
	}
	c := st.Clients[id]
	if c == nil {
		return nil, invalid
	}
	if c.public() {
		if secret != "" {
			return nil, invalid
		}
		return c, nil
	}
	if secret == "" || subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(c.SecretHash)) != 1 {
		return nil, invalid
	}
	return c, nil
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		s.writeTokenError(w, &oauthError{status: http.StatusBadRequest, code: "invalid_request", desc: "malformed form body"})
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.tokenFromCode(w, r)
	case "refresh_token":
		s.tokenFromRefresh(w, r)
	default:
		s.writeTokenError(w, &oauthError{status: http.StatusBadRequest, code: "unsupported_grant_type", desc: "use authorization_code or refresh_token"})
	}
}

func verifyPKCE(verifier, challenge string) bool {
	if l := len(verifier); l < 43 || l > 128 {
		return false
	}
	for _, c := range verifier {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
}

func (s *Server) tokenFromCode(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.now()
	code := r.PostForm.Get("code")
	codeHash := hashSecret(code)

	s.codesMu.Lock()
	ac := s.codes[codeHash]
	var replayGrant string
	if ac != nil && ac.Used {
		replayGrant = ac.GrantID
	}
	s.codesMu.Unlock()

	var resp *tokenResponse
	var oerr *oauthError
	err := s.store.update(now, func(st *persistedState) (bool, error) {
		client, cerr := authenticateClient(r, st)
		if cerr != nil {
			oerr = cerr
			return false, nil
		}
		if ac == nil || now.After(ac.Expires) || ac.ClientID != client.ID {
			oerr = &oauthError{status: http.StatusBadRequest, code: "invalid_grant", desc: "authorization code is invalid or expired"}
			return false, nil
		}
		if replayGrant != "" {
			// RFC 6749 §4.1.2: a reused code revokes everything issued from it.
			deleteGrantLocked(st, replayGrant)
			oerr = &oauthError{status: http.StatusBadRequest, code: "invalid_grant", desc: "authorization code was already used"}
			return true, nil
		}
		redirect := r.PostForm.Get("redirect_uri")
		if (ac.RedirectExplicit && redirect != ac.RedirectURI) || (redirect != "" && redirect != ac.RedirectURI) {
			oerr = &oauthError{status: http.StatusBadRequest, code: "invalid_grant", desc: "redirect_uri does not match the authorization request"}
			return false, nil
		}
		if !verifyPKCE(r.PostForm.Get("code_verifier"), ac.Challenge) {
			oerr = &oauthError{status: http.StatusBadRequest, code: "invalid_grant", desc: "PKCE verification failed"}
			return false, nil
		}
		for _, res := range r.PostForm["resource"] {
			if !s.resourceMatches(res) {
				oerr = &oauthError{status: http.StatusBadRequest, code: "invalid_target", desc: "this server only issues tokens for " + s.resource}
				return false, nil
			}
		}
		grant := &grantRecord{
			ID:         randomToken("g_", 12),
			ClientID:   client.ID,
			Scope:      ac.Scope,
			Resource:   ac.Resource,
			CreatedAt:  now,
			LastUsedAt: now,
		}
		refresh := randomToken("mcplex_rt_", 32)
		grant.RefreshHash = hashSecret(refresh)
		grant.RefreshExpires = now.Add(refreshTokenTTL)
		st.Grants[grant.ID] = grant
		access := s.issueAccessLocked(st, grant.ID, now)
		client.LastUsedAt = now

		s.codesMu.Lock()
		if cur := s.codes[codeHash]; cur != nil {
			cur.Used = true
			cur.GrantID = grant.ID
		}
		s.codesMu.Unlock()

		resp = &tokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(accessTokenTTL.Seconds()), RefreshToken: refresh, Scope: grant.Scope}
		return true, nil
	})
	s.finishToken(w, resp, oerr, err)
}

func (s *Server) tokenFromRefresh(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.now()
	presented := r.PostForm.Get("refresh_token")
	h := hashSecret(presented)
	var resp *tokenResponse
	var oerr *oauthError
	err := s.store.update(now, func(st *persistedState) (bool, error) {
		client, cerr := authenticateClient(r, st)
		if cerr != nil {
			oerr = cerr
			return false, nil
		}
		var grant *grantRecord
		for _, g := range st.Grants {
			if subtle.ConstantTimeCompare([]byte(g.RefreshHash), []byte(h)) == 1 {
				grant = g
				break
			}
		}
		if grant == nil {
			// Replay of a rotated-out refresh token: revoke the whole grant (OAuth 2.1 §4.3.1).
			for id, g := range st.Grants {
				for _, prev := range g.PrevRefreshHashes {
					if prev == h && g.ClientID == client.ID {
						deleteGrantLocked(st, id)
						oerr = &oauthError{status: http.StatusBadRequest, code: "invalid_grant", desc: "refresh token was already used; the grant has been revoked"}
						return true, nil
					}
				}
			}
			oerr = &oauthError{status: http.StatusBadRequest, code: "invalid_grant", desc: "refresh token is invalid, expired or revoked"}
			return false, nil
		}
		if grant.ClientID != client.ID || now.After(grant.RefreshExpires) {
			oerr = &oauthError{status: http.StatusBadRequest, code: "invalid_grant", desc: "refresh token is invalid, expired or revoked"}
			return false, nil
		}
		for _, res := range r.PostForm["resource"] {
			if !s.resourceMatches(res) {
				oerr = &oauthError{status: http.StatusBadRequest, code: "invalid_target", desc: "this server only issues tokens for " + s.resource}
				return false, nil
			}
		}
		refresh := presented
		if client.public() {
			// Public clients: rotate (OAuth 2.1 §4.3.1) and remember a few old hashes for replay detection.
			refresh = randomToken("mcplex_rt_", 32)
			grant.PrevRefreshHashes = append(grant.PrevRefreshHashes, grant.RefreshHash)
			if n := len(grant.PrevRefreshHashes); n > 5 {
				grant.PrevRefreshHashes = grant.PrevRefreshHashes[n-5:]
			}
			grant.RefreshHash = hashSecret(refresh)
		}
		// Confidential clients keep the same refresh token with a sliding expiry (Gemini persistent linking).
		grant.RefreshExpires = now.Add(refreshTokenTTL)
		grant.LastUsedAt = now
		client.LastUsedAt = now
		access := s.issueAccessLocked(st, grant.ID, now)
		resp = &tokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(accessTokenTTL.Seconds()), RefreshToken: refresh, Scope: grant.Scope}
		return true, nil
	})
	s.finishToken(w, resp, oerr, err)
}

func (s *Server) issueAccessLocked(st *persistedState, grantID string, now time.Time) string {
	access := randomToken("mcplex_at_", 32)
	st.Access[hashSecret(access)] = &accessRecord{GrantID: grantID, Expires: now.Add(accessTokenTTL)}
	return access
}

func (s *Server) finishToken(w http.ResponseWriter, resp *tokenResponse, oerr *oauthError, err error) {
	switch {
	case oerr != nil:
		s.writeTokenError(w, oerr)
	case err != nil || resp == nil:
		s.writeTokenError(w, &oauthError{status: http.StatusInternalServerError, code: "server_error", desc: "failed to persist tokens"})
	default:
		writeJSON(w, http.StatusOK, resp)
	}
}

// handleRevoke implements RFC 7009. Revoking either token of a grant invalidates the whole grant, which is what
// Gemini expects when a user unlinks the app. Unknown tokens still yield 200.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		s.writeTokenError(w, &oauthError{status: http.StatusBadRequest, code: "invalid_request", desc: "malformed form body"})
		return
	}
	tok := r.PostForm.Get("token")
	h := hashSecret(tok)
	var oerr *oauthError
	_ = s.store.update(s.cfg.now(), func(st *persistedState) (bool, error) {
		client, cerr := authenticateClient(r, st)
		if cerr != nil {
			oerr = cerr
			return false, nil
		}
		grantID := ""
		if a := st.Access[h]; a != nil {
			grantID = a.GrantID
		} else {
			for id, g := range st.Grants {
				if g.RefreshHash == h {
					grantID = id
					break
				}
			}
		}
		if grantID == "" {
			return false, nil
		}
		if g := st.Grants[grantID]; g == nil || g.ClientID != client.ID {
			return false, nil
		}
		deleteGrantLocked(st, grantID)
		return true, nil
	})
	if oerr != nil {
		s.writeTokenError(w, oerr)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// VerifyAccessToken validates an OAuth access token for the MCP endpoint: it must exist, be unexpired, belong to
// a live grant and be bound to this server's resource (RFC 8707 audience).
func (s *Server) VerifyAccessToken(token string) bool {
	if !strings.HasPrefix(token, "mcplex_at_") {
		return false
	}
	now := s.cfg.now()
	h := hashSecret(token)
	ok := false
	_ = s.store.view(func(st *persistedState) {
		a := st.Access[h]
		if a == nil || now.After(a.Expires) {
			return
		}
		g := st.Grants[a.GrantID]
		if g == nil || g.Resource != s.resource || st.Clients[g.ClientID] == nil {
			return
		}
		ok = true
	})
	return ok
}
