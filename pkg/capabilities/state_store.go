package capabilities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

const (
	MaxStateEntryBytes = 2*1024*1024 + 128*1024 // ~2.125 MB per persisted user key (>= MaxSandboxReadBytes + JSON escaping)
	MaxStateTotalBytes = 8 * 1024 * 1024        // 8 MB per workspace session (persisted user entries)
	MaxSpillEntryBytes = 4 * 1024 * 1024        // 4 MB per in-memory auto-spill entry
	MaxSpillTotalBytes = 4 * 1024 * 1024        // 4 MB in-memory cap per session for auto-spills
	maxGlobalSpillPool = 32 * 1024 * 1024
	lastResultKey      = "__last_result"
	spillKeyPrefix     = "__result_"
)

type sessionSpillBucket struct {
	spills       map[string]stateEntry
	seq          int64
	lastSpillKey string
}

// StateStore provides fast in-memory + disk-persisted key-value storage across PTC turns.
// User-set entries persist per workspace; auto-spills (`__result_N` / `__last_result` alias)
// live in session-scoped in-memory buckets so concurrent HTTP sessions never cross-contaminate.
type StateStore struct {
	mu            sync.RWMutex
	dataDir       string
	filePath      string
	entries       map[string]stateEntry          // persisted user KV
	sessionSpills map[string]*sessionSpillBucket // sessionID ("" = default) -> auto-spills
	globalSeq     int64
	saveSeq       int64
	dirty         bool // pending user KV changes; persisted by Flush (once per exec)
}

// Flush persists pending user KV changes to disk.
func (s *StateStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		s.saveToDiskLocked()
		s.dirty = false
	}
}

type stateEntry struct {
	Value     any    `json:"value"`
	Bytes     int    `json:"bytes"`
	UpdatedAt string `json:"updatedAt"`
	Seq       int64  `json:"seq,omitempty"`
}

func normalizeWorkspaceHashKeyForOS(workspaceDir, goos string) string {
	cleaned := filepath.ToSlash(filepath.Clean(workspaceDir))
	if goos == "windows" || goos == "darwin" {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

// NewStateStore creates or loads a workspace-scoped state store under dataDir.
func NewStateStore(dataDir, workspaceDir string) *StateStore {
	s := &StateStore{
		dataDir:       dataDir,
		entries:       map[string]stateEntry{},
		sessionSpills: map[string]*sessionSpillBucket{},
	}
	if dataDir != "" {
		h := sha256.Sum256([]byte(normalizeWorkspaceHashKeyForOS(workspaceDir, runtime.GOOS)))
		id := hex.EncodeToString(h[:8])
		sessDir := filepath.Join(dataDir, "sessions")
		_ = os.MkdirAll(sessDir, 0o755)
		s.filePath = filepath.Join(sessDir, id+".json")
		s.loadFromDisk()
	}
	return s
}

func isAutoSpillKey(key string) bool {
	if !strings.HasPrefix(key, spillKeyPrefix) {
		return false
	}
	suffix := key[len(spillKeyPrefix):]
	if suffix == "" {
		return false
	}
	for i := 0; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return false
		}
	}
	return true
}

func isReservedSpillKey(key string) bool {
	return key == lastResultKey || isAutoSpillKey(key)
}

func (s *StateStore) loadFromDisk() {
	if s.filePath == "" {
		return
	}
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		return
	}
	var loaded map[string]stateEntry
	if err := json.Unmarshal(raw, &loaded); err == nil && loaded != nil {
		// Drop legacy persisted auto-spills so a fresh session never inherits stale output from prior runs.
		var maxSeq int64
		for k, e := range loaded {
			if isReservedSpillKey(k) {
				delete(loaded, k)
				continue
			}
			if e.Seq > maxSeq {
				maxSeq = e.Seq
			}
		}
		s.entries = loaded
		if maxSeq > s.globalSeq {
			s.globalSeq = maxSeq
		}
	}
}

func (s *StateStore) saveToDiskLocked() {
	if s.filePath == "" {
		return
	}
	raw, err := json.Marshal(s.entries)
	if err != nil {
		return
	}
	s.saveSeq++
	tmp := fmt.Sprintf("%s.tmp.%d.%d", s.filePath, os.Getpid(), s.saveSeq)
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, s.filePath); err != nil {
		_ = os.Remove(tmp)
	}
}

func detachValue(val any, raw []byte) any {
	switch v := val.(type) {
	case nil, string, bool, float64:
		return v
	}
	var detached any
	if err := json.Unmarshal(raw, &detached); err != nil {
		return val
	}
	return detached
}

func cloneValue(val any) any {
	switch v := val.(type) {
	case nil, string, bool, float64:
		return v
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return val
	}
	var cp any
	if err := json.Unmarshal(raw, &cp); err != nil {
		return val
	}
	return cp
}

// Set stores a JSON-serializable value under key. Reserved spill keys (`__last_result`, `__result_N`) are rejected.
func (s *StateStore) Set(key string, val any) error {
	_, err := s.SetWithEvict(key, val)
	return err
}

// SetWithEvict stores a JSON-serializable value under key, automatically evicting the oldest workspace entries
// in FIFO/LRU order if adding key would exceed MaxStateTotalBytes (8 MB). Returns any evicted key names.
func (s *StateStore) SetWithEvict(key string, val any) ([]string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("state key cannot be empty")
	}
	if isReservedSpillKey(key) {
		return nil, fmt.Errorf("state key %q is reserved for automatic output spills (choose a custom key name)", key)
	}
	if val == nil {
		s.Delete(key)
		return nil, nil
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return nil, fmt.Errorf("state value must be JSON-serializable: %w", err)
	}
	if len(raw) > MaxStateEntryBytes {
		return nil, fmt.Errorf("state value for '%s' (%d bytes) exceeds per-key limit (%d bytes)", key, len(raw), MaxStateEntryBytes)
	}

	detached := detachValue(val, raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	total := len(raw)
	for k, e := range s.entries {
		if k != key {
			total += e.Bytes
		}
	}
	var evicted []string
	if total > MaxStateTotalBytes {
		type cand struct {
			key      string
			reserved bool
			updated  string
			seq      int64
			bytes    int
		}
		cands := make([]cand, 0, len(s.entries))
		for k, e := range s.entries {
			if k == key {
				continue
			}
			cands = append(cands, cand{
				key:      k,
				reserved: isReservedSpillKey(k),
				updated:  e.UpdatedAt,
				seq:      e.Seq,
				bytes:    e.Bytes,
			})
		}
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].reserved != cands[j].reserved {
				return cands[i].reserved
			}
			if cands[i].updated != cands[j].updated {
				return cands[i].updated < cands[j].updated
			}
			if cands[i].seq != cands[j].seq {
				return cands[i].seq < cands[j].seq
			}
			return cands[i].key < cands[j].key
		})
		for _, c := range cands {
			if total <= MaxStateTotalBytes {
				break
			}
			delete(s.entries, c.key)
			total -= c.bytes
			evicted = append(evicted, c.key)
		}
	}
	if total > MaxStateTotalBytes {
		return nil, fmt.Errorf("state store total size (%d bytes) exceeds session limit (%d bytes); call state.clear() or state.delete(key)", total, MaxStateTotalBytes)
	}

	s.globalSeq++
	s.entries[key] = stateEntry{
		Value:     detached,
		Bytes:     len(raw),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Seq:       s.globalSeq,
	}
	s.dirty = true
	return evicted, nil
}

func (s *StateStore) getOrCreateBucketLocked(sessionID string) *sessionSpillBucket {
	sid := strings.TrimSpace(sessionID)
	b, ok := s.sessionSpills[sid]
	if !ok {
		b = &sessionSpillBucket{spills: map[string]stateEntry{}}
		s.sessionSpills[sid] = b
	}
	return b
}

// SaveAutoSpill atomically saves an oversized return value into the default session spill pool.
func (s *StateStore) SaveAutoSpill(val any) string {
	return s.SaveAutoSpillForSession("", val)
}

// SaveAutoSpillForSession atomically saves an oversized return value into sessionID's in-memory spill pool
// under a unique `__result_N` key and updates that session's `__last_result` alias under the same lock.
func (s *StateStore) SaveAutoSpillForSession(sessionID string, val any) string {
	raw, err := json.Marshal(val)
	if err != nil || len(raw) > MaxSpillEntryBytes {
		return ""
	}
	detached := detachValue(val, raw)
	sid := strings.TrimSpace(sessionID)

	s.mu.Lock()
	defer s.mu.Unlock()

	bucket := s.getOrCreateBucketLocked(sid)
	bucketTotal := len(raw)
	for _, e := range bucket.spills {
		bucketTotal += e.Bytes
	}
	if bucketTotal > MaxSpillTotalBytes {
		evictBucketSpillsLocked(bucket, bucketTotal-MaxSpillTotalBytes)
	}

	globalTotal := len(raw)
	for _, b := range s.sessionSpills {
		for _, e := range b.spills {
			globalTotal += e.Bytes
		}
	}
	if globalTotal > maxGlobalSpillPool {
		s.evictGlobalSpillsLocked(globalTotal - maxGlobalSpillPool)
		bucket = s.getOrCreateBucketLocked(sid)
	}

	s.globalSeq++
	bucket.seq++
	id := fmt.Sprintf("%s%d", spillKeyPrefix, bucket.seq)
	bucket.spills[id] = stateEntry{
		Value:     detached,
		Bytes:     len(raw),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Seq:       s.globalSeq,
	}
	bucket.lastSpillKey = id
	return id
}

// GetResolved retrieves a detached copy of key's value in the default session along with its resolved key.
func (s *StateStore) GetResolved(key string) (val any, resolvedKey string, found bool) {
	return s.GetResolvedForSession("", key)
}

// GetResolvedForSession retrieves a detached copy of key's value scoped to sessionID for spill keys
// (`__last_result`, `__result_N`) while sharing workspace user KV entries.
func (s *StateStore) GetResolvedForSession(sessionID, key string) (val any, resolvedKey string, found bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, "", false
	}
	sid := strings.TrimSpace(sessionID)

	s.mu.RLock()
	var e stateEntry
	if entry, ok := s.entries[key]; ok {
		e = entry
		resolvedKey = key
		found = true
	} else if b := s.sessionSpills[sid]; b != nil {
		if entry, ok := b.spills[key]; ok {
			e = entry
			resolvedKey = key
			found = true
		} else if key == lastResultKey && b.lastSpillKey != "" {
			if entry, ok := b.spills[b.lastSpillKey]; ok {
				e = entry
				resolvedKey = b.lastSpillKey
				found = true
			}
		}
	}
	s.mu.RUnlock()

	if !found {
		return nil, "", false
	}
	return cloneValue(e.Value), resolvedKey, true
}

// Get retrieves a detached copy of key's value in the default session (or nil if absent).
func (s *StateStore) Get(key string) any {
	val, _, ok := s.GetResolvedForSession("", key)
	if !ok {
		return nil
	}
	return val
}

// Delete removes a key from the store in the default session.
func (s *StateStore) Delete(key string) bool {
	return s.DeleteForSession("", key)
}

// DeleteForSession removes a user key or a session-scoped spill key (`__result_N` / `__last_result`).
func (s *StateStore) DeleteForSession(sessionID, key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	sid := strings.TrimSpace(sessionID)

	s.mu.Lock()
	defer s.mu.Unlock()
	existed := false
	if _, ok := s.entries[key]; ok {
		delete(s.entries, key)
		s.dirty = true
		existed = true
	}
	if b := s.sessionSpills[sid]; b != nil {
		if _, ok := b.spills[key]; ok {
			delete(b.spills, key)
			existed = true
			if b.lastSpillKey == key {
				b.lastSpillKey = ""
			}
		}
		if key == lastResultKey && b.lastSpillKey != "" {
			b.lastSpillKey = ""
			existed = true
		}
	}
	return existed
}

// Keys lists all keys currently readable in the default session.
func (s *StateStore) Keys() []string {
	return s.KeysForSession("")
}

// KeysForSession lists all user keys plus sessionID's active spill keys (`__result_N` and `__last_result`).
func (s *StateStore) KeysForSession(sessionID string) []string {
	sid := strings.TrimSpace(sessionID)
	s.mu.RLock()
	defer s.mu.RUnlock()

	b := s.sessionSpills[sid]
	spillCap := 0
	if b != nil {
		spillCap = len(b.spills) + 1
	}
	out := make([]string, 0, len(s.entries)+spillCap)
	for k := range s.entries {
		out = append(out, k)
	}
	if b != nil {
		for k := range b.spills {
			out = append(out, k)
		}
		if b.lastSpillKey != "" {
			if _, ok := b.spills[b.lastSpillKey]; ok {
				if _, manual := s.entries[lastResultKey]; !manual {
					out = append(out, lastResultKey)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// ClearSessionSpills removes all in-memory auto-spills belonging to sessionID without touching user KV or other sessions.
func (s *StateStore) ClearSessionSpills(sessionID string) {
	sid := strings.TrimSpace(sessionID)
	s.mu.Lock()
	delete(s.sessionSpills, sid)
	s.mu.Unlock()
}

// ClearForSession clears persisted user KV entries and sessionID's auto-spills without dropping other sessions' active spills.
func (s *StateStore) ClearForSession(sessionID string) {
	sid := strings.TrimSpace(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) > 0 {
		s.dirty = true
	}
	s.entries = map[string]stateEntry{}
	delete(s.sessionSpills, sid)
}

// Clear wipes all persisted entries and all in-memory auto-spills in the store.
func (s *StateStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) > 0 {
		s.dirty = true
	}
	s.entries = map[string]stateEntry{}
	s.sessionSpills = map[string]*sessionSpillBucket{}
}

func evictBucketSpillsLocked(b *sessionSpillBucket, bytesNeeded int) {
	type cand struct {
		key   string
		seq   int64
		bytes int
	}
	cands := make([]cand, 0, len(b.spills))
	for k, e := range b.spills {
		cands = append(cands, cand{key: k, seq: e.Seq, bytes: e.Bytes})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].seq != cands[j].seq {
			return cands[i].seq < cands[j].seq
		}
		return cands[i].key < cands[j].key
	})
	freed := 0
	for _, c := range cands {
		delete(b.spills, c.key)
		if b.lastSpillKey == c.key {
			b.lastSpillKey = ""
		}
		freed += c.bytes
		if freed >= bytesNeeded {
			break
		}
	}
}

func (s *StateStore) evictGlobalSpillsLocked(bytesNeeded int) {
	type cand struct {
		sid   string
		key   string
		seq   int64
		bytes int
	}
	var cands []cand
	for sid, b := range s.sessionSpills {
		for k, e := range b.spills {
			cands = append(cands, cand{sid: sid, key: k, seq: e.Seq, bytes: e.Bytes})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].seq != cands[j].seq {
			return cands[i].seq < cands[j].seq
		}
		return cands[i].key < cands[j].key
	})
	freed := 0
	for _, c := range cands {
		if b := s.sessionSpills[c.sid]; b != nil {
			delete(b.spills, c.key)
			if b.lastSpillKey == c.key {
				b.lastSpillKey = ""
			}
			if len(b.spills) == 0 && b.seq == 0 {
				delete(s.sessionSpills, c.sid)
			}
		}
		freed += c.bytes
		if freed >= bytesNeeded {
			break
		}
	}
}

func parsePageArgs(args []any) (key string, explicitKey bool, pageNum int, pageSize int, err error) {
	key = lastResultKey
	pageNum = 1
	pageSize = output.PageBytes
	if len(args) == 0 || args[0] == nil {
		return key, false, pageNum, pageSize, nil
	}

	applyOpts := func(m map[string]any, allowPage bool) error {
		if rawKey, ok := m["key"]; ok && rawKey != nil {
			ks, ok := rawKey.(string)
			if !ok {
				return sandbox.NewHostError(sandbox.KindBadArgs, nil, "key must be a string")
			}
			ks = strings.TrimSpace(ks)
			if ks == "" {
				return sandbox.NewHostError(sandbox.KindBadArgs, nil, "key cannot be empty")
			}
			key = ks
			explicitKey = true
		}
		if allowPage {
			if v, ok := toInt(m["page"]); ok {
				pageNum = v
			} else if v, ok := toInt(m["n"]); ok {
				pageNum = v
			}
		}
		if v, ok := toInt(m["size"]); ok && v >= 500 {
			pageSize = v
		}
		return nil
	}

	switch first := args[0].(type) {
	case string:
		ks := strings.TrimSpace(first)
		if ks == "" {
			return "", false, 0, 0, sandbox.NewHostError(sandbox.KindBadArgs, nil, "key cannot be empty")
		}
		key = ks
		explicitKey = true
		if len(args) > 1 && args[1] != nil {
			if m, ok := args[1].(map[string]any); ok {
				if err := applyOpts(m, true); err != nil {
					return "", false, 0, 0, err
				}
			} else if v, ok := toInt(args[1]); ok {
				pageNum = v
			}
		}
		if len(args) > 2 && args[2] != nil {
			if m, ok := args[2].(map[string]any); ok {
				if err := applyOpts(m, false); err != nil {
					return "", false, 0, 0, err
				}
			} else if v, ok := toInt(args[2]); ok && v >= 500 {
				pageSize = v
			}
		}
	case map[string]any:
		if err := applyOpts(first, true); err != nil {
			return "", false, 0, 0, err
		}
	default:
		if v, ok := toInt(first); ok {
			pageNum = v
		}
		if len(args) > 1 && args[1] != nil {
			switch second := args[1].(type) {
			case string:
				ks := strings.TrimSpace(second)
				if ks == "" {
					return "", false, 0, 0, sandbox.NewHostError(sandbox.KindBadArgs, nil, "key cannot be empty")
				}
				key = ks
				explicitKey = true
			case map[string]any:
				if err := applyOpts(second, false); err != nil {
					return "", false, 0, 0, err
				}
			default:
				if v, ok := toInt(second); ok && v >= 500 {
					pageSize = v
				}
			}
		}
		if len(args) > 2 && args[2] != nil {
			if v, ok := toInt(args[2]); ok && v >= 500 {
				pageSize = v
			}
		}
	}
	return key, explicitKey, pageNum, pageSize, nil
}

// StateBinder binds the `state` namespace (`get`, `set`, `delete`, `keys`, `clear`, `page`) into the sandbox.
type StateBinder struct {
	Store     *StateStore
	SessionID string
}

func (b *StateBinder) Bind(env *sandbox.ExecutionEnv) error {
	syncMethods := map[string]sandbox.SyncHostFunc{
		"get": func(_ context.Context, args []any) (any, error) {
			key, err := requireStringArg(args, 0, "key")
			if err != nil {
				return nil, err
			}
			val, _, ok := b.Store.GetResolvedForSession(b.SessionID, key)
			if !ok {
				return nil, nil
			}
			return val, nil
		},
		"set": func(_ context.Context, args []any) (any, error) {
			key, err := requireStringArg(args, 0, "key")
			if err != nil {
				return nil, err
			}
			var val any
			if len(args) > 1 {
				val = args[1]
			}
			evicted, err := b.Store.SetWithEvict(key, val)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"ok": true, "key": key}
			if len(evicted) > 0 {
				env.AddWarning(fmt.Sprintf("state.set: auto-evicted %d oldest key(s) (%s) to stay within the 8MB workspace limit", len(evicted), joinLimited(evicted, 5)))
				out["evicted"] = evicted
			}
			return out, nil
		},
		"delete": func(_ context.Context, args []any) (any, error) {
			key, err := requireStringArg(args, 0, "key")
			if err != nil {
				return nil, err
			}
			return map[string]any{"deleted": b.Store.DeleteForSession(b.SessionID, key)}, nil
		},
		"keys": func(_ context.Context, _ []any) (any, error) {
			return b.Store.KeysForSession(b.SessionID), nil
		},
		"clear": func(_ context.Context, _ []any) (any, error) {
			b.Store.ClearForSession(b.SessionID)
			return map[string]any{"ok": true}, nil
		},
		// state.page(key, n?, size?) or state.page(n, size?) returns page n (negative = from the end)
		// of a spilled/stored value with a footer locked to the concrete key so concurrent calls never mix.
		"page": func(_ context.Context, args []any) (any, error) {
			key, explicitKey, n, size, err := parsePageArgs(args)
			if err != nil {
				return nil, err
			}
			val, resolvedKey, ok := b.Store.GetResolvedForSession(b.SessionID, key)
			if !ok {
				if explicitKey && key != lastResultKey {
					return nil, sandbox.NewHostError(sandbox.KindNotFound, nil, "key '%s' not found in this workspace (spills are scoped to the session and cwd that produced them, or may have been evicted/cleared)", key)
				}
				return nil, sandbox.NewHostError(sandbox.KindNotFound, nil, "nothing to page yet (pages the last truncated output)")
			}
			text, isStr := val.(string)
			if !isStr {
				text = output.MarshalPretty(val)
			}
			pages := output.Paginate(text, size)
			origN := n
			if n < 0 {
				n = len(pages) + n + 1
			}
			if n < 1 {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "page out of range (1..%d)", len(pages))
			}
			footerKey := ""
			if resolvedKey != "" && resolvedKey != lastResultKey {
				footerKey = resolvedKey
			}
			displayKey := footerKey
			if displayKey == "" {
				displayKey = key
			}
			if n > len(pages) {
				env.AddWarning(fmt.Sprintf("state.page: requested page %d exceeds total %d page(s) for '%s' (size=%d)", origN, len(pages), displayKey, size))
				return fmt.Sprintf("[end of %s — requested page %d exceeds %d total page(s)]", displayKey, origN, len(pages)), nil
			}
			total := pages[len(pages)-1].LastLine
			return pages[n-1].Text + "\n" + output.PageFooter(pages[n-1], n, len(pages), total, size, footerKey), nil
		},
	}
	return env.SetNamespace("state", nil, syncMethods)
}
