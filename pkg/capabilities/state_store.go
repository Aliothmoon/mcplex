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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

const (
	MaxStateEntryBytes = 512 * 1024      // 512 KB per persisted user key
	MaxStateTotalBytes = 4 * 1024 * 1024 // 4 MB per workspace session (persisted user entries)
	MaxSpillEntryBytes = 4 * 1024 * 1024 // 4 MB per in-memory auto-spill entry
	MaxSpillTotalBytes = 4 * 1024 * 1024 // 4 MB in-memory cap for process-local auto-spills
	lastResultKey      = "__last_result"
	spillKeyPrefix     = "__result_"
)

// StateStore provides fast in-memory + disk-persisted key-value storage across PTC turns.
// User-set entries persist per workspace; auto-spills (`__result_N` / `__last_result` alias)
// live in a dedicated process-local ring so concurrent sessions never cross-contaminate.
type StateStore struct {
	mu           sync.RWMutex
	dataDir      string
	filePath     string
	entries      map[string]stateEntry // persisted user KV
	spills       map[string]stateEntry // process-local auto-spills (__result_N)
	seq          int64
	saveSeq      int64
	lastSpillKey string // alias target for "__last_result" unless manually overridden via Set
	dirty        bool   // pending user KV changes; persisted by Flush (once per exec)
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

// NewStateStore creates or loads a workspace-scoped state store under dataDir.
func NewStateStore(dataDir, workspaceDir string) *StateStore {
	s := &StateStore{
		dataDir: dataDir,
		entries: map[string]stateEntry{},
		spills:  map[string]stateEntry{},
	}
	if dataDir != "" {
		h := sha256.Sum256([]byte(strings.ToLower(filepath.ToSlash(filepath.Clean(workspaceDir)))))
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
		for k := range loaded {
			if k == lastResultKey || isAutoSpillKey(k) {
				delete(loaded, k)
			}
		}
		s.entries = loaded
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

// Set stores a JSON-serializable value under key.
func (s *StateStore) Set(key string, val any) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("state key cannot be empty")
	}
	if val == nil {
		s.Delete(key)
		return nil
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("state value must be JSON-serializable: %w", err)
	}
	if len(raw) > MaxStateEntryBytes {
		return fmt.Errorf("state value for '%s' (%d bytes) exceeds per-key limit (%d bytes)", key, len(raw), MaxStateEntryBytes)
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
	if total > MaxStateTotalBytes {
		return fmt.Errorf("state store total size (%d bytes) exceeds session limit (%d bytes); call state.clear() or state.delete(key)", total, MaxStateTotalBytes)
	}

	s.entries[key] = stateEntry{
		Value:     detached,
		Bytes:     len(raw),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	// Keep user KV and spill map disjoint so manual overrides behave predictably.
	delete(s.spills, key)
	if key == lastResultKey || key == s.lastSpillKey {
		s.lastSpillKey = ""
	}
	s.dirty = true
	return nil
}

// SaveAutoSpill atomically saves an oversized return value into the in-memory spill pool
// under a unique `__result_N` key and updates the `__last_result` alias in the same lock.
func (s *StateStore) SaveAutoSpill(val any) string {
	raw, err := json.Marshal(val)
	if err != nil || len(raw) > MaxSpillEntryBytes {
		return ""
	}
	detached := detachValue(val, raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	total := len(raw)
	for _, e := range s.spills {
		total += e.Bytes
	}
	if total > MaxSpillTotalBytes {
		s.evictSpillsLocked(total - MaxSpillTotalBytes)
	}

	s.seq++
	id := fmt.Sprintf("%s%d", spillKeyPrefix, s.seq)
	s.spills[id] = stateEntry{
		Value:     detached,
		Bytes:     len(raw),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Seq:       s.seq,
	}
	s.lastSpillKey = id
	// If the user previously set an explicit manual "__last_result", let the new spill supersede it.
	if _, existed := s.entries[lastResultKey]; existed {
		delete(s.entries, lastResultKey)
		s.dirty = true
	}
	return id
}

// GetResolved retrieves a detached copy of key's value along with the concrete key it resolved to
// (for example, "__last_result" resolves to "__result_N" when backed by an auto-spill).
func (s *StateStore) GetResolved(key string) (val any, resolvedKey string, found bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, "", false
	}
	s.mu.RLock()
	var e stateEntry
	if entry, ok := s.entries[key]; ok {
		e = entry
		resolvedKey = key
		found = true
	} else if entry, ok := s.spills[key]; ok {
		e = entry
		resolvedKey = key
		found = true
	} else if key == lastResultKey && s.lastSpillKey != "" {
		if entry, ok := s.spills[s.lastSpillKey]; ok {
			e = entry
			resolvedKey = s.lastSpillKey
			found = true
		}
	}
	s.mu.RUnlock()
	if !found {
		return nil, "", false
	}
	return cloneValue(e.Value), resolvedKey, true
}

// Get retrieves a detached copy of key's value (or nil if absent).
func (s *StateStore) Get(key string) any {
	val, _, ok := s.GetResolved(key)
	if !ok {
		return nil
	}
	return val
}

// Delete removes a key from the store.
func (s *StateStore) Delete(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existed := false
	if _, ok := s.entries[key]; ok {
		delete(s.entries, key)
		s.dirty = true
		existed = true
	}
	if _, ok := s.spills[key]; ok {
		delete(s.spills, key)
		existed = true
		if s.lastSpillKey == key {
			s.lastSpillKey = ""
		}
	}
	if key == lastResultKey && s.lastSpillKey != "" {
		s.lastSpillKey = ""
		existed = true
	}
	return existed
}

// Keys lists all keys currently readable in the store.
func (s *StateStore) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.entries)+len(s.spills)+1)
	for k := range s.entries {
		out = append(out, k)
	}
	for k := range s.spills {
		out = append(out, k)
	}
	if s.lastSpillKey != "" {
		if _, ok := s.spills[s.lastSpillKey]; ok {
			if _, manual := s.entries[lastResultKey]; !manual {
				out = append(out, lastResultKey)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Clear wipes all persisted entries and in-memory auto-spills in the store.
func (s *StateStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) > 0 {
		s.dirty = true
	}
	s.entries = map[string]stateEntry{}
	s.spills = map[string]stateEntry{}
	s.lastSpillKey = ""
}

func (s *StateStore) evictSpillsLocked(bytesNeeded int) {
	type cand struct {
		key   string
		seq   int64
		bytes int
	}
	cands := make([]cand, 0, len(s.spills))
	for k, e := range s.spills {
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
		delete(s.spills, c.key)
		if s.lastSpillKey == c.key {
			s.lastSpillKey = ""
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
	Store *StateStore
}

func (b *StateBinder) Bind(env *sandbox.ExecutionEnv) error {
	syncMethods := map[string]sandbox.SyncHostFunc{
		"get": func(_ context.Context, args []any) (any, error) {
			key, err := requireStringArg(args, 0, "key")
			if err != nil {
				return nil, err
			}
			return b.Store.Get(key), nil
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
			if err := b.Store.Set(key, val); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true, "key": key}, nil
		},
		"delete": func(_ context.Context, args []any) (any, error) {
			key, err := requireStringArg(args, 0, "key")
			if err != nil {
				return nil, err
			}
			return map[string]any{"deleted": b.Store.Delete(key)}, nil
		},
		"keys": func(_ context.Context, _ []any) (any, error) {
			return b.Store.Keys(), nil
		},
		"clear": func(_ context.Context, _ []any) (any, error) {
			b.Store.Clear()
			return map[string]any{"ok": true}, nil
		},
		// state.page(key, n?, size?) or state.page(n, size?) returns page n (negative = from the end)
		// of a spilled/stored value with a footer locked to the concrete key so concurrent calls never mix.
		"page": func(_ context.Context, args []any) (any, error) {
			key, explicitKey, n, size, err := parsePageArgs(args)
			if err != nil {
				return nil, err
			}
			val, resolvedKey, ok := b.Store.GetResolved(key)
			if !ok {
				if explicitKey && key != lastResultKey {
					return nil, sandbox.NewHostError(sandbox.KindNotFound, nil, "key '%s' not found in this workspace (spills are scoped to the cwd that produced them, or may have been evicted/cleared)", key)
				}
				return nil, sandbox.NewHostError(sandbox.KindNotFound, nil, "nothing to page yet (pages the last truncated output)")
			}
			text, isStr := val.(string)
			if !isStr {
				text = output.MarshalPretty(val)
			}
			pages := output.Paginate(text, size)
			if n < 0 {
				n = len(pages) + n + 1
			}
			if n < 1 || n > len(pages) {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "page out of range (1..%d)", len(pages))
			}
			footerKey := ""
			if resolvedKey != "" && resolvedKey != lastResultKey {
				footerKey = resolvedKey
			}
			total := pages[len(pages)-1].LastLine
			return pages[n-1].Text + "\n" + output.PageFooter(pages[n-1], n, len(pages), total, size, footerKey), nil
		},
	}
	return env.SetNamespace("state", nil, syncMethods)
}
