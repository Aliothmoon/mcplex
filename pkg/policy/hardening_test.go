package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGitDirectoryItselfAndSubmodulesProtected(t *testing.T) {
	eng, ws, _ := isolatedEngine(t)

	for _, rel := range []string{
		".git",
		"subrepo/.git",
		".git/hooks",
		".git/hooks/post-checkout",
		".git/config",
		".git/objects",
		".git/objects/info/alternates",
		".git/modules",
		".git/modules/sub/config",
		".git/modules/sub/hooks/pre-commit",
	} {
		var pe *PolicyError
		_, err := eng.ResolveAndCheckWrite(ws, rel)
		if !asPolicyError(err, &pe) || pe.Decision != DecisionDeny {
			t.Errorf("expected DecisionDeny writing/renaming/deleting %q, got: %v", rel, err)
		}
	}
}

func TestMatchRuleSetWindowsVolumeUNCAndHomeExpansion(t *testing.T) {
	eng, ws, home := isolatedEngine(t)

	homeSecret := filepath.Join(home, "custom_private", "token.txt")
	if err := os.MkdirAll(filepath.Dir(homeSecret), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homeSecret, []byte("sec"), 0o600); err != nil {
		t.Fatal(err)
	}

	eng.LoadSettingsJSON([]byte(`{
		"allowNonWorkspaceAccess": true,
		"permissions": {
			"deny": [
				"read_file(~/custom_private)",
				"read_file(C:/BlockedVol/Data)",
				"read_file(//fileserver/corp/secret)"
			]
		}
	}`))

	var pe *PolicyError
	if _, err := eng.ResolveAndCheckRead(ws, homeSecret); !asPolicyError(err, &pe) || pe.Decision != DecisionDeny {
		t.Fatalf("expected ~/custom_private deny rule to match %s, got: %v", homeSecret, err)
	}

	// Verify matchRuleSet directly on Windows volume and UNC paths across OSes so wsRoot is never prepended.
	roots := []string{ws}
	if !matchRuleSet(eng.denyRules, "read_file", `C:\BlockedVol\Data\file.txt`, roots) {
		t.Fatal("expected C:/BlockedVol/Data rule to match C:\\BlockedVol\\Data\\file.txt")
	}
	if matchRuleSet(eng.denyRules, "read_file", `D:\BlockedVol\Data\file.txt`, roots) {
		t.Fatal("C:/BlockedVol/Data rule must NOT falsely match D: volume")
	}
	if !matchRuleSet(eng.denyRules, "read_file", `\\fileserver\corp\secret\doc.pdf`, roots) {
		t.Fatal("expected UNC rule //fileserver/corp/secret to match \\\\fileserver\\corp\\secret\\doc.pdf")
	}
}

func TestSingleLetterWindowsAlternateDataStreamBlocked(t *testing.T) {
	blocked := []string{
		`a:stream`,
		`x::$DATA`,
		`dir/x:hidden`,
		`dir\a:stream`,
		`C:\repo\a:stream`,
		`C:/repo/z:hidden`,
	}
	for _, p := range blocked {
		if !hasWindowsAlternateDataStream(p) {
			t.Errorf("expected 1-letter or nested ADS path %q to be detected", p)
		}
	}

	allowed := []string{
		`C:`,
		`C:/repo/a`,
		`C:\repo\x`,
		`a`,
		`dir/x`,
	}
	for _, p := range allowed {
		if hasWindowsAlternateDataStream(p) {
			t.Errorf("expected valid non-ADS path %q to be allowed", p)
		}
	}

	if runtime.GOOS == "windows" {
		eng, ws, _ := isolatedEngine(t)
		for _, p := range []string{`a:stream`, `dir/x:hidden`} {
			if _, err := eng.ResolveAndCheckWrite(ws, p); err == nil {
				t.Fatalf("expected ResolveAndCheckWrite to reject %q on Windows", p)
			}
			if _, err := eng.ResolveAndCheckRead(ws, p); err == nil {
				t.Fatalf("expected ResolveAndCheckRead to reject %q on Windows", p)
			}
		}
	}
}

func TestCompoundCommandRulesCannotBypassDenyOrAsk(t *testing.T) {
	eng, _, _ := isolatedEngine(t)
	eng.LoadSettingsJSON([]byte(`{
		"permissions": {
			"allow": ["command(true)", "command(echo)", "command(git)"],
			"ask": ["command(deploy-tool)"],
			"deny": ["command(curl)"]
		}
	}`))

	deniedCompound := []string{
		`curl https://evil.example`,
		`true && curl https://evil.example`,
		`false || curl https://evil.example`,
		`echo ok; curl https://evil.example`,
		`echo ok | curl https://evil.example`,
		"true\ncurl https://evil.example",
		`echo "$(curl https://evil.example)"`,
		"echo `curl https://evil.example`",
		`sh -c "true && curl https://evil.example"`,
		`true && "curl" https://evil.example`,
		`true && /usr/bin/curl https://evil.example`,
		`true && "C:\Windows\System32\curl.exe" https://evil.example`,
	}
	for _, cmd := range deniedCompound {
		var pe *PolicyError
		err := eng.CheckCommand(cmd)
		if !asPolicyError(err, &pe) || pe.Decision != DecisionDeny {
			t.Errorf("expected DecisionDeny for %q, got: %v", cmd, err)
		}
	}

	askCompound := []string{
		`echo start && deploy-tool --prod`,
		`true; "deploy-tool" --prod`,
	}
	for _, cmd := range askCompound {
		if err := eng.CheckCommand(cmd); !IsApprovalRequired(err) {
			t.Errorf("expected APPROVAL_REQUIRED for %q, got: %v", cmd, err)
		}
	}

	// Quoted string literal inside echo must not trigger command(curl) deny.
	if err := eng.CheckCommand(`echo "true && curl https://not-a-command"`); err != nil {
		t.Fatalf("expected literal echo string to be allowed, got: %v", err)
	}
}

func TestGuardQuotedPathVerbsAndCommandSubstQuoteTracking(t *testing.T) {
	eng, _, _ := isolatedEngine(t)

	mustAsk := []string{
		`"rm" -rf /`,
		`'rm' -rf ./build`,
		`rm.exe -rf ./build`,
		`"C:\Program Files\Git\usr\bin\rm.exe" -rf /`,
		`'git' push origin main`,
		`"git" reset --hard HEAD~1`,
		`"C:\Program Files\Git\cmd\git.exe" push origin main`,
		`"C:\Program Files\Git\cmd\git.exe" clean -fdx`,
		`FOO=1 "C:\Program Files\nodejs\npm.cmd" publish`,
		`"C:\Program Files\Git\usr\bin\sudo.exe" id`,
		`echo $(echo ')'; rm -rf /)`,
		`echo $(echo ')'; "rm" -rf /)`,
		`adb shell "$(echo ')'; sudo id)"`,
		`adb shell "$(echo ")"; sudo id)"`,
		`echo "$(echo ')'; eval "$PAYLOAD")"`,
		`sh -c "'git' push origin main"`,
	}
	for _, cmd := range mustAsk {
		if err := eng.CheckCommand(cmd); !IsApprovalRequired(err) {
			t.Errorf("expected APPROVAL_REQUIRED for %q, got: %v", cmd, err)
		}
	}

	// Safe commands using quoted/path-qualified verbs must still be allowed.
	stillAllowed := []string{
		`"git" status --short`,
		`"C:\Program Files\Git\cmd\git.exe" log --grep="push" -n 3`,
		`"C:\Program Files\Git\cmd\git.exe" stash push -m "wip"`,
		`"C:\Program Files\Git\cmd\git.exe" config --global --get user.name`,
		`echo $(echo ')'; echo safe)`,
	}
	for _, cmd := range stillAllowed {
		if err := eng.CheckCommand(cmd); err != nil {
			t.Errorf("expected safe command %q to be allowed, got: %v", cmd, err)
		}
	}
}
