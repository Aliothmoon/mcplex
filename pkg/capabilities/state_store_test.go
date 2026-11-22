package capabilities

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestSaveAutoSpillConcurrentAtomicityAndIsolation(t *testing.T) {
	store := NewStateStore(t.TempDir(), t.TempDir())
	const workers = 32
	const perWorker = 8

	type item struct {
		key string
		val string
	}
	results := make(chan item, workers*perWorker)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				payload := fmt.Sprintf("worker-%02d-iter-%02d-%s", id, i, strings.Repeat("x", 256))
				key := store.SaveAutoSpill(payload)
				if key == "" {
					t.Errorf("worker %d iter %d got empty spill key", id, i)
					return
				}
				results <- item{key: key, val: payload}
				// Interleave concurrent reads and key listings while writers are active.
				got, resolved, ok := store.GetResolved(key)
				if !ok || resolved != key || got != payload {
					t.Errorf("concurrent read mismatch for %s: ok=%v resolved=%s", key, ok, resolved)
				}
				_ = store.Get("__last_result")
				_ = store.Keys()
			}
		}(w)
	}
	wg.Wait()
	close(results)

	byKey := map[string]string{}
	maxSeq := 0
	for it := range results {
		if prev, dup := byKey[it.key]; dup {
			t.Fatalf("duplicate spill key %s: %q vs %q", it.key, prev, it.val)
		}
		byKey[it.key] = it.val
		n, err := strconv.Atoi(strings.TrimPrefix(it.key, "__result_"))
		if err != nil {
			t.Fatalf("unexpected spill key format %q: %v", it.key, err)
		}
		if n > maxSeq {
			maxSeq = n
		}
	}

	if len(byKey) != workers*perWorker {
		t.Fatalf("expected %d distinct spill keys, got %d", workers*perWorker, len(byKey))
	}

	// __last_result must atomically point to the highest sequence number (never inverted!).
	wantLastKey := fmt.Sprintf("__result_%d", maxSeq)
	gotLast, resolved, ok := store.GetResolved("__last_result")
	if !ok {
		t.Fatal("__last_result missing after concurrent spills")
	}
	if resolved != wantLastKey {
		t.Fatalf("__last_result resolved to %s, want highest seq %s (order inversion!)", resolved, wantLastKey)
	}
	if gotLast != byKey[wantLastKey] {
		t.Fatalf("__last_result payload mismatch for %s", wantLastKey)
	}
}

func TestAutoSpillEvictionIsStrictFIFOAndNeverEvictsNewestOrBlocksOnUserKV(t *testing.T) {
	store := NewStateStore("", "")

	// Fill user KV close to MaxStateTotalBytes (3.5 MB).
	bigUser := strings.Repeat("u", 450*1024)
	for i := 0; i < 8; i++ {
		if err := store.Set(fmt.Sprintf("user_%d", i), bigUser); err != nil {
			t.Fatalf("user Set %d failed: %v", i, err)
		}
	}

	// Rapidly write 12 spills of 450 KB each (~5.4 MB > MaxSpillTotalBytes=4 MB) inside the same millisecond.
	// Every spill must succeed, never evict the newest spill, and evict oldest (__result_1, __result_2...) first.
	keys := make([]string, 12)
	for i := 0; i < 12; i++ {
		payload := fmt.Sprintf("SPILL-%02d-", i) + strings.Repeat("s", 450*1024)
		k := store.SaveAutoSpill(payload)
		if k == "" {
			t.Fatalf("spill %d failed despite dedicated spill pool", i)
		}
		keys[i] = k
		// Immediately after writing, both the spill key and __last_result must return the newly written payload.
		if got := store.Get(k); got != payload {
			t.Fatalf("spill %d (%s) was evicted or corrupted immediately after write", i, k)
		}
		if gotLast, res, ok := store.GetResolved("__last_result"); !ok || res != k || gotLast != payload {
			t.Fatalf("__last_result mismatch right after spill %d: ok=%v res=%s", i, ok, res)
		}
	}

	// Oldest spills were evicted, newest spills remain intact, and user KV is untouched.
	if store.Get(keys[0]) != nil {
		t.Fatalf("expected oldest spill %s to be evicted", keys[0])
	}
	for i := 5; i < 12; i++ {
		if store.Get(keys[i]) == nil {
			t.Fatalf("expected recent spill %s to still be present", keys[i])
		}
	}
	if store.Get("user_0") == nil || store.Get("user_7") == nil {
		t.Fatal("auto-spill eviction must never delete user KV")
	}
}

func TestAutoSpillsDoNotPersistOrLeakAcrossProcessesAndStripLegacySpills(t *testing.T) {
	dataDir := t.TempDir()
	ws := t.TempDir()

	s1 := NewStateStore(dataDir, ws)
	if err := s1.Set("persisted", "hello"); err != nil {
		t.Fatal(err)
	}
	spillKey := s1.SaveAutoSpill("process-1-secret-spill")
	if spillKey != "__result_1" {
		t.Fatalf("expected __result_1, got %s", spillKey)
	}
	s1.Flush()

	// Process 2 opening the same workspace must see persisted user KV, but MUST NOT inherit Process 1's spills.
	s2 := NewStateStore(dataDir, ws)
	if got := s2.Get("persisted"); got != "hello" {
		t.Fatalf("persisted user key lost: %v", got)
	}
	if got := s2.Get("__last_result"); got != nil {
		t.Fatalf("process 2 leaked process 1 __last_result: %v", got)
	}
	if got := s2.Get("__result_1"); got != nil {
		t.Fatalf("process 2 leaked process 1 __result_1: %v", got)
	}

	// Verify legacy v0.2.0 disk files containing __last_result and __result_1 are cleanly ignored on load.
	raw, err := os.ReadFile(s1.filePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "process-1-secret-spill") {
		t.Fatal("auto-spill must not be serialized to shared session file")
	}
	legacy := `{"persisted":{"value":"hello","bytes":7,"updatedAt":"2026-01-01T00:00:00Z"},"__last_result":{"value":"old","bytes":5,"updatedAt":"2026-01-01T00:00:00Z"},"__result_1":{"value":"old","bytes":5,"updatedAt":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Clean(s1.filePath), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s3 := NewStateStore(dataDir, ws)
	if s3.Get("persisted") != "hello" {
		t.Fatalf("expected persisted key from legacy file, got %v", s3.Get("persisted"))
	}
	if s3.Get("__last_result") != nil || s3.Get("__result_1") != nil {
		t.Fatalf("legacy disk spills must not be loaded into new session")
	}
}

func TestReservedSpillKeysRejectedAndDeleteKeysWork(t *testing.T) {
	store := NewStateStore("", "")
	k1 := store.SaveAutoSpill("spilled-1")
	if !slices.Contains(store.Keys(), "__last_result") || !slices.Contains(store.Keys(), k1) {
		t.Fatalf("expected __last_result and %s in Keys(), got %v", k1, store.Keys())
	}

	// Reserved spill keys must be rejected by Set.
	for _, reserved := range []string{"__last_result", "__result_1", "__result_99"} {
		if err := store.Set(reserved, "manual"); err == nil {
			t.Fatalf("expected Set(%q) to fail as reserved key", reserved)
		}
	}

	// Subsequent auto-spill updates __last_result cleanly.
	k2 := store.SaveAutoSpill("spilled-2")
	if val, res, ok := store.GetResolved("__last_result"); !ok || res != k2 || val != "spilled-2" {
		t.Fatalf("spill failed: ok=%v res=%s val=%v", ok, res, val)
	}

	// Deleting __last_result clears the alias without deleting __result_2.
	if !store.Delete("__last_result") {
		t.Fatal("expected Delete(__last_result) to return true")
	}
	if store.Get("__last_result") != nil {
		t.Fatal("__last_result should be nil after delete")
	}
	if store.Get(k2) != "spilled-2" {
		t.Fatalf("%s should remain intact after deleting __last_result alias", k2)
	}

	// Verify >512KB files read via fs.read (up to 2MB, e.g. 1.2MB build.ninja) store cleanly in state.set.
	largeNinja := strings.Repeat("build obj/foo.o: cc src/foo.c\n", 40000) // ~1.2 MB
	if err := store.Set("ninja", largeNinja); err != nil {
		t.Fatalf("expected 1.2MB build.ninja string to fit in state.set, got: %v", err)
	}
	if store.Get("ninja") != largeNinja {
		t.Fatal("1.2MB state value round-trip mismatch")
	}
}
