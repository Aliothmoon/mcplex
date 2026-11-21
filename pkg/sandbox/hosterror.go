package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Error kinds exposed to guest JS as `e.name`. Short, semantic, and stable so programs can branch on them.
const (
	KindNotFound      = "NotFound"      // path does not exist
	KindNoMatch       = "NoMatch"       // edit target not found
	KindAmbiguous     = "Ambiguous"     // edit target matches several places
	KindNeedsApproval = "NeedsApproval" // policy requires the user's confirmation via a native tool
	KindDenied        = "Denied"        // policy forbids the operation
	KindTimeout       = "Timeout"
	KindBadArgs       = "BadArgs"
	KindCmdFailed     = "CmdFailed" // command exited non-zero with throwOnError
	KindError         = "Error"     // anything else
)

// HostError is a capability failure carrying a semantic kind and optional structured data (exposed as `e.data`).
type HostError struct {
	Kind string
	Msg  string
	Data map[string]any
}

func (e *HostError) Error() string { return e.Msg }

// NewHostError builds a HostError with a formatted message.
func NewHostError(kind string, data map[string]any, format string, args ...any) *HostError {
	return &HostError{Kind: kind, Msg: fmt.Sprintf(format, args...), Data: data}
}

// KindedError lets errors from other packages (e.g. policy.PolicyError) declare their kind without importing sandbox.
// Detail is the message without the kind word, so `name: op: detail` never repeats itself.
type KindedError interface {
	error
	Kind() string
	Detail() string
}

// classifyHostError maps a Go error to (kind, message, data) for the guest-visible error object.
func (env *ExecutionEnv) classifyHostError(err error) (string, string, map[string]any) {
	var he *HostError
	if errors.As(err, &he) {
		msg := he.Msg
		if he != err { // wrapped: keep outer context
			msg = err.Error()
		}
		return he.Kind, msg, he.Data
	}
	var ke KindedError
	if errors.As(err, &ke) {
		if ke == err {
			return ke.Kind(), ke.Detail(), nil
		}
		return ke.Kind(), err.Error(), nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return KindNotFound, env.notFoundMessage(pe.Path), nil
		}
		return KindNotFound, err.Error(), nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout, err.Error(), nil
	}
	return KindError, err.Error(), nil
}

// notFoundMessage renders `<rel> not found` plus up to 3 similar sibling names, or the first missing directory.
func (env *ExecutionEnv) notFoundMessage(absPath string) string {
	rel := env.displayPath(absPath)
	dir := filepath.Dir(absPath)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		missing := dir
		for {
			parent := filepath.Dir(missing)
			if parent == missing {
				break
			}
			if _, err := os.Stat(parent); err == nil {
				break
			}
			missing = parent
		}
		return fmt.Sprintf("%s not found (no dir %s)", rel, env.displayPath(missing))
	}
	if similar := similarNames(dir, filepath.Base(absPath), 3); len(similar) > 0 {
		relDir := filepath.Dir(rel)
		for i, s := range similar {
			if relDir != "." {
				similar[i] = filepath.ToSlash(filepath.Join(relDir, s))
			}
		}
		return fmt.Sprintf("%s not found; similar: %s", rel, strings.Join(similar, ", "))
	}
	return rel + " not found"
}

func (env *ExecutionEnv) displayPath(absPath string) string {
	if env.Cwd != "" {
		if rel, err := filepath.Rel(env.Cwd, absPath); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(absPath)
}

// similarNames returns up to n entries of dir whose names are close to want (case/extension/typo variants).
func similarNames(dir, want string, n int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	if len(entries) > 5000 {
		entries = entries[:5000]
	}
	wantLow := strings.ToLower(want)
	wantStem := strings.TrimSuffix(wantLow, filepath.Ext(wantLow))
	type cand struct {
		name  string
		score int
	}
	var cands []cand
	for _, e := range entries {
		name := e.Name()
		low := strings.ToLower(name)
		stem := strings.TrimSuffix(low, filepath.Ext(low))
		// Rank by edit distance; a shared stem (other extension) qualifies even when far, and breaks ties.
		score := -1
		d := levenshtein(low, wantLow)
		sameStem := stem == wantStem && wantStem != ""
		if low == wantLow {
			score = 0
		} else if sameStem || d <= max(2, len(wantLow)/4) {
			score = 2 * d
			if !sameStem {
				score++
			}
		}
		if score >= 0 {
			if e.IsDir() {
				name += "/"
			}
			cands = append(cands, cand{name, score})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score < cands[j].score })
	var out []string
	for i := 0; i < len(cands) && i < n; i++ {
		out = append(out, cands[i].name)
	}
	return out
}

func levenshtein(a, b string) int {
	if len(a) > 64 || len(b) > 64 {
		return 99
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// PublicOpName hides internal raw methods: `tx._begin` -> `tx.begin`, `tx._edit` -> `t.edit` (the handle the user holds).
// Other raw `ns._m` helpers wrapped by the bootstrap (e.g. fs._bytes) surface as `ns.m`.
func PublicOpName(capability string) string {
	if m, ok := strings.CutPrefix(capability, "tx._"); ok {
		if m == "begin" {
			return "tx.begin"
		}
		return "t." + m
	}
	if ns, m, ok := strings.Cut(capability, "._"); ok {
		return ns + "." + m
	}
	return capability
}

// sideEffectOps are listed in error envelopes so the model does not blindly repeat non-idempotent work.
// Read-only ops are omitted to save tokens; t.edit/t.write are omitted because uncommitted tx edits are rolled back.
var sideEffectOps = map[string]bool{
	"fs.write": true, "fs.mkdir": true, "fs.rm": true, "fs.rename": true, "fs.cp": true,
	"edit.apply": true, "edit.replace": true, "t.commit": true,
	"sh.exec": true, "sh.checks": true, "t.verify": true, "mcp.call": true, "tools.call": true,
}

// IsSideEffectOp reports whether a (public) op name may have changed state outside the sandbox.
func IsSideEffectOp(op string) bool { return sideEffectOps[op] }

// approxSize estimates how many bytes a host result puts into the program (exact for strings).
func approxSize(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case string:
		return len(x)
	case []byte:
		return len(x)
	case *OrderedMap:
		n := 0
		for i, k := range x.Keys {
			n += len(k) + approxSize(x.Vals[i])
		}
		return n
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}
