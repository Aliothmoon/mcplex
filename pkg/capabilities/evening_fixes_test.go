package capabilities

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func TestEveningFixesStashShowStateEvictRelDedupDynamicImportAndIterableHint(t *testing.T) {
	// --- 1. git.query("stash show ...") allowed; stash mutations still rejected ---
	if err := validateReadOnlyGitArgs([]string{"stash", "show", "--stat", "stash@{0}"}); err != nil {
		t.Fatalf("expected 'git stash show --stat stash@{0}' to be allowed, got: %v", err)
	}
	if err := validateReadOnlyGitArgs([]string{"stash", "show", "-p", "-U3", "0"}); err != nil {
		t.Fatalf("expected 'git stash show -p -U3 0' to be allowed, got: %v", err)
	}
	if err := validateReadOnlyGitArgs([]string{"stash", "show", "--include-untracked", "stash@{1}", "--", "pkg/foo.go"}); err != nil {
		t.Fatalf("expected 'git stash show --include-untracked stash@{1} -- pkg/foo.go' to be allowed, got: %v", err)
	}
	for _, mut := range [][]string{
		{"stash"},
		{"stash", "pop"},
		{"stash", "apply", "stash@{0}"},
		{"stash", "drop", "stash@{0}"},
		{"stash", "push", "-m", "wip"},
		{"stash", "clear"},
		{"stash", "show", "--output=/tmp/leak.patch"},
	} {
		if err := validateReadOnlyGitArgs(mut); err == nil {
			t.Fatalf("expected mutating/unsafe git args %v to be rejected", mut)
		}
	}

	if _, lookErr := exec.LookPath("git"); lookErr == nil {
		repo := t.TempDir()
		runGit := func(args ...string) {
			cmd := exec.Command("git", args...)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
				"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
			}
		}
		runGit("init")
		if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "hello.txt")
		runGit("commit", "-m", "init")
		if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("v2-stashed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit("stash", "push", "-m", "wip-stash")

		pol := policy.NewEngineWithOptions(repo, policy.Options{StrictWorkspace: true})
		res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
			Code: `
				const stat = await git.query("stash show --stat stash@{0}");
				const patch = await git.query("stash show -p stash@{0}");
				return { stat, patch };
			`,
			Cwd:     repo,
			Timeout: 10 * time.Second,
		}, &ShellGitBinder{Policy: pol})
		if err != nil {
			t.Fatalf("git.query stash show failed: %v", err)
		}
		m, _ := res.Value.(map[string]any)
		if !strings.Contains(m["stat"].(string), "hello.txt") || !strings.Contains(m["patch"].(string), "+v2-stashed") {
			t.Fatalf("unexpected stash show output: %+v", m)
		}
	}

	// --- 2. StateStore auto-evicts oldest keys when exceeding MaxStateTotalBytes (8 MB) ---
	store := NewStateStore("", "")
	chunk := strings.Repeat("x", 1800*1024) // 1.8 MB per entry (< MaxStateEntryBytes)
	for i := 0; i < 4; i++ {
		if err := store.Set(
			"old_"+string(rune('a'+i)),
			chunk,
		); err != nil {
			t.Fatalf("seed Set %d failed: %v", i, err)
		}
	}
	// Total is ~7.2 MB; writing another 1.8 MB entry (~9.0 MB > 8 MB) must auto-evict `old_a` instead of failing!
	stateRes, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			const r = state.set("stashDiff", P.bigDiff);
			return {
				res: r,
				hasOldA: state.get("old_a") !== null,
				hasOldB: state.get("old_b") !== null,
				hasNew: (state.get("stashDiff") as string).length === P.bigDiff.length,
			};
		`,
		Payloads: map[string]string{"bigDiff": chunk},
		Cwd:      t.TempDir(),
		Timeout:  5 * time.Second,
	}, &StateBinder{Store: store})
	if err != nil {
		t.Fatalf("expected state.set to auto-evict oldest entry instead of failing, got: %v", err)
	}
	sm, _ := stateRes.Value.(map[string]any)
	if sm["hasOldA"] != false || sm["hasOldB"] != true || sm["hasNew"] != true {
		t.Fatalf("unexpected state auto-eviction outcome: %+v (warnings=%v)", sm, stateRes.Warnings)
	}
	if len(stateRes.Warnings) == 0 || !strings.Contains(stateRes.Warnings[0], "auto-evicted") {
		t.Fatalf("expected auto-eviction warning, got: %v", stateRes.Warnings)
	}

	// --- 3. Workspace-relative `<dir>/<dir>/<rest>` deduplication on read (search.glob + `${dir}/${f}`) ---
	ws := t.TempDir()
	relDir := "build/scratch/20261124-run/device"
	fullDir := filepath.Join(ws, filepath.FromSlash(relDir))
	if err := os.MkdirAll(fullDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fullDir, "display.json"), []byte(`{"vm02_diag":"ok"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Also test 1-segment dir in fs.read (`scripts/scripts/run.sh` -> `scripts/run.sh`) while keeping fs.exists("scripts/scripts") false!
	if err := os.MkdirAll(filepath.Join(ws, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "scripts", "run.sh"), []byte("echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	dedupRes, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			const dir = "build/scratch/20261124-run/device";
			const files = await search.glob("**/*", { path: dir });
			const txt = await fs.read(` + "`${dir}/${files[0]}`" + `);
			const existsDupFile = fs.exists(` + "`${dir}/${files[0]}`" + `);
			const existsFakeNestedDir = fs.exists(` + "`${dir}/${dir}`" + `);
			const oneSegTxt = await fs.read("scripts/scripts/run.sh");
			const existsOneSegDir = fs.exists("scripts/scripts");
			return { files, txt, existsDupFile, existsFakeNestedDir, oneSegTxt, existsOneSegDir };
		`,
		Cwd:     ws,
		Timeout: 5 * time.Second,
	}, &FSSearchBinder{Policy: pol})
	if err != nil {
		t.Fatalf("expected workspace-relative repeated prefix read to succeed, got: %v", err)
	}
	dm, _ := dedupRes.Value.(map[string]any)
	if dm["txt"] != `{"vm02_diag":"ok"}` || dm["existsDupFile"] != true || dm["existsFakeNestedDir"] != false ||
		dm["oneSegTxt"] != "echo hi\n" || dm["existsOneSegDir"] != false {
		t.Fatalf("unexpected relative dedup result: %+v", dm)
	}

	// --- 4. Dynamic `await import("node:...")`, `require("node:...")`, `node:module`, `fs.existsSync`, and `object is not iterable` hint ---
	dynRes, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			const nodePath = await import("node:path");
			const { createHash, default: cryptoDef } = await import("node:crypto");
			const nodeFs = await import("node:fs");
			const assertMod = await import("node:assert");
			const { createRequire, isBuiltin } = await import("node:module");
			const req = createRequire("F:/Project/package.json");
			const cjsZlib = req("node:zlib");
			const cjsUtil = require("util");

			assertMod(true);
			assertMod.strictEqual(nodePath.join("a", "b").replace(/\\/g, "/"), "a/b");
			assertMod.strictEqual(typeof cryptoDef.randomUUID, "function");
			assertMod.strictEqual(createHash("sha256").update("abc").digest("hex").length, 64);
			assertMod.strictEqual(nodeFs.existsSync("scripts/run.sh"), true);
			assertMod.strictEqual(isBuiltin("node:path"), true);
			assertMod.strictEqual(isBuiltin("left-pad"), false);
			assertMod.strictEqual(cjsZlib.gunzipSync(cjsZlib.gzipSync("ok")).toString(), "ok");
			assertMod.strictEqual(cjsUtil.format("%s-%d", "hi", 7), "hi-7");

			// Ensure method shorthand { import() {} } and obj.import() and strings with import(...) are untouched!
			const obj = { import(x: number) { return x + 1; } };
			const lit = "await import('untouched')";
			let externalErr = "";
			try {
				req("./local-script.js");
			} catch (e: any) {
				externalErr = String(e.message || e);
			}
			return { m: obj.import(41), lit, externalErr };
		`,
		Cwd:     ws,
		Timeout: 5 * time.Second,
	}, &FSSearchBinder{Policy: pol})
	if err != nil {
		t.Fatalf("dynamic import / require test failed: %v", err)
	}
	im, _ := dynRes.Value.(map[string]any)
	if fmt.Sprint(im["m"]) != "42" || im["lit"] != "await import('untouched')" || !strings.Contains(im["externalErr"].(string), "sh.run") {
		t.Fatalf("unexpected dynamic import result: %+v", im)
	}

	// Verify `TypeError: object is not iterable` includes Object.values / Object.entries hint!
	_, iterErr := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			const sc: any = { groups: { a: 1, b: 2 } };
			for (const g of sc.groups || []) {
				print(g);
			}
		`,
		Cwd:     ws,
		Timeout: 5 * time.Second,
	})
	if iterErr == nil || !strings.Contains(iterErr.Error(), "Object.values(obj)") {
		t.Fatalf("expected Object.values hint on non-iterable plain object, got: %v", iterErr)
	}
}
