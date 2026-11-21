package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyWorkspaceConfinementAndGitGuard(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()

	eng := NewEngine(ws)
	eng.LoadSettingsJSON([]byte(`{"allowNonWorkspaceAccess": false}`))

	// Inside workspace write is allowed
	inFile, err := eng.ResolveAndCheckWrite(ws, "internal/app.go")
	if err != nil {
		t.Fatalf("expected workspace write allowed, got: %v", err)
	}
	if !strings.HasSuffix(filepath.ToSlash(inFile), "internal/app.go") {
		t.Fatalf("unexpected resolved path: %s", inFile)
	}

	// Outside workspace write is blocked
	_, err = eng.ResolveAndCheckWrite(ws, filepath.Join(outside, "evil.txt"))
	if err == nil {
		t.Fatal("expected outside-workspace write to be blocked")
	}

	// .git/hooks write inside workspace is blocked unconditionally
	_ = os.MkdirAll(filepath.Join(ws, ".git", "hooks"), 0o755)
	_, err = eng.ResolveAndCheckWrite(ws, ".git/hooks/pre-commit")
	if err == nil {
		t.Fatal("expected .git/hooks write to be denied")
	}
}

func TestPolicySettingsPrecedenceAndDestructiveCommands(t *testing.T) {
	ws := t.TempDir()
	eng := NewEngine(ws)
	eng.LoadSettingsJSON([]byte(`{"allowNonWorkspaceAccess": false}`))
	eng.LoadSettingsJSON([]byte(`{
		"permissions": {
			"allow": ["command(git)", "mcp(zvec_grep/*)"],
			"ask": ["command(git push)"],
			"deny": ["command(rm -rf)", "write_file(secrets/)", "mcp(zvec_grep/drop_index)"]
		}
	}`))

	// Allow rule
	if err := eng.CheckCommand("git status --short"); err != nil {
		t.Fatalf("expected git status allowed, got: %v", err)
	}
	// Ask overrides Allow
	err := eng.CheckCommand("git push origin main")
	if !IsApprovalRequired(err) {
		t.Fatalf("expected Ask rule to require approval on git push, got: %v", err)
	}
	// Destructive built-in guard
	if err := eng.CheckCommand("git reset --hard HEAD~1"); !IsApprovalRequired(err) {
		t.Fatalf("expected git reset --hard to require host approval, got: %v", err)
	}
	// Deny rule on path
	if _, err := eng.ResolveAndCheckWrite(ws, "secrets/api.key"); err == nil {
		t.Fatal("expected write to secrets/ to be denied")
	}
	// Deny rule overrides wildcard Allow on MCP
	if err := eng.CheckMCP("zvec_grep", "drop_index"); err == nil {
		t.Fatal("expected mcp(zvec_grep/drop_index) to be denied")
	}
	if err := eng.CheckMCP("zvec_grep", "zvec_grep_search"); err != nil {
		t.Fatalf("expected zvec_grep_search allowed, got: %v", err)
	}
}

func TestPolicyDesktopPluginDataPolicyOverride(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	pluginData := t.TempDir()
	t.Setenv("PLUGIN_DATA", pluginData)

	policyJSON := `{
		"allowNonWorkspaceAccess": false,
		"permissions": {
			"deny": ["write_file(blocked_dir/)"]
		}
	}`
	if err := os.WriteFile(filepath.Join(pluginData, "policy.json"), []byte(policyJSON), 0o644); err != nil {
		t.Fatalf("write policy.json: %v", err)
	}

	eng := NewEngine(ws)
	if _, err := eng.ResolveAndCheckWrite(ws, filepath.Join(outside, "out.txt")); err == nil {
		t.Fatal("expected outside write blocked by PLUGIN_DATA/policy.json")
	}
	if _, err := eng.ResolveAndCheckWrite(ws, "blocked_dir/file.txt"); err == nil {
		t.Fatal("expected blocked_dir write denied by PLUGIN_DATA/policy.json")
	}
}

func TestPolicyCommandPrivEscQuoteAwareness(t *testing.T) {
	eng := NewEngine(t.TempDir())

	allowed := []string{
		`adb -s 1558416067007OZ shell "getprop ro.product.model; getprop ro.build.version.release; getprop ro.product.cpu.abi; id; su -c id 2>/dev/null || echo NO_ROOT; pm list packages | grep -i -E 'maa|meow'; ps -A | grep -i -E 'maa|meow'; which simpleperf perfetto 2>&1"`,
		`adb -s e867092c shell 'id; su -c id 2>/dev/null || echo NO_ROOT'`,
		`rg "; sudo" internal/policy`,
		`git log --grep="; su -" -n 5`,
		"echo \"line1`nline2\"",
	}
	for _, cmd := range allowed {
		if err := eng.CheckCommand(cmd); err != nil {
			t.Fatalf("expected non-host quoted command allowed, got %v for: %s", err, cmd)
		}
	}

	mustAsk := []string{
		`sudo ls -la`,
		`"sudo" ls -la`,
		`s"u"do ls -la`,
		`echo ok; su -`,
		`echo ok && doas id`,
		`echo ok | sudo tee /tmp/x`,
		`sh -c "sudo id"`,
		`"sh" -c "sudo id"`,
		`sh -c "echo ok; sudo id"`,
		`pwsh -NoProfile -Command "sudo id"`,
		`adb -s 1558416067007OZ shell "$(sudo id)"`,
		"adb -s 1558416067007OZ shell \"`sudo id`\"",
		`adb -s 1558416067007OZ shell "su -c rm -rf /"`,
		`adb -s 1558416067007OZ shell "reboot"`,
	}
	for _, cmd := range mustAsk {
		if err := eng.CheckCommand(cmd); !IsApprovalRequired(err) {
			t.Fatalf("expected APPROVAL_REQUIRED for %q, got: %v", cmd, err)
		}
	}
}
