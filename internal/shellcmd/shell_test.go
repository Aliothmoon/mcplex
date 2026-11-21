package shellcmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultShellAndUnknownShell(t *testing.T) {
	def := DefaultShellName()
	if def == "" {
		t.Fatal("DefaultShellName returned empty")
	}
	exe, args, dialect, err := Resolve("")
	if err != nil {
		t.Fatalf("Resolve default failed: %v", err)
	}
	if exe == "" || dialect == "" || len(args) == 0 {
		t.Fatalf("unexpected Resolve result: exe=%q args=%v dialect=%q", exe, args, dialect)
	}

	_, _, _, err = Resolve("totally-unknown-shell")
	if err == nil || IsNotFound(err) {
		t.Fatalf("expected BadArgs (NotFound=false) for unknown shell, got: %v", err)
	}
}

func TestIsWSLSystemBashExcludesSystem32AndSysnative(t *testing.T) {
	blocked := []string{
		`C:\Windows\System32\bash.exe`,
		`c:/windows/system32/bash.exe`,
		`C:\Windows\Sysnative\bash.exe`,
		`C:\Users\me\AppData\Local\Microsoft\WindowsApps\bash.exe`,
	}
	for _, p := range blocked {
		if !IsWSLSystemBash(p) {
			t.Errorf("expected %q to be recognized as WSL bash launcher", p)
		}
	}

	allowed := []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files\Git\usr\bin\bash.exe`,
		`C:\Program Files\Git\usr\bin\sh.exe`,
		`/bin/bash`,
		`/usr/bin/sh`,
	}
	for _, p := range allowed {
		if IsWSLSystemBash(p) {
			t.Errorf("expected %q NOT to be treated as WSL bash launcher", p)
		}
	}
}

func TestGitForWindowsShellLayoutDiscovery(t *testing.T) {
	fakeGitRoot := t.TempDir()
	cmdDir := filepath.Join(fakeGitRoot, "cmd")
	usrBin := filepath.Join(fakeGitRoot, "usr", "bin")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(usrBin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGit := filepath.Join(cmdDir, "git.exe")
	fakeBash := filepath.Join(usrBin, "bash.exe")
	if err := os.WriteFile(fakeGit, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakeBash, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := findGitForWindowsShell(fakeGit, "bash")
	if got != fakeBash {
		t.Fatalf("expected %q, got %q", fakeBash, got)
	}
}

func TestWindowsCmdBuildCommandPreservesQuotesWithoutBackslashEscaping(t *testing.T) {
	if runtime.GOOS != "windows" {
		_, _, _, err := Resolve("cmd")
		if err == nil || IsNotFound(err) {
			t.Fatalf("expected cmd on non-Windows to fail as BadArgs, got %v", err)
		}
		return
	}
	cmd, err := BuildCommand(context.Background(), "cmd", `echo "a b"`)
	if err != nil {
		t.Fatalf("BuildCommand(cmd) failed: %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd execution failed: %v (out=%q)", err, string(out))
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed != `"a b"` {
		t.Fatalf(`expected raw quotes "a b", got %q`, trimmed)
	}
}
