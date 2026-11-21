package capabilities

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

type fileLockEntry struct {
	owner string
	refs  int
}

// FileLockTable serializes concurrent file writes/edits/transactions on overlapping files across parallel `exec` calls
// so an uncommitted transaction's rollback can never clobber another concurrent call's committed write.
// Same-owner acquisitions are ref-counted and re-entrant (so `tx` + `fs.write`/`edit.replace` inside one `exec`
// never self-deadlock), and cross-exec wait cycles are detected immediately across multi-edge wait graphs.
type FileLockTable struct {
	mu      sync.Mutex
	entries map[string]*fileLockEntry // normalized absPath -> {owner, refs}
	waitFor map[string]map[string]int // waiting ownerID -> blocker ownerID -> active wait count
	wakeCh  chan struct{}
}

// NewFileLockTable creates a process-shared file lock coordinator.
func NewFileLockTable() *FileLockTable {
	return &FileLockTable{
		entries: map[string]*fileLockEntry{},
		waitFor: map[string]map[string]int{},
		wakeCh:  make(chan struct{}),
	}
}

// Acquire atomically claims all requested paths for ownerID (re-entrant with reference counting).
func (l *FileLockTable) Acquire(ctx context.Context, ownerID string, paths ...string) (func(), error) {
	return l.AcquireIn(ctx, "", ownerID, paths...)
}

// AcquireIn atomically claims all requested paths for ownerID and formats any conflict/timeout error
// relative to cwd with original filename casing preserved.
func (l *FileLockTable) AcquireIn(ctx context.Context, cwd, ownerID string, paths ...string) (func(), error) {
	noop := func() {}
	if l == nil || ownerID == "" || len(paths) == 0 {
		return noop, nil
	}
	keys := make([]string, 0, len(paths))
	displayByKey := make(map[string]string, len(paths))
	for _, p := range paths {
		k := normalizeLockKey(p)
		if k != "" {
			if _, dup := displayByKey[k]; !dup {
				keys = append(keys, k)
				displayByKey[k] = formatLockDisplayPath(cwd, p)
			}
		}
	}
	if len(keys) == 0 {
		return noop, nil
	}
	sort.Strings(keys)

	var activeBlocker string
	clearWait := func() {
		if activeBlocker != "" {
			l.removeWaitEdgeLocked(ownerID, activeBlocker)
			activeBlocker = ""
		}
	}

	for {
		l.mu.Lock()
		var blockerKey, blockerOwner string
		for _, k := range keys {
			if e, held := l.entries[k]; held && e.owner != ownerID {
				blockerKey = k
				blockerOwner = e.owner
				break
			}
		}
		if blockerOwner == "" {
			clearWait()
			for _, k := range keys {
				if e, ok := l.entries[k]; ok {
					e.refs++
				} else {
					l.entries[k] = &fileLockEntry{owner: ownerID, refs: 1}
				}
			}
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					l.releaseKeys(ownerID, keys)
				})
			}, nil
		}

		// Detect wait-for cycle across concurrent execs immediately instead of hanging until timeout.
		if l.hasWaitCycleLocked(ownerID, blockerOwner) {
			clearWait()
			l.mu.Unlock()
			return noop, sandbox.NewHostError(
				sandbox.KindError,
				nil,
				"deadlock detected acquiring file lock on %s (held by %s); acquire files together in one t.edit([...]) call",
				displayByKey[blockerKey], blockerOwner,
			)
		}
		if activeBlocker != blockerOwner {
			clearWait()
			l.addWaitEdgeLocked(ownerID, blockerOwner)
			activeBlocker = blockerOwner
		}
		ch := l.wakeCh
		l.mu.Unlock()

		select {
		case <-ch:
			continue
		case <-ctx.Done():
			l.mu.Lock()
			clearWait()
			l.mu.Unlock()
			return noop, fmt.Errorf("timed out waiting for file lock on %s (held by %s): %w", displayByKey[blockerKey], blockerOwner, ctx.Err())
		}
	}
}

func (l *FileLockTable) addWaitEdgeLocked(waiter, blocker string) {
	edges := l.waitFor[waiter]
	if edges == nil {
		edges = map[string]int{}
		l.waitFor[waiter] = edges
	}
	edges[blocker]++
}

func (l *FileLockTable) removeWaitEdgeLocked(waiter, blocker string) {
	edges := l.waitFor[waiter]
	if edges == nil {
		return
	}
	edges[blocker]--
	if edges[blocker] <= 0 {
		delete(edges, blocker)
	}
	if len(edges) == 0 {
		delete(l.waitFor, waiter)
	}
}

func (l *FileLockTable) hasWaitCycleLocked(waiter, blocker string) bool {
	visited := map[string]bool{blocker: true}
	queue := []string{blocker}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == waiter {
			return true
		}
		for next, cnt := range l.waitFor[cur] {
			if cnt > 0 && !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

func (l *FileLockTable) releaseKeys(ownerID string, keys []string) {
	if l == nil || ownerID == "" || len(keys) == 0 {
		return
	}
	l.mu.Lock()
	released := false
	for _, k := range keys {
		if e, ok := l.entries[k]; ok && e.owner == ownerID {
			e.refs--
			if e.refs <= 0 {
				delete(l.entries, k)
				released = true
			}
		}
	}
	if released {
		close(l.wakeCh)
		l.wakeCh = make(chan struct{})
	}
	l.mu.Unlock()
}

// ReleaseOwner force-releases every file lock currently held by ownerID and wakes waiting writers.
func (l *FileLockTable) ReleaseOwner(ownerID string) {
	if l == nil || ownerID == "" {
		return
	}
	l.mu.Lock()
	delete(l.waitFor, ownerID)
	released := false
	for k, e := range l.entries {
		if e.owner == ownerID {
			delete(l.entries, k)
			released = true
		}
	}
	if released {
		close(l.wakeCh)
		l.wakeCh = make(chan struct{})
	}
	l.mu.Unlock()
}

func lockOwnerForEnv(env *sandbox.ExecutionEnv) string {
	if env != nil && env.ExecID != "" {
		return env.ExecID
	}
	return fmt.Sprintf("fsop:%d", txOwnerSeq.Add(1))
}

func formatLockDisplayPath(cwd, p string) string {
	cleaned := filepath.Clean(p)
	if cwd != "" {
		if rel, err := filepath.Rel(cwd, cleaned); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(cleaned)
}

func normalizeLockKey(p string) string {
	s := filepath.ToSlash(filepath.Clean(p))
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		s = strings.ToLower(s)
	}
	return s
}
