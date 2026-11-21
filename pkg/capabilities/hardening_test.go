package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func TestShellExecWindowsCmdRawQuotesAndErrWaitDelay(t *testing.T) {
	ws := t.TempDir()
	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	binder := &ShellGitBinder{Policy: pol}

	if runtime.GOOS == "windows" {
		res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
			Code:    `const r = await sh.exec('echo "a b"', { shell: "cmd" }); return r;`,
			Cwd:     ws,
			Timeout: 10 * time.Second,
		}, binder)
		if err != nil {
			t.Fatalf("sh.exec(cmd) failed: %v", err)
		}
		m, _ := res.Value.(map[string]any)
		if m["ok"] != true || strings.TrimSpace(fmt.Sprint(m["output"])) != `"a b"` {
			t.Fatalf(`expected sh.exec cmd output "a b", got %+v`, m)
		}
	}

	// ErrWaitDelay when main process exits 0 while grandchild holds pipe must report ok:true, exitCode:0, timedOut:false.
	var bgCmd string
	if runtime.GOOS == "windows" {
		bgCmd = `Start-Process -NoNewWindow powershell -ArgumentList '-NoProfile','-Command','Start-Sleep -Milliseconds 1200'; Write-Output 'WAITDELAY_OK'; exit 0`
	} else {
		bgCmd = `(sleep 1.2) & echo WAITDELAY_OK; exit 0`
	}
	res, err := binder.RunSingleCommandDuration(context.Background(), ws, bgCmd, 5*time.Second, true, false)
	if err != nil {
		t.Fatalf("RunSingleCommandDuration failed: %v", err)
	}
	if res["ok"] != true || res["exitCode"] != 0 || res["timedOut"] != false {
		t.Fatalf("expected ok=true exitCode=0 timedOut=false on ErrWaitDelay, got %+v", res)
	}

	// Also verify sh.run (RunArgv) on ErrWaitDelay with exitCode 0.
	env := &sandbox.ExecutionEnv{Cwd: ws}
	var runRes map[string]any
	if runtime.GOOS == "windows" {
		runRes, err = binder.RunArgv(context.Background(), env, ws, "powershell", []string{"-NoProfile", "-Command", bgCmd}, nil)
	} else {
		runRes, err = binder.RunArgv(context.Background(), env, ws, "sh", []string{"-c", bgCmd}, nil)
	}
	if err != nil {
		t.Fatalf("RunArgv failed on ErrWaitDelay: %v", err)
	}
	if runRes["ok"] != true || runRes["exitCode"] != 0 || runRes["timedOut"] != false {
		t.Fatalf("expected RunArgv ok=true exitCode=0 timedOut=false, got %+v", runRes)
	}
}

func TestFsCpRejectsSelfDescendantAndEnforcesWalkReadPolicyAndSymlinks(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	outsideSecret := filepath.Join(outside, "secret.txt")
	_ = os.WriteFile(outsideSecret, []byte("TOP_SECRET"), 0o600)

	srcDir := filepath.Join(ws, "src")
	deniedSub := filepath.Join(srcDir, "denied_dir")
	_ = os.MkdirAll(deniedSub, 0o755)
	_ = os.WriteFile(filepath.Join(srcDir, "ok.txt"), []byte("hello"), 0o644)

	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	// Note: t.TempDir() lives under os.TempDir() (allowed scratch), so explicitly deny outside to test read-policy enforcement on symlink targets!
	pol.LoadSettingsJSON([]byte(fmt.Sprintf(`{"permissions":{"deny":["read_file(src/denied_dir/)","read_file(%s)"]}}`, filepath.ToSlash(outside))))
	fsb := &FSSearchBinder{Policy: pol, Locks: NewFileLockTable()}

	// 1. Reject copying directory into itself or its own descendant.
	for _, code := range []string{
		`await fs.cp("src", "src", { recursive: true });`,
		`await fs.cp("src", "src/sub/copy", { recursive: true });`,
	} {
		if _, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{Code: code, Cwd: ws, Timeout: 5 * time.Second}, fsb); err == nil {
			t.Fatalf("expected self/descendant fs.cp to fail for: %s", code)
		}
	}

	// 2. Reject copying directory containing read-denied entry.
	_ = os.WriteFile(filepath.Join(deniedSub, "priv.txt"), []byte("nope"), 0o600)
	if _, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `await fs.cp("src", "dst_denied", { recursive: true });`, Cwd: ws, Timeout: 5 * time.Second,
	}, fsb); err == nil {
		t.Fatal("expected fs.cp to enforce ResolveAndCheckRead on nested denied directory")
	}
	_ = os.RemoveAll(deniedSub)

	// 3. Reject symlink inside src pointing outside workspace read policy.
	symLink := filepath.Join(srcDir, "escape.txt")
	if err := os.Symlink(outsideSecret, symLink); err == nil {
		if _, cpErr := sandbox.Execute(context.Background(), sandbox.ExecRequest{
			Code: `await fs.cp("src", "dst_sym", { recursive: true });`, Cwd: ws, Timeout: 5 * time.Second,
		}, fsb); cpErr == nil {
			t.Fatal("expected fs.cp to refuse symlink pointing outside allowed read policy")
		}
		_ = os.Remove(symLink)
	}

	// 4. Valid recursive copy succeeds.
	if _, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `await fs.cp("src", "dst_ok", { recursive: true });`, Cwd: ws, Timeout: 5 * time.Second,
	}, fsb); err != nil {
		t.Fatalf("expected clean fs.cp to succeed, got: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, "dst_ok", "ok.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("copied file mismatch: got=%q err=%v", string(got), err)
	}
}

func TestWriteFileForRollbackNeverOverwritesExternalSymlinkAndRestoresReadOnly(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("EXTERNAL_INTACT"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Read-only regular file restoration (critical on Windows).
	roFile := filepath.Join(ws, "readonly.txt")
	if err := os.WriteFile(roFile, []byte("modified"), 0o444); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(roFile, 0o444)
	if err := writeFileForRollback(roFile, []byte("restored-ro"), 0o644); err != nil {
		t.Fatalf("writeFileForRollback failed on read-only file: %v", err)
	}
	if b, _ := os.ReadFile(roFile); string(b) != "restored-ro" {
		t.Fatalf("expected restored-ro, got %q", string(b))
	}

	// 2. Symlink replacement attack: rollback must remove symlink and NOT write through to victim.
	workFile := filepath.Join(ws, "work.txt")
	if err := os.Symlink(victim, workFile); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	if err := writeFileForRollback(workFile, []byte("SNAPSHOT_BYTES"), 0o644); err != nil {
		t.Fatalf("writeFileForRollback failed on symlinked target: %v", err)
	}
	if vBytes, _ := os.ReadFile(victim); string(vBytes) != "EXTERNAL_INTACT" {
		t.Fatalf("CRITICAL: rollback overwrote external symlink target! got %q", string(vBytes))
	}
	fi, err := os.Lstat(workFile)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected workFile to be restored as a regular file, mode=%v err=%v", fi.Mode(), err)
	}
	if wBytes, _ := os.ReadFile(workFile); string(wBytes) != "SNAPSHOT_BYTES" {
		t.Fatalf("expected SNAPSHOT_BYTES in restored regular file, got %q", string(wBytes))
	}
}

func TestReadOnlyGitBlocksEscapeFlagsDisablesFsmonitorAndChecksPaths(t *testing.T) {
	// 1. Blocked escape & helper flags.
	for _, bad := range [][]string{
		{"diff", "--no-index", "/etc/passwd", "/dev/null"},
		{"blame", "--contents=/etc/passwd", "a.go"},
		{"blame", "--contents", "/etc/passwd", "a.go"},
		{"diff", "-O", "/etc/passwd"},
		{"diff", "-O/etc/passwd"},
		{"diff", "--textconv"},
		{"show", "--ext-diff"},
		{"log", "--output=/tmp/leak"},
		{"diff", "--output", "/tmp/leak"},
	} {
		if err := validateReadOnlyGitArgs(bad); err == nil {
			t.Errorf("expected validateReadOnlyGitArgs to block %v", bad)
		}
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not installed")
	}

	// 2. Real git repository integration test (verifies -c core.fsmonitor=false and --no-ext-diff/--no-textconv work on real git!).
	repo := t.TempDir()
	runGitInit := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	runGitInit("init")
	runGitInit("config", "user.email", "test@example.com")
	runGitInit("config", "user.name", "Test")

	_ = os.WriteFile(filepath.Join(repo, "pub.txt"), []byte("public line\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(repo, "vault"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, "vault", "secret.txt"), []byte("secret token\n"), 0o644)
	runGitInit("add", ".")
	runGitInit("commit", "-m", "init commit")

	// Configure a malicious fsmonitor command in .git/config that creates a marker file if ever executed!
	marker := filepath.Join(repo, "FSMONITOR_PWNED")
	runGitInit("config", "core.fsmonitor", fmt.Sprintf("touch %q", filepath.ToSlash(marker)))

	pol := policy.NewEngineWithOptions(repo, policy.Options{StrictWorkspace: true})
	pol.LoadSettingsJSON([]byte(`{"permissions":{"deny":["read_file(vault/)"]}}`))
	binder := &ShellGitBinder{Policy: pol}

	code := `
		const st = await git.status();
		const lg = await git.log({ n: 1 });
		const shw = await git.show({ stat: true });
		const df = await git.diff({ path: "pub.txt" });
		const bl = await git.blame("pub.txt");
		const rev = await git.query("rev-parse HEAD");
		return { isRepo: st.isRepo, lg, shw, df, bl, rev };
	`
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: code, Cwd: repo, Timeout: 15 * time.Second,
	}, binder)
	if err != nil {
		t.Fatalf("real git read-only suite failed: %v", err)
	}
	m, _ := res.Value.(map[string]any)
	if m["isRepo"] != true || strings.TrimSpace(fmt.Sprint(m["rev"])) == "" {
		t.Fatalf("unexpected git results: %+v", m)
	}
	if _, mErr := os.Stat(marker); !os.IsNotExist(mErr) {
		t.Fatal("CRITICAL: git core.fsmonitor executed during read-only git.*!")
	}

	// 3. Policy-denied path arguments must be rejected across git.blame, git.diff, git.log, and git.query.
	deniedScripts := []string{
		`await git.blame("vault/secret.txt");`,
		`await git.diff({ path: "vault/secret.txt" });`,
		`await git.log({ path: "vault/secret.txt" });`,
		`await git.query("log -n 1 -- vault/secret.txt");`,
		`await git.query("blame vault/secret.txt");`,
		`await git.query("show HEAD:vault/secret.txt");`,
	}
	for _, sc := range deniedScripts {
		if _, gErr := sandbox.Execute(context.Background(), sandbox.ExecRequest{
			Code: sc, Cwd: repo, Timeout: 5 * time.Second,
		}, binder); gErr == nil {
			t.Errorf("expected read policy to block: %s", sc)
		}
	}
}

func TestASTLineDirectiveNoPanicGlobBraceCapAndTreePolicy(t *testing.T) {
	ws := t.TempDir()
	genGo := strings.Join([]string{
		"package gen",
		"//line parser.y:99999",
		"func Generated() int {",
		"\treturn 42",
		"}",
	}, "\n")
	if err := os.WriteFile(filepath.Join(ws, "gen.go"), []byte(genGo), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(ws, "vault"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "vault", "key.pem"), []byte("SECRET"), 0o600)
	_ = os.MkdirAll(filepath.Join(ws, "visible"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "visible", "ok.md"), []byte("# ok"), 0o644)

	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	pol.LoadSettingsJSON([]byte(`{"permissions":{"deny":["read_file(vault/)"]}}`))
	astBinder := &ASTBinder{Policy: pol}
	fsBinder := &FSSearchBinder{Policy: pol}

	// 1. ast.read on file with //line directive must not panic and must return exact physical lines.
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code:    `return await ast.read("gen.go", "Generated");`,
		Cwd:     ws,
		Timeout: 5 * time.Second,
	}, astBinder)
	if err != nil {
		t.Fatalf("ast.read on //line Go file failed: %v", err)
	}
	symMap, _ := res.Value.(map[string]any)
	if !strings.Contains(fmt.Sprint(symMap["text"]), "return 42") {
		t.Fatalf("unexpected ast.read body: %+v", symMap)
	}

	// 2. search.glob combinatorial brace explosion (20x {a,b}) must fail fast with KindBadArgs.
	boomPat := strings.Repeat("{a,b}", 20) + ".go"
	_, _, _, globErr := fsBinder.runGlobSearch(context.Background(), ws, ws, boomPat, 50)
	var he *sandbox.HostError
	if !errors.As(globErr, &he) || he.Kind != sandbox.KindBadArgs {
		t.Fatalf("expected BadArgs HostError on 20x {a,b} glob DoS, got: %v", globErr)
	}

	// Valid brace glob still works via precompiled matcher.
	matched, _, _, err := fsBinder.runGlobSearch(context.Background(), ws, ws, "**/*.{go,md}", 50)
	if err != nil || len(matched) != 2 {
		t.Fatalf("expected 2 matched files (gen.go, visible/ok.md), got %v (err=%v)", matched, err)
	}

	// 3. fs.tree must enforce per-entry read policy (skipping vault/).
	treeRes, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code:    `return await fs.tree(".", 3);`,
		Cwd:     ws,
		Timeout: 5 * time.Second,
	}, fsBinder)
	if err != nil {
		t.Fatalf("fs.tree failed: %v", err)
	}
	treeStr := fmt.Sprint(treeRes.Value)
	if strings.Contains(treeStr, "vault") || strings.Contains(treeStr, "key.pem") {
		t.Fatalf("fs.tree leaked policy-denied directory: %s", treeStr)
	}
	if !strings.Contains(treeStr, "visible/") || !strings.Contains(treeStr, "visible/ok.md") {
		t.Fatalf("fs.tree missing allowed entries: %s", treeStr)
	}
}

func TestTxCaseInsensitiveSnapshotsMidBatchUndoBacktickIndentAndEditVerify(t *testing.T) {
	ws := t.TempDir()
	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	locks := NewFileLockTable()
	shellBinder := &ShellGitBinder{Policy: pol}
	editBinder := &EditTxBinder{Policy: pol, Locks: locks, ShellRunner: shellBinder}

	// 1. Case-insensitive snapshot key on Windows/macOS: editing App.go then app.go and rolling back restores original v0.
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		appPath := filepath.Join(ws, "App.go")
		_ = os.WriteFile(appPath, []byte("package main\nconst v = \"v0\"\n"), 0o644)
		_, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
			Code: `
				const t = await tx.begin();
				await t.edit("App.go", [{ target: '"v0"', replacement: '"v1"' }]);
				await t.edit("app.go", [{ target: '"v1"', replacement: '"v2"' }]);
				await t.rollback();
			`,
			Cwd:     ws,
			Timeout: 5 * time.Second,
		}, editBinder)
		if err != nil {
			t.Fatalf("case-insensitive tx test failed: %v", err)
		}
		got, _ := os.ReadFile(appPath)
		if !strings.Contains(string(got), `"v0"`) {
			t.Fatalf("expected rollback to restore original v0 across App.go/app.go, got: %q", string(got))
		}
	}

	// 2. Mid-batch t.edit failure restores files 0..K-1 to pre-call bytes immediately (before rollback!).
	f0 := filepath.Join(ws, "first.txt")
	f1 := filepath.Join(ws, "second.txt")
	_ = os.WriteFile(f0, []byte("clean-0\n"), 0o644)
	_ = os.WriteFile(f1, []byte("clean-1\n"), 0o644)

	txState := &activeTx{id: "tx_mid", lockOwner: "owner-mid", snapshots: map[string]fileSnapshot{}}
	// Plan manually or trigger write error on f1 by making f1 a directory right before write?
	// On Unix 0o444 causes writeFileWithMode to fail with EACCES; on all OSes we can test applyFileSpecs mid-batch undo
	// by making f1 a read-only file on Unix, or locking/replacing on Windows. Let's test via a directory blocker after planning
	// or read-only on Unix; to make it 100% deterministic on Windows + Linux + macOS, create a file `second.txt` whose parent or
	// target is blocked on Windows via an open exclusive file handle or test Unix 0o444 + cross-OS mid-batch helper!
	if runtime.GOOS != "windows" {
		_ = os.Chmod(f1, 0o444)
		defer os.Chmod(f1, 0o644)
		_, batchErr := editBinder.applyFileSpecs(context.Background(), ws, []FileEditSpec{
			{Path: "first.txt", Hunks: []EditHunk{{Target: "clean-0", Replacement: "dirty-0"}}},
			{Path: "second.txt", Hunks: []EditHunk{{Target: "clean-1", Replacement: "dirty-1"}}},
		}, false, false, txState)
		if batchErr == nil {
			t.Fatal("expected mid-batch t.edit write failure on read-only second.txt")
		}
		if b0, _ := os.ReadFile(f0); string(b0) != "clean-0\n" {
			t.Fatalf("expected first.txt restored immediately on mid-batch t.edit failure, got %q", string(b0))
		}
	} else {
		// On Windows, setting FILE_ATTRIBUTE_READONLY (0o444) causes os.Rename / os.WriteFile in writeFileWithMode
		// (allowOverwriteReadOnly=false) to fail with Access Denied!
		_ = os.Chmod(f1, 0o444)
		defer os.Chmod(f1, 0o666)
		_, batchErr := editBinder.applyFileSpecs(context.Background(), ws, []FileEditSpec{
			{Path: "first.txt", Hunks: []EditHunk{{Target: "clean-0", Replacement: "dirty-0"}}},
			{Path: "second.txt", Hunks: []EditHunk{{Target: "clean-1", Replacement: "dirty-1"}}},
		}, false, false, txState)
		if batchErr == nil {
			t.Fatal("expected mid-batch t.edit write failure on read-only second.txt on Windows")
		}
		if b0, _ := os.ReadFile(f0); string(b0) != "clean-0\n" {
			t.Fatalf("expected first.txt restored immediately on mid-batch t.edit failure, got %q", string(b0))
		}
	}

	// 3. Multiline backtick raw string interior must NOT be re-indented or space-collapsed,
	// and whitespace-only matched[0].raw must NOT prepend leading spaces onto line 0.
	btFile := filepath.Join(ws, "raw.go")
	btSrc := strings.Join([]string{
		"package demo",
		"    ", // line 2: whitespace-only line!
		"func query() string {",
		"\tq := `SELECT *",
		"  FROM   users",
		"  WHERE  id = 1`",
		"\treturn q",
		"}",
		"",
	}, "\n")
	_ = os.WriteFile(btFile, []byte(btSrc), 0o644)

	// 3a: Differing spaces INSIDE multiline backtick (`FROM users` vs `FROM   users`) must NOT fuzzy-match.
	if _, err := planSingleFile(btFile, "raw.go", FileEditSpec{
		Path: "raw.go",
		Hunks: []EditHunk{{
			Target:      "\tq := `SELECT *\n  FROM users\n  WHERE  id = 1`\n\treturn q",
			Replacement: "\treturn \"\"",
		}},
	}); err == nil {
		t.Fatal("spaces inside multiline backtick string must NOT be collapsed during fuzzy match")
	}

	// 3b: Over-indented outer code lines (2 tabs vs 1 tab) realign code lines WITHOUT touching raw string interior!
	pw, err := planSingleFile(btFile, "raw.go", FileEditSpec{
		Path: "raw.go",
		Hunks: []EditHunk{{
			Target:      "\t\tq := `SELECT *\n  FROM   users\n  WHERE  id = 1`\n\t\treturn q",
			Replacement: "\t\tq := `SELECT id\n  FROM   users\n  WHERE  id = 1`\n\t\treturn q",
		}},
	})
	if err != nil {
		t.Fatalf("expected over-indented hunk with exact raw string interior to match: %v", err)
	}
	if !strings.Contains(string(pw.newBytes), "\tq := `SELECT id\n  FROM   users\n  WHERE  id = 1`\n\treturn q") {
		t.Fatalf("raw string interior was corrupted or code indent not realigned:\n%s", string(pw.newBytes))
	}

	// 3c: Whitespace-only matched[0].raw guard.
	pwWS, err := planSingleFile(btFile, "raw.go", FileEditSpec{
		Path: "raw.go",
		Hunks: []EditHunk{{
			Target:      "\nfunc  query() string {",
			Replacement: "// doc\nfunc query() string {",
		}},
	})
	if err != nil {
		t.Fatalf("whitespace-only line 0 hunk failed: %v", err)
	}
	if strings.Contains(string(pwWS.newBytes), "    // doc") {
		t.Fatalf("whitespace-only matched[0].raw polluted replacement line 0 indent:\n%s", string(pwWS.newBytes))
	}

	// 4. edit.apply with failing verify sets top-level ok: false.
	vFile := filepath.Join(ws, "ver.txt")
	_ = os.WriteFile(vFile, []byte("hello\n"), 0o644)
	applyRes, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			return await edit.apply({
				path: "ver.txt",
				hunks: [{ target: "hello", replacement: "world" }],
				verify: "exit 1"
			});
		`,
		Cwd:     ws,
		Timeout: 10 * time.Second,
	}, editBinder)
	if err != nil {
		t.Fatalf("edit.apply returned unexpected error: %v", err)
	}
	resMap, _ := applyRes.Value.(map[string]any)
	if resMap["ok"] != false {
		t.Fatalf("expected top-level ok=false when edit.apply verify fails, got: %+v", resMap)
	}
}

func TestStateStoreSessionScopedSpillsAndLinuxCaseHash(t *testing.T) {
	// 1. OS-aware workspace hash casing.
	kLinuxUpper := normalizeWorkspaceHashKeyForOS("/Work/App", "linux")
	kLinuxLower := normalizeWorkspaceHashKeyForOS("/work/app", "linux")
	if kLinuxUpper == kLinuxLower {
		t.Fatal("linux workspace hash key must preserve path case")
	}
	if normalizeWorkspaceHashKeyForOS(`C:\Work\App`, "windows") != normalizeWorkspaceHashKeyForOS(`c:\work\app`, "windows") {
		t.Fatal("windows workspace hash key must fold case")
	}
	if normalizeWorkspaceHashKeyForOS("/Users/Me/App", "darwin") != normalizeWorkspaceHashKeyForOS("/users/me/app", "darwin") {
		t.Fatal("darwin workspace hash key must fold case")
	}

	// 2. Session-scoped spill isolation across concurrent HTTP sessions.
	store := NewStateStore("", "")
	if err := store.Set("shared_cfg", "v1"); err != nil {
		t.Fatal(err)
	}

	ka := store.SaveAutoSpillForSession("sess-A", "alpha-secret-spill")
	kb := store.SaveAutoSpillForSession("sess-B", "bravo-secret-spill")
	kd := store.SaveAutoSpill("default-spill")

	if got, res, ok := store.GetResolvedForSession("sess-A", "__last_result"); !ok || res != ka || got != "alpha-secret-spill" {
		t.Fatalf("sess-A __last_result mismatch: ok=%v res=%s got=%v", ok, res, got)
	}
	if got, res, ok := store.GetResolvedForSession("sess-B", "__last_result"); !ok || res != kb || got != "bravo-secret-spill" {
		t.Fatalf("sess-B __last_result mismatch: ok=%v res=%s got=%v", ok, res, got)
	}
	if got, res, ok := store.GetResolved("__last_result"); !ok || res != kd || got != "default-spill" {
		t.Fatalf("default __last_result mismatch: ok=%v res=%s got=%v", ok, res, got)
	}

	// StateBinder with SessionID="sess-A" sees sess-A's spill & shared user KV, and clearing/closing sess-B does not affect sess-A!
	resA, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			return {
				shared: state.get("shared_cfg"),
				last: state.get("__last_result"),
				keys: state.keys(),
				page1: state.page(1),
			};
		`,
		Cwd:     t.TempDir(),
		Timeout: 5 * time.Second,
	}, &StateBinder{Store: store, SessionID: "sess-A"})
	if err != nil {
		t.Fatalf("StateBinder sess-A exec failed: %v", err)
	}
	mA, _ := resA.Value.(map[string]any)
	if mA["shared"] != "v1" || mA["last"] != "alpha-secret-spill" || !strings.Contains(fmt.Sprint(mA["page1"]), "alpha-secret-spill") {
		t.Fatalf("unexpected StateBinder sess-A result: %+v", mA)
	}

	store.ClearSessionSpills("sess-B")
	if _, _, ok := store.GetResolvedForSession("sess-B", "__last_result"); ok {
		t.Fatal("expected sess-B spills cleared")
	}
	if got, _, ok := store.GetResolvedForSession("sess-A", "__last_result"); !ok || got != "alpha-secret-spill" {
		t.Fatal("clearing sess-B spills must NOT affect sess-A spills")
	}
	if !slices.Contains(store.KeysForSession("sess-A"), "shared_cfg") {
		t.Fatal("ClearSessionSpills must NOT remove shared user KV")
	}
}

func TestMCPBridgePolicyOrderTimeoutRedirectsSSENotificationsPerServerLockAndStdioCancel(t *testing.T) {
	ws := t.TempDir()

	// 1. CheckMCP runs BEFORE resolveCachedToolName calls live discoverServerTools!
	var deniedServerHit atomic.Bool
	deniedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deniedServerHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer deniedSrv.Close()

	// Also test exactTool re-check: allow server generally, deny hyphenated `resolve-library-id`, call via `resolve_library_id`.
	var hyphenCallHit atomic.Bool
	hyphenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-11-25"}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{{"name": "resolve-library-id", "description": "demo"}},
				},
			})
		case "tools/call":
			hyphenCallHit.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		}
	}))
	defer hyphenSrv.Close()

	// Cross-host redirect target: must NOT receive Authorization or X-Secret-Token!
	var leakedAuthHeader atomic.Bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Secret-Token") != "" || r.Header.Get("Mcp-Session-Id") != "" {
			leakedAuthHeader.Store(true)
		}
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Respond with SSE containing `: ping` comment + trailing notification to test SSE parser too!
		w.Header().Set("Content-Type", "text/event-stream")
		idJSON, _ := json.Marshal(req.ID)
		_, _ = fmt.Fprintf(w, ": keepalive comment\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"step\":1}}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"sse-redirect-ok\"}]}}\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"msg\":\"trailing-notif\"}}\n\n", string(idJSON))
	}))
	defer redirectTarget.Close()

	redirectOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 307 preserves POST body across redirect while changing host:port!
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer redirectOrigin.Close()

	// Slow server for timeoutSeconds=1 test.
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-11-25"}})
			return
		}
		time.Sleep(2500 * time.Millisecond)
	}))
	defer slowSrv.Close()

	mcpCfgPath := filepath.Join(ws, "mcp.json")
	cfgJSON, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"blocked_srv": map[string]any{"url": deniedSrv.URL},
			"hyphen_srv":  map[string]any{"url": hyphenSrv.URL},
			"redir_srv": map[string]any{
				"url": redirectOrigin.URL,
				"headers": map[string]string{
					"Authorization":  "Bearer super-secret",
					"X-Secret-Token": "custom-secret",
				},
			},
			"slow_srv": map[string]any{
				"url":            slowSrv.URL,
				"timeoutSeconds": 1,
			},
		},
	})
	_ = os.WriteFile(mcpCfgPath, cfgJSON, 0o600)

	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	pol.LoadSettingsJSON([]byte(`{
		"permissions": {
			"deny": ["mcp(blocked_srv/*)", "mcp(hyphen_srv/resolve-library-id)"]
		}
	}`))
	br := NewMCPBridgeWithOptions(pol, ws, MCPBridgeOptions{ConfigPaths: []string{mcpCfgPath}})
	defer br.Close()

	// 1a: Denied server with underscore toolHint must NEVER trigger live tools/list!
	if _, err := br.CallExternalTool(context.Background(), ws, "blocked_srv/some_underscore_tool", nil); err == nil {
		t.Fatal("expected blocked_srv call to be denied")
	}
	if deniedServerHit.Load() {
		t.Fatal("CRITICAL: denied server was contacted for live tools/list before CheckMCP!")
	}

	// 1b: Re-check after resolving underscore -> hyphenated tool name!
	if _, err := br.CallExternalTool(context.Background(), ws, "hyphen_srv/resolve_library_id", nil); err == nil {
		t.Fatal("expected hyphen_srv/resolve-library-id to be denied on post-resolution CheckMCP")
	}
	if hyphenCallHit.Load() {
		t.Fatal("CRITICAL: denied exactTool executed tools/call!")
	}

	// 2: Cross-host redirect strips Authorization/custom headers AND SSE parser skips `: comment` + trailing notification!
	redirOut, err := br.CallExternalTool(context.Background(), ws, "redir_srv/ping", map[string]any{})
	if err != nil {
		t.Fatalf("redir_srv SSE call failed: %v", err)
	}
	if leakedAuthHeader.Load() {
		t.Fatal("CRITICAL: Authorization or custom header leaked across cross-host HTTP redirect!")
	}
	if rm, _ := redirOut.(map[string]any); rm["text"] != "sse-redirect-ok" {
		t.Fatalf("expected sse-redirect-ok from matching JSON-RPC ID frame, got %+v", redirOut)
	}

	// 3: timeoutSeconds clamping & enforcement.
	if d := resolveMCPServerTimeout(0); d != 25*time.Second {
		t.Fatalf("expected default 25s, got %v", d)
	}
	if d := resolveMCPServerTimeout(9999); d != 300*time.Second {
		t.Fatalf("expected 300s max clamp, got %v", d)
	}
	start := time.Now()
	if _, err := br.CallExternalTool(context.Background(), ws, "slow_srv/hang", nil); err == nil {
		t.Fatal("expected slow_srv with timeoutSeconds=1 to time out")
	}
	if elapsed := time.Since(start); elapsed > 2200*time.Millisecond {
		t.Fatalf("expected timeoutSeconds=1 to abort in ~1s, took %v", elapsed)
	}

	// 4: Per-server stdio lock: a blocked server A slot must NEVER block server B in getOrStartStdio!
	slotA := &stdioSlot{}
	slotA.mu.Lock()
	br.mu.Lock()
	br.stdioPool["stuck_a"] = slotA
	br.mu.Unlock()

	fastClient := &stdioClient{pending: map[int64]chan []byte{}}
	slotB := &stdioSlot{}
	slotB.client.Store(fastClient)
	br.mu.Lock()
	br.stdioPool["fast_b"] = slotB
	br.mu.Unlock()

	doneB := make(chan error, 1)
	go func() {
		got, gErr := br.getOrStartStdio("fast_b", externalServerCfg{})
		if gErr != nil {
			doneB <- gErr
			return
		}
		if got != fastClient {
			doneB <- errors.New("unexpected client")
			return
		}
		doneB <- nil
	}()
	select {
	case bErr := <-doneB:
		if bErr != nil {
			t.Fatalf("fast_b failed: %v", bErr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("getOrStartStdio on fast_b was blocked by stuck_a's lock!")
	}
	slotA.mu.Unlock()

	// 5: stdioClient ctx.Done() while waiting on c.mu BEFORE writing to stdin must NOT close shared server,
	// and close() must close stdout pipe and unblock readLoop!
	inR, inW := io.Pipe()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer outW.Close()

	sc := &stdioClient{
		stdin:   inW,
		stdout:  outR,
		pending: map[int64]chan []byte{},
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		sc.readLoop(outR)
	}()

	// Hold sc.mu to simulate Caller 1 mid-write while Caller 2's context cancels waiting for sc.mu.
	sc.mu.Lock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel2()
	wErr := sc.writeLineWithTimeout(ctx2, []byte(`{"id":99}`))
	if !errors.Is(wErr, context.DeadlineExceeded) {
		sc.mu.Unlock()
		t.Fatalf("expected context.DeadlineExceeded, got %v", wErr)
	}
	if sc.closed.Load() {
		sc.mu.Unlock()
		t.Fatal("CRITICAL: canceled waiter on c.mu closed the shared stdioClient!")
	}
	sc.mu.Unlock()

	// Verify Caller 2's aborted goroutine never writes anything to inR, while a fresh Caller 3 succeeds!
	gotLine := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := inR.Read(buf)
		gotLine <- string(buf[:n])
	}()
	if err := sc.writeLineWithTimeout(context.Background(), []byte(`{"id":100}`)); err != nil {
		t.Fatalf("subsequent healthy write failed: %v", err)
	}
	if line := <-gotLine; line != "{\"id\":100}\n" {
		t.Fatalf("expected only healthy caller's frame, got %q", line)
	}

	// Verify close() closes stdout (even while outW is still held open!) so readLoop exits promptly.
	var closeWG sync.WaitGroup
	closeWG.Add(1)
	go func() {
		defer closeWG.Done()
		sc.close()
	}()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("sc.close() did not close stdout / unblock readLoop")
	}
	closeWG.Wait()
}
