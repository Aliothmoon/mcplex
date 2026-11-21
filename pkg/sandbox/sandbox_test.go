package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicOpName(t *testing.T) {
	for in, want := range map[string]string{
		"tx._begin": "tx.begin", "tx._edit": "t.edit", "tx._rename": "t.rename",
		"fs._bytes": "fs.bytes", "fs.read": "fs.read", "mcp.call": "mcp.call",
	} {
		if got := PublicOpName(in); got != want {
			t.Errorf("PublicOpName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApproxSize(t *testing.T) {
	om := &OrderedMap{Keys: []string{"ab", "c"}, Vals: []any{"xyz", nil}}
	cases := []struct {
		v    any
		want int
	}{
		{nil, 0}, {"hello", 5}, {[]byte("abc"), 3}, {om, 2 + 3 + 1}, {map[string]any{"a": 1}, len(`{"a":1}`)},
	}
	for _, c := range cases {
		if got := approxSize(c.v); got != c.want {
			t.Errorf("approxSize(%v) = %d, want %d", c.v, got, c.want)
		}
	}
}

func TestIsSecretEnvName(t *testing.T) {
	secret := []string{"GITHUB_TOKEN", "MY_API_KEY", "AWS_SECRET_ACCESS_KEY", "DB_PASSWORD", "NPM_AUTH", "OPENAI_APIKEY", "SSH_AUTH_SOCK", "GH_PAT", "SENTRY_DSN"}
	public := []string{"PATH", "HOME", "GOOS", "GIT_AUTHOR_NAME", "KEYBOARD_LAYOUT", "PWD", "CARGO_HOME", "ANDROID_HOME", "TERM"}
	for _, n := range secret {
		if !isSecretEnvName(n) {
			t.Errorf("%s should be redacted", n)
		}
	}
	for _, n := range public {
		if isSecretEnvName(n) {
			t.Errorf("%s should stay visible", n)
		}
	}
}

func TestSimilarNamesAndLevenshtein(t *testing.T) {
	if d := levenshtein("kitten", "sitting"); d != 3 {
		t.Fatalf("levenshtein = %d, want 3", d)
	}
	dir := t.TempDir()
	for _, n := range []string{"lib.rs", "main.rs", "Lib.go", "unrelated.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := similarNames(dir, "lib.r", 3)
	if len(got) == 0 || got[0] != "lib.rs" {
		t.Fatalf("similarNames = %v, want lib.rs first", got)
	}
	for _, g := range got {
		if g == "unrelated.txt" {
			t.Fatalf("unrelated file suggested: %v", got)
		}
	}
}

type kinded struct{}

func (kinded) Error() string  { return "denied: x — rule" }
func (kinded) Kind() string   { return KindDenied }
func (kinded) Detail() string { return "x — rule" }

func TestClassifyHostError(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "lib.rs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env := &ExecutionEnv{Cwd: ws}
	_, statErr := os.Stat(filepath.Join(ws, "lib.r"))

	cases := []struct {
		err      error
		kind     string
		contains string
	}{
		{statErr, KindNotFound, "lib.r not found; similar: lib.rs"},
		{NewHostError(KindAmbiguous, nil, "2 matches"), KindAmbiguous, "2 matches"},
		{fmt.Errorf("wrap: %w", NewHostError(KindNoMatch, nil, "inner")), KindNoMatch, "wrap: inner"},
		{kinded{}, KindDenied, "x — rule"},
		{context.DeadlineExceeded, KindTimeout, "deadline"},
		{fmt.Errorf("boom"), KindError, "boom"},
	}
	for _, c := range cases {
		kind, msg, _ := env.classifyHostError(c.err)
		if kind != c.kind || !strings.Contains(msg, c.contains) {
			t.Errorf("classify(%v) = (%s, %q), want (%s, contains %q)", c.err, kind, msg, c.kind, c.contains)
		}
	}
	if _, msg, _ := env.classifyHostError(kinded{}); strings.HasPrefix(msg, "denied:") {
		t.Errorf("kinded detail must not repeat the kind word: %q", msg)
	}
}

func TestWarningsAreDeduplicated(t *testing.T) {
	env := &ExecutionEnv{}
	env.AddWarning("a")
	env.AddWarning("b")
	env.AddWarning("a")
	env.AddWarning("a")
	got := strings.Join(env.getWarnings(), "|")
	if got != "a (×3)|b" {
		t.Fatalf("warnings = %q", got)
	}
}
