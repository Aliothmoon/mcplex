package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWindowsAlternateDataStreamDetection(t *testing.T) {
	blocked := []string{
		`.git/config::$DATA`,
		`~/.git-credentials::$DATA`,
		`C:\repo\.git\config::$DATA`,
		`foo.txt:hidden`,
		`\\.\PhysicalDrive0`,
		`\\?\C:\Windows\win.ini`,
	}
	for _, p := range blocked {
		if !hasWindowsAlternateDataStream(p) {
			t.Errorf("expected ADS/device path %q to be detected", p)
		}
	}
	allowed := []string{
		`C:\Project\repo\main.go`,
		`D:/repo/.git/HEAD`,
		`./relative/file.txt`,
		`~/notes.md`,
	}
	for _, p := range allowed {
		if hasWindowsAlternateDataStream(p) {
			t.Errorf("expected normal path %q NOT to trigger ADS check", p)
		}
	}

	if runtime.GOOS == "windows" {
		ws := t.TempDir()
		eng := NewEngineWithOptions(ws, Options{StrictWorkspace: true})
		if _, err := eng.ResolveAndCheckWrite(ws, `.git/config::$DATA`); err == nil {
			t.Fatal("expected ResolveAndCheckWrite to block .git/config::$DATA on Windows")
		}
		if _, err := eng.ResolveAndCheckRead(ws, `~/.git-credentials::$DATA`); err == nil {
			t.Fatal("expected ResolveAndCheckRead to block ~/.git-credentials::$DATA on Windows")
		}
	}
}

func TestCaseInsensitiveCredentialGuardsAndNewCLICreds(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no user home dir")
	}
	ws := t.TempDir()
	eng := NewEngineWithOptions(ws, Options{StrictWorkspace: true})

	cases := []string{
		filepath.Join(home, ".SSH", "id_rsa"),
		filepath.Join(home, ".gemini", ".env"),
		filepath.Join(home, ".claude", ".credentials.json"),
		filepath.Join(home, ".codex", "auth.json"),
		filepath.Join(home, ".GEMINI", "oauth_creds.json"),
	}
	for _, p := range cases {
		if _, err := eng.ResolveAndCheckRead(ws, p); err == nil {
			t.Errorf("expected sensitive credential path %q to be blocked for read", p)
		}
		if _, err := eng.ResolveAndCheckWrite(ws, p); err == nil {
			t.Errorf("expected sensitive credential path %q to be blocked for write", p)
		}
	}

	// Non-brain files under ~/.gemini must NOT be treated as scratch in strict mode.
	nonBrain := filepath.Join(home, ".gemini", "settings_other.json")
	if eng.isTempOrScratch(nonBrain) {
		t.Fatalf("expected %q NOT to be treated as scratch", nonBrain)
	}
	brainFile := filepath.Join(home, ".gemini", "antigravity-cli", "brain", "conv-1", "notes.md")
	if !eng.isTempOrScratch(brainFile) {
		t.Fatalf("expected %q to be treated as scratch", brainFile)
	}
}

func TestExplicitPolicyAndMCPConfigSelfProtection(t *testing.T) {
	ws := t.TempDir()
	polFile := filepath.Join(ws, "my-policy.json")
	mcpFile := filepath.Join(ws, "downstream-mcp.json")
	_ = os.WriteFile(polFile, []byte(`{}`), 0o644)
	_ = os.WriteFile(mcpFile, []byte(`{"mcpServers":{}}`), 0o644)

	eng := NewEngineWithOptions(ws, Options{
		StrictWorkspace: true,
		PolicyFiles:     []string{polFile},
		ProtectedFiles:  []string{mcpFile},
	})

	if _, err := eng.ResolveAndCheckWrite(ws, polFile); err == nil {
		t.Fatal("expected --policy-file target to be write-protected")
	}
	if _, err := eng.ResolveAndCheckWrite(ws, mcpFile); err == nil {
		t.Fatal("expected --mcp-config target to be write-protected")
	}
}

func TestProtectedSystemRootDoesNotBreakNonSystemDrivesOrDockerWorkdir(t *testing.T) {
	if runtime.GOOS == "windows" {
		ws := t.TempDir()
		eng := NewEngineWithOptions(ws, Options{})
		if eng.isProtectedSystemRoot(`D:\bin\myproject\main.go`) {
			t.Fatal("D:\\bin\\myproject should not be treated as a Windows system root")
		}
		if !eng.isProtectedSystemRoot(`C:\Windows\System32\cmd.exe`) {
			t.Fatal("C:\\Windows\\System32 should be protected")
		}
	} else {
		eng := NewEngineWithOptions("/usr/src/app", Options{StrictWorkspace: true})
		if eng.isProtectedSystemRoot("/usr/src/app/main.go") {
			t.Fatal("explicit workspace root /usr/src/app should be writable")
		}
		if !eng.isProtectedSystemRoot("/etc/passwd") {
			t.Fatal("/etc/passwd must remain protected")
		}
	}
}
