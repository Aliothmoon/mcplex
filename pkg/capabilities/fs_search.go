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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aliothmoon/mcplex/pkg/events"
	"github.com/aliothmoon/mcplex/pkg/output"
	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
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
			tree := buildTreeList(absPath, maxDepth, treeMaxItems)
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
				if len(kept) >= 3 {
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

func runRgJSON(ctx context.Context, pol *policy.Engine, rgBin, workspaceCwd, searchRoot string, q searchQuery) ([]SearchHit, int, error) {
	rgArgs := []string{"--json", "--line-number", "--column", "--no-heading", "--color=never"}
	if q.literal {
		rgArgs = append(rgArgs, "--fixed-strings")
	}
	if !q.caseSensitive {
		rgArgs = append(rgArgs, "--ignore-case")
	} else {
		rgArgs = append(rgArgs, "--case-sensitive")
	}
	if q.contextLines > 0 {
		rgArgs = append(rgArgs, fmt.Sprintf("-C%d", q.contextLines))
	}
	if q.glob != "" {
		rgArgs = append(rgArgs, "-g", q.glob)
	}
	rgArgs = append(rgArgs, "--", q.pattern, searchRoot)

	rgCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(rgCtx, rgBin, rgArgs...)
	setSysProcAttrHide(cmd)
	stdout := newLimitedSyncBuffer(2 * 1024 * 1024)
	stderr := newLimitedSyncBuffer(128 * 1024)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	// rg exit code 1 means 0 matches; exit code 2 means regex/syntax error.
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && rgCtx.Err() == nil {
			if exitErr.ExitCode() == 1 {
				return []SearchHit{}, 0, nil
			}
			if exitErr.ExitCode() == 2 {
				msg := strings.TrimSpace(stderr.String())
				if strings.Contains(strings.ToLower(msg), "regex") {
					return nil, -1, invalidRegexError(q.pattern, msg)
				}
				return nil, -1, fmt.Errorf("invalid search pattern: %s", msg)
			}
		}
		if rgCtx.Err() != nil {
			return nil, -1, fmt.Errorf("search.rg timed out: %w", rgCtx.Err())
		}
		return nil, -1, err
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
	permCache := map[string]bool{}
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
			skipFile = !canSearchPath(pol, workspaceCwd, ev.Data.Path.Text, permCache)
			continue
		case "match", "context":
			if skipFile || !canSearchPath(pol, workspaceCwd, ev.Data.Path.Text, permCache) {
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
	var err error
	pat := q.pattern
	if q.literal {
		pat = regexp.QuoteMeta(pat)
	}
	if !q.caseSensitive {
		pat = "(?i)" + pat
	}
	re, err = regexp.Compile(pat)
	if err != nil {
		return nil, invalidRegexError(q.pattern, err.Error())
	}

	var files []string
	info, err := os.Stat(searchRoot)
	if err != nil {
		return nil, err
	}
	permCache := map[string]bool{}
	if !info.IsDir() {
		if canSearchPath(pol, workspaceCwd, searchRoot, permCache) {
			files = []string{searchRoot}
		}
	} else {
		_ = filepath.WalkDir(searchRoot, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				if p != searchRoot && (skippedWalkDirs[d.Name()] || !canSearchPath(pol, workspaceCwd, p, permCache)) {
					return filepath.SkipDir
				}
				return nil
			}
			if !canSearchPath(pol, workspaceCwd, p, permCache) {
				return nil
			}
			if q.glob != "" && !matchSimpleGlob(q.glob, filepath.ToSlash(p), d.Name()) {
				return nil
			}
			files = append(files, p)
			return nil
		})
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
			loc := re.FindStringIndex(line)
			if loc == nil {
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
				Column:  loc[0] + 1,
				Text:    txt,
				Before:  before,
				After:   after,
				Subtext: line[loc[0]:loc[1]],
			})
			if len(hits) >= q.maxResults {
				break
			}
		}
	}
	return hits, nil
}

// runGlobSearch returns up to maxResults files plus the total number of matches.
func (b *FSSearchBinder) runGlobSearch(ctx context.Context, workspaceCwd, searchRoot, pattern string, maxResults int) ([]string, int, string, error) {
	if maxResults <= 0 {
		maxResults = 200
	}
	allFiles := collectWorkspaceFiles(ctx, b.Policy, workspaceCwd, searchRoot)
	total := 0
	matchFiles := func(pat string) []string {
		var matched []string
		total = 0
		for _, abs := range allFiles {
			relToSearch, err := filepath.Rel(searchRoot, abs)
			if err != nil {
				relToSearch = abs
			}
			relSlash := filepath.ToSlash(relToSearch)
			if matchGlobPattern(pat, relSlash) {
				total++
				if len(matched) < maxResults {
					matched = append(matched, relDisplayPath(workspaceCwd, abs))
				}
			}
		}
		sort.Strings(matched)
		return matched
	}

	exact := matchFiles(pattern)
	if len(exact) > 0 {
		return exact, total, "", nil
	}

	// Smart fallback 1 (inspired by agy-fast v0.2.2): expand regex alternation `a|b` mistakenly passed to glob
	if strings.Contains(pattern, "|") && !strings.Contains(pattern, "{") {
		expanded := "*{" + strings.ReplaceAll(pattern, "|", ",") + "}*"
		recovered := matchFiles(expanded)
		if len(recovered) > 0 {
			return recovered, total, fmt.Sprintf("auto-expanded regex '|' to glob '%s'", expanded), nil
		}
	}

	// Smart fallback 2: bare keyword without wildcards, slashes, or extension dots -> substring match
	if !strings.ContainsAny(pattern, "*?[]{}/\\.") {
		sub := strings.ToLower(pattern)
		var recovered []string
		total = 0
		for _, abs := range allFiles {
			rel := relDisplayPath(workspaceCwd, abs)
			if strings.Contains(strings.ToLower(rel), sub) {
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

func collectWorkspaceFiles(ctx context.Context, pol *policy.Engine, workspaceCwd, root string) []string {
	var out []string
	permCache := map[string]bool{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && (skippedWalkDirs[d.Name()] || !canSearchPath(pol, workspaceCwd, p, permCache)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !canSearchPath(pol, workspaceCwd, p, permCache) {
			return nil
		}
		out = append(out, p)
		return nil
	})
	return out
}

func matchGlobPattern(pattern, relSlash string) bool {
	// Expand single-level `{a,b,c}` braces
	for _, expanded := range expandBraces(filepath.ToSlash(pattern)) {
		if globSingle(expanded, relSlash) {
			return true
		}
	}
	return false
}

func expandBraces(pat string) []string {
	open := strings.IndexByte(pat, '{')
	closeIdx := strings.IndexByte(pat, '}')
	if open >= 0 && closeIdx > open {
		prefix := pat[:open]
		suffix := pat[closeIdx+1:]
		alts := strings.Split(pat[open+1:closeIdx], ",")
		var out []string
		for _, a := range alts {
			out = append(out, expandBraces(prefix+strings.TrimSpace(a)+suffix)...)
		}
		return out
	}
	return []string{pat}
}

func globSingle(pat, relSlash string) bool {
	if !strings.Contains(pat, "/") {
		ok, _ := path.Match(pat, path.Base(relSlash))
		if ok {
			return true
		}
	}
	regexStr := "^"
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				if i+2 < len(pat) && pat[i+2] == '/' {
					regexStr += "(?:.*/)?"
					i += 2
				} else {
					regexStr += ".*"
					i++
				}
			} else {
				regexStr += "[^/]*"
			}
		case '?':
			regexStr += "[^/]"
		case '.', '+', '(', ')', '|', '^', '$', '[', ']', '\\':
			regexStr += "\\" + string(c)
		default:
			regexStr += string(c)
		}
	}
	regexStr += "$"
	re, err := regexp.Compile(regexStr)
	if err != nil {
		return false
	}
	return re.MatchString(relSlash)
}

func matchSimpleGlob(globPat, fullSlash, baseName string) bool {
	return matchGlobPattern(globPat, fullSlash) || matchGlobPattern(globPat, baseName)
}

func buildTreeList(root string, maxDepth, maxItems int) []string {
	out := []string{}
	var walk func(dir, relPrefix string, depth int)
	walk = func(dir, relPrefix string, depth int) {
		if depth > maxDepth || len(out) >= maxItems {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if len(out) >= maxItems {
				return
			}
			name := e.Name()
			if e.IsDir() && skippedWalkDirs[name] {
				continue
			}
			rel := name
			if relPrefix != "" {
				rel = relPrefix + "/" + name
			}
			if e.IsDir() {
				out = append(out, rel+"/")
				if depth < maxDepth {
					walk(filepath.Join(dir, name), rel, depth+1)
				}
			} else {
				out = append(out, rel)
			}
		}
	}
	walk(root, "", 1)
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
