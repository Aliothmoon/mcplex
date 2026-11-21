package capabilities

import (
	"context"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// FileLockTable serializes concurrent edit/tx writes on overlapping files across parallel `exec` calls
// so an uncommitted transaction's rollback can never clobber another concurrent call's committed write.
type FileLockTable struct {
	mu     sync.Mutex
	owners map[string]string // normalized absPath -> ownerID
	wakeCh chan struct{}
}

// NewFileLockTable creates a process-shared file lock coordinator.
func NewFileLockTable() *FileLockTable {
	return &FileLockTable{
		owners: map[string]string{},
		wakeCh: make(chan struct{}),
	}
}

// Acquire atomically claims all requested paths for ownerID (re-entrant for paths already owned by ownerID).
// Waits cleanly until all paths are free or ctx expires/cancels.
func (l *FileLockTable) Acquire(ctx context.Context, ownerID string, paths ...string) error {
	if l == nil || ownerID == "" || len(paths) == 0 {
		return nil
	}
	keys := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		k := normalizeLockKey(p)
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for {
		l.mu.Lock()
		conflict := false
		for _, k := range keys {
			if cur, held := l.owners[k]; held && cur != ownerID {
				conflict = true
				break
			}
		}
		if !conflict {
			for _, k := range keys {
				l.owners[k] = ownerID
			}
			l.mu.Unlock()
			return nil
		}
		ch := l.wakeCh
		l.mu.Unlock()

		select {
		case <-ch:
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ReleaseOwner releases every file lock currently held by ownerID and wakes waiting writers.
func (l *FileLockTable) ReleaseOwner(ownerID string) {
	if l == nil || ownerID == "" {
		return
	}
	l.mu.Lock()
	released := false
	for k, cur := range l.owners {
		if cur == ownerID {
			delete(l.owners, k)
			released = true
		}
	}
	if released {
		close(l.wakeCh)
		l.wakeCh = make(chan struct{})
	}
	l.mu.Unlock()
}

func normalizeLockKey(p string) string {
	s := filepath.ToSlash(filepath.Clean(p))
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		s = strings.ToLower(s)
	}
	return s
}
