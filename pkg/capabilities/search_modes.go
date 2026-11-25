package capabilities

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

const defaultRgListResults = 1000

type fileCount struct {
	file  string
	count int
}

// runListSearch implements search.rg({ filesOnly: true }) -> string[] and search.rg({ count: true }) -> { [file]: n }
// (like rg -l / rg -c), sorted by path / by count desc. Uses rg when available, else the Go fallback.
func (b *FSSearchBinder) runListSearch(ctx context.Context, env *sandbox.ExecutionEnv, searchRoot string, q searchQuery) (any, error) {
	var counts []fileCount
	var err error
	if rgBin := b.resolveRg(); rgBin != "" {
		var overflow bool
		counts, overflow, err = runRgCounts(ctx, b.Policy, rgBin, env.Cwd, searchRoot, q)
		if overflow {
			env.AddWarning("search.rg: count output exceeded 8MB, some files are missing; narrow path/glob")
		}
		var he *sandbox.HostError
		if err != nil && errors.As(err, &he) {
			return nil, err
		}
	}
	if counts == nil && (err == nil || ctx.Err() == nil) {
		fq := q
		fq.contextLines = 0
		fq.maxResults = 200000
		hits, _, ferr := runGoFallbackSearch(ctx, b.Policy, env.Cwd, searchRoot, fq)
		if ferr != nil {
			return nil, ferr
		}
		idx := map[string]int{}
		counts = counts[:0]
		for _, h := range hits {
			if i, ok := idx[h.File]; ok {
				counts[i].count++
				continue
			}
			idx[h.File] = len(counts)
			counts = append(counts, fileCount{h.File, 1})
		}
	} else if err != nil {
		return nil, err
	}

	if q.mode == "files" {
		sort.Slice(counts, func(i, j int) bool { return counts[i].file < counts[j].file })
	} else {
		sort.SliceStable(counts, func(i, j int) bool {
			if counts[i].count != counts[j].count {
				return counts[i].count > counts[j].count
			}
			return counts[i].file < counts[j].file
		})
	}
	if len(counts) > q.maxResults {
		env.AddWarning(fmt.Sprintf("search.rg: capped at %d of %d files; narrow path/glob or pass { maxResults }", q.maxResults, len(counts)))
		counts = counts[:q.maxResults]
	}
	if q.mode == "files" {
		files := make([]any, len(counts))
		for i, c := range counts {
			files[i] = c.file
		}
		return files, nil
	}
	out := &sandbox.OrderedMap{}
	for _, c := range counts {
		out.Keys = append(out.Keys, c.file)
		out.Vals = append(out.Vals, c.count)
	}
	return out, nil
}

// runRgCounts runs `rg --count` (matching lines per file); files mode reuses it since rg -l is just count > 0.
func runRgCounts(ctx context.Context, pol *policy.Engine, rgBin, workspaceCwd, searchRoot string, q searchQuery) ([]fileCount, bool, error) {
	baseFlags := []string{"--count", "--with-filename", "--no-heading", "--color=never"}
	stdout, noMatches, err := execRgSearch(ctx, rgBin, baseFlags, q, searchRoot, 8*1024*1024)
	if err != nil {
		return nil, false, err
	}
	if noMatches {
		return []fileCount{}, false, nil
	}
	var out []fileCount
	checker := newFastPathChecker(pol, workspaceCwd)
	sc := bufio.NewScanner(strings.NewReader(stdout.String()))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		i := strings.LastIndexByte(line, ':') // paths may contain ':' (Windows drives), the count never does
		if i <= 0 {
			continue
		}
		n, err := strconv.Atoi(line[i+1:])
		if err != nil {
			continue
		}
		fp := line[:i]
		if !checker.allowFile(fp) {
			continue
		}
		out = append(out, fileCount{relDisplayPath(workspaceCwd, fp), n})
	}
	if out == nil {
		out = []fileCount{}
	}
	return out, stdout.droppedBytes() > 0, nil
}
