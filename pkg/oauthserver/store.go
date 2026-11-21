package oauthserver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// clientRecord is a dynamically registered OAuth client. Secrets are stored only as SHA-256 hashes.
type clientRecord struct {
	ID           string    `json:"id"`
	SecretHash   string    `json:"secretHash,omitempty"`
	Name         string    `json:"name,omitempty"`
	RedirectURIs []string  `json:"redirectUris"`
	AuthMethod   string    `json:"authMethod"` // none | client_secret_basic | client_secret_post
	CreatedAt    time.Time `json:"createdAt"`
	LastUsedAt   time.Time `json:"lastUsedAt"`
}

func (c *clientRecord) public() bool { return c.AuthMethod == "none" }

// grantRecord is one user approval (authorization) of a client; access tokens hang off a grant.
type grantRecord struct {
	ID                string    `json:"id"`
	ClientID          string    `json:"clientId"`
	Scope             string    `json:"scope"`
	Resource          string    `json:"resource"`
	RefreshHash       string    `json:"refreshHash"`
	RefreshExpires    time.Time `json:"refreshExpires"`
	PrevRefreshHashes []string  `json:"prevRefreshHashes,omitempty"` // rotated-out refresh tokens (replay detection)
	CreatedAt         time.Time `json:"createdAt"`
	LastUsedAt        time.Time `json:"lastUsedAt"`
}

type accessRecord struct {
	GrantID string    `json:"grantId"`
	Expires time.Time `json:"expires"`
}

type persistedState struct {
	Version int                      `json:"version"`
	Clients map[string]*clientRecord `json:"clients"`
	Grants  map[string]*grantRecord  `json:"grants"`
	Access  map[string]*accessRecord `json:"access"` // key: sha256(access token)
}

func newPersistedState() *persistedState {
	return &persistedState{
		Version: 1,
		Clients: map[string]*clientRecord{},
		Grants:  map[string]*grantRecord{},
		Access:  map[string]*accessRecord{},
	}
}

// store persists OAuth state to <data-dir>/oauth-server/state.json (0600, atomic replace). It reloads when the
// file changes on disk so `mcplex oauth revoke` takes effect on a running server.
type store struct {
	mu      sync.Mutex
	path    string
	st      *persistedState
	modTime time.Time
	size    int64
}

// openStore opens the persisted state. create=false (CLI listing/revocation) never creates directories, so
// inspecting a data dir that never ran `serve --oauth` leaves no trace.
func openStore(dataDir string, create bool) (*store, error) {
	if dataDir == "" {
		return nil, errors.New("oauth server needs a data dir to persist clients and grants")
	}
	dir := StateDir(dataDir)
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	s := &store{path: filepath.Join(dir, "state.json"), st: newPersistedState()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadIfChangedLocked(true); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *store) reloadIfChangedLocked(force bool) error {
	info, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if force || !s.modTime.IsZero() {
				s.st = newPersistedState()
				s.modTime, s.size = time.Time{}, 0
			}
			return nil
		}
		return err
	}
	if !force && info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	st := newPersistedState()
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, st); err != nil {
			return err
		}
	}
	if st.Clients == nil {
		st.Clients = map[string]*clientRecord{}
	}
	if st.Grants == nil {
		st.Grants = map[string]*grantRecord{}
	}
	if st.Access == nil {
		st.Access = map[string]*accessRecord{}
	}
	s.st = st
	s.modTime, s.size = info.ModTime(), info.Size()
	return nil
}

func (s *store) saveLocked() error {
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	_ = tmp.Chmod(0o600)
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	if info, err := os.Stat(s.path); err == nil {
		s.modTime, s.size = info.ModTime(), info.Size()
	}
	return nil
}

// update reloads external changes, applies fn, prunes expired records and persists when fn reports a change.
func (s *store) update(now time.Time, fn func(st *persistedState) (changed bool, err error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadIfChangedLocked(false); err != nil {
		return err
	}
	changed, err := fn(s.st)
	if pruneExpired(s.st, now) {
		changed = true
	}
	if changed {
		if saveErr := s.saveLocked(); saveErr != nil && err == nil {
			err = saveErr
		}
	}
	return err
}

// view reloads external changes and runs fn read-only.
func (s *store) view(fn func(st *persistedState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadIfChangedLocked(false); err != nil {
		return err
	}
	fn(s.st)
	return nil
}

func pruneExpired(st *persistedState, now time.Time) bool {
	changed := false
	for h, a := range st.Access {
		if now.After(a.Expires) || st.Grants[a.GrantID] == nil {
			delete(st.Access, h)
			changed = true
		}
	}
	for id, g := range st.Grants {
		if now.After(g.RefreshExpires) || st.Clients[g.ClientID] == nil {
			deleteGrantLocked(st, id)
			changed = true
		}
	}
	return changed
}

func deleteGrantLocked(st *persistedState, grantID string) {
	delete(st.Grants, grantID)
	for h, a := range st.Access {
		if a.GrantID == grantID {
			delete(st.Access, h)
		}
	}
}

// evictClientsLocked keeps at most limit clients, dropping the least recently used clients without live grants.
func evictClientsLocked(st *persistedState, limit int) bool {
	if len(st.Clients) < limit {
		return true
	}
	active := map[string]bool{}
	for _, g := range st.Grants {
		active[g.ClientID] = true
	}
	var idle []*clientRecord
	for _, c := range st.Clients {
		if !active[c.ID] {
			idle = append(idle, c)
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].LastUsedAt.Before(idle[j].LastUsedAt) })
	for _, c := range idle {
		if len(st.Clients) < limit {
			break
		}
		delete(st.Clients, c.ID)
	}
	return len(st.Clients) < limit
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomToken(prefix string, nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// ClientInfo is a read-only summary of a registered client for `mcplex oauth clients`.
type ClientInfo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	AuthMethod string    `json:"authMethod"`
	Redirects  []string  `json:"redirectUris"`
	Grants     int       `json:"activeGrants"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
}

// ListClients returns registered clients persisted under dataDir.
func ListClients(dataDir string) ([]ClientInfo, error) {
	s, err := openStore(dataDir, false)
	if err != nil {
		return nil, err
	}
	var out []ClientInfo
	_ = s.view(func(st *persistedState) {
		counts := map[string]int{}
		for _, g := range st.Grants {
			counts[g.ClientID]++
		}
		for _, c := range st.Clients {
			out = append(out, ClientInfo{
				ID: c.ID, Name: c.Name, AuthMethod: c.AuthMethod, Redirects: append([]string(nil), c.RedirectURIs...),
				Grants: counts[c.ID], CreatedAt: c.CreatedAt, LastUsedAt: c.LastUsedAt,
			})
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// RevokeClients deletes the given client IDs (or all clients when ids contains "all") together with their grants
// and tokens. A running server picks the change up on its next request. Returns the number of clients removed.
func RevokeClients(dataDir string, ids []string) (int, error) {
	s, err := openStore(dataDir, false)
	if err != nil {
		return 0, err
	}
	removed := 0
	err = s.update(time.Now(), func(st *persistedState) (bool, error) {
		all := false
		want := map[string]bool{}
		for _, id := range ids {
			if id == "all" {
				all = true
			}
			want[id] = true
		}
		for id := range st.Clients {
			if all || want[id] {
				delete(st.Clients, id)
				removed++
				for gid, g := range st.Grants {
					if g.ClientID == id {
						deleteGrantLocked(st, gid)
					}
				}
			}
		}
		return removed > 0, nil
	})
	return removed, err
}
