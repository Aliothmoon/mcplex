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

	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

// EditHunk represents a single non-overlapping replacement inside a file.
type EditHunk struct {
	Target      string `json:"target"`
	Replacement string `json:"replacement"`
	StartLine   int    `json:"startLine,omitempty"`
	EndLine     int    `json:"endLine,omitempty"`
	ReplaceAll  bool   `json:"replaceAll,omitempty"`
	Nth         int    `json:"nth,omitempty"`    // 1-based: replace only the Nth match (like sed s/x/y/N)
	Symbol      string `json:"symbol,omitempty"` // restrict matching to a symbol's lines, e.g. "Editor::on_escape"
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
	snapshots  map[string]fileSnapshot
	touched    []string
	committed  bool
	rolledBack bool
	lastVerify map[string]any
}

var txOwnerSeq atomic.Int64

// EditTxBinder exposes `edit.*` (`apply`, `replace`) and `tx.*` (`begin` -> atomic transactions with rollback).
type EditTxBinder struct {
	Policy      *policy.Engine
	ShellRunner *ShellGitBinder
	Locks       *FileLockTable
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
				b.Locks.ReleaseOwner(tx.lockOwner)
				continue
			}
			restored, failed := rollbackSnapshots(tx)
			b.Locks.ReleaseOwner(tx.lockOwner)
			if len(restored) > 0 {
				env.AddWarning(fmt.Sprintf("%s was never committed — auto-rolled back %d file(s): %s (call `await t.commit()` to keep tx edits)", id, len(restored), joinLimited(restored, 10)))
			}
			if len(failed) > 0 {
				env.AddWarning(fmt.Sprintf("ROLLBACK FAILED for %s: %s — these files may still contain uncommitted tx edits; inspect them before retrying", id, formatRollbackFailures(failed)))
			}
		}
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
			res, err := b.applyFileSpecs(ctx, env.Cwd, specs, dryRun, bestEffort, nil)
			if err != nil {
				return nil, err
			}
			if verifyCmd != "" && !dryRun && b.ShellRunner != nil {
				vRes, vErr := b.ShellRunner.RunSingleCommand(ctx, env.Cwd, verifyCmd, 60, true, true)
				if vErr != nil {
					return nil, vErr
				}
				res["verify"] = vRes
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
					Replacement: repl,
					StartLine:   optInt(opts, "startLine", 0),
					EndLine:     optInt(opts, "endLine", 0),
					ReplaceAll:  optBool(opts, "replaceAll", optBool(opts, "all", false)),
					Nth:         optInt(opts, "nth", optInt(opts, "occurrence", 0)),
					Symbol:      firstString(opts, "symbol", "in"),
				}},
			}
			return b.applyFileSpecs(ctx, env.Cwd, []FileEditSpec{spec}, false, false, nil)
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
			owner := fmt.Sprintf("tx:%d:%s", txOwnerSeq.Add(1), id)
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
			return b.applyFileSpecs(ctx, env.Cwd, specs, false, false, tx)
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
			return b.applyFileSpecs(ctx, env.Cwd, []FileEditSpec{spec}, false, false, tx)
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
			b.Locks.ReleaseOwner(tx.lockOwner)
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
			b.Locks.ReleaseOwner(tx.lockOwner)
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
			if err := b.Locks.Acquire(ctx, tx.lockOwner, abs); err != nil {
				return nil, err
			}
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
			if _, seen := tx.snapshots[abs]; !seen {
				tx.snapshots[abs] = fileSnapshot{absPath: abs, relPath: rel, existed: true, mode: info.Mode().Perm(), origBytes: orig}
				tx.touched = append(tx.touched, rel)
			}
			return nil, os.Remove(abs)
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
			if err := b.Locks.Acquire(ctx, tx.lockOwner, absFrom, absTo); err != nil {
				return nil, err
			}
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
				if _, seen := tx.snapshots[snap.absPath]; !seen {
					tx.snapshots[snap.absPath] = snap
					tx.touched = append(tx.touched, snap.relPath)
				}
			}
			if err := os.MkdirAll(filepath.Dir(absTo), 0o755); err != nil {
				return nil, err
			}
			return nil, os.Rename(absFrom, absTo)
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
			err = writeFileWithMode(snap.absPath, snap.origBytes, snap.mode)
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

	// Acquire per-file locks BEFORE reading/planning so concurrent exec calls never interleave dirty reads or clobber rollbacks (#8).
	var lockOwner string
	releaseOnReturn := false
	if !dryRun && b.Locks != nil && len(merged) > 0 {
		paths := make([]string, len(merged))
		for i, m := range merged {
			paths[i] = m.absPath
		}
		if tx != nil {
			lockOwner = tx.lockOwner
		} else {
			lockOwner = fmt.Sprintf("edit:%d", txOwnerSeq.Add(1))
			releaseOnReturn = true
		}
		if err := b.Locks.Acquire(ctx, lockOwner, paths...); err != nil {
			return nil, err
		}
		if releaseOnReturn {
			defer b.Locks.ReleaseOwner(lockOwner)
		}
	}

	var planned []plannedFileWrite
	for _, m := range merged {
		pw, planErr := planSingleFile(m.absPath, m.rel, m.spec)
		if planErr != nil {
			if bestEffort {
				failures = append(failures, map[string]any{"path": m.rel, "error": planErr.Error()})
				continue
			}
			return nil, planErr
		}
		planned = append(planned, pw)
	}

	if !dryRun {
		var written []plannedFileWrite
		for _, pw := range planned {
			if tx != nil {
				if _, seen := tx.snapshots[pw.absPath]; !seen {
					tx.snapshots[pw.absPath] = fileSnapshot{
						absPath:   pw.absPath,
						relPath:   pw.relPath,
						existed:   pw.existed,
						mode:      pw.mode,
						origBytes: pw.origBytes,
					}
					tx.touched = append(tx.touched, pw.relPath)
				}
			}
			if err := writeFileWithMode(pw.absPath, pw.newBytes, pw.mode); err != nil {
				if tx == nil && len(written) > 0 {
					// Outside a tx, keep multi-file edit.apply all-or-nothing: undo files already written in this call.
					var undoFailed []map[string]any
					for i := len(written) - 1; i >= 0; i-- {
						w := written[i]
						if uErr := restoreSnapshot(fileSnapshot{absPath: w.absPath, relPath: w.relPath, existed: w.existed, mode: w.mode, origBytes: w.origBytes}); uErr != nil {
							undoFailed = append(undoFailed, map[string]any{"path": w.relPath, "error": uErr.Error()})
						}
					}
					if len(undoFailed) > 0 {
						return nil, fmt.Errorf("failed writing '%s': %w; ALSO failed to undo earlier writes: %s", pw.relPath, err, formatRollbackFailures(undoFailed))
					}
					return nil, fmt.Errorf("failed writing '%s': %w (earlier files in this edit.apply call were restored)", pw.relPath, err)
				}
				return nil, fmt.Errorf("failed writing '%s': %w", pw.relPath, err)
			}
			written = append(written, pw)
		}
	}

	applied := make([]map[string]any, len(planned))
	for i, pw := range planned {
		applied[i] = map[string]any{
			"path":      pw.relPath,
			"hunks":     pw.hunksApplied,
			"lineDelta": pw.lineDelta,
		}
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
		start int
		end   int
		repl  string
		idx   int
	}
	var spans []span

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
	sep := symbolSep(relPath)

	for hIdx, h := range spec.Hunks {
		normTarget := strings.ReplaceAll(h.Target, "\r\n", "\n")
		scope := fmt.Sprintf("L%d-%d", h.StartLine, h.EndLine)
		if h.Symbol != "" {
			s, sErr := resolveSymbol(outline(), h.Symbol, relPath)
			if sErr != nil {
				return plannedFileWrite{}, prefixHunkErr(sErr, hIdx)
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
		normRepl := strings.ReplaceAll(h.Replacement, "\r\n", "\n")
		if normTarget == "" {
			return plannedFileWrite{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s hunk %d: empty target", relPath, hIdx+1)
		}
		winStart, winEnd, wErr := lineWindowBounds(normText, lineStarts, h.StartLine, h.EndLine)
		if wErr != nil {
			return plannedFileWrite{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s hunk %d: %v", relPath, hIdx+1, wErr)
		}
		windowText := normText[winStart:winEnd]

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
				spans = append(spans, span{start: absStart, end: absEnd, repl: normRepl, idx: hIdx})
				searchOff += idx + len(normTarget)
				count++
			}
			if count == 0 {
				hint := closestLineHint(normText, normTarget)
				return plannedFileWrite{}, sandbox.NewHostError(sandbox.KindNoMatch, nil, "%s hunk %d: target not found%s", relPath, hIdx+1, hint)
			}
		} else {
			firstIdx := strings.Index(windowText, normTarget)
			if firstIdx < 0 {
				// Check if target exists outside the specified startLine..endLine window (drifted lines hint!)
				if (h.StartLine > 0 || h.EndLine > 0 || h.Symbol != "") && strings.Contains(normText, normTarget) {
					actualOff := strings.Index(normText, normTarget)
					actualLine := offsetToLineNumber(lineStarts, actualOff)
					return plannedFileWrite{}, sandbox.NewHostError(sandbox.KindNoMatch, map[string]any{"lines": []int{actualLine}}, "%s hunk %d: target not in %s; exact match at L%d", relPath, hIdx+1, scope, actualLine)
				}
				hint := closestLineHint(normText, normTarget)
				return plannedFileWrite{}, sandbox.NewHostError(sandbox.KindNoMatch, nil, "%s hunk %d: target not found%s", relPath, hIdx+1, hint)
			}
			if h.Nth > 0 {
				offs := matchOffsets(windowText, normTarget)
				if h.Nth > len(offs) {
					lines := matchLines(windowText, normTarget, winStart, lineStarts)
					return plannedFileWrite{}, sandbox.NewHostError(sandbox.KindNoMatch, map[string]any{"lines": lines},
						"%s hunk %d: nth %d but only %d match(es) (%s)", relPath, hIdx+1, h.Nth, len(offs), formatLineList(lines, 6))
				}
				firstIdx = offs[h.Nth-1]
			} else if strings.Contains(windowText[firstIdx+len(normTarget):], normTarget) {
				lines := matchLines(windowText, normTarget, winStart, lineStarts)
				where := make([]string, len(lines))
				for i, l := range lines {
					where[i] = symbolAt(outline(), l, sep)
				}
				return plannedFileWrite{}, sandbox.NewHostError(sandbox.KindAmbiguous, map[string]any{"lines": lines, "symbols": where},
					"%s hunk %d: target matches %d places (%s); set symbol, nth, or replaceAll: true", relPath, hIdx+1, len(lines), formatMatchList(lines, where, 6))
			}
			absStart := winStart + firstIdx
			spans = append(spans, span{start: absStart, end: absStart + len(normTarget), repl: normRepl, idx: hIdx})
		}
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return plannedFileWrite{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s: hunks %d and %d overlap", relPath, spans[i-1].idx+1, spans[i].idx+1)
		}
	}

	var bld strings.Builder
	cursor := 0
	for _, sp := range spans {
		bld.WriteString(normText[cursor:sp.start])
		bld.WriteString(sp.repl)
		cursor = sp.end
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

// closestLineHint finds the closest matching anchor line in the file so the caller can self-correct without an extra read turn.
func closestLineHint(fileText, target string) string {
	targetLines := strings.Split(target, "\n")
	anchor := ""
	for _, l := range targetLines {
		t := strings.TrimSpace(l)
		if utf8.RuneCountInString(t) >= 3 {
			anchor = t
			break
		}
	}
	if anchor == "" {
		return ""
	}
	fileLines := strings.Split(fileText, "\n")
	bestIdx := -1
	bestScore := 0
	anchorLower := strings.ToLower(anchor)
	anchorRunes := utf8.RuneCountInString(anchorLower)
	words := strings.Fields(anchorLower)
	anchorBigrams := runeBigrams(anchorLower)

	for i, fl := range fileLines {
		ft := strings.TrimSpace(fl)
		if ft == "" {
			continue
		}
		flLower := strings.ToLower(ft)
		score := 0
		if ft == anchor {
			score = 100
		} else if flLower == anchorLower {
			score = 90
		} else {
			if strings.Contains(flLower, anchorLower) {
				s := 75
				if len(flLower) > 0 {
					s += (10 * len(anchorLower)) / len(flLower)
				}
				if s > score {
					score = s
				}
			} else if strings.Contains(anchorLower, flLower) {
				// Only reward candidate-inside-anchor containment when the candidate line
				// is substantial (prevents 1-char punctuation lines like `]` or `}` from
				// hijacking the closest hint for a long target line).
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
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	if bestIdx < 0 || bestScore < 35 {
		return ""
	}
	from := maxInt(0, bestIdx-1)
	to := minInt(len(fileLines)-1, bestIdx+minInt(len(targetLines), 5))
	var snip []string
	for i := from; i <= to; i++ {
		line := fileLines[i]
		if len(line) > 120 {
			line = output.SliceUTF8Bytes(line, 120) + "…"
		}
		snip = append(snip, fmt.Sprintf("    %d: %s", i+1, line))
	}
	return fmt.Sprintf("; closest L%d-%d:\n%s", from+1, to+1, strings.Join(snip, "\n"))
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
	if target == "" {
		return EditHunk{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "hunk requires non-empty 'target' (or 'oldText')")
	}
	if !foundRepl {
		for _, typo := range []string{"replace", "replaceWith", "replacementText", "text", "value", "content", "body"} {
			if _, exists := m[typo]; exists {
				return EditHunk{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "hunk missing required 'replacement' field (found %q instead — rename it to 'replacement', or pass replacement: \"\" explicitly to delete)", typo)
			}
		}
		return EditHunk{}, sandbox.NewHostError(sandbox.KindBadArgs, nil, "hunk missing required 'replacement' field (pass replacement: \"\" explicitly to delete target)")
	}
	return EditHunk{
		Target:      target,
		Replacement: repl,
		StartLine:   optInt(m, "startLine", 0),
		EndLine:     optInt(m, "endLine", 0),
		ReplaceAll:  optBool(m, "replaceAll", optBool(m, "all", false)),
		Nth:         optInt(m, "nth", optInt(m, "occurrence", 0)),
		Symbol:      firstString(m, "symbol", "in"),
	}, nil
}
