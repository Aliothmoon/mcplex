package capabilities

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

// diffFile is one file's before/after content for a unified diff.
type diffFile struct {
	rel               string
	before, after     []byte
	beforeOK, afterOK bool // whether the file exists on that side
}

// gitDiff renders a unified diff via `git diff --no-index` over a temp mirror (a/ = before, b/ = after).
// Content is LF-normalized so CRLF preservation never shows as a change; the user's git config is isolated
// (no external diff tools, colors or custom prefixes); `diff --git` / `index` / mode lines are dropped to save tokens.
func gitDiff(files []diffFile) (string, error) {
	var changed []diffFile
	for _, f := range files {
		if f.beforeOK != f.afterOK || !bytes.Equal(lf(f.before), lf(f.after)) {
			changed = append(changed, f)
		}
	}
	if len(changed) == 0 {
		return "", nil
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return "", sandbox.NewHostError(sandbox.KindNotFound, nil, "git: command not found (diff needs git)")
	}
	tmp, err := os.MkdirTemp("", "agy-ptc-diff-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	realName := map[string]string{} // temp-safe rel path -> displayed path
	for i, f := range changed {
		safe := safeDiffPath(f.rel, i)
		realName[safe] = f.rel
		for _, side := range []struct {
			dir    string
			data   []byte
			exists bool
		}{{"a", f.before, f.beforeOK}, {"b", f.after, f.afterOK}} {
			p := filepath.Join(tmp, side.dir, filepath.FromSlash(safe))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			if side.exists {
				if err := os.WriteFile(p, lf(side.data), 0o644); err != nil {
					return "", err
				}
			}
		}
	}
	emptyCfg := filepath.Join(tmp, "empty.gitconfig")
	if err := os.WriteFile(emptyCfg, nil, 0o644); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBin, "-c", "core.quotepath=off", "-c", "core.autocrlf=false",
		"diff", "--no-index", "--no-color", "--no-ext-diff", "--no-textconv", "--no-prefix", fmt.Sprintf("-U%d", diffContext), "a", "b")
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+emptyCfg)
	setSysProcAttrHide(cmd)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 { // exit 1 = differences found
			return "", fmt.Errorf("git diff failed: %v %s", err, strings.TrimSpace(stderr.String()))
		}
	}

	var sb strings.Builder
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "), strings.HasPrefix(line, "index "),
			strings.HasPrefix(line, "new file mode "), strings.HasPrefix(line, "deleted file mode "),
			strings.HasPrefix(line, "old mode "), strings.HasPrefix(line, "new mode "):
			continue
		case strings.HasPrefix(line, "--- a/"):
			if r, ok := realName[strings.TrimPrefix(line, "--- a/")]; ok {
				line = "--- a/" + r
			}
		case strings.HasPrefix(line, "+++ b/"):
			if r, ok := realName[strings.TrimPrefix(line, "+++ b/")]; ok {
				line = "+++ b/" + r
			}
		case strings.HasPrefix(line, "rename from a/"): // git's rename detection: concise for t.rename
			if r, ok := realName[strings.TrimPrefix(line, "rename from a/")]; ok {
				line = "rename from " + r
			}
		case strings.HasPrefix(line, "rename to b/"):
			if r, ok := realName[strings.TrimPrefix(line, "rename to b/")]; ok {
				line = "rename to " + r
			}
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}

// diffContext is the number of unchanged lines around each change (2 instead of diff's 3 to save tokens).
const diffContext = 2

func lf(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }

// safeDiffPath maps a displayed path (possibly absolute or ../) to a relative path usable inside the temp mirror.
func safeDiffPath(rel string, i int) string {
	p := filepath.ToSlash(rel)
	if filepath.IsAbs(rel) || strings.HasPrefix(p, "../") || strings.Contains(p, ":") {
		return fmt.Sprintf("_ext%d/%s", i, filepath.Base(rel))
	}
	return strings.TrimPrefix(p, "./")
}
