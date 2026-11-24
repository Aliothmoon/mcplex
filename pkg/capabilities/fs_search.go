package capabilities

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aliothmoon/mcplex/pkg/events"
	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
	"github.com/dlclark/regexp2/v2"
)

const (
	MaxSandboxReadBytes = 2 * 1024 * 1024 // 2 MB per file inside sandbox memory (never directly enters LLM context)
	MaxSearchLineBytes  = 300             // UTF-8-safe per-line preview cap for search.rg (long lines warn)
)

var skippedWalkDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".next":        true,
	".nuxt":        true,
	"dist":         true,
	"build":        true,
	"target":       true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".gradle":      true,
	".cxx":         true,
	".cache":       true,
}

// SearchHit represents a single structured search match returned to guest TypeScript.
type SearchHit struct {
	File    string   `json:"file"`
	Line    int      `json:"line"`
	Column  int      `json:"column,omitempty"`
	Text    string   `json:"text"`
	Before  []string `json:"before,omitempty"`
	After   []string `json:"after,omitempty"`
	Subtext string   `json:"match,omitempty"`
}

// FSSearchBinder exposes `fs.*` and `search.*` to guest programs.
type FSSearchBinder struct {
	Policy    *policy.Engine
	Locks     *FileLockTable
	Events    *events.Hub
	SessionID string
	RunID     string
	hasRg     sync.Once
	rgBin     string
}

func (b *FSSearchBinder) resolveRg() string {
	b.hasRg.Do(func() {
		if p, err := exec.LookPath("rg"); err == nil {
			b.rgBin = p
		}
	})
	return b.rgBin
}

// readText implements fs.read for one path (shared by fs.read and fs.cat).
func (b *FSSearchBinder) readText(cwd, targetPath string, opts map[string]any) (string, error) {
	absPath, err := b.Policy.ResolveAndCheckRead(cwd, targetPath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is a directory; use fs.list/fs.tree", targetPath)
	}
	startLine := optInt(opts, "startLine", optInt(opts, "offset", 0))
	endLine := optInt(opts, "endLine", 0)
	limit := optInt(opts, "limit", 0)
	hasRange := startLine > 0 || endLine > 0 || limit > 0
	withNumbers := optBool(opts, "lineNumbers", false)
	if tail := optInt(opts, "tail", 0); tail > 0 {
		return readTailLines(absPath, targetPath, tail, withNumbers)
	}
	if info.Size() > MaxSandboxReadBytes {
		if !hasRange {
			return "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is %d bytes (>2MB); pass { startLine, endLine }, { tail: N }, or use search.rg", targetPath, info.Size())
		}
		return readBoundedLineRange(absPath, targetPath, startLine, endLine, limit, withNumbers)
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		return "", sandbox.NewHostError(sandbox.KindBadArgs, nil, "%s is binary", targetPath)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if withNumbers && !hasRange {
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		for i, l := range lines {
			lines[i] = fmt.Sprintf("%d: %s", i+1, l)
		}
		return strings.Join(lines, "\n"), nil
	}
	if hasRange {
		lines := strings.Split(text, "\n")
		s := maxInt(1, startLine)
		e := len(lines)
		if endLine > 0 && endLine < e {
			e = endLine
		} else if limit > 0 && s+limit-1 < e {
			e = s + limit - 1
		}
		if s > len(lines) {
			return "", nil
		}
		if e < s {
			return "", fmt.Errorf("invalid range: startLine (%d) > endLine (%d)", s, e)
		}
		selected := lines[s-1 : e]
		if withNumbers {
			numbered := make([]string, len(selected))
			for i, l := range selected {
				numbered[i] = fmt.Sprintf("%d: %s", s+i, l)
			}
			return strings.Join(numbered, "\n"), nil
		}
		return strings.Join(selected, "\n"), nil
	}
	return text, nil
}

func (b *FSSearchBinder) Bind(env *sandbox.ExecutionEnv) error {
	fsAsync := map[string]sandbox.AsyncHostFunc{
		"read": func(_ context.Context, args []any) (any, error) {
			targetPath, opts, err := parsePathAndOpts(args)
			if err != nil {
				return nil, err
			}
			text, err := b.readText(env.Cwd, targetPath, opts)
			if err == nil && b.Events.Active() {
				absRead := targetPath
				if !filepath.IsAbs(absRead) {
					absRead = filepath.Join(env.Cwd, absRead)
				}
				emitFileRead(b.Events, b.SessionID, b.RunID, "fs.read", env.Cwd, relDisplayPath(env.Cwd, absRead), filepath.Clean(absRead), formatReadOptsSummary(opts, len(text)), text)
			}
			return text, err
		},
		// fs.cat(paths, opts?) reads many files in parallel like `cat a b c`: unreadable files don't abort the call;
		// they map to null and are listed once under Warnings. Returns { [path]: text | null } in input order.
		"cat": func(_ context.Context, args []any) (any, error) {
			if len(args) == 0 || args[0] == nil {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "pass an array of paths")
			}
			paths := extractStringSlice(args[0])
			var opts map[string]any
			if len(args) > 1 {
				opts, _ = args[1].(map[string]any)
			}
			if len(paths) > maxCatFiles {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "max %d files per call (got %d)", maxCatFiles, len(paths))
			}
			texts := make([]string, len(paths))
			errs := make([]error, len(paths))
			var wg sync.WaitGroup
			sem := make(chan struct{}, 8)
			for i, p := range paths {
				wg.Add(1)
				go func(i int, p string) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					texts[i], errs[i] = b.readText(env.Cwd, p, opts)
				}(i, p)
			}
			wg.Wait()
			out := &sandbox.OrderedMap{}
			var failed []string
			for i, p := range paths {
				out.Keys = append(out.Keys, p)
				if errs[i] != nil {
					out.Vals = append(out.Vals, nil)
					failed = append(failed, p+" ("+catFailReason(errs[i])+")")
					continue
				}
				out.Vals = append(out.Vals, texts[i])
			}
			if len(failed) > 0 {
				env.AddWarning("fs.cat: null for " + joinLimited(failed, 8))
			}
			return out, nil
		},
		"write": func(ctx context.Context, args []any) (any, error) {
			var targetPath, content, eol string
			if len(args) >= 1 {
				if m, ok := args[0].(map[string]any); ok {
					targetPath = firstString(m, "path", "file", "file_path")
					content = firstString(m, "content", "text", "contents", "body")
					eol = firstString(m, "eol")
				} else if s, ok := args[0].(string); ok {
					targetPath = s
					if len(args) >= 2 {
						if c, ok := args[1].(string); ok {
							content = c
						}
					}
					eol = firstString(optsAt(args, 2), "eol")
				}
			}
			if targetPath == "" {
				return nil, errors.New("fs.write requires path and content")
			}
			absPath, err := b.Policy.ResolveAndCheckWrite(env.Cwd, targetPath)
			if err != nil {
				return nil, err
			}
			if b.Locks != nil {
				owner := env.ExecID
				if owner == "" {
					owner = fmt.Sprintf("fswrite:%d", txOwnerSeq.Add(1))
				}
				relLock, lockErr := b.Locks.AcquireIn(ctx, env.Cwd, owner, absPath)
				if lockErr != nil {
					return nil, lockErr
				}
				defer relLock()
			}
			orig, rErr := os.ReadFile(absPath)
			data, err := applyEOL(targetPath, orig, rErr == nil, []byte(content), eol)
			if err != nil {
				return nil, err
			}
			if err := writeFilePreserveMode(absPath, data); err != nil {
				return nil, err
			}
			emitFileDiff(b.Events, b.SessionID, b.RunID, "", "", "fs.write", env.Cwd, relDisplayPath(env.Cwd, absPath), absPath, orig, data, rErr == nil, true)
			return map[string]any{
				"ok":    true,
				"path":  relDisplayPath(env.Cwd, absPath),
				"bytes": len(content),
			}, nil
		},
		"stat": func(_ context.Context, args []any) (any, error) {
			targetPath, err := requireStringArg(args, 0, "path")
			if err != nil {
				return nil, err
			}
			absPath, err := b.Policy.ResolveAndCheckRead(env.Cwd, targetPath)
			if err != nil {
				return nil, err
			}
			info, err := os.Stat(absPath)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"path":    relDisplayPath(env.Cwd, absPath),
				"size":    info.Size(),
				"isDir":   info.IsDir(),
				"modTime": info.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
			}, nil
		},
		"list": func(_ context.Context, args []any) (any, error) {
			targetPath := "."
			var opts map[string]any
			if len(args) > 0 && args[0] != nil {
				targetPath, opts, _ = parsePathAndOpts(args)
				if targetPath == "" {
					targetPath = "."
				}
			}
			absPath, err := b.Policy.ResolveAndCheckRead(env.Cwd, targetPath)
			if err != nil {
				return nil, err
			}
			entries, err := os.ReadDir(absPath)
			if err != nil {
				return nil, err
			}
			limit := optInt(opts, "limit", 200)
			detail := optBool(opts, "detail", false)
			names := []any{}
			items := []map[string]any{}
			for _, e := range entries {
				if len(names)+len(items) >= limit {
					env.AddWarning(fmt.Sprintf("fs.list: capped at %d of %d entries; pass { limit }", limit, len(entries)))
					break
				}
				if !detail {
					// Like `ls -p`: plain names, directories end with "/".
					name := e.Name()
					if e.IsDir() {
						name += "/"
					}
					names = append(names, name)
					continue
				}
				info, _ := e.Info()
				var sz int64
				if info != nil {
					sz = info.Size()
				}
				name := e.Name()
				if e.IsDir() {
					name += "/" // same convention as the string form
				}
				items = append(items, map[string]any{
					"name":  name,
					"isDir": e.IsDir(),
					"size":  sz,
				})
			}
			if detail {
				return items, nil
			}
			return names, nil
		},
		"tree": func(_ context.Context, args []any) (any, error) {
			targetPath := "."
			maxDepth := 2
			if len(args) > 0 && args[0] != nil {
				p, opts, _ := parsePathAndOpts(args)
				if p != "" {
					targetPath = p
				}
				maxDepth = optInt(opts, "depth", optInt(opts, "maxDepth", 2))
			}
			if len(args) > 1 {
				if d, ok := toInt(args[1]); ok && d > 0 {
					maxDepth = d
				}
			}
			if maxDepth < 1 {
				maxDepth = 1
			}
			if maxDepth > 5 {
				maxDepth = 5
			}
			absPath, err := b.Policy.ResolveAndCheckRead(env.Cwd, targetPath)
			if err != nil {
				return nil, err
			}
			tree := buildTreeList(b.Policy, env.Cwd, absPath, maxDepth, treeMaxItems)
			if len(tree) >= treeMaxItems {
				env.AddWarning(fmt.Sprintf("fs.tree: capped at %d entries; lower depth or use search.glob", treeMaxItems))
			}
			return tree, nil
		},
	}

	for name, fn := range b.fileOps(env) {
		fsAsync[name] = fn
	}
	// Sync: fs.exists returns a boolean directly, so `if (fs.exists(p))` works even without `await`.
	fsSync := map[string]sandbox.SyncHostFunc{
		"exists": func(_ context.Context, args []any) (any, error) {
			targetPath, err := requireStringArg(args, 0, "path")
			if err != nil {
				return nil, err
			}
			absPath, err := b.Policy.ResolveAndCheckRead(env.Cwd, targetPath)
			if err != nil {
				return nil, err // Denied / NeedsApproval: don't pretend a blocked path is missing
			}
			_, statErr := os.Stat(absPath)
			return statErr == nil, nil
		},
	}
	if err := env.SetNamespace("fs", fsAsync, fsSync); err != nil {
		return err
	}

	searchAsync := map[string]sandbox.AsyncHostFunc{
		"rg": func(ctx context.Context, args []any) (any, error) {
			q, err := parseSearchQuery(args)
			if err != nil {
				return nil, err
			}
			searchRoot, err := b.Policy.ResolveAndCheckRead(env.Cwd, q.path)
			if err != nil {
				return nil, err
			}
			if _, stErr := os.Stat(searchRoot); stErr != nil {
				return nil, stErr
			}
			if q.mode != "" {
				return b.runListSearch(ctx, env, searchRoot, q)
			}
			hits, total, err := b.runTextOrRegexSearch(ctx, env.Cwd, searchRoot, q)
			if err == nil && len(hits) >= q.maxResults {
				env.AddWarning(fmt.Sprintf("search.rg: capped at %d%s hits; narrow path/glob, pass { maxResults }, or use { count: true } for totals", q.maxResults, ofTotal(total)))
			}
			if cut := countCutLines(hits); cut > 0 {
				env.AddWarning(fmt.Sprintf("search.rg: %d long line(s) cut at %dB; fs.read the line range for full text", cut, MaxSearchLineBytes))
			}
			if err == nil && b.Events.Active() {
				emitSearchHits(b.Events, b.SessionID, b.RunID, "search.rg", searchRoot, q.pattern, hits, total)
			}
			return hits, err
		},
		"glob": func(ctx context.Context, args []any) (any, error) {
			pat, opts, err := parsePatternAndOpts(args)
			if err != nil {
				return nil, err
			}
			subPath := firstString(opts, "path", "dir", "cwd")
			if subPath == "" {
				subPath = "."
			}
			searchRoot, err := b.Policy.ResolveAndCheckRead(env.Cwd, subPath)
			if err != nil {
				return nil, err
			}
			maxResults := optInt(opts, "maxResults", optInt(opts, "limit", 500))
			if maxResults > 5000 {
				maxResults = 5000
			}
			files, total, note, err := b.runGlobSearch(ctx, env.Cwd, searchRoot, pat, maxResults)
			if err != nil {
				return nil, err
			}
			if total > len(files) {
				env.AddWarning(fmt.Sprintf("search.glob: capped at %d of %d files; narrow path or pass { maxResults }", len(files), total))
			}
			if b.Events.Active() {
				emitSearchFiles(b.Events, b.SessionID, b.RunID, "search.glob", searchRoot, pat, files, total)
			}
			if optBool(opts, "withMeta", false) {
				return map[string]any{"files": files, "note": note}, nil
			}
			return files, nil
		},
	}
	return env.SetNamespace("search", searchAsync, nil)
}

const (
	maxCatFiles         = 200
	maxContextLines     = 50
	defaultRgMaxResults = 100
	treeMaxItems        = 300
)

type searchQuery struct {
	pattern       string
	path          string
	glob          string
	literal       bool
	pcre2         bool
	caseSensitive bool
	contextLines  int
	maxResults    int
	mode          string // "" (hits) | "files" (rg -l) | "count" (rg -c)
}

func parseSearchQuery(args []any) (searchQuery, error) {
	q := searchQuery{
		path:          ".",
		caseSensitive: true,
		maxResults:    defaultRgMaxResults,
	}
	if len(args) == 0 || args[0] == nil {
		return q, sandbox.NewHostError(sandbox.KindBadArgs, nil, "requires a pattern string or options object")
	}
	var opts map[string]any
	if s, ok := args[0].(string); ok {
		q.pattern = s
		if len(args) > 1 {
			if m, ok := args[1].(map[string]any); ok {
				opts = m
			} else if p, ok := args[1].(string); ok && p != "" {
				q.path = p
			}
		}
	} else if m, ok := args[0].(map[string]any); ok {
		opts = m
		q.pattern = firstString(m, "pattern", "query", "regex", "text", "q")
	}
	if q.pattern == "" {
		return q, sandbox.NewHostError(sandbox.KindBadArgs, nil, "'pattern' cannot be empty")
	}
	if opts != nil {
		if p := firstString(opts, "path", "dir", "directory"); p != "" {
			q.path = p
		}
		q.glob = firstString(opts, "glob", "globPattern", "include")
		q.literal = optBool(opts, "literal", optBool(opts, "fixedStrings", false))
		q.pcre2 = optBool(opts, "pcre2", optBool(opts, "P", false))
		if optBool(opts, "ignoreCase", optBool(opts, "ic", false)) {
			q.caseSensitive = false
		} else if v, exists := opts["caseSensitive"]; exists {
			if b, ok := v.(bool); ok {
				q.caseSensitive = b
			}
		}
		q.contextLines = optInt(opts, "contextLines", optInt(opts, "context", optInt(opts, "C", 0)))
		if q.contextLines > maxContextLines {
			q.contextLines = maxContextLines
		}
		switch {
		case optBool(opts, "filesOnly", optBool(opts, "l", optBool(opts, "filesWithMatches", false))):
			q.mode = "files"
		case optBool(opts, "count", optBool(opts, "c", false)):
			q.mode = "count"
		}
		defaultMax := defaultRgMaxResults
		if q.mode != "" {
			defaultMax = defaultRgListResults
		}
		q.maxResults = optInt(opts, "maxResults", optInt(opts, "limit", defaultMax))
		if q.maxResults <= 0 {
			q.maxResults = defaultMax
		}
		if q.maxResults > 2000 {
			q.maxResults = 2000
		}
	}
	return q, nil
}

func invalidRegexError(pattern, detail string) error {
	detail = strings.TrimSpace(detail)
	if strings.Contains(detail, "\n") {
		var kept []string
		for _, l := range strings.Split(detail, "\n") {
			if t := strings.TrimSpace(l); t != "" {
				kept = append(kept, t)
				if len(kept) >= 6 {
					break
				}
			}
		}
		detail = strings.Join(kept, " | ")
	}
	return sandbox.NewHostError(sandbox.KindBadArgs, nil,
		"invalid regex %q: %s (in JS strings/templates escape backslashes as \\\\ or pass { literal: true })",
		pattern, detail)
}

var errRgPCRE2Unavailable = errors.New("ripgrep compiled without PCRE2")

// patternLikelyNeedsPCRE2 detects unescaped ECMAScript/PCRE2 lookarounds (`(?=`, `(?!`, `(?<=`, `(?<!`)
// and backreferences (`\1`..`\9`, `\k<`) that Rust's default `regex` crate rejects without `--pcre2`.
func patternLikelyNeedsPCRE2(pat string) bool {
	inClass := false
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		if c == '\\' {
			if i+1 < len(pat) {
				next := pat[i+1]
				if !inClass && ((next >= '1' && next <= '9') || next == 'k') {
					return true
				}
				i++
			}
			continue
		}
		if c == '[' && !inClass {
			inClass = true
			continue
		}
		if c == ']' && inClass {
			inClass = false
			continue
		}
		if !inClass && c == '(' && i+2 < len(pat) && pat[i+1] == '?' {
			switch pat[i+2] {
			case '=', '!':
				return true
			case '<':
				if i+3 < len(pat) && (pat[i+3] == '=' || pat[i+3] == '!') {
					return true
				}
			}
		}
	}
	return false
}

// execRgSearch runs `rg` with automatic `--pcre2` promotion when the regex uses look-around/backreferences
// (either detected upfront or when Rust `regex` reports `"is not supported"`), and returns `errRgPCRE2Unavailable`
// if the installed `rg` binary lacks PCRE2 support so callers seamlessly fall back to Go `regexp2`.
func execRgSearch(ctx context.Context, rgBin string, baseFlags []string, q searchQuery, searchRoot string, maxStdout int) (*limitedSyncBuffer, bool, error) {
	usePCRE2 := !q.literal && (q.pcre2 || patternLikelyNeedsPCRE2(q.pattern))

	runOnce := func(withPCRE2 bool) (*limitedSyncBuffer, string, int, error) {
		args := make([]string, 0, len(baseFlags)+8)
		args = append(args, baseFlags...)
		if q.literal {
			args = append(args, "--fixed-strings")
		} else if withPCRE2 {
			args = append(args, "--pcre2")
		}
		if !q.caseSensitive {
			args = append(args, "--ignore-case")
		} else {
			args = append(args, "--case-sensitive")
		}
		if q.glob != "" {
			args = append(args, "-g", q.glob)
		}
		args = append(args, "--", q.pattern, searchRoot)

		rgCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(rgCtx, rgBin, args...)
		setSysProcAttrHide(cmd)
		stdout := newLimitedSyncBuffer(maxStdout)
		stderr := newLimitedSyncBuffer(128 * 1024)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		err := cmd.Run()
		if err == nil {
			return stdout, "", 0, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && rgCtx.Err() == nil {
			return stdout, strings.TrimSpace(stderr.String()), exitErr.ExitCode(), err
		}
		if rgCtx.Err() != nil {
			return nil, "", -1, fmt.Errorf("search.rg timed out: %w", rgCtx.Err())
		}
		return nil, "", -1, err
	}

	stdout, errMsg, code, err := runOnce(usePCRE2)
	if err == nil {
		return stdout, false, nil
	}
	if code == 1 {
		return stdout, true, nil
	}
	if code == 2 && !q.literal {
		low := strings.ToLower(errMsg)
		if !usePCRE2 && strings.Contains(low, "is not supported") {
			stdout, errMsg, code, err = runOnce(true)
			if err == nil {
				return stdout, false, nil
			}
			if code == 1 {
				return stdout, true, nil
			}
			low = strings.ToLower(errMsg)
		}
		if strings.Contains(low, "pcre2 is not available") {
			return nil, false, errRgPCRE2Unavailable
		}
		if strings.Contains(low, "regex") || strings.Contains(low, "pcre2") {
			return nil, false, invalidRegexError(q.pattern, errMsg)
		}
		return nil, false, fmt.Errorf("invalid search pattern: %s", errMsg)
	}
	if code == 2 {
		return nil, false, fmt.Errorf("invalid search pattern: %s", errMsg)
	}
	return nil, false, err
}

// runTextOrRegexSearch returns hits and the total number of matches (-1 when unknown, e.g. Go fallback).
func (b *FSSearchBinder) runTextOrRegexSearch(ctx context.Context, workspaceCwd, searchRoot string, q searchQuery) ([]SearchHit, int, error) {
	if rgBin := b.resolveRg(); rgBin != "" {
		hits, total, err := runRgJSON(ctx, b.Policy, rgBin, workspaceCwd, searchRoot, q)
		if err == nil {
			return hits, total, nil
		}
		if ctx.Err() != nil {
			return nil, -1, ctx.Err()
		}
		var he *sandbox.HostError
		if errors.As(err, &he) {
			return nil, -1, err
		}
	}
	hits, err := runGoFallbackSearch(ctx, b.Policy, workspaceCwd, searchRoot, q)
	return hits, -1, err
}

func canSearchPath(pol *policy.Engine, workspaceCwd, p string, cache map[string]bool) bool {
	if pol == nil {
		return true
	}
	if cache != nil {
		if allowed, ok := cache[p]; ok {
			return allowed
		}
	}
	_, err := pol.ResolveAndCheckRead(workspaceCwd, p)
	allowed := err == nil
	if cache != nil {
		cache[p] = allowed
	}
	return allowed
}

type cachedRealDir struct {
	realDir string
	ok      bool
}

// fastPathChecker caches symlink-resolved parent directories (`ResolveRealPath` once per unique directory)
// and evaluates `pol.CheckResolvedRead` in memory for regular non-symlink files, avoiding per-file
// `EvalSymlinks` (`CreateFileW` + `GetFinalPathNameByHandleW`) syscall storms on large trees.
type fastPathChecker struct {
	pol          *policy.Engine
	workspaceCwd string
	fileCache    map[string]bool
	dirCache     map[string]cachedRealDir
}

func newFastPathChecker(pol *policy.Engine, workspaceCwd string) *fastPathChecker {
	return &fastPathChecker{
		pol:          pol,
		workspaceCwd: workspaceCwd,
		fileCache:    map[string]bool{},
		dirCache:     map[string]cachedRealDir{},
	}
}

func (c *fastPathChecker) allowFile(p string) bool {
	if c == nil || c.pol == nil {
		return true
	}
	if allowed, ok := c.fileCache[p]; ok {
		return allowed
	}
	var absP string
	if filepath.IsAbs(p) {
		absP = filepath.Clean(p)
	} else {
		absP = filepath.Clean(filepath.Join(c.workspaceCwd, p))
	}
	base := filepath.Base(absP)
	if runtime.GOOS == "windows" && strings.ContainsRune(base, ':') {
		c.fileCache[p] = false
		return false
	}
	dir := filepath.Dir(absP)
	de, ok := c.dirCache[dir]
	if !ok {
		_, realD, err := c.pol.ResolveRealPath(c.workspaceCwd, dir)
		de = cachedRealDir{realDir: realD, ok: err == nil}
		c.dirCache[dir] = de
	}
	var allowed bool
	if de.ok {
		if fi, lerr := os.Lstat(absP); lerr == nil && fi.Mode().Type() == 0 {
			allowed = c.pol.CheckResolvedRead(p, absP, filepath.Join(de.realDir, base)) == nil
		} else {
			_, err := c.pol.ResolveAndCheckRead(c.workspaceCwd, p)
			allowed = err == nil
		}
	} else {
		_, err := c.pol.ResolveAndCheckRead(c.workspaceCwd, p)
		allowed = err == nil
	}
	c.fileCache[p] = allowed
	return allowed
}

func runRgJSON(ctx context.Context, pol *policy.Engine, rgBin, workspaceCwd, searchRoot string, q searchQuery) ([]SearchHit, int, error) {
	baseFlags := []string{"--json", "--line-number", "--column", "--no-heading", "--color=never"}
	if q.contextLines > 0 {
		baseFlags = append(baseFlags, fmt.Sprintf("-C%d", q.contextLines))
	}
	stdout, noMatches, err := execRgSearch(ctx, rgBin, baseFlags, q, searchRoot, 2*1024*1024)
	if err != nil {
		return nil, -1, err
	}
	if noMatches {
		return []SearchHit{}, 0, nil
	}

	type rgEvent struct {
		Type string `json:"type"`
		Data struct {
			Path struct {
				Text string `json:"text"`
			} `json:"path"`
			Lines struct {
				Text string `json:"text"`
			} `json:"lines"`
			LineNumber int `json:"line_number"`
			Submatches []struct {
				Start int `json:"start"`
				Match struct {
					Text string `json:"text"`
				} `json:"match"`
			} `json:"submatches"`
		} `json:"data"`
	}

	hits := []SearchHit{}
	total := 0
	// Context lines arrive as separate "context" events (and adjacent matches double as context),
	// so keep a rolling window for `before` and extend recent hits' `after` as later lines stream in.
	type seenLine struct {
		n    int
		text string
	}
	var window []seenLine
	fileStart := 0 // index in hits of the first hit of the current file
	full := false
	skipFile := false
	checker := newFastPathChecker(pol, workspaceCwd)
	scanner := bufio.NewScanner(strings.NewReader(stdout.String()))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var ev rgEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "begin":
			window = window[:0]
			fileStart = len(hits)
			skipFile = !checker.allowFile(ev.Data.Path.Text)
			continue
		case "match", "context":
			if skipFile || !checker.allowFile(ev.Data.Path.Text) {
				continue
			}
		default:
			continue
		}
		if ev.Type == "match" {
			total++
		}
		n := ev.Data.LineNumber
		lineText := strings.TrimRight(ev.Data.Lines.Text, "\r\n")
		if len(lineText) > MaxSearchLineBytes {
			lineText = output.SliceUTF8Bytes(lineText, MaxSearchLineBytes) + "..."
		}
		if q.contextLines > 0 {
			for i := fileStart; i < len(hits); i++ {
				if n > hits[i].Line && n <= hits[i].Line+q.contextLines {
					hits[i].After = append(hits[i].After, lineText)
				}
			}
		}
		if ev.Type == "match" && !full {
			col := 1
			sub := ""
			if len(ev.Data.Submatches) > 0 {
				col = ev.Data.Submatches[0].Start + 1
				sub = ev.Data.Submatches[0].Match.Text
			}
			hit := SearchHit{
				File:    relDisplayPath(workspaceCwd, ev.Data.Path.Text),
				Line:    n,
				Column:  col,
				Text:    lineText,
				Subtext: sub,
			}
			for _, w := range window {
				if w.n >= n-q.contextLines && w.n < n {
					hit.Before = append(hit.Before, w.text)
				}
			}
			hits = append(hits, hit)
			full = len(hits) >= q.maxResults
		}
		if q.contextLines > 0 {
			window = append(window, seenLine{n, lineText})
			if len(window) > q.contextLines {
				window = window[1:]
			}
		}
	}
	if stdout.droppedBytes() > 0 {
		total = -1 // rg output overflowed the buffer: true total unknown
	}
	return hits, total, nil
}

func runGoFallbackSearch(ctx context.Context, pol *policy.Engine, workspaceCwd, searchRoot string, q searchQuery) ([]SearchHit, error) {
	var re *regexp.Regexp
	var re2 *regexp2.Regexp
	var err error

	pat := q.pattern
	if q.literal {
		pat = regexp.QuoteMeta(pat)
	}
	goPat := pat
	if !q.caseSensitive {
		goPat = "(?i)" + goPat
	}
	if !q.pcre2 && !patternLikelyNeedsPCRE2(q.pattern) {
		re, err = regexp.Compile(goPat)
	} else {
		err = errRgPCRE2Unavailable
	}
	if err != nil {
		opts := regexp2.None
		if !q.caseSensitive {
			opts |= regexp2.IgnoreCase
		}
		var err2 error
		re2, err2 = regexp2.Compile(pat, opts)
		if err2 != nil {
			detail := err.Error()
			if errors.Is(err, errRgPCRE2Unavailable) {
				detail = err2.Error()
			}
			return nil, invalidRegexError(q.pattern, detail)
		}
		re2.MatchTimeout = 100 * time.Millisecond
	}

	matchLine := func(line string) (int, int, bool) {
		if re != nil {
			loc := re.FindStringIndex(line)
			if loc == nil {
				return 0, 0, false
			}
			return loc[0], loc[1], true
		}
		m, mErr := re2.FindStringMatch(line)
		if mErr != nil || m == nil {
			return 0, 0, false
		}
		bOff, bLen := m.ByteRange()
		bOff = maxInt(0, minInt(bOff, len(line)))
		return bOff, minInt(len(line), bOff+maxInt(0, bLen)), true
	}

	var globMatcher *compiledGlob
	if q.glob != "" {
		globMatcher, err = compileGlob(q.glob)
		if err != nil {
			return nil, err
		}
	}

	var files []string
	info, err := os.Stat(searchRoot)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		checker := newFastPathChecker(pol, workspaceCwd)
		if checker.allowFile(searchRoot) {
			files = []string{searchRoot}
		}
	} else {
		skipStd := !pathTouchesSkippedDir(workspaceCwd, searchRoot)
		cand := walkFilesParallel(ctx, pol, workspaceCwd, []string{searchRoot}, skipStd)
		if globMatcher == nil {
			files = cand
		} else {
			for _, p := range cand {
				if globMatcher.MatchSimple(filepath.ToSlash(p), filepath.Base(p)) {
					files = append(files, p)
				}
			}
		}
	}

	hits := []SearchHit{}
	for _, f := range files {
		if ctx.Err() != nil || len(hits) >= q.maxResults {
			break
		}
		fi, err := os.Stat(f)
		if err != nil || fi.Size() > MaxSandboxReadBytes {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil || bytes.IndexByte(raw, 0) >= 0 {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
		rel := relDisplayPath(workspaceCwd, f)
		for idx, line := range lines {
			bStart, bEnd, ok := matchLine(line)
			if !ok {
				continue
			}
			var before, after []string
			if q.contextLines > 0 {
				for b := maxInt(0, idx-q.contextLines); b < idx; b++ {
					before = append(before, lines[b])
				}
				for a := idx + 1; a <= minInt(len(lines)-1, idx+q.contextLines); a++ {
					after = append(after, lines[a])
				}
			}
			txt := line
			if len(txt) > MaxSearchLineBytes {
				txt = output.SliceUTF8Bytes(txt, MaxSearchLineBytes) + "..."
			}
			hits = append(hits, SearchHit{
				File:    rel,
				Line:    idx + 1,
				Column:  bStart + 1,
				Text:    txt,
				Before:  before,
				After:   after,
				Subtext: line[bStart:bEnd],
			})
			if len(hits) >= q.maxResults {
				break
			}
		}
	}
	return hits, nil
}

// runGlobSearch returns up to maxResults files plus the total number of matches.
// Combines literal directory prefix pushdown, ripgrep (`rg --files`) `.gitignore`-aware fast path,
// and a bounded parallel zero-syscall Go walker so even 200k-file C++/Android/AOSP trees resolve in milliseconds.
func (b *FSSearchBinder) runGlobSearch(ctx context.Context, workspaceCwd, searchRoot, pattern string, maxResults int) ([]string, int, string, error) {
	if maxResults <= 0 {
		maxResults = 200
	}
	// Precompile and validate brace expansion caps BEFORE walking the workspace!
	exactGlob, err := compileGlob(pattern)
	if err != nil {
		return nil, 0, "", err
	}

	rootInfo, stErr := os.Stat(searchRoot)
	if stErr != nil {
		return []string{}, 0, "", nil
	}
	if !rootInfo.IsDir() {
		checker := newFastPathChecker(b.Policy, workspaceCwd)
		if checker.allowFile(searchRoot) && exactGlob.Match(filepath.Base(searchRoot)) {
			return []string{relDisplayPath(workspaceCwd, searchRoot)}, 1, "", nil
		}
		return []string{}, 0, "", nil
	}

	type candidateSet struct {
		files     []string
		unchecked bool // true when returned raw from `rg --files` so `checker.allowFile` runs lazily ONLY on glob matches!
	}

	checker := newFastPathChecker(b.Policy, workspaceCwd)
	total := 0
	matchFiles := func(cs candidateSet, cg *compiledGlob) []string {
		var matched []string
		total = 0
		for _, abs := range cs.files {
			relToSearch, relErr := filepath.Rel(searchRoot, abs)
			if relErr != nil {
				relToSearch = abs
			}
			relSlash := filepath.ToSlash(relToSearch)
			if cg.Match(relSlash) {
				if cs.unchecked && !checker.allowFile(abs) {
					continue
				}
				total++
				if len(matched) < maxResults {
					matched = append(matched, relDisplayPath(workspaceCwd, abs))
				}
			}
		}
		sort.Strings(matched)
		return matched
	}

	explicitSubPath := filepath.Clean(searchRoot) != filepath.Clean(workspaceCwd)
	rootTouchesSkipped := pathTouchesSkippedDir(workspaceCwd, searchRoot)
	rgBin := b.resolveRg()

	collectForGlob := func(cg *compiledGlob, forceNoIgnore bool) (candidateSet, bool, []string) {
		prefixes, patTouchesSkipped := extractGlobDirPrefixes(cg.parts)
		touchesSkipped := rootTouchesSkipped || patTouchesSkipped
		targetDirs, directFiles := resolveGlobTargets(b.Policy, workspaceCwd, searchRoot, prefixes)
		if len(targetDirs) == 0 {
			return candidateSet{files: directFiles}, false, prefixes
		}
		if rgBin != "" {
			noIgnore := forceNoIgnore || touchesSkipped
			if rgFiles, ok := runRgFiles(ctx, rgBin, workspaceCwd, targetDirs, noIgnore); ok {
				if len(directFiles) > 0 {
					rgFiles = append(rgFiles, directFiles...)
				}
				return candidateSet{files: rgFiles, unchecked: true}, !noIgnore, prefixes
			}
		}
		walked := walkFilesParallel(ctx, b.Policy, workspaceCwd, targetDirs, !touchesSkipped && !forceNoIgnore)
		if len(directFiles) > 0 {
			walked = append(walked, directFiles...)
		}
		return candidateSet{files: walked, unchecked: false}, false, prefixes
	}

	allFiles, usedRgWithIgnore, exactPrefixes := collectForGlob(exactGlob, false)
	exact := matchFiles(allFiles, exactGlob)
	// If `rg --files` skipped a directory with inner `.ignore`/`*` rules (e.g. `build/.ignore` or `out/staging/**/*`)
	// when the caller explicitly scoped `path` or a literal prefix, retry with unskipped parallel walk!
	if len(exact) == 0 && usedRgWithIgnore && (explicitSubPath || rootTouchesSkipped || !(len(exactPrefixes) == 1 && exactPrefixes[0] == "")) {
		targetDirs, directFiles := resolveGlobTargets(b.Policy, workspaceCwd, searchRoot, exactPrefixes)
		walked := walkFilesParallel(ctx, b.Policy, workspaceCwd, targetDirs, false)
		walked = append(walked, directFiles...)
		exact = matchFiles(candidateSet{files: walked}, exactGlob)
	}
	if len(exact) > 0 || ctx.Err() != nil {
		return exact, total, "", nil
	}

	ensureFullRootFiles := func() candidateSet {
		if len(exactPrefixes) == 1 && exactPrefixes[0] == "" {
			return allFiles
		}
		fullGlob := &compiledGlob{parts: []compiledGlobPart{{raw: "**/*"}}}
		cs, _, _ := collectForGlob(fullGlob, false)
		return cs
	}

	// Smart fallback 1 (inspired by agy-fast v0.2.2): expand regex alternation `a|b` mistakenly passed to glob
	if strings.Contains(pattern, "|") && !strings.Contains(pattern, "{") {
		expanded := "*{" + strings.ReplaceAll(pattern, "|", ",") + "}*"
		altGlob, altErr := compileGlob(expanded)
		if altErr != nil {
			return nil, 0, "", altErr
		}
		altFiles, _, _ := collectForGlob(altGlob, false)
		recovered := matchFiles(altFiles, altGlob)
		if len(recovered) > 0 {
			return recovered, total, fmt.Sprintf("auto-expanded regex '|' to glob '%s'", expanded), nil
		}
	}

	// Smart fallback 2: bare keyword without wildcards, slashes, or extension dots -> substring match
	if !strings.ContainsAny(pattern, "*?[]{}/\\.") {
		sub := strings.ToLower(pattern)
		fullCS := ensureFullRootFiles()
		var recovered []string
		total = 0
		for _, abs := range fullCS.files {
			rel := relDisplayPath(workspaceCwd, abs)
			if strings.Contains(strings.ToLower(rel), sub) {
				if fullCS.unchecked && !checker.allowFile(abs) {
					continue
				}
				total++
				if len(recovered) < maxResults {
					recovered = append(recovered, rel)
				}
			}
		}
		sort.Strings(recovered)
		if len(recovered) > 0 {
			return recovered, total, fmt.Sprintf("bare keyword matched as '*%s*'", pattern), nil
		}
	}

	return []string{}, 0, "", nil
}

// extractGlobDirPrefixes extracts static literal directory prefixes from brace-expanded glob branches
// (e.g. `scripts/{staging,guest}/**/*` -> `["scripts/staging", "scripts/guest"]`, `native/**/*.map` -> `["native"]`)
// and reports whether any literal segment explicitly names a `skippedWalkDirs` directory (such as `build` or `dist`).
func extractGlobDirPrefixes(parts []compiledGlobPart) ([]string, bool) {
	if len(parts) == 0 {
		return []string{""}, false
	}
	touchesSkipped := false
	rawPrefixes := make([]string, 0, len(parts))
	hasRootWildcard := false

	for _, part := range parts {
		p := strings.TrimPrefix(part.raw, "./")
		if strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			hasRootWildcard = true
		}
		segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
		for _, seg := range segs {
			if seg != "" && !strings.ContainsAny(seg, "*?[]") && skippedWalkDirs[seg] {
				touchesSkipped = true
			}
		}
		if hasRootWildcard {
			continue
		}
		limit := len(segs) - 1
		if strings.HasSuffix(p, "/") {
			limit = len(segs)
		}
		k := 0
		for k < limit && segs[k] != "" && segs[k] != "." && !strings.ContainsAny(segs[k], "*?[]") {
			k++
		}
		if k == 0 {
			hasRootWildcard = true
		} else {
			rawPrefixes = append(rawPrefixes, strings.Join(segs[:k], "/"))
		}
	}

	if hasRootWildcard || len(rawPrefixes) == 0 {
		return []string{""}, touchesSkipped
	}
	sort.Strings(rawPrefixes)
	collapsed := make([]string, 0, len(rawPrefixes))
	for _, pref := range rawPrefixes {
		if len(collapsed) > 0 {
			prev := collapsed[len(collapsed)-1]
			if pref == prev || strings.HasPrefix(pref, prev+"/") {
				continue
			}
		}
		collapsed = append(collapsed, pref)
	}
	return collapsed, touchesSkipped
}

func pathTouchesSkippedDir(workspaceCwd, targetDir string) bool {
	if workspaceCwd != "" {
		if rel, err := filepath.Rel(workspaceCwd, targetDir); err == nil && !strings.HasPrefix(rel, "..") {
			for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
				if skippedWalkDirs[seg] {
					return true
				}
			}
			return false
		}
	}
	for _, seg := range strings.Split(filepath.ToSlash(filepath.Clean(targetDir)), "/") {
		if skippedWalkDirs[seg] {
			return true
		}
	}
	return false
}

func resolveGlobTargets(pol *policy.Engine, workspaceCwd, searchRoot string, prefixes []string) (targetDirs []string, directFiles []string) {
	checker := newFastPathChecker(pol, workspaceCwd)
	seen := map[string]bool{}
	for _, pref := range prefixes {
		td := searchRoot
		if pref != "" {
			td = filepath.Join(searchRoot, filepath.FromSlash(pref))
		}
		td = filepath.Clean(td)
		if seen[td] {
			continue
		}
		seen[td] = true
		fi, err := os.Stat(td)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if checker.allowFile(td) {
				directFiles = append(directFiles, td)
			}
			continue
		}
		if pol != nil {
			if _, err := pol.ResolveAndCheckRead(workspaceCwd, td); err != nil {
				continue
			}
		}
		targetDirs = append(targetDirs, td)
	}
	return targetDirs, directFiles
}

// runRgFiles uses `rg --files --hidden -g !.git` to enumerate files in parallel Rust threads.
// Policy checks are deferred lazily to matching candidates so non-matching files incur zero `Lstat` syscalls.
func runRgFiles(ctx context.Context, rgBin, workspaceCwd string, targetDirs []string, noIgnore bool) ([]string, bool) {
	if len(targetDirs) == 0 {
		return []string{}, true
	}
	args := make([]string, 0, 8+len(targetDirs))
	args = append(args, "--files", "--hidden", "-g", "!.git", "--color=never")
	if noIgnore {
		args = append(args, "--no-ignore")
	}
	args = append(args, "--")
	args = append(args, targetDirs...)

	rgCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(rgCtx, rgBin, args...)
	setSysProcAttrHide(cmd)
	stdout := newLimitedSyncBuffer(16 * 1024 * 1024)
	cmd.Stdout = stdout
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && rgCtx.Err() == nil && exitErr.ExitCode() == 1 {
			return []string{}, true
		}
		return nil, false
	}
	if stdout.droppedBytes() > 0 {
		return nil, false
	}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(stdout.String()))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			break
		}
		fp := strings.TrimRight(sc.Text(), "\r")
		if fp == "" {
			continue
		}
		if !filepath.IsAbs(fp) {
			fp = filepath.Clean(filepath.Join(workspaceCwd, fp))
		}
		out = append(out, fp)
	}
	if sc.Err() != nil {
		return nil, false
	}
	return out, true
}

func collectWorkspaceFiles(ctx context.Context, pol *policy.Engine, workspaceCwd, root string) []string {
	return walkFilesParallel(ctx, pol, workspaceCwd, []string{root}, true)
}

type walkDirWork struct {
	absDir  string
	realDir string
}

// walkFilesParallel traverses targetDirs concurrently across a bounded worker pool using `os.ReadDir`
// and lock-free `pol.CheckResolvedRead` on regular entries (0 per-file `EvalSymlinks` syscalls).
// Symlinks and Windows reparse points still go through full `ResolveAndCheckRead`, and symlinked
// directories are never recursed into so symlink cycles cannot loop.
func walkFilesParallel(ctx context.Context, pol *policy.Engine, workspaceCwd string, targetDirs []string, skipStandardDirs bool) []string {
	if len(targetDirs) == 0 || ctx.Err() != nil {
		return nil
	}
	queue := make([]walkDirWork, 0, len(targetDirs)*4)
	for _, td := range targetDirs {
		absRoot := filepath.Clean(td)
		realRoot := absRoot
		if pol != nil {
			var err error
			absRoot, realRoot, err = pol.ResolveReadWithReal(workspaceCwd, td)
			if err != nil {
				continue
			}
		} else if r, err := filepath.EvalSymlinks(absRoot); err == nil {
			realRoot = r
		}
		queue = append(queue, walkDirWork{absDir: absRoot, realDir: realRoot})
	}
	if len(queue) == 0 {
		return nil
	}

	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	active := 0
	stopped := false
	var allFiles []string

	stopCh := make(chan struct{})
	defer close(stopCh)
	go func() {
		select {
		case <-ctx.Done():
			mu.Lock()
			stopped = true
			cond.Broadcast()
			mu.Unlock()
		case <-stopCh:
		}
	}()

	workers := minInt(8, maxInt(2, runtime.GOMAXPROCS(0)))
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				for len(queue) == 0 && active > 0 && !stopped {
					cond.Wait()
				}
				if stopped || (len(queue) == 0 && active == 0) {
					cond.Broadcast()
					mu.Unlock()
					return
				}
				item := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				active++
				mu.Unlock()

				entries, rdErr := os.ReadDir(item.absDir)
				var localFiles []string
				var childDirs []walkDirWork
				if rdErr == nil && ctx.Err() == nil {
					for _, e := range entries {
						name := e.Name()
						if runtime.GOOS == "windows" && strings.ContainsRune(name, ':') {
							continue
						}
						typ := e.Type()
						switch typ {
						case 0: // Plain regular file (no symlink/reparse point) -> 0 syscalls!
							absChild := filepath.Join(item.absDir, name)
							if pol == nil {
								localFiles = append(localFiles, absChild)
							} else {
								realChild := filepath.Join(item.realDir, name)
								if pol.CheckResolvedRead(absChild, absChild, realChild) == nil {
									localFiles = append(localFiles, absChild)
								}
							}
						case fs.ModeDir: // True non-symlink, non-junction directory -> 0 syscalls!
							if name == ".git" || (skipStandardDirs && skippedWalkDirs[name]) {
								continue
							}
							absChild := filepath.Join(item.absDir, name)
							realChild := filepath.Join(item.realDir, name)
							if pol != nil && pol.CheckResolvedRead(absChild, absChild, realChild) != nil {
								continue
							}
							childDirs = append(childDirs, walkDirWork{absDir: absChild, realDir: realChild})
						default: // Symlink, Windows junction, or irregular file -> full symlink evaluation; never recurse into symlinked dirs!
							if name == ".git" {
								continue
							}
							absChild := filepath.Join(item.absDir, name)
							if pol != nil {
								if _, err := pol.ResolveAndCheckRead(workspaceCwd, absChild); err != nil {
									continue
								}
							}
							if fi, stErr := os.Stat(absChild); stErr == nil && !fi.IsDir() {
								localFiles = append(localFiles, absChild)
							}
						}
					}
				}

				mu.Lock()
				if len(localFiles) > 0 {
					allFiles = append(allFiles, localFiles...)
				}
				if !stopped && len(childDirs) > 0 {
					queue = append(queue, childDirs...)
				}
				active--
				if len(childDirs) > 0 || (len(queue) == 0 && active == 0) {
					cond.Broadcast()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return allFiles
}

const maxGlobBraceExpansions = 64

type compiledGlobPart struct {
	raw      string
	hasSlash bool
	re       *regexp.Regexp
}

type compiledGlob struct {
	parts []compiledGlobPart
}

func compileGlob(pattern string) (*compiledGlob, error) {
	expanded, err := expandBracesChecked(filepath.ToSlash(pattern), maxGlobBraceExpansions)
	if err != nil {
		return nil, err
	}
	parts := make([]compiledGlobPart, 0, len(expanded))
	for _, p := range expanded {
		re, cErr := compileSingleGlobRegex(p)
		if cErr != nil {
			continue
		}
		parts = append(parts, compiledGlobPart{
			raw:      p,
			hasSlash: strings.Contains(p, "/"),
			re:       re,
		})
	}
	return &compiledGlob{parts: parts}, nil
}

func (cg *compiledGlob) Match(relSlash string) bool {
	if cg == nil {
		return false
	}
	base := path.Base(relSlash)
	for _, p := range cg.parts {
		if !p.hasSlash {
			if ok, _ := path.Match(p.raw, base); ok {
				return true
			}
		}
		if p.re != nil && p.re.MatchString(relSlash) {
			return true
		}
	}
	return false
}

func (cg *compiledGlob) MatchSimple(fullSlash, baseName string) bool {
	return cg.Match(fullSlash) || cg.Match(baseName)
}

func expandBracesChecked(pat string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = maxGlobBraceExpansions
	}
	queue := []string{pat}
	var out []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		closeIdx := strings.IndexByte(cur, '}')
		open := -1
		if closeIdx > 0 {
			open = strings.LastIndexByte(cur[:closeIdx], '{')
		}
		if open < 0 || closeIdx < 0 {
			out = append(out, cur)
			if len(out) > limit {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "glob pattern %q exceeds max %d brace expansions", pat, limit)
			}
			continue
		}
		prefix := cur[:open]
		suffix := cur[closeIdx+1:]
		alts := strings.Split(cur[open+1:closeIdx], ",")
		if len(out)+len(queue)+len(alts) > limit {
			return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "glob pattern %q exceeds max %d brace expansions", pat, limit)
		}
		for _, a := range alts {
			queue = append(queue, prefix+strings.TrimSpace(a)+suffix)
		}
	}
	return out, nil
}

func expandBraces(pat string) []string {
	out, err := expandBracesChecked(pat, maxGlobBraceExpansions)
	if err != nil {
		return nil
	}
	return out
}

func compileSingleGlobRegex(pat string) (*regexp.Regexp, error) {
	var regexStr strings.Builder
	regexStr.WriteByte('^')
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				if i+2 < len(pat) && pat[i+2] == '/' {
					regexStr.WriteString("(?:.*/)?")
					i += 2
				} else {
					regexStr.WriteString(".*")
					i++
				}
			} else {
				regexStr.WriteString("[^/]*")
			}
		case '?':
			regexStr.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '[', ']', '\\':
			regexStr.WriteByte('\\')
			regexStr.WriteByte(c)
		default:
			regexStr.WriteByte(c)
		}
	}
	regexStr.WriteByte('$')
	return regexp.Compile(regexStr.String())
}

func buildTreeList(pol *policy.Engine, workspaceCwd, root string, maxDepth, maxItems int) []string {
	out := []string{}
	absRoot := filepath.Clean(root)
	realRoot := absRoot
	if pol != nil {
		var err error
		absRoot, realRoot, err = pol.ResolveReadWithReal(workspaceCwd, root)
		if err != nil {
			return out
		}
	} else if r, err := filepath.EvalSymlinks(absRoot); err == nil {
		realRoot = r
	}
	var walk func(absDir, realDir, relPrefix string, depth int)
	walk = func(absDir, realDir, relPrefix string, depth int) {
		if depth > maxDepth || len(out) >= maxItems {
			return
		}
		entries, err := os.ReadDir(absDir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if len(out) >= maxItems {
				return
			}
			name := e.Name()
			if runtime.GOOS == "windows" && strings.ContainsRune(name, ':') {
				continue
			}
			typ := e.Type()
			if typ == fs.ModeDir && skippedWalkDirs[name] {
				continue
			}
			fullPath := filepath.Join(absDir, name)
			childReal := filepath.Join(realDir, name)
			isDir := false
			switch typ {
			case 0:
				if pol != nil && pol.CheckResolvedRead(fullPath, fullPath, childReal) != nil {
					continue
				}
			case fs.ModeDir:
				if pol != nil && pol.CheckResolvedRead(fullPath, fullPath, childReal) != nil {
					continue
				}
				isDir = true
			default:
				if name == ".git" || skippedWalkDirs[name] {
					continue
				}
				if pol != nil {
					var rErr error
					_, childReal, rErr = pol.ResolveReadWithReal(workspaceCwd, fullPath)
					if rErr != nil {
						continue
					}
				}
				if fi, stErr := os.Stat(fullPath); stErr == nil && fi.IsDir() {
					isDir = true
				}
			}
			rel := name
			if relPrefix != "" {
				rel = relPrefix + "/" + name
			}
			if isDir {
				out = append(out, rel+"/")
				if depth < maxDepth && typ == fs.ModeDir {
					walk(fullPath, childReal, rel, depth+1)
				}
			} else {
				out = append(out, rel)
			}
		}
	}
	walk(absRoot, realRoot, "", 1)
	return out
}

func readBoundedLineRange(absPath, targetPath string, startLine, endLine, limit int, withNumbers bool) (string, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	s := maxInt(1, startLine)
	e := endLine
	if e <= 0 && limit > 0 {
		e = s + limit - 1
	}
	if e <= 0 {
		e = s + 499
	}
	if e < s {
		return "", fmt.Errorf("invalid range: startLine (%d) > endLine (%d)", s, e)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), MaxSandboxReadBytes)
	var selected []string
	totalBytes := 0
	lineNum := 0
	for scanner.Scan() {
		rawLine := scanner.Bytes()
		lineNum++
		if bytes.IndexByte(rawLine, 0) >= 0 || !utf8.Valid(rawLine) {
			return "", fmt.Errorf("file '%s' appears to be binary", targetPath)
		}
		if lineNum >= s && lineNum <= e {
			clean := string(rawLine)
			if withNumbers {
				clean = fmt.Sprintf("%d: %s", lineNum, clean)
			}
			totalBytes += len(clean) + 1
			if totalBytes > MaxSandboxReadBytes {
				return "", fmt.Errorf("requested line range from '%s' exceeds 2MB sandbox read cap; request fewer lines", targetPath)
			}
			selected = append(selected, clean)
		}
		if lineNum >= e {
			break
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		if errors.Is(scanErr, bufio.ErrTooLong) {
			return "", fmt.Errorf("line %d in '%s' exceeds 2MB sandbox line cap; use search.rg()", lineNum+1, targetPath)
		}
		return "", scanErr
	}
	if len(selected) == 0 {
		return "", nil
	}
	return strings.Join(selected, "\n"), nil
}

// catFailReason is a short per-file reason for fs.cat warnings.
func catFailReason(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "not found"
	case policy.IsApprovalRequired(err):
		return "needs approval"
	}
	var pe *policy.PolicyError
	if errors.As(err, &pe) {
		return "denied"
	}
	msg := err.Error()
	if r := []rune(msg); len(r) > 60 {
		msg = string(r[:60]) + "…"
	}
	return msg
}

// countCutLines counts hit/context lines shortened to MaxSearchLineBytes (they end with "...").
func countCutLines(hits []SearchHit) int {
	n := 0
	cut := func(s string) bool { return len(s) > MaxSearchLineBytes && strings.HasSuffix(s, "...") }
	for _, h := range hits {
		if cut(h.Text) {
			n++
		}
		for _, l := range h.Before {
			if cut(l) {
				n++
			}
		}
		for _, l := range h.After {
			if cut(l) {
				n++
			}
		}
	}
	return n
}

// ofTotal renders " of N" for cap warnings when the total is known.
func ofTotal(total int) string {
	if total < 0 {
		return ""
	}
	return fmt.Sprintf(" of %d", total)
}
