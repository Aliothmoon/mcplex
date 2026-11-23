package capabilities

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aliothmoon/mcplex/pkg/events"
	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

// EditHunk represents a single non-overlapping replacement inside a file.
type EditHunk struct {
	Target        string `json:"target"`
	EndTarget     string `json:"endTarget,omitempty"` // optional end boundary anchor: replaces closed span [target .. endTarget]
	Replacement   string `json:"replacement"`
	StartLine     int    `json:"startLine,omitempty"`
	EndLine       int    `json:"endLine,omitempty"`
	ReplaceAll    bool   `json:"replaceAll,omitempty"`
	Nth           int    `json:"nth,omitempty"`           // 1-based: replace only the Nth match (like sed s/x/y/N)
	Symbol        string `json:"symbol,omitempty"`        // restrict matching to a symbol's lines, e.g. "Editor::on_escape"
	ReplaceSymbol bool   `json:"replaceSymbol,omitempty"` // replace entire resolved symbol span (0 target tokens)
}

// FileEditSpec describes edits or creation for one file.
type FileEditSpec struct {
	Path      string     `json:"path"`
	Hunks     []EditHunk `json:"hunks,omitempty"`
	Create    *string    `json:"create,omitempty"`
	Overwrite bool       `json:"overwrite,omitempty"`
	EOL       string     `json:"eol,omitempty"`
}

type plannedFileWrite struct {
	absPath      string
	relPath      string
	existed      bool
	mode         os.FileMode
	origBytes    []byte
	newBytes     []byte
	hunksApplied int
	lineDelta    int
	spanLabels   []string
}

type fileSnapshot struct {
	absPath   string
	relPath   string
	existed   bool
	mode      os.FileMode
	origBytes []byte
}

type activeTx struct {
	id         string
	lockOwner  string
	releases   []func()
	snapshots  map[string]fileSnapshot
	touched    []string
	committed  bool
	rolledBack bool
	lastVerify map[string]any
}

func (tx *activeTx) releaseLocks() {
	for _, rel := range tx.releases {
		if rel != nil {
			rel()
		}
	}
	tx.releases = nil
}

var txOwnerSeq atomic.Int64

// EditTxBinder exposes `edit.*` (`apply`, `replace`) and `tx.*` (`begin` -> atomic transactions with rollback).
type EditTxBinder struct {
	Policy      *policy.Engine
	ShellRunner *ShellGitBinder
	Locks       *FileLockTable
	Events      *events.Hub
	SessionID   string
	RunID       string
}

func (b *EditTxBinder) Bind(env *sandbox.ExecutionEnv) error {
	var txMu sync.Mutex
	txSeq := 0
	txMap := map[string]*activeTx{}

	// Automatic safety net: any uncommitted transaction at the end of `exec` automatically rolls back,
	// and the outcome (including files that could NOT be restored) is surfaced to the model as a warning.
	env.RegisterCleanup(func() {
		txMu.Lock()
		defer txMu.Unlock()
		ids := make([]string, 0, len(txMap))
		for id := range txMap {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			tx := txMap[id]
			if tx.committed || tx.rolledBack {
				tx.releaseLocks()
				continue
			}
			restored, failed := rollbackSnapshots(tx)
			tx.releaseLocks()
			emitTxStatus(b.Events, b.SessionID, b.RunID, id, "rolled_back", restored, "auto-rolled back (uncommitted)")
			if len(restored) > 0 {
				env.AddWarning(fmt.Sprintf("%s was never committed — auto-rolled back %d file(s): %s (call `await t.commit()` to keep tx edits)", id, len(restored), joinLimited(restored, 10)))
			}
			if len(failed) > 0 {
				env.AddWarning(fmt.Sprintf("ROLLBACK FAILED for %s: %s — these files may still contain uncommitted tx edits; inspect them before retrying", id, formatRollbackFailures(failed)))
			}
		}
		b.Locks.ReleaseOwner(env.ExecID)
	})

	editAsync := map[string]sandbox.AsyncHostFunc{
		"apply": func(ctx context.Context, args []any) (any, error) {
			if len(args) == 0 || args[0] == nil {
				return nil, errors.New("edit.apply requires an options object or array of file edits")
			}
			specs, dryRun, bestEffort, verifyCmd, err := parseEditApplyArgs(args)
			if err != nil {
				return nil, err
			}
			res, err := b.applyFileSpecsWithOwner(ctx, env.Cwd, env.ExecID, specs, dryRun, bestEffort, nil)
			if err != nil {
				return nil, err
			}
			if verifyCmd != "" && !dryRun && b.ShellRunner != nil {
				vRes, vErr := b.ShellRunner.RunSingleCommand(ctx, env.Cwd, verifyCmd, 60, true, true)
				if vErr != nil {
					return nil, vErr
				}
				res["verify"] = vRes
				if vOk, _ := vRes["ok"].(bool); !vOk {
					res["ok"] = false
				}
			}
			return res, nil
		},
		"replace": func(ctx context.Context, args []any) (any, error) {
			if len(args) < 3 {
				return nil, errors.New("edit.replace requires (path, target, replacement, opts?)")
			}
			p, _ := args[0].(string)
			target, _ := args[1].(string)
			repl, _ := args[2].(string)
			var opts map[string]any
			if len(args) > 3 {
				opts, _ = args[3].(map[string]any)
			}
			spec := FileEditSpec{
				Path: p,
				Hunks: []EditHunk{{
					Target:      target,
					EndTarget:   firstString(opts, "endTarget", "end_target"),
					Replacement: repl,
					StartLine:   optInt(opts, "startLine", 0),
					EndLine:     optInt(opts, "endLine", 0),
					ReplaceAll:  optBool(opts, "replaceAll", optBool(opts, "all", false)),
					Nth:         optInt(opts, "nth", optInt(opts, "occurrence", 0)),
					Symbol:      firstString(opts, "symbol", "in"),
				}},
			}
			return b.applyFileSpecsWithOwner(ctx, env.Cwd, env.ExecID, []FileEditSpec{spec}, false, false, nil)
		},
		"replaceSymbol": func(ctx context.Context, args []any) (any, error) {
			if len(args) < 3 {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "edit.replaceSymbol requires (path, symbol, replacement)")
			}
			p, _ := args[0].(string)
			sym, _ := args[1].(string)
			repl, ok := args[2].(string)
			if !ok {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "edit.replaceSymbol replacement must be a string")
			}
			if strings.TrimSpace(sym) == "" {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "edit.replaceSymbol requires non-empty symbol")
			}
			spec := FileEditSpec{
				Path: p,
				Hunks: []EditHunk{{
					Symbol:        sym,
					ReplaceSymbol: true,
					Replacement:   repl,
				}},
			}
			return b.applyFileSpecsWithOwner(ctx, env.Cwd, env.ExecID, []FileEditSpec{spec}, false, false, nil)
		},
	}

	if err := env.SetNamespace("edit", editAsync, nil); err != nil {
		return err
	}

	txAsync := map[string]sandbox.AsyncHostFunc{
		"_begin": func(_ context.Context, _ []any) (any, error) {
			txMu.Lock()
			defer txMu.Unlock()
			txSeq++
			id := fmt.Sprintf("tx_%d", txSeq)
			owner := env.ExecID
			if owner == "" {
				owner = fmt.Sprintf("tx:%d:%s", txOwnerSeq.Add(1), id)
			}
			txMap[id] = &activeTx{
				id:        id,
				lockOwner: owner,
				snapshots: map[string]fileSnapshot{},
				touched:   []string{},
			}
			return map[string]any{"id": id, "status": "open"}, nil
		},
		"_edit": func(ctx context.Context, args []any) (any, error) {
			id, _ := requireStringArg(args, 0, "id")
			txMu.Lock()
			tx, ok := txMap[id]
			txMu.Unlock()
			if !ok || tx.committed || tx.rolledBack {
				return nil, fmt.Errorf("transaction '%s' is not active", id)
			}
			specs, err := parseTxEditArgs(args[1:])
			if err != nil {
				return nil, err
			}
			txMu.Lock()
			defer txMu.Unlock()
			return b.applyFileSpecsWithOwner(ctx, env.Cwd, env.ExecID, specs, false, false, tx)
		},
		"_replaceSymbol": func(ctx context.Context, args []any) (any, error) {
			id, _ := requireStringArg(args, 0, "id")
			if len(args) < 4 {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "t.replaceSymbol requires (path, symbol, replacement)")
			}
			p, _ := args[1].(string)
			sym, _ := args[2].(string)
			repl, ok := args[3].(string)
			if !ok {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "t.replaceSymbol replacement must be a string")
			}
			if strings.TrimSpace(sym) == "" {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "t.replaceSymbol requires non-empty symbol")
			}
			txMu.Lock()
			tx, active := txMap[id]
			if !active || tx.committed || tx.rolledBack {
				txMu.Unlock()
				return nil, fmt.Errorf("transaction '%s' is not active", id)
			}
			defer txMu.Unlock()
			spec := FileEditSpec{
				Path: p,
				Hunks: []EditHunk{{
					Symbol:        sym,
					ReplaceSymbol: true,
					Replacement:   repl,
				}},
			}
			return b.applyFileSpecsWithOwner(ctx, env.Cwd, env.ExecID, []FileEditSpec{spec}, false, false, tx)
		},
		"_write": func(ctx context.Context, args []any) (any, error) {
			id, _ := requireStringArg(args, 0, "id")
			if len(args) < 3 {
				return nil, errors.New("tx.write requires (path, content)")
			}
			p, _ := args[1].(string)
			content, _ := args[2].(string)
			eol := firstString(optsAt(args, 3), "eol")
			txMu.Lock()
			tx, ok := txMap[id]
			if !ok || tx.committed || tx.rolledBack {
				txMu.Unlock()
				return nil, fmt.Errorf("transaction '%s' is not active", id)
			}
			defer txMu.Unlock()
			spec := FileEditSpec{Path: p, Create: &content, Overwrite: true, EOL: eol}
			return b.applyFileSpecsWithOwner(ctx, env.Cwd, env.ExecID, []FileEditSpec{spec}, false, false, tx)
		},
		"_verify": func(ctx context.Context, args []any) (any, error) {
			id, _ := requireStringArg(args, 0, "id")
			txMu.Lock()
			tx, ok := txMap[id]
			txMu.Unlock()
			if !ok || tx.committed || tx.rolledBack {
				return nil, fmt.Errorf("transaction '%s' is not active", id)
			}
			if b.ShellRunner == nil {
				return nil, errors.New("shell runner unavailable for tx.verify")
			}
			if len(args) < 2 {
				return nil, errors.New("tx.verify requires commands array or command string")
			}
			var checkArgs []any
			if s, isStr := args[1].(string); isStr {
				checkArgs = []any{[]any{map[string]any{"command": s}}}
			} else {
				checkArgs = args[1:]
			}
			warnChecksTimeoutUnit(env, "t.verify", checkArgs)
			res, err := b.ShellRunner.RunChecksBatch(ctx, env.Cwd, checkArgs)
			if err != nil {
				return nil, err
			}
			warnTruncatedChecks(env, "t.verify", res)
			txMu.Lock()
			tx.lastVerify = res
			txMu.Unlock()
			return res, nil
		},
		"_commit": func(_ context.Context, args []any) (any, error) {
			id, _ := requireStringArg(args, 0, "id")
			txMu.Lock()
			defer txMu.Unlock()
			tx, ok := txMap[id]
			if !ok || tx.rolledBack {
				return nil, fmt.Errorf("transaction '%s' cannot be committed (not open)", id)
			}
			tx.committed = true
			tx.releaseLocks()
			emitTxStatus(b.Events, b.SessionID, b.RunID, id, "committed", tx.touched, "")
			return map[string]any{
				"ok":           true,
				"id":           id,
				"status":       "committed",
				"touchedFiles": tx.touched,
			}, nil
		},
		"_rollback": func(_ context.Context, args []any) (any, error) {
			id, _ := requireStringArg(args, 0, "id")
			txMu.Lock()
			defer txMu.Unlock()
			tx, ok := txMap[id]
			if !ok {
				return nil, fmt.Errorf("unknown transaction '%s'", id)
			}
			if tx.rolledBack {
				return map[string]any{"ok": true, "id": id, "status": "already_rolled_back"}, nil
			}
			restored, failed := rollbackSnapshots(tx)
			tx.releaseLocks()
			emitTxStatus(b.Events, b.SessionID, b.RunID, id, "rolled_back", restored, "")
			res := map[string]any{
				"ok":            len(failed) == 0,
				"id":            id,
				"status":        "rolled_back",
				"restoredFiles": restored,
			}
			if len(failed) > 0 {
				res["status"] = "rollback_incomplete"
				res["failedFiles"] = failed
				env.AddWarning(fmt.Sprintf("ROLLBACK FAILED for %s: %s — these files may still contain tx edits", id, formatRollbackFailures(failed)))
			}
			return res, nil
		},
		// t.rm(path): deletes a file inside the tx; rollback restores it.
		"_rm": func(ctx context.Context, args []any) (any, error) {
			id, _ := requireStringArg(args, 0, "id")
			p, err := requireStringArg(args, 1, "path")
			if err != nil {
				return nil, err
			}
			txMu.Lock()
			defer txMu.Unlock()
			tx, ok := txMap[id]
			if !ok || tx.committed || tx.rolledBack {
				return nil, fmt.Errorf("transaction '%s' is not active", id)
			}
			abs, err := b.Policy.ResolveAndCheckWrite(env.Cwd, p)
			if err != nil {
				return nil, err
			}
			relLock, lockErr := b.Locks.AcquireIn(ctx, env.Cwd, tx.lockOwner, abs)
			if lockErr != nil {
				return nil, lockErr
			}
			tx.releases = append(tx.releases, relLock)
			info, err := os.Stat(abs)
			if err != nil {
				return nil, err
			}
			if info.IsDir() {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is a directory; t.rm deletes files", p)
			}
			orig, err := os.ReadFile(abs)
			if err != nil {
				return nil, err
			}
			rel := relDisplayPath(env.Cwd, abs)
			snapKey := snapshotKeyForPath(abs)
			if _, seen := tx.snapshots[snapKey]; !seen {
				tx.snapshots[snapKey] = fileSnapshot{absPath: abs, relPath: rel, existed: true, mode: info.Mode().Perm(), origBytes: orig}
				tx.touched = append(tx.touched, rel)
			}
			if rmErr := os.Remove(abs); rmErr != nil {
				return nil, rmErr
			}
			emitFileDiff(b.Events, b.SessionID, b.RunID, tx.id, "open", "tx.rm", env.Cwd, rel, abs, orig, nil, true, false)
			return nil, nil
		},
		// t.rename(from, to) / t.mv: moves a file inside the tx; rollback restores both paths.
		"_rename": func(ctx context.Context, args []any) (any, error) {
			id, _ := requireStringArg(args, 0, "id")
			from, err := requireStringArg(args, 1, "from")
			if err != nil {
				return nil, err
			}
			to, err := requireStringArg(args, 2, "to")
			if err != nil {
				return nil, err
			}
			txMu.Lock()
			defer txMu.Unlock()
			tx, ok := txMap[id]
			if !ok || tx.committed || tx.rolledBack {
				return nil, fmt.Errorf("transaction '%s' is not active", id)
			}
			absFrom, err := b.Policy.ResolveAndCheckWrite(env.Cwd, from)
			if err != nil {
				return nil, err
			}
			absTo, err := b.Policy.ResolveAndCheckWrite(env.Cwd, to)
			if err != nil {
				return nil, err
			}
			relLock, lockErr := b.Locks.AcquireIn(ctx, env.Cwd, tx.lockOwner, absFrom, absTo)
			if lockErr != nil {
				return nil, lockErr
			}
			tx.releases = append(tx.releases, relLock)
			info, err := os.Stat(absFrom)
			if err != nil {
				return nil, err
			}
			if info.IsDir() {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is a directory; t.rename moves files (fs.rename moves dirs, not rolled back)", from)
			}
			src, err := os.ReadFile(absFrom)
			if err != nil {
				return nil, err
			}
			for _, snap := range []fileSnapshot{
				{absPath: absFrom, relPath: relDisplayPath(env.Cwd, absFrom), existed: true, mode: info.Mode().Perm(), origBytes: src},
				func() fileSnapshot {
					var toMode os.FileMode
					if ti, sErr := os.Stat(absTo); sErr == nil {
						toMode = ti.Mode().Perm()
					}
					orig, rErr := os.ReadFile(absTo)
					return fileSnapshot{absPath: absTo, relPath: relDisplayPath(env.Cwd, absTo), existed: rErr == nil, mode: toMode, origBytes: orig}
				}(),
			} {
				snapKey := snapshotKeyForPath(snap.absPath)
				if _, seen := tx.snapshots[snapKey]; !seen {
					tx.snapshots[snapKey] = snap
					tx.touched = append(tx.touched, snap.relPath)
				}
			}
			if err := os.MkdirAll(filepath.Dir(absTo), 0o755); err != nil {
				return nil, err
			}
			if mvErr := os.Rename(absFrom, absTo); mvErr != nil {
				return nil, mvErr
			}
			emitFileRename(b.Events, b.SessionID, b.RunID, tx.id, "open", "tx.rename", env.Cwd, relDisplayPath(env.Cwd, absFrom), absFrom, relDisplayPath(env.Cwd, absTo), absTo)
			return nil, nil
		},
		// t.diff() -> unified diff of every file touched by the tx (current disk content vs. snapshot).
		"_diff": func(_ context.Context, args []any) (any, error) {
			id, _ := requireStringArg(args, 0, "id")
			txMu.Lock()
			tx, ok := txMap[id]
			var files []diffFile
			if ok {
				for _, s := range tx.snapshots {
					cur, err := os.ReadFile(s.absPath)
					files = append(files, diffFile{rel: s.relPath, before: s.origBytes, after: cur, beforeOK: s.existed, afterOK: err == nil})
				}
			}
			txMu.Unlock()
			if !ok {
				return nil, fmt.Errorf("unknown transaction '%s'", id)
			}
			sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
			return gitDiff(files)
		},
		"_status": func(_ context.Context, args []any) (any, error) {
			id, _ := requireStringArg(args, 0, "id")
			txMu.Lock()
			defer txMu.Unlock()
			tx, ok := txMap[id]
			if !ok {
				return nil, fmt.Errorf("unknown transaction '%s'", id)
			}
			state := "open"
			if tx.committed {
				state = "committed"
			} else if tx.rolledBack {
				state = "rolled_back"
			}
			return map[string]any{
				"id":           id,
				"status":       state,
				"touchedFiles": tx.touched,
				"lastVerify":   tx.lastVerify,
			}, nil
		},
	}

	return env.SetNamespace("tx", txAsync, nil)
}

// rollbackSnapshots restores every snapshotted file, retrying briefly (Windows editors, indexers and AV scanners
// commonly hold transient locks). It reports restored paths and per-file failures instead of claiming success.
func rollbackSnapshots(tx *activeTx) (restored []string, failed []map[string]any) {
	restored = []string{}
	for _, snap := range tx.snapshots {
		if err := restoreSnapshot(snap); err != nil {
			failed = append(failed, map[string]any{"path": snap.relPath, "error": err.Error()})
			continue
		}
		restored = append(restored, snap.relPath)
	}
	sort.Strings(restored)
	sort.Slice(failed, func(i, j int) bool { return failed[i]["path"].(string) < failed[j]["path"].(string) })
	tx.rolledBack = true
	tx.committed = false
	return restored, failed
}

func restoreSnapshot(snap fileSnapshot) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*75) * time.Millisecond)
		}
		if snap.existed {
			err = writeFileForRollback(snap.absPath, snap.origBytes, snap.mode)
		} else {
			err = os.Remove(snap.absPath)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		if err == nil {
			return nil
		}
	}
	return err
}

// matchLines returns the 1-based lines of every occurrence of target inside window (which starts at winStart).
func matchLines(window, target string, winStart int, lineStarts []int) []int {
	offs := matchOffsets(window, target)
	lines := make([]int, len(offs))
	for i, o := range offs {
		lines[i] = offsetToLineNumber(lineStarts, winStart+o)
	}
	return lines
}

// matchOffsets returns the byte offsets of every non-overlapping occurrence of target in window.
func matchOffsets(window, target string) []int {
	var offs []int
	for off := 0; ; {
		idx := strings.Index(window[off:], target)
		if idx < 0 {
			return offs
		}
		offs = append(offs, off+idx)
		off += idx + len(target)
	}
}

// formatMatchList renders "L2 in a, L6 in b" (symbol omitted when unknown), at most limit entries.
func formatMatchList(lines []int, where []string, limit int) string {
	parts := make([]string, 0, minInt(len(lines), limit)+1)
	for i, l := range lines {
		if i == limit {
			parts = append(parts, fmt.Sprintf("+%d", len(lines)-limit))
			break
		}
		if where[i] != "" {
			parts = append(parts, fmt.Sprintf("L%d in %s", l, where[i]))
		} else {
			parts = append(parts, fmt.Sprintf("L%d", l))
		}
	}
	return strings.Join(parts, ", ")
}

// prefixHunkErr adds "hunk N" context to a symbol resolution error while keeping its kind.
func prefixHunkErr(err error, hIdx int) error {
	var he *sandbox.HostError
	if errors.As(err, &he) {
		return &sandbox.HostError{Kind: he.Kind, Msg: strings.Replace(he.Msg, ": symbol", fmt.Sprintf(" hunk %d: symbol", hIdx+1), 1), Data: he.Data}
	}
	return err
}

// formatLineList renders "L2, L6, L9" (at most limit entries, then "+N").
func formatLineList(lines []int, limit int) string {
	parts := make([]string, 0, minInt(len(lines), limit)+1)
	for i, l := range lines {
		if i == limit {
			parts = append(parts, fmt.Sprintf("+%d", len(lines)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("L%d", l))
	}
	return strings.Join(parts, ", ")
}

func formatRollbackFailures(failed []map[string]any) string {
	parts := make([]string, 0, len(failed))
	for _, f := range failed {
		parts = append(parts, fmt.Sprintf("%v (%v)", f["path"], f["error"]))
	}
	return joinLimited(parts, 10)
}

func joinLimited(items []string, limit int) string {
	if len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:limit], ", ") + fmt.Sprintf(", ...+%d more", len(items)-limit)
}

func (b *EditTxBinder) applyFileSpecs(ctx context.Context, cwd string, specs []FileEditSpec, dryRun, bestEffort bool, tx *activeTx) (map[string]any, error) {
	return b.applyFileSpecsWithOwner(ctx, cwd, "", specs, dryRun, bestEffort, tx)
}

func (b *EditTxBinder) applyFileSpecsWithOwner(ctx context.Context, cwd, execOwner string, specs []FileEditSpec, dryRun, bestEffort bool, tx *activeTx) (map[string]any, error) {
	if len(specs) == 0 {
		return nil, errors.New("edits list cannot be empty")
	}

	// Resolve paths up front and coalesce duplicate paths in one edit.apply/t.edit call
	// so earlier hunks on the same file are NEVER silently overwritten by a later spec (#6).
	type resolvedSpec struct {
		absPath string
		rel     string
		spec    FileEditSpec
	}
	var merged []resolvedSpec
	idxByKey := map[string]int{}
	failures := []map[string]any{}

	for _, spec := range specs {
		absPath, err := b.Policy.ResolveAndCheckWrite(cwd, spec.Path)
		if err != nil {
			if bestEffort {
				failures = append(failures, map[string]any{"path": spec.Path, "error": err.Error()})
				continue
			}
			return nil, err
		}
		rel := relDisplayPath(cwd, absPath)
		key := normalizeLockKey(absPath)
		if prevIdx, dup := idxByKey[key]; dup {
			prev := &merged[prevIdx]
			if prev.spec.Create != nil || spec.Create != nil {
				err := sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s appears multiple times in one edit call with create/overwrite", rel)
				if bestEffort {
					failures = append(failures, map[string]any{"path": rel, "error": err.Error()})
					continue
				}
				return nil, err
			}
			prev.spec.Hunks = append(prev.spec.Hunks, spec.Hunks...)
			continue
		}
		idxByKey[key] = len(merged)
		merged = append(merged, resolvedSpec{absPath: absPath, rel: rel, spec: spec})
	}

	// Acquire per-file locks BEFORE reading/planning so concurrent exec calls never interleave dirty reads or clobber rollbacks.
	// Same-exec operations share execOwner and are ref-counted re-entrant so tx + edit/fs.write never self-deadlock.
	if !dryRun && b.Locks != nil && len(merged) > 0 {
		paths := make([]string, len(merged))
		for i, m := range merged {
			paths[i] = m.absPath
		}
		lockOwner := execOwner
		if tx != nil && tx.lockOwner != "" {
			lockOwner = tx.lockOwner
		}
		if lockOwner == "" {
			lockOwner = fmt.Sprintf("edit:%d", txOwnerSeq.Add(1))
		}
		relLock, lockErr := b.Locks.AcquireIn(ctx, cwd, lockOwner, paths...)
		if lockErr != nil {
			return nil, lockErr
		}
		if tx != nil {
			tx.releases = append(tx.releases, relLock)
		} else {
			defer relLock()
		}
	}

	var planned []plannedFileWrite
	var planErrs []error
	for _, m := range merged {
		pw, planErr := planSingleFile(m.absPath, m.rel, m.spec)
		if planErr != nil {
			if bestEffort {
				failures = append(failures, map[string]any{"path": m.rel, "error": planErr.Error()})
				continue
			}
			planErrs = append(planErrs, planErr)
			continue
		}
		planned = append(planned, pw)
	}
	if len(planErrs) > 0 {
		return nil, combinePlanErrors(planErrs)
	}

	if !dryRun {
		var written []plannedFileWrite
		var newSnapKeys []string
		prevTouchedLen := 0
		if tx != nil {
			prevTouchedLen = len(tx.touched)
		}
		for _, pw := range planned {
			var addedKey string
			if tx != nil {
				snapKey := snapshotKeyForPath(pw.absPath)
				if _, seen := tx.snapshots[snapKey]; !seen {
					tx.snapshots[snapKey] = fileSnapshot{
						absPath:   pw.absPath,
						relPath:   pw.relPath,
						existed:   pw.existed,
						mode:      pw.mode,
						origBytes: pw.origBytes,
					}
					tx.touched = append(tx.touched, pw.relPath)
					addedKey = snapKey
					newSnapKeys = append(newSnapKeys, snapKey)
				}
			}
			if err := writeFileWithMode(pw.absPath, pw.newBytes, pw.mode); err != nil {
				if addedKey != "" {
					// This file was never written on disk, so drop its fresh snapshot entry if added in this call.
					delete(tx.snapshots, addedKey)
					newSnapKeys = newSnapKeys[:len(newSnapKeys)-1]
				}
				if len(written) > 0 {
					// Keep multi-file edit.apply AND t.edit([...]) atomic per call: undo files 0..K-1 back to pre-call bytes.
					var undoFailed []map[string]any
					failedKeys := map[string]bool{}
					for i := len(written) - 1; i >= 0; i-- {
						w := written[i]
						if uErr := restoreSnapshot(fileSnapshot{absPath: w.absPath, relPath: w.relPath, existed: w.existed, mode: w.mode, origBytes: w.origBytes}); uErr != nil {
							undoFailed = append(undoFailed, map[string]any{"path": w.relPath, "error": uErr.Error()})
							failedKeys[snapshotKeyForPath(w.absPath)] = true
						}
					}
					if tx != nil {
						for _, sk := range newSnapKeys {
							if !failedKeys[sk] {
								delete(tx.snapshots, sk)
							}
						}
						if len(undoFailed) == 0 {
							tx.touched = tx.touched[:prevTouchedLen]
						}
					}
					if len(undoFailed) > 0 {
						return nil, fmt.Errorf("failed writing '%s': %w; ALSO failed to undo earlier writes: %s", pw.relPath, err, formatRollbackFailures(undoFailed))
					}
					callLabel := "edit.apply"
					if tx != nil {
						callLabel = "t.edit"
					}
					return nil, fmt.Errorf("failed writing '%s': %w (earlier files in this %s call were restored)", pw.relPath, err, callLabel)
				}
				if tx != nil {
					tx.touched = tx.touched[:prevTouchedLen]
				}
				return nil, fmt.Errorf("failed writing '%s': %w", pw.relPath, err)
			}
			written = append(written, pw)
		}
		if b.Events.Active() {
			toolName := "edit.apply"
			txID, txPhase := "", ""
			if tx != nil {
				toolName = "tx.edit"
				txID = tx.id
				txPhase = "open"
			}
			for _, pw := range written {
				emitFileDiff(b.Events, b.SessionID, b.RunID, txID, txPhase, toolName, cwd, pw.relPath, pw.absPath, pw.origBytes, pw.newBytes, pw.existed, true)
			}
		}
	}

	applied := make([]map[string]any, len(planned))
	for i, pw := range planned {
		entry := map[string]any{
			"path":      pw.relPath,
			"hunks":     pw.hunksApplied,
			"lineDelta": pw.lineDelta,
		}
		if len(pw.spanLabels) > 0 {
			entry["spans"] = pw.spanLabels
		}
		applied[i] = entry
	}
	res := map[string]any{
		"ok":      len(failures) == 0,
		"dryRun":  dryRun,
		"applied": applied,
	}
	if dryRun {
		files := make([]diffFile, len(planned))
		for i, pw := range planned {
			files[i] = diffFile{rel: pw.relPath, before: pw.origBytes, after: pw.newBytes, beforeOK: pw.existed, afterOK: true}
		}
		if d, err := gitDiff(files); err != nil {
			res["diffError"] = err.Error()
		} else {
			res["diff"] = d
		}
	}
	if len(failures) > 0 {
		res["failures"] = failures
	}
	return res, nil
}

func planSingleFile(absPath, relPath string, spec FileEditSpec) (plannedFileWrite, error) {
	var origMode os.FileMode
	if info, sErr := os.Stat(absPath); sErr == nil {
		origMode = info.Mode().Perm()
	}
	origBytes, readErr := os.ReadFile(absPath)
	existed := readErr == nil

	if spec.Create != nil {
		if existed && !spec.Overwrite {
			return plannedFileWrite{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s exists; pass overwrite: true", relPath)
		}
		newBytes, eolErr := applyEOL(relPath, origBytes, existed, []byte(*spec.Create), spec.EOL)
		if eolErr != nil {
			return plannedFileWrite{}, eolErr
		}
		oldLines := 0
		if existed {
			oldLines = strings.Count(string(origBytes), "\n") + 1
		}
		newLines := strings.Count(*spec.Create, "\n") + 1
		return plannedFileWrite{
			absPath:      absPath,
			relPath:      relPath,
			existed:      existed,
			mode:         origMode,
			origBytes:    origBytes,
			newBytes:     newBytes,
			hunksApplied: 1,
			lineDelta:    newLines - oldLines,
		}, nil
	}

	if !existed {
		return plannedFileWrite{}, sandbox.NewHostError(sandbox.KindNotFound, nil, "%s not found", relPath)
	}
	if len(spec.Hunks) == 0 {
		return plannedFileWrite{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s: no hunks or create content", relPath)
	}

	rawStr := string(origBytes)
	crlfCount := strings.Count(rawStr, "\r\n")
	lfCount := strings.Count(rawStr, "\n")
	useCRLF := crlfCount > 0 && crlfCount*2 >= lfCount
	normText := strings.ReplaceAll(rawStr, "\r\n", "\n")
	lineStarts := computeLineOffsets(normText)

	type span struct {
		start     int
		end       int
		repl      string
		idx       int
		startLine int
		endLine   int
		label     string
	}
	var spans []span
	var hunkErrs []error

	var syms []SymbolInfo
	var symLines []string
	symsLoaded := false
	outline := func() []SymbolInfo {
		if !symsLoaded {
			syms, symLines = outlineFromSource(absPath, origBytes)
			symsLoaded = true
		}
		return syms
	}

	var pyStates []pyLineState
	pyStatesLoaded := false
	getPyStates := func() []pyLineState {
		if !pyStatesLoaded {
			pyStates = scanPythonLines(strings.Split(normText, "\n"))
			pyStatesLoaded = true
		}
		return pyStates
	}
	sep := symbolSep(relPath)

	for hIdx, h := range spec.Hunks {
		normRepl := strings.ReplaceAll(h.Replacement, "\r\n", "\n")

		if h.ReplaceSymbol {
			s, sErr := resolveSymbol(outline(), h.Symbol, relPath)
			if sErr != nil {
				hunkErrs = append(hunkErrs, prefixHunkErr(sErr, hIdx))
				continue
			}
			repStart, repEnd, docStatus, vErr := resolveSymbolReplaceSpan(symLines, outline(), s, relPath, normRepl)
			if vErr != nil {
				hunkErrs = append(hunkErrs, prefixHunkErr(vErr, hIdx))
				continue
			}
			alignedRepl := alignSymbolReplacementIndent(symLines[repStart-1], normRepl, relPath)
			alignedRepl = strings.TrimSuffix(alignedRepl, "\n")
			spStart, spEnd := exactLineSpanOffsets(normText, lineStarts, repStart, repEnd)
			qName := qualifiedName(outline(), s, sep)
			lbl := fmt.Sprintf("L%d-%d (symbol %s)", repStart, repEnd, qName)
			if docStatus != "" {
				lbl = fmt.Sprintf("L%d-%d (symbol %s, %s)", repStart, repEnd, qName, docStatus)
			}
			spans = append(spans, span{
				start:     spStart,
				end:       spEnd,
				repl:      alignedRepl,
				idx:       hIdx,
				startLine: repStart,
				endLine:   repEnd,
				label:     lbl,
			})
			continue
		}

		normTarget := strings.ReplaceAll(h.Target, "\r\n", "\n")
		scope := fmt.Sprintf("L%d-%d", h.StartLine, h.EndLine)
		if h.Symbol != "" {
			s, sErr := resolveSymbol(outline(), h.Symbol, relPath)
			if sErr != nil {
				hunkErrs = append(hunkErrs, prefixHunkErr(sErr, hIdx))
				continue
			}
			symStart := leadingDocStartLine(symLines, syms, s, relPath)
			if h.StartLine < symStart {
				h.StartLine = symStart
			}
			if h.EndLine == 0 || h.EndLine > s.EndLine {
				h.EndLine = s.EndLine
			}
			scope = h.Symbol + fmt.Sprintf(" (L%d-%d)", symStart, s.EndLine)
		}
		if normTarget == "" {
			hunkErrs = append(hunkErrs, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s hunk %d: empty target", relPath, hIdx+1))
			continue
		}
		winStart, winEnd, wErr := lineWindowBounds(normText, lineStarts, h.StartLine, h.EndLine)
		if wErr != nil {
			hunkErrs = append(hunkErrs, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s hunk %d: %v", relPath, hIdx+1, wErr))
			continue
		}
		windowText := normText[winStart:winEnd]

		if h.EndTarget != "" {
			normEnd := strings.ReplaceAll(h.EndTarget, "\r\n", "\n")
			spStart, spEnd, adjRepl, sLine, eLine, bErr := resolveEndTargetHunk(
				normText, windowText, normTarget, normEnd, normRepl, relPath, scope,
				h, hIdx, winStart, lineStarts, outline, sep, getPyStates,
			)
			if bErr != nil {
				hunkErrs = append(hunkErrs, bErr)
				continue
			}
			spans = append(spans, span{
				start:     spStart,
				end:       spEnd,
				repl:      adjRepl,
				idx:       hIdx,
				startLine: sLine,
				endLine:   eLine,
				label:     fmt.Sprintf("L%d-%d", sLine, eLine),
			})
			continue
		}

		if h.ReplaceAll {
			searchOff := 0
			count := 0
			for {
				idx := strings.Index(windowText[searchOff:], normTarget)
				if idx < 0 {
					break
				}
				absStart := winStart + searchOff + idx
				absEnd := absStart + len(normTarget)
				sLine := offsetToLineNumber(lineStarts, absStart)
				eLine := offsetToLineNumber(lineStarts, maxInt(absStart, absEnd-1))
				spans = append(spans, span{
					start:     absStart,
					end:       absEnd,
					repl:      normRepl,
					idx:       hIdx,
					startLine: sLine,
					endLine:   eLine,
					label:     fmt.Sprintf("L%d-%d", sLine, eLine),
				})
				searchOff += idx + len(normTarget)
				count++
			}
			if count == 0 {
				hint := closestLineHint(normText, normTarget)
				hunkErrs = append(hunkErrs, sandbox.NewHostError(sandbox.KindNoMatch, nil, "%s hunk %d: target not found%s", relPath, hIdx+1, hint))
			}
		} else {
			firstIdx := strings.Index(windowText, normTarget)
			if firstIdx < 0 {
				if h.StartLine > 0 || h.EndLine > 0 || h.Symbol != "" {
					if actualLine := findTargetAnywhereLine(normText, normTarget, relPath, lineStarts, getPyStates); actualLine > 0 {
						hunkErrs = append(hunkErrs, sandbox.NewHostError(sandbox.KindNoMatch, map[string]any{"lines": []int{actualLine}},
							"%s hunk %d: target not in %s; exact match at L%d", relPath, hIdx+1, scope, actualLine))
						continue
					}
				}
				if h.Nth <= 1 {
					wsStart, wsEnd, wsRepl, wsLines := matchWhitespaceNormalized(windowText, normTarget, normRepl, relPath, winStart, lineStarts, getPyStates)
					if len(wsLines) == 1 {
						absStart := winStart + wsStart
						absEnd := winStart + wsEnd
						sLine := offsetToLineNumber(lineStarts, absStart)
						eLine := offsetToLineNumber(lineStarts, maxInt(absStart, absEnd-1))
						spans = append(spans, span{
							start:     absStart,
							end:       absEnd,
							repl:      wsRepl,
							idx:       hIdx,
							startLine: sLine,
							endLine:   eLine,
							label:     fmt.Sprintf("L%d-%d", sLine, eLine),
						})
						continue
					}
					if len(wsLines) > 1 {
						where := make([]string, len(wsLines))
						for i, l := range wsLines {
							where[i] = symbolAt(outline(), l, sep)
						}
						hunkErrs = append(hunkErrs, sandbox.NewHostError(sandbox.KindAmbiguous, map[string]any{"lines": wsLines, "symbols": where},
							"%s hunk %d: target matches %d places via whitespace normalization (%s); set symbol or startLine/endLine", relPath, hIdx+1, len(wsLines), formatMatchList(wsLines, where, 6)))
						continue
					}
				}
				hint := closestLineHint(normText, normTarget)
				hunkErrs = append(hunkErrs, sandbox.NewHostError(sandbox.KindNoMatch, nil, "%s hunk %d: target not found%s", relPath, hIdx+1, hint))
				continue
			}
			if h.Nth > 0 {
				offs := matchOffsets(windowText, normTarget)
				if h.Nth > len(offs) {
					lines := matchLines(windowText, normTarget, winStart, lineStarts)
					hunkErrs = append(hunkErrs, sandbox.NewHostError(sandbox.KindNoMatch, map[string]any{"lines": lines},
						"%s hunk %d: nth %d but only %d match(es) (%s)", relPath, hIdx+1, h.Nth, len(offs), formatLineList(lines, 6)))
					continue
				}
				firstIdx = offs[h.Nth-1]
			} else if strings.Contains(windowText[firstIdx+len(normTarget):], normTarget) {
				lines := matchLines(windowText, normTarget, winStart, lineStarts)
				where := make([]string, len(lines))
				for i, l := range lines {
					where[i] = symbolAt(outline(), l, sep)
				}
				hunkErrs = append(hunkErrs, sandbox.NewHostError(sandbox.KindAmbiguous, map[string]any{"lines": lines, "symbols": where},
					"%s hunk %d: target matches %d places (%s); set symbol, nth, or replaceAll: true", relPath, hIdx+1, len(lines), formatMatchList(lines, where, 6)))
				continue
			}
			absStart := winStart + firstIdx
			absEnd := absStart + len(normTarget)
			sLine := offsetToLineNumber(lineStarts, absStart)
			eLine := offsetToLineNumber(lineStarts, maxInt(absStart, absEnd-1))
			spans = append(spans, span{
				start:     absStart,
				end:       absEnd,
				repl:      normRepl,
				idx:       hIdx,
				startLine: sLine,
				endLine:   eLine,
				label:     fmt.Sprintf("L%d-%d", sLine, eLine),
			})
		}
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			a, bSpan := spans[i-1], spans[i]
			if a.idx > bSpan.idx {
				a, bSpan = bSpan, a
			}
			hunkErrs = append(hunkErrs, sandbox.NewHostError(sandbox.KindBadArgs, nil,
				"%s: hunks %d (L%d-%d) and %d (L%d-%d) overlap",
				relPath, a.idx+1, a.startLine, a.endLine, bSpan.idx+1, bSpan.startLine, bSpan.endLine))
		}
	}
	if len(hunkErrs) > 0 {
		return plannedFileWrite{}, combinePlanErrors(hunkErrs)
	}

	var bld strings.Builder
	var spanLabels []string
	cursor := 0
	for _, sp := range spans {
		bld.WriteString(normText[cursor:sp.start])
		bld.WriteString(sp.repl)
		cursor = sp.end
		if sp.label != "" {
			spanLabels = append(spanLabels, sp.label)
		}
	}
	bld.WriteString(normText[cursor:])
	finalNorm := bld.String()

	oldLines := strings.Count(normText, "\n") + 1
	newLines := strings.Count(finalNorm, "\n") + 1

	finalOut := finalNorm
	if useCRLF {
		finalOut = strings.ReplaceAll(finalNorm, "\n", "\r\n")
	}

	return plannedFileWrite{
		absPath:      absPath,
		relPath:      relPath,
		existed:      true,
		mode:         origMode,
		origBytes:    origBytes,
		newBytes:     []byte(finalOut),
		hunksApplied: len(spans),
		lineDelta:    newLines - oldLines,
		spanLabels:   spanLabels,
	}, nil
}

func computeLineOffsets(text string) []int {
	starts := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func offsetToLineNumber(starts []int, offset int) int {
	lo, hi := 0, len(starts)-1
	for lo <= hi {
		mid := (lo + hi) >> 1
		if starts[mid] <= offset {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if hi+1 < 1 {
		return 1
	}
	return hi + 1
}

func lineWindowBounds(text string, starts []int, startLine, endLine int) (int, int, error) {
	total := len(starts)
	s := 1
	if startLine > 0 {
		s = startLine
	}
	e := total
	if endLine > 0 && endLine < total {
		e = endLine
	}
	if s > total {
		return len(text), len(text), nil
	}
	if e < s {
		return 0, 0, fmt.Errorf("startLine (%d) > endLine (%d)", s, e)
	}
	winStart := starts[s-1]
	winEnd := len(text)
	if e < total {
		winEnd = starts[e]
	}
	return winStart, winEnd, nil
}

type winLineSpan struct {
	start int // byte offset relative to windowText
	end   int // byte offset relative to windowText (excluding '\n')
	raw   string
}

func splitWindowLines(windowText string) []winLineSpan {
	var out []winLineSpan
	lineStart := 0
	for i := 0; i < len(windowText); i++ {
		if windowText[i] == '\n' {
			out = append(out, winLineSpan{start: lineStart, end: i, raw: windowText[lineStart:i]})
			lineStart = i + 1
		}
	}
	if lineStart <= len(windowText) {
		out = append(out, winLineSpan{start: lineStart, end: len(windowText), raw: windowText[lineStart:]})
	}
	return out
}

func isIndentSensitivePath(relPath string) bool {
	base := strings.ToLower(filepath.Base(relPath))
	if base == "makefile" || base == "gnumakefile" {
		return true
	}
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".py", ".pyw", ".yaml", ".yml", ".star", ".mk":
		return true
	}
	return false
}

func isBacktickEscapeLang(relPath string) bool {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".mts", ".cts":
		return true
	}
	return false
}

func splitLeadingHSpace(s string) (lead, rest string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[:i], strings.TrimRight(s[i:], " \t")
}

// canonicalizeCodeLine collapses runs of horizontal whitespace (' ' / '\t') OUTSIDE string literals
// while preserving exact characters inside "...", '...', and `...`.
func canonicalizeCodeLine(s string, keepIndent bool) string {
	canon, _ := canonicalizeCodeLineStateful(s, keepIndent, false, false)
	return canon
}

// canonicalizeCodeLineStateful tracks multiline backtick literals (`...`) across lines so interior
// raw/template string lines keep their exact whitespace without collapsing spaces or stripping leading indent.
func canonicalizeCodeLineStateful(s string, keepIndent, inBacktick, btEscape bool) (string, bool) {
	var b strings.Builder
	b.Grow(len(s) + 8)

	inQuote := rune(0)
	escaped := false
	inSpace := false
	rest := s

	if inBacktick {
		inQuote = '`'
		b.WriteString("\x00BT:")
	} else {
		i := 0
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		lead := s[:i]
		rest = s[i:]
		if keepIndent {
			for j := 0; j < len(lead); j++ {
				if lead[j] == '\t' {
					b.WriteString("    ")
				} else {
					b.WriteByte(' ')
				}
			}
			b.WriteString("::")
		}
	}

	for _, r := range rest {
		if inQuote != 0 {
			b.WriteRune(r)
			if (inQuote != '`' || btEscape) && escaped {
				escaped = false
				continue
			}
			if (inQuote != '`' || btEscape) && r == '\\' {
				escaped = true
				continue
			}
			if r == inQuote {
				inQuote = 0
			}
			continue
		}
		if r == '"' || r == '\'' || r == '`' {
			if inSpace {
				b.WriteByte(' ')
				inSpace = false
			}
			inQuote = r
			b.WriteRune(r)
			continue
		}
		if r == ' ' || r == '\t' {
			inSpace = true
			continue
		}
		if inSpace {
			b.WriteByte(' ')
			inSpace = false
		}
		b.WriteRune(r)
	}
	return b.String(), inQuote == '`'
}

// trackMultilineBackticks returns a per-line boolean slice indicating whether line i begins inside an
// already-open multiline backtick string (`...`).
func trackMultilineBackticks(lines []string, btEscape bool) []bool {
	startsInBT := make([]bool, len(lines))
	inBT := false
	for i, line := range lines {
		startsInBT[i] = inBT
		inQuote := rune(0)
		if inBT {
			inQuote = '`'
		}
		escaped := false
		for _, r := range line {
			if inQuote != 0 {
				if (inQuote != '`' || btEscape) && escaped {
					escaped = false
					continue
				}
				if (inQuote != '`' || btEscape) && r == '\\' {
					escaped = true
					continue
				}
				if r == inQuote {
					inQuote = 0
				}
				continue
			}
			if r == '"' || r == '\'' || r == '`' {
				inQuote = r
			}
		}
		inBT = inQuote == '`'
	}
	return startsInBT
}

func isPythonContinuationPath(relPath string) bool {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".py", ".pyw", ".star":
		return true
	}
	return false
}

func isBraceConservationExt(relPath string) bool {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".go", ".c", ".h", ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx", ".ino",
		".rs", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts",
		".java", ".cs", ".kt", ".kts":
		return true
	}
	return false
}

func countIdentRunes(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n
}

func canonicalizePythonLine(s string, keepIndent bool) string {
	var b strings.Builder
	b.Grow(len(s) + 8)

	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	lead := s[:i]
	rest := strings.TrimRight(s[i:], " \t")
	if keepIndent {
		for j := 0; j < len(lead); j++ {
			if lead[j] == '\t' {
				b.WriteString("    ")
			} else {
				b.WriteByte(' ')
			}
		}
		b.WriteString("::")
	}

	inSpace := false
	for k := 0; k < len(rest); {
		if strings.HasPrefix(rest[k:], `"""`) || strings.HasPrefix(rest[k:], `'''`) {
			if inSpace {
				b.WriteByte(' ')
				inSpace = false
			}
			delim := rest[k : k+3]
			b.WriteString(delim)
			k += 3
			for k < len(rest) {
				if rest[k] == '\\' && k+1 < len(rest) {
					b.WriteString(rest[k : k+2])
					k += 2
					continue
				}
				if strings.HasPrefix(rest[k:], delim) {
					b.WriteString(delim)
					k += 3
					break
				}
				b.WriteByte(rest[k])
				k++
			}
			continue
		}
		c := rest[k]
		if c == '"' || c == '\'' {
			if inSpace {
				b.WriteByte(' ')
				inSpace = false
			}
			b.WriteByte(c)
			k++
			for k < len(rest) {
				if rest[k] == '\\' && k+1 < len(rest) {
					b.WriteString(rest[k : k+2])
					k += 2
					continue
				}
				b.WriteByte(rest[k])
				if rest[k] == c {
					k++
					break
				}
				k++
			}
			continue
		}
		if c == ' ' || c == '\t' {
			inSpace = true
			k++
			continue
		}
		if inSpace {
			b.WriteByte(' ')
			inSpace = false
		}
		r, w := utf8.DecodeRuneInString(rest[k:])
		b.WriteRune(r)
		k += w
	}
	return b.String()
}

func matchWhitespaceNormalized(windowText, normTarget, normRepl, relPath string, winStart int, lineStarts []int, getPyStates func() []pyLineState) (start, end int, adjustedRepl string, matchedLineNums []int) {
	offs, repls, lines := matchWhitespaceNormalizedAll(windowText, normTarget, normRepl, relPath, winStart, lineStarts, getPyStates)
	if len(offs) != 1 {
		return 0, 0, "", lines
	}
	return offs[0][0], offs[0][1], repls[0], lines
}

func matchWhitespaceNormalizedAll(windowText, normTarget, normRepl, relPath string, winStart int, lineStarts []int, getPyStates func() []pyLineState) (relSpans [][2]int, adjustedRepls []string, matchedLineNums []int) {
	trimmedTarget := strings.TrimSuffix(normTarget, "\n")
	hasTrailingNL := strings.HasSuffix(normTarget, "\n")
	if trimmedTarget == "" || countIdentRunes(trimmedTarget) < 6 {
		return nil, nil, nil
	}

	tLines := strings.Split(trimmedTarget, "\n")
	k := len(tLines)
	wLines := splitWindowLines(windowText)
	if len(wLines) < k {
		return nil, nil, nil
	}

	if isPythonContinuationPath(relPath) && getPyStates != nil {
		pyStates := getPyStates()
		winStartLine := offsetToLineNumber(lineStarts, winStart)

		tStrict := make([]string, k)
		tRelaxed := make([]string, k)
		for j, tl := range tLines {
			tStrict[j] = canonicalizePythonLine(tl, true)
			tRelaxed[j] = canonicalizePythonLine(tl, false)
		}

		wStrict := make([]string, len(wLines))
		wRelaxed := make([]string, len(wLines))
		for i, wl := range wLines {
			wStrict[i] = canonicalizePythonLine(wl.raw, true)
			wRelaxed[i] = canonicalizePythonLine(wl.raw, false)
		}

		for i := 0; i <= len(wLines)-k; i++ {
			ok := true
			for j := 0; j < k; j++ {
				fIdx := (winStartLine - 1) + i + j
				var st pyLineState
				if fIdx >= 0 && fIdx < len(pyStates) {
					st = pyStates[fIdx]
				}
				if st.startTriple != "" {
					if wLines[i+j].raw != tLines[j] {
						ok = false
						break
					}
				} else if st.startParen > 0 {
					if wRelaxed[i+j] != tRelaxed[j] {
						ok = false
						break
					}
				} else {
					if wStrict[i+j] != tStrict[j] {
						ok = false
						break
					}
				}
			}
			if ok {
				s := wLines[i].start
				e := wLines[i+k-1].end
				if hasTrailingNL && e < len(windowText) && windowText[e] == '\n' {
					e++
				}
				relSpans = append(relSpans, [2]int{s, e})
				adjustedRepls = append(adjustedRepls, normRepl)
				matchedLineNums = append(matchedLineNums, offsetToLineNumber(lineStarts, winStart+s))
			}
		}
		return relSpans, adjustedRepls, matchedLineNums
	}

	keepIndent := isIndentSensitivePath(relPath)
	btEscape := isBacktickEscapeLang(relPath)

	tCanon := make([]string, k)
	tInBT := false
	for i, tl := range tLines {
		tCanon[i], tInBT = canonicalizeCodeLineStateful(tl, keepIndent, tInBT, btEscape)
	}

	wCanon := make([]string, len(wLines))
	wInBT := false
	for i, wl := range wLines {
		wCanon[i], wInBT = canonicalizeCodeLineStateful(wl.raw, keepIndent, wInBT, btEscape)
	}

	for i := 0; i <= len(wLines)-k; i++ {
		ok := true
		for j := 0; j < k; j++ {
			if wCanon[i+j] != tCanon[j] {
				ok = false
				break
			}
		}
		if ok {
			s := wLines[i].start
			e := wLines[i+k-1].end
			if hasTrailingNL && e < len(windowText) && windowText[e] == '\n' {
				e++
			}
			adj := realignReplacementIndent(wLines[i:i+k], tLines, normRepl, keepIndent, btEscape)
			relSpans = append(relSpans, [2]int{s, e})
			adjustedRepls = append(adjustedRepls, adj)
			matchedLineNums = append(matchedLineNums, offsetToLineNumber(lineStarts, winStart+s))
		}
	}
	return relSpans, adjustedRepls, matchedLineNums
}

func findAnchorCandidates(windowText, normAnchor, relPath string, winStart int, lineStarts []int, getPyStates func() []pyLineState) (relSpans [][2]int, lines []int, exact bool) {
	offs := matchOffsets(windowText, normAnchor)
	if len(offs) > 0 {
		relSpans = make([][2]int, len(offs))
		lines = make([]int, len(offs))
		for i, o := range offs {
			relSpans[i] = [2]int{o, o + len(normAnchor)}
			lines[i] = offsetToLineNumber(lineStarts, winStart+o)
		}
		return relSpans, lines, true
	}
	wsSpans, _, wsLines := matchWhitespaceNormalizedAll(windowText, normAnchor, "", relPath, winStart, lineStarts, getPyStates)
	return wsSpans, wsLines, false
}

func findTargetAnywhereLine(normText, normTarget, relPath string, lineStarts []int, getPyStates func() []pyLineState) int {
	if idx := strings.Index(normText, normTarget); idx >= 0 {
		return offsetToLineNumber(lineStarts, idx)
	}
	_, _, wsLines := matchWhitespaceNormalizedAll(normText, normTarget, "", relPath, 0, lineStarts, getPyStates)
	if len(wsLines) > 0 {
		return wsLines[0]
	}
	return 0
}

const defaultMaxEndTargetSpanLines = 120

func resolveEndTargetHunk(
	normText, windowText, normStart, normEnd, normRepl, relPath, scope string,
	h EditHunk, hIdx, winStart int, lineStarts []int,
	outline func() []SymbolInfo, sep string, getPyStates func() []pyLineState,
) (absStart, absEnd int, finalRepl string, sLine, eLine int, err error) {
	endBody := strings.TrimSuffix(normEnd, "\n")
	if h.EndLine == 0 && !strings.Contains(endBody, "\n") && isBoilerplateAnchorLine(strings.TrimSpace(endBody)) && countIdentRunes(endBody) < 6 {
		return 0, 0, "", 0, 0, sandbox.NewHostError(sandbox.KindBadArgs, nil,
			"%s hunk %d: endTarget %q is a bare closing delimiter; include the preceding statement in endTarget (e.g. \"return ok;\\n}\"), set endLine, or use replaceSymbol",
			relPath, hIdx+1, strings.TrimSpace(endBody))
	}

	startSpans, startLines, startExact := findAnchorCandidates(windowText, normStart, relPath, winStart, lineStarts, getPyStates)
	if len(startSpans) == 0 {
		if h.StartLine > 0 || h.EndLine > 0 || h.Symbol != "" {
			if actualLine := findTargetAnywhereLine(normText, normStart, relPath, lineStarts, getPyStates); actualLine > 0 {
				return 0, 0, "", 0, 0, sandbox.NewHostError(sandbox.KindNoMatch, map[string]any{"lines": []int{actualLine}},
					"%s hunk %d: start target not in %s; match found at L%d", relPath, hIdx+1, scope, actualLine)
			}
		}
		hint := closestLineHint(normText, normStart)
		return 0, 0, "", 0, 0, sandbox.NewHostError(sandbox.KindNoMatch, nil, "%s hunk %d: start target not found%s", relPath, hIdx+1, hint)
	}
	if len(startSpans) > 1 {
		where := make([]string, len(startLines))
		for i, l := range startLines {
			where[i] = symbolAt(outline(), l, sep)
		}
		return 0, 0, "", 0, 0, sandbox.NewHostError(sandbox.KindAmbiguous, map[string]any{"lines": startLines, "symbols": where},
			"%s hunk %d: start target matches %d places (%s); set symbol or startLine/endLine",
			relPath, hIdx+1, len(startLines), formatMatchList(startLines, where, 6))
	}

	sRel := startSpans[0]
	sLine = startLines[0]
	sEndLine := offsetToLineNumber(lineStarts, winStart+maxInt(sRel[0], sRel[1]-1))

	afterOff := sRel[1]
	subEnd := len(windowText)
	cappedBy120 := false
	if h.Symbol == "" && h.EndLine == 0 {
		maxLine := sEndLine + defaultMaxEndTargetSpanLines
		if maxLine < len(lineStarts) {
			capRel := lineStarts[maxLine] - winStart
			if capRel < subEnd && capRel >= afterOff {
				subEnd = capRel
				cappedBy120 = true
			}
		}
	}

	subWindow := windowText[afterOff:subEnd]
	endSpans, endLines, _ := findAnchorCandidates(subWindow, normEnd, relPath, winStart+afterOff, lineStarts, getPyStates)
	if len(endSpans) == 0 {
		if cappedBy120 && subEnd < len(windowText) {
			farSpans, farLines, _ := findAnchorCandidates(windowText[subEnd:], normEnd, relPath, winStart+subEnd, lineStarts, getPyStates)
			if len(farSpans) > 0 {
				return 0, 0, "", 0, 0, sandbox.NewHostError(sandbox.KindNoMatch, map[string]any{"lines": []int{farLines[0]}},
					"%s hunk %d: target matched at L%d, but endTarget is at L%d (%d lines away, exceeds %d-line span limit); set symbol or endLine",
					relPath, hIdx+1, sLine, farLines[0], farLines[0]-sLine, defaultMaxEndTargetSpanLines)
			}
		}
		hint := closestLineHint(subWindow, normEnd)
		return 0, 0, "", 0, 0, sandbox.NewHostError(sandbox.KindNoMatch, nil,
			"%s hunk %d: target matched at L%d, but endTarget not found after L%d%s", relPath, hIdx+1, sLine, sLine, hint)
	}
	if len(endSpans) > 1 {
		where := make([]string, len(endLines))
		for i, l := range endLines {
			where[i] = symbolAt(outline(), l, sep)
		}
		return 0, 0, "", 0, 0, sandbox.NewHostError(sandbox.KindAmbiguous, map[string]any{"lines": endLines, "symbols": where},
			"%s hunk %d: endTarget matches %d places after L%d (%s); include preceding statement in endTarget or set endLine",
			relPath, hIdx+1, len(endLines), sLine, formatMatchList(endLines, where, 6))
	}

	totalRelStart := sRel[0]
	totalRelEnd := afterOff + endSpans[0][1]
	absStart = winStart + totalRelStart
	absEnd = winStart + totalRelEnd
	eLine = offsetToLineNumber(lineStarts, maxInt(absStart, absEnd-1))

	// If `target` started with whitespace (intended as a full-line anchor with approximate indent) and matched as
	// a suffix of line sLine's leading indentation while `replacement` supplies its own leading indent on line 0,
	// snap absStart to the beginning of line sLine so leftover spaces are never double-prepended!
	line0Start := lineStarts[sLine-1]
	if absStart > line0Start {
		prefix := normText[line0Start:absStart]
		if strings.Trim(prefix, " \t") == "" {
			tLead, _ := splitLeadingHSpace(normStart)
			rLead, _ := splitLeadingHSpace(normRepl)
			if tLead != "" && rLead != "" {
				absStart = line0Start
				totalRelStart = absStart - winStart
			}
		}
	}

	finalRepl = normRepl
	if !startExact && !isIndentSensitivePath(relPath) {
		matchedStartLine := normText[lineStarts[sLine-1]:]
		if nl := strings.IndexByte(matchedStartLine, '\n'); nl >= 0 {
			matchedStartLine = matchedStartLine[:nl]
		}
		tFirst := strings.SplitN(normStart, "\n", 2)[0]
		finalRepl = realignReplacementIndent([]winLineSpan{{raw: matchedStartLine}}, []string{tFirst}, normRepl, false, isBacktickEscapeLang(relPath))
	}

	if isBraceConservationExt(relPath) {
		oldDelta, oldOK := netBraceDeltaLexed(windowText[totalRelStart:totalRelEnd], relPath)
		newDelta, newOK := netBraceDeltaLexed(finalRepl, relPath)
		if !oldOK || !newOK || oldDelta != newDelta {
			return 0, 0, "", 0, 0, sandbox.NewHostError(sandbox.KindBadArgs, nil,
				"%s hunk %d: target..endTarget span L%d-%d has net brace delta %+d vs replacement %+d; verify endTarget or use a standard target hunk",
				relPath, hIdx+1, sLine, eLine, oldDelta, newDelta)
		}
	}

	return absStart, absEnd, finalRepl, sLine, eLine, nil
}

func netBraceDeltaLexed(src, relPath string) (delta int, ok bool) {
	ext := strings.ToLower(filepath.Ext(relPath))
	jsLike := isJSLikeExt(ext)
	inBlock := false
	inBacktick := false

	lines := strings.Split(src, "\n")
	for _, line := range lines {
		for j := 0; j < len(line); {
			if inBlock {
				if j+1 < len(line) && line[j] == '*' && line[j+1] == '/' {
					inBlock = false
					j += 2
					continue
				}
				j++
				continue
			}
			if inBacktick {
				if line[j] == '\\' && ext != ".go" && j+1 < len(line) {
					j += 2
					continue
				}
				if line[j] == '`' {
					inBacktick = false
				}
				j++
				continue
			}
			if j+1 < len(line) && line[j] == '/' && line[j+1] == '/' {
				break
			}
			if j+1 < len(line) && line[j] == '/' && line[j+1] == '*' {
				inBlock = true
				j += 2
				continue
			}
			if line[j] == '"' {
				j++
				closed := false
				for j < len(line) {
					if line[j] == '\\' && j+1 < len(line) {
						j += 2
						continue
					}
					if line[j] == '"' {
						closed = true
						j++
						break
					}
					j++
				}
				if !closed {
					return 0, false
				}
				continue
			}
			if (jsLike || ext == ".go") && line[j] == '`' {
				inBacktick = true
				j++
				continue
			}
			if jsLike && line[j] == '\'' {
				j++
				closed := false
				for j < len(line) {
					if line[j] == '\\' && j+1 < len(line) {
						j += 2
						continue
					}
					if line[j] == '\'' {
						closed = true
						j++
						break
					}
					j++
				}
				if !closed {
					return 0, false
				}
				continue
			}
			if !jsLike && line[j] == '\'' {
				if next := scanClosedCharLiteral(line, j); next > j {
					j = next
					continue
				}
			}
			switch line[j] {
			case '{':
				delta++
			case '}':
				delta--
			}
			j++
		}
	}
	if inBlock || inBacktick {
		return 0, false
	}
	return delta, true
}

func alignSymbolReplacementIndent(origStartLine, normRepl, relPath string) string {
	baseIndent, _ := splitLeadingHSpace(origStartLine)
	if baseIndent == "" {
		return normRepl
	}
	hasTrailingNL := strings.HasSuffix(normRepl, "\n")
	body := strings.TrimSuffix(normRepl, "\n")
	rLines := strings.Split(body, "\n")

	firstNonBlank := -1
	for i, l := range rLines {
		if strings.TrimSpace(l) != "" {
			firstNonBlank = i
			break
		}
	}
	if firstNonBlank < 0 {
		return normRepl
	}
	replLead, _ := splitLeadingHSpace(rLines[firstNonBlank])
	if replLead != "" {
		return normRepl
	}

	if isPythonContinuationPath(relPath) {
		pySt := scanPythonLines(rLines)
		for i, rl := range rLines {
			if pySt[i].startTriple == "" && strings.TrimSpace(rl) != "" {
				rLines[i] = baseIndent + rl
			}
		}
	} else {
		startsInBT := trackMultilineBackticks(rLines, isBacktickEscapeLang(relPath))
		for i, rl := range rLines {
			if !startsInBT[i] && strings.TrimSpace(rl) != "" {
				rLines[i] = baseIndent + rl
			}
		}
	}
	out := strings.Join(rLines, "\n")
	if hasTrailingNL {
		out += "\n"
	}
	return out
}

func exactLineSpanOffsets(text string, starts []int, startLine, endLine int) (int, int) {
	s := maxInt(1, minInt(startLine, len(starts)))
	e := maxInt(s, minInt(endLine, len(starts)))
	startOff := starts[s-1]
	endOff := len(text)
	if e < len(starts) {
		endOff = starts[e] - 1
	}
	if endOff < startOff {
		endOff = startOff
	}
	return startOff, endOff
}

func combinePlanErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	if len(errs) == 1 {
		return errs[0]
	}
	primaryKind := sandbox.KindNoMatch
	var primaryData map[string]any
	msgs := make([]string, 0, minInt(len(errs), 5)+1)
	for i, e := range errs {
		var he *sandbox.HostError
		msg := e.Error()
		if errors.As(e, &he) {
			msg = he.Msg
			if i == 0 {
				primaryKind = he.Kind
				primaryData = he.Data
			}
		}
		if i < 5 {
			if i == 0 {
				msgs = append(msgs, msg)
			} else {
				msgs = append(msgs, "  also: "+msg)
			}
		}
	}
	if len(errs) > 5 {
		msgs = append(msgs, fmt.Sprintf("  (+%d more hunk/file errors)", len(errs)-5))
	}
	return &sandbox.HostError{
		Kind: primaryKind,
		Data: primaryData,
		Msg:  strings.Join(msgs, "\n"),
	}
}

func realignReplacementIndent(matched []winLineSpan, tLines []string, normRepl string, keepIndent, btEscape bool) string {
	if keepIndent || len(matched) == 0 || len(matched) != len(tLines) {
		return normRepl
	}
	hasTrailingNL := strings.HasSuffix(normRepl, "\n")
	replBody := strings.TrimSuffix(normRepl, "\n")
	rLines := strings.Split(replBody, "\n")

	tStartsInBT := trackMultilineBackticks(tLines, btEscape)
	rStartsInBT := trackMultilineBackticks(rLines, btEscape)

	extraPrefix := ""
	missingPrefix := ""
	mode := 0
	for i := range tLines {
		if tStartsInBT[i] || strings.TrimSpace(tLines[i]) == "" || strings.TrimSpace(matched[i].raw) == "" {
			continue
		}
		fLead, _ := splitLeadingHSpace(matched[i].raw)
		tLead, _ := splitLeadingHSpace(tLines[i])
		if fLead == tLead {
			mode = -1
			break
		}
		if strings.HasPrefix(tLead, fLead) {
			diff := tLead[len(fLead):]
			if mode == 0 {
				mode = 1
				extraPrefix = diff
			} else if mode != 1 || diff != extraPrefix {
				mode = -1
				break
			}
		} else if strings.HasPrefix(fLead, tLead) {
			diff := fLead[len(tLead):]
			if mode == 0 {
				mode = 2
				missingPrefix = diff
			} else if mode != 2 || diff != missingPrefix {
				mode = -1
				break
			}
		} else {
			mode = -1
			break
		}
	}

	switch mode {
	case 1:
		allHave := true
		for i, rl := range rLines {
			if !rStartsInBT[i] && strings.TrimSpace(rl) != "" && !strings.HasPrefix(rl, extraPrefix) {
				allHave = false
				break
			}
		}
		if allHave {
			for i, rl := range rLines {
				if !rStartsInBT[i] && strings.TrimSpace(rl) != "" {
					rLines[i] = rl[len(extraPrefix):]
				}
			}
		}
	case 2:
		for i, rl := range rLines {
			if !rStartsInBT[i] && strings.TrimSpace(rl) != "" {
				rLines[i] = missingPrefix + rl
			}
		}
	default:
		if !tStartsInBT[0] && !rStartsInBT[0] &&
			strings.TrimSpace(matched[0].raw) != "" &&
			strings.TrimSpace(tLines[0]) != "" &&
			strings.TrimSpace(rLines[0]) != "" {
			fLead0, _ := splitLeadingHSpace(matched[0].raw)
			tLead0, _ := splitLeadingHSpace(tLines[0])
			rLead0, _ := splitLeadingHSpace(rLines[0])
			if fLead0 != "" && tLead0 == "" && rLead0 == "" {
				rLines[0] = fLead0 + rLines[0]
			}
		}
	}

	out := strings.Join(rLines, "\n")
	if hasTrailingNL {
		out += "\n"
	}
	return out
}

func isBoilerplateAnchorLine(trimmed string) bool {
	low := strings.ToLower(trimmed)
	switch low {
	case "}", "};", ")", ");", "]", "];", "} else {", "else {", "else", "return", "return;", "return nil", "return err",
		"break", "break;", "continue", "continue;", "default:", "try {", "} catch {", "finally {":
		return true
	}
	return false
}

func anchorLineRank(l string) int {
	t := strings.TrimSpace(l)
	rc := utf8.RuneCountInString(t)
	if rc < 3 {
		return -1
	}
	if isBoilerplateAnchorLine(t) {
		return 1
	}
	alnum := 0
	for _, r := range t {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			alnum++
		}
	}
	return alnum*3 + minInt(rc, 80)
}

func selectBestAnchorLine(targetLines []string) (bestIdx int, bestAnchor string) {
	bestIdx = -1
	bestRank := -1
	for idx, l := range targetLines {
		rank := anchorLineRank(l)
		if rank > bestRank {
			bestRank = rank
			bestIdx = idx
			bestAnchor = strings.TrimSpace(l)
		}
	}
	return bestIdx, bestAnchor
}

func selectDiverseAnchors(targetLines []string) []int {
	var out []int
	seen := map[int]bool{}
	add := func(idx int) {
		if idx >= 0 && idx < len(targetLines) && !seen[idx] && strings.TrimSpace(targetLines[idx]) != "" {
			seen[idx] = true
			out = append(out, idx)
		}
	}

	for i, l := range targetLines {
		if anchorLineRank(l) > 1 {
			add(i)
			break
		}
	}
	for i := len(targetLines) - 1; i >= 0; i-- {
		if anchorLineRank(lAt(targetLines, i)) > 1 {
			add(i)
			break
		}
	}
	mid := len(targetLines) / 2
	if best1, _ := selectBestAnchorLine(targetLines[:maxInt(1, mid)]); best1 >= 0 {
		add(best1)
	}
	if mid < len(targetLines) {
		if best2, _ := selectBestAnchorLine(targetLines[mid:]); best2 >= 0 {
			add(mid + best2)
		}
	}
	if len(out) == 0 {
		if b, _ := selectBestAnchorLine(targetLines); b >= 0 {
			add(b)
		}
	}
	return out
}

func lAt(s []string, i int) string {
	if i >= 0 && i < len(s) {
		return s[i]
	}
	return ""
}

func scoreLineSimilarity(ft, anchor, anchorLower string, anchorRunes int, words []string, anchorBigrams map[string]struct{}) int {
	if ft == "" {
		return 0
	}
	if ft == anchor {
		return 100
	}
	flLower := strings.ToLower(ft)
	if flLower == anchorLower {
		return 90
	}
	score := 0
	if strings.Contains(flLower, anchorLower) {
		s := 75
		if len(flLower) > 0 {
			s += (10 * len(anchorLower)) / len(flLower)
		}
		if s > score {
			score = s
		}
	} else if strings.Contains(anchorLower, flLower) {
		flRunes := utf8.RuneCountInString(flLower)
		if flRunes >= 8 && flRunes*3 >= anchorRunes {
			s := 30 + (45*flRunes)/anchorRunes
			if s > score {
				score = s
			}
		}
	}
	if len(words) > 0 {
		matched := 0
		for _, w := range words {
			if utf8.RuneCountInString(w) >= 3 && strings.Contains(flLower, w) {
				matched++
			}
		}
		if matched > 0 {
			ws := (matched * 65) / len(words)
			if ws > score {
				score = ws
			}
		}
	}
	if len(anchorBigrams) > 0 {
		flBigrams := runeBigrams(flLower)
		if len(flBigrams) > 0 {
			overlap := 0
			for bg := range anchorBigrams {
				if _, ok := flBigrams[bg]; ok {
					overlap++
				}
			}
			if overlap > 0 {
				bs := (2 * overlap * 80) / (len(anchorBigrams) + len(flBigrams))
				if bs > score {
					score = bs
				}
			}
		}
	}
	return score
}

func truncateHintLine(s string) string {
	if len(s) > 120 {
		return output.SliceUTF8Bytes(s, 120) + "…"
	}
	return s
}

func describeLeadWhitespace(s string) string {
	spaces, tabs := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			spaces++
		} else if s[i] == '\t' {
			tabs++
		} else {
			break
		}
	}
	switch {
	case spaces > 0 && tabs > 0:
		return fmt.Sprintf("%d space(s) + %d tab(s)", spaces, tabs)
	case tabs > 0:
		return fmt.Sprintf("%d tab(s)", tabs)
	default:
		return fmt.Sprintf("%d space(s)", spaces)
	}
}

func simulateJSEscapeLoss(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\', '"', '\'', '`':
				b.WriteByte(s[i])
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func closestLineHint(fileText, target string) string {
	trimmedTarget := strings.TrimSuffix(target, "\n")
	targetLines := strings.Split(trimmedTarget, "\n")
	anchors := selectDiverseAnchors(targetLines)
	if len(anchors) == 0 {
		return ""
	}
	fileLines := strings.Split(fileText, "\n")
	if len(fileLines) == 0 {
		return ""
	}

	candidateTops := map[int]int{}
	for _, aIdx := range anchors {
		anchor := strings.TrimSpace(targetLines[aIdx])
		anchorLower := strings.ToLower(anchor)
		anchorRunes := utf8.RuneCountInString(anchorLower)
		words := strings.Fields(anchorLower)
		bgs := runeBigrams(anchorLower)

		type hit struct {
			fileIdx int
			score   int
		}
		var topHits []hit
		for i, fl := range fileLines {
			sc := scoreLineSimilarity(strings.TrimSpace(fl), anchor, anchorLower, anchorRunes, words, bgs)
			if sc < 35 {
				continue
			}
			if isBoilerplateAnchorLine(anchor) && sc < 100 {
				continue
			}
			topHits = append(topHits, hit{fileIdx: i, score: sc})
		}
		sort.Slice(topHits, func(a, b int) bool { return topHits[a].score > topHits[b].score })
		if len(topHits) > 12 {
			topHits = topHits[:12]
		}
		for _, h := range topHits {
			top := maxInt(0, h.fileIdx-aIdx)
			if h.score > candidateTops[top] {
				candidateTops[top] = h.score
			}
		}
	}
	if len(candidateTops) == 0 {
		return ""
	}

	bestTop := -1
	bestWindowScore := -1
	bestSingleScore := 0
	for top, singleSc := range candidateTops {
		winScore := singleSc
		if len(targetLines) > 1 {
			fCursor := top
			for _, tl := range targetLines {
				tt := strings.TrimSpace(tl)
				if tt == "" {
					fCursor++
					continue
				}
				bestDelta := 0
				bestMatch := 0
				for d := -1; d <= 2; d++ {
					fi := fCursor + d
					if fi < 0 || fi >= len(fileLines) {
						continue
					}
					ft := strings.TrimSpace(fileLines[fi])
					if ft == tt {
						m := 35
						if isBoilerplateAnchorLine(tt) {
							m = 6
						}
						if d == 0 {
							m += 5
						}
						if m > bestMatch {
							bestMatch = m
							bestDelta = d
						}
					} else if ft != "" && strings.EqualFold(ft, tt) && 20 > bestMatch {
						bestMatch = 20
						bestDelta = d
					}
				}
				winScore += bestMatch
				fCursor += 1 + bestDelta
			}
		}
		if winScore > bestWindowScore || (winScore == bestWindowScore && top < bestTop) {
			bestWindowScore = winScore
			bestTop = top
			bestSingleScore = singleSc
		}
	}
	if bestTop < 0 || bestSingleScore < 35 {
		return ""
	}

	firstCodeDiff := -1
	firstRawDiff := -1
	matchedPrefix := 0
	for j := 0; j < len(targetLines); j++ {
		fi := bestTop + j
		if fi >= len(fileLines) {
			if firstCodeDiff < 0 {
				firstCodeDiff = j
			}
			break
		}
		if targetLines[j] != fileLines[fi] && firstRawDiff < 0 {
			firstRawDiff = j
		}
		if strings.TrimSpace(targetLines[j]) != strings.TrimSpace(fileLines[fi]) {
			if firstCodeDiff < 0 {
				firstCodeDiff = j
			}
		} else if firstCodeDiff < 0 {
			matchedPrefix++
		}
	}

	diffTargetIdx := firstCodeDiff
	pureWhitespaceDiff := false
	if diffTargetIdx < 0 {
		pureWhitespaceDiff = true
		if firstRawDiff >= 0 {
			diffTargetIdx = firstRawDiff
		} else {
			diffTargetIdx = 0
		}
	}
	diffFileIdx := minInt(len(fileLines)-1, bestTop+diffTargetIdx)

	var extraNotes []string
	tDiffLine := targetLines[diffTargetIdx]
	fDiffLine := fileLines[diffFileIdx]
	if pureWhitespaceDiff && strings.TrimSpace(tDiffLine) == strings.TrimSpace(fDiffLine) {
		tWS := describeLeadWhitespace(tDiffLine)
		fWS := describeLeadWhitespace(fDiffLine)
		if tWS != fWS {
			extraNotes = append(extraNotes, fmt.Sprintf("indent differs at L%d: target has %s, file has %s", diffFileIdx+1, tWS, fWS))
		}
	}
	if strings.Contains(fDiffLine, `\`) && fDiffLine != tDiffLine {
		sim := simulateJSEscapeLoss(fDiffLine)
		if sim == tDiffLine || strings.TrimSpace(sim) == strings.TrimSpace(tDiffLine) {
			extraNotes = append(extraNotes, "Hint: backslashes in 'target' were consumed by JS string escaping; pass raw text via top-level 'payloads' (P.old / P.new) or String.raw")
		}
	} else if strings.Contains(fileText, `\`) && strings.Contains(simulateJSEscapeLoss(fileText), trimmedTarget) {
		extraNotes = append(extraNotes, "Hint: backslashes in 'target' were consumed by JS string escaping; pass raw text via top-level 'payloads' (P.old / P.new) or String.raw")
	}

	var from, to int
	if len(targetLines) <= 8 {
		from = maxInt(0, bestTop-1)
		if bestTop > 0 && isBoilerplateAnchorLine(strings.TrimSpace(targetLines[0])) {
			from = bestTop
		}
		to = minInt(len(fileLines)-1, bestTop+len(targetLines)+1)
	} else {
		from = maxInt(bestTop, diffFileIdx-2)
		to = minInt(len(fileLines)-1, diffFileIdx+4)
	}
	var snip []string
	for i := from; i <= to; i++ {
		snip = append(snip, fmt.Sprintf("    %d: %s", i+1, truncateHintLine(fileLines[i])))
	}

	var b strings.Builder
	if len(targetLines) > 1 && matchedPrefix > 0 && !pureWhitespaceDiff {
		fmt.Fprintf(&b, "; matched first %d/%d line(s) from L%d, mismatch at target line %d vs L%d:\n",
			matchedPrefix, len(targetLines), bestTop+1, diffTargetIdx+1, diffFileIdx+1)
		fmt.Fprintf(&b, "  - target (%d): %s\n  + file  L%-4d: %s\n",
			diffTargetIdx+1, truncateHintLine(tDiffLine), diffFileIdx+1, truncateHintLine(fDiffLine))
		fmt.Fprintf(&b, "  closest L%d-%d:\n%s", from+1, to+1, strings.Join(snip, "\n"))
	} else {
		fmt.Fprintf(&b, "; closest L%d-%d", from+1, to+1)
		if len(extraNotes) > 0 && pureWhitespaceDiff {
			fmt.Fprintf(&b, " (%s)", extraNotes[0])
			extraNotes = extraNotes[1:]
		}
		fmt.Fprintf(&b, ":\n  - target (%d): %s\n  + file  L%-4d: %s\n%s",
			diffTargetIdx+1, truncateHintLine(tDiffLine), diffFileIdx+1, truncateHintLine(fDiffLine), strings.Join(snip, "\n"))
	}
	for _, note := range extraNotes {
		b.WriteString("\n  " + note)
	}
	return b.String()
}

// runeBigrams builds a bounded set of non-whitespace Unicode character bigrams for
// language-agnostic line similarity (especially effective on CJK/unspaced text).
func runeBigrams(s string) map[string]struct{} {
	runes := make([]rune, 0, minInt(len(s), 256))
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		runes = append(runes, r)
		if len(runes) >= 256 {
			break
		}
	}
	if len(runes) < 2 {
		return nil
	}
	out := make(map[string]struct{}, len(runes)-1)
	for i := 0; i < len(runes)-1; i++ {
		out[string(runes[i:i+2])] = struct{}{}
	}
	return out
}

func parseEditApplyArgs(args []any) (specs []FileEditSpec, dryRun bool, bestEffort bool, verifyCmd string, err error) {
	if arr, ok := args[0].([]any); ok {
		specs, err = decodeFileSpecs(arr)
		return specs, false, false, "", err
	}
	m, ok := args[0].(map[string]any)
	if !ok {
		return nil, false, false, "", errors.New("edit.apply argument must be an object or array")
	}
	dryRun = optBool(m, "dryRun", false)
	bestEffort = optBool(m, "bestEffort", false)
	verifyCmd = firstString(m, "verify")
	if editsRaw, hasEdits := m["edits"].([]any); hasEdits {
		// Distinguish `{ path, edits: [{ oldText, newText }] }` (single-file) vs `{ edits: [{ path, hunks }] }` (multi-file)
		if topPath := firstString(m, "path", "file"); topPath != "" {
			hunks, hErr := decodeHunks(editsRaw)
			if hErr != nil {
				return nil, false, false, "", hErr
			}
			return []FileEditSpec{{Path: topPath, Hunks: hunks}}, dryRun, bestEffort, verifyCmd, nil
		}
		specs, err = decodeFileSpecs(editsRaw)
		return specs, dryRun, bestEffort, verifyCmd, err
	}
	// Single-file `{ path, hunks: [...] }` or `{ path, target, replacement }`
	sp, sErr := decodeSingleFileMap(m)
	if sErr != nil {
		return nil, false, false, "", sErr
	}
	return []FileEditSpec{sp}, dryRun, bestEffort, verifyCmd, nil
}

func parseTxEditArgs(args []any) ([]FileEditSpec, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, errors.New("tx.edit requires (path, hunks) or ({ path, hunks }) or ([...])")
	}
	if s, ok := args[0].(string); ok {
		if len(args) < 2 {
			return nil, errors.New("tx.edit(path, hunks) requires hunks array as second argument")
		}
		if arr, isArr := args[1].([]any); isArr {
			hunks, err := decodeHunks(arr)
			if err != nil {
				return nil, err
			}
			return []FileEditSpec{{Path: s, Hunks: hunks}}, nil
		}
		if m, isMap := args[1].(map[string]any); isMap {
			h, err := decodeSingleHunk(m)
			if err != nil {
				return nil, err
			}
			return []FileEditSpec{{Path: s, Hunks: []EditHunk{h}}}, nil
		}
	}
	specs, _, _, _, err := parseEditApplyArgs(args)
	return specs, err
}

func decodeFileSpecs(arr []any) ([]FileEditSpec, error) {
	out := make([]FileEditSpec, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("each file edit item must be an object")
		}
		sp, err := decodeSingleFileMap(m)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, nil
}

func decodeSingleFileMap(m map[string]any) (FileEditSpec, error) {
	p := firstString(m, "path", "file", "file_path")
	if p == "" {
		return FileEditSpec{}, errors.New("file edit requires 'path'")
	}
	if createVal, hasCreate := m["create"]; hasCreate && createVal != nil {
		if cs, ok := createVal.(string); ok {
			return FileEditSpec{
				Path:      p,
				Create:    &cs,
				Overwrite: optBool(m, "overwrite", false),
				EOL:       firstString(m, "eol"),
			}, nil
		}
	}
	if hRaw, ok := m["hunks"].([]any); ok {
		hunks, err := decodeHunks(hRaw)
		if err != nil {
			return FileEditSpec{}, err
		}
		return FileEditSpec{Path: p, Hunks: hunks}, nil
	}
	if eRaw, ok := m["edits"].([]any); ok {
		hunks, err := decodeHunks(eRaw)
		if err != nil {
			return FileEditSpec{}, err
		}
		return FileEditSpec{Path: p, Hunks: hunks}, nil
	}
	// Inline single hunk on the file object itself
	h, err := decodeSingleHunk(m)
	if err != nil {
		return FileEditSpec{}, err
	}
	return FileEditSpec{Path: p, Hunks: []EditHunk{h}}, nil
}

func decodeHunks(arr []any) ([]EditHunk, error) {
	out := make([]EditHunk, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("each hunk must be an object")
		}
		h, err := decodeSingleHunk(m)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

func decodeSingleHunk(m map[string]any) (EditHunk, error) {
	target := firstString(m, "target", "oldText", "old_string", "old", "from")
	endTarget := firstString(m, "endTarget", "end_target")
	replaceSym := optBool(m, "replaceSymbol", false)
	sym := firstString(m, "symbol", "in")
	startLine := optInt(m, "startLine", 0)
	endLine := optInt(m, "endLine", 0)
	replaceAll := optBool(m, "replaceAll", optBool(m, "all", false))
	nth := optInt(m, "nth", optInt(m, "occurrence", 0))

	var repl string
	foundRepl := false
	for _, k := range []string{"replacement", "newText", "new_string", "new", "to"} {
		if v, exists := m[k]; exists {
			s, ok := v.(string)
			if !ok {
				return EditHunk{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "hunk field %q must be a string (pass %q: \"\" to delete)", k, k)
			}
			repl = s
			foundRepl = true
			break
		}
	}
	if !foundRepl {
		for _, typo := range []string{"replace", "replaceWith", "replacementText", "text", "value", "content", "body"} {
			if _, exists := m[typo]; exists {
				return EditHunk{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "hunk missing required 'replacement' field (found %q instead — rename it to 'replacement', or pass replacement: \"\" explicitly to delete)", typo)
			}
		}
		return EditHunk{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "hunk missing required 'replacement' field (pass replacement: \"\" explicitly to delete target)")
	}

	if replaceSym {
		if strings.TrimSpace(sym) == "" {
			return EditHunk{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "replaceSymbol: true requires non-empty 'symbol'")
		}
		if target != "" || endTarget != "" {
			return EditHunk{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "replaceSymbol: true cannot be combined with 'target' or 'endTarget' (omit target to replace the whole symbol, or remove replaceSymbol: true)")
		}
		if replaceAll || nth > 0 || startLine > 0 || endLine > 0 {
			return EditHunk{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "replaceSymbol: true cannot be combined with startLine, endLine, nth, or replaceAll")
		}
		return EditHunk{
			Symbol:        sym,
			ReplaceSymbol: true,
			Replacement:   repl,
		}, nil
	}

	if target == "" {
		return EditHunk{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "hunk requires non-empty 'target' (or pass { symbol, replaceSymbol: true, replacement } to replace an entire symbol)")
	}
	if endTarget != "" && (replaceAll || nth > 0) {
		return EditHunk{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "endTarget cannot be combined with replaceAll or nth")
	}

	return EditHunk{
		Target:      target,
		EndTarget:   endTarget,
		Replacement: repl,
		StartLine:   startLine,
		EndLine:     endLine,
		ReplaceAll:  replaceAll,
		Nth:         nth,
		Symbol:      sym,
	}, nil
}
