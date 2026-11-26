package capabilities

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func encodeUTF16WithBOM(s string, bigEndian bool) []byte {
	var buf bytes.Buffer
	if bigEndian {
		buf.Write([]byte{0xFE, 0xFF})
	} else {
		buf.Write([]byte{0xFF, 0xFE})
	}
	u16s := utf16.Encode([]rune(s))
	var pair [2]byte
	for _, u := range u16s {
		if bigEndian {
			binary.BigEndian.PutUint16(pair[:], u)
		} else {
			binary.LittleEndian.PutUint16(pair[:], u)
		}
		buf.Write(pair[:])
	}
	return buf.Bytes()
}

func TestFsReadUTF16BOMTextFiles(t *testing.T) {
	dir := t.TempDir()
	sample := "11-26 19:46:50.001 INFO boot start\r\n11-26 19:46:51.002 WARN 中文日志 😀\r\n11-26 19:46:52.003 INFO done\r\n"

	lePath := filepath.Join(dir, "live_guest_logcat.txt")
	if err := os.WriteFile(lePath, encodeUTF16WithBOM(sample, false), 0o644); err != nil {
		t.Fatal(err)
	}
	bePath := filepath.Join(dir, "be_log.txt")
	if err := os.WriteFile(bePath, encodeUTF16WithBOM(sample, true), 0o644); err != nil {
		t.Fatal(err)
	}
	// Gzipped UTF-16LE file
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write(encodeUTF16WithBOM(sample, false))
	_ = gw.Close()
	if err := os.WriteFile(filepath.Join(dir, "le_log.txt.gz"), gzBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// Large UTF-16LE file (>2MB) to exercise streaming readBoundedLineRange + readTailLinesUTF16
	var big strings.Builder
	for i := 1; i <= 40000; i++ {
		big.WriteString("line content for utf16 streaming test\r\n")
	}
	bigPath := filepath.Join(dir, "big_utf16.log")
	if err := os.WriteFile(bigPath, encodeUTF16WithBOM(big.String(), false), 0o644); err != nil {
		t.Fatal(err)
	}
	// Binary file starting with FF FE + embedded NUL codepoint (UTF-32LE / binary) MUST still be rejected.
	if err := os.WriteFile(filepath.Join(dir, "fake_bin.dat"), []byte{0xFF, 0xFE, 0x00, 0x00, 0x41, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}

	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	binder := &FSSearchBinder{Policy: pol}

	code := `
const le = await fs.read("live_guest_logcat.txt");
const be = await fs.read("be_log.txt");
const gz = await fs.read("le_log.txt.gz");
const tail = await fs.read("live_guest_logcat.txt", { tail: 2, lineNumbers: true });
const bigRange = await fs.read("big_utf16.log", { startLine: 39999, endLine: 40000, lineNumbers: true });
const bigTail = await fs.read("big_utf16.log", { tail: 2, lineNumbers: true });
let binBlocked = false;
try {
  await fs.read("fake_bin.dat");
} catch (e) {
  binBlocked = e.name === "BadArgs" && e.message.includes("is binary");
}
return { le, be, gz, tail, bigRange, bigTail, binBlocked };
`
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{Cwd: dir, Code: code}, binder)
	if err != nil {
		t.Fatalf("UTF-16 fs.read test failed: %v", err)
	}
	m := res.Value.(map[string]any)
	want := "11-26 19:46:50.001 INFO boot start\n11-26 19:46:51.002 WARN 中文日志 😀\n11-26 19:46:52.003 INFO done\n"
	if m["le"] != want {
		t.Fatalf("UTF-16LE mismatch:\ngot:  %q\nwant: %q", m["le"], want)
	}
	if m["be"] != want {
		t.Fatalf("UTF-16BE mismatch:\ngot:  %q\nwant: %q", m["be"], want)
	}
	if m["gz"] != want {
		t.Fatalf("gzipped UTF-16LE mismatch:\ngot:  %q\nwant: %q", m["gz"], want)
	}
	if !strings.Contains(m["tail"].(string), "2: 11-26 19:46:51.002 WARN 中文日志 😀\n3: 11-26 19:46:52.003 INFO done") {
		t.Fatalf("unexpected UTF-16 tail: %q", m["tail"])
	}
	if !strings.Contains(m["bigRange"].(string), "39999: line content") || !strings.Contains(m["bigRange"].(string), "40000: line content") {
		t.Fatalf("unexpected big UTF-16 range: %q", m["bigRange"])
	}
	if !strings.Contains(m["bigTail"].(string), "39999: line content") || !strings.Contains(m["bigTail"].(string), "40000: line content") {
		t.Fatalf("unexpected big UTF-16 tail: %q", m["bigTail"])
	}
	if m["binBlocked"] != true {
		t.Fatalf("expected binary file with FF FE 00 00 to be blocked, got %+v", m["binBlocked"])
	}
}

func TestGitFormatAndRefPathSpecErgonomics(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "a.txt", "b.txt")
	runGit("commit", "-m", "initial commit")

	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	binder := &ShellGitBinder{Policy: pol}

	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Cwd: dir,
		Code: `
const logOneline = await git.log({ n: 5, format: "--oneline" });
const logShort = await git.log({ n: 5, format: "--short" });
const showA = await git.show({ ref: "HEAD -- a.txt" });
return { logOneline, logShort, showA };
`,
	}, binder)
	if err != nil {
		t.Fatalf("git ergonomics test failed: %v", err)
	}
	m := res.Value.(map[string]any)
	if !strings.Contains(m["logOneline"].(string), "initial commit") {
		t.Fatalf("expected git.log({ format: '--oneline' }) output, got: %q", m["logOneline"])
	}
	if !strings.Contains(m["logShort"].(string), "initial commit") {
		t.Fatalf("expected git.log({ format: '--short' }) output, got: %q", m["logShort"])
	}
	showA := m["showA"].(string)
	if !strings.Contains(showA, "a.txt") || !strings.Contains(showA, "+alpha") || strings.Contains(showA, "+beta") {
		t.Fatalf("expected git.show({ ref: 'HEAD -- a.txt' }) to show only a.txt, got:\n%s", showA)
	}

	// Ensure policy still blocks outside-workspace paths and git pathspec magic inside ref pathspecs.
	for _, expr := range []string{
		`await git.show({ ref: "HEAD -- ../outside.txt" });`,
		`await git.show({ ref: "HEAD -- :(top)a.txt" });`,
	} {
		if _, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{Cwd: dir, Code: expr}, binder); err == nil {
			t.Fatalf("expected policy to block %s", expr)
		}
	}
}
