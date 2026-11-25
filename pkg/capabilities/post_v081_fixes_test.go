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

func TestPostV081FixesMtimeMsExprReplaceSymbolGitPlumbingStructEndTargetAndGlobDetail(t *testing.T) {
	ws := t.TempDir()
	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})

	// --- 1 & 5. fs.stat mtimeMs + search.glob({ detail: true }) + fs.read({ maxBytes }) ---
	if err := os.MkdirAll(filepath.Join(ws, "assets", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "assets", "a.bin"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "assets", "sub", "b.bin"), []byte("1234567"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a 2.4 MB JSON file (> 2 MB default cap, < 16 MB explicit maxBytes)
	bigJSON := `{"status":"ok","pad":"` + strings.Repeat("a", 2400*1024) + `"}`
	if err := os.WriteFile(filepath.Join(ws, "display.json"), []byte(bigJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	fsRes, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			const st = await fs.stat("assets/a.bin");
			const iso = new Date(st.mtimeMs).toISOString();
			const detailed = await search.glob("**/*", { path: "assets", detail: true });
			const totalBytes = detailed.reduce((acc: number, it: any) => acc + it.size, 0);
			let defaultBlocked = false;
			try {
				await fs.read("display.json");
			} catch (e: any) {
				defaultBlocked = String(e.message || e).includes("maxBytes");
			}
			const parsed = JSON.parse(await fs.read("display.json", { maxBytes: 6 * 1024 * 1024 }));
			return {
				hasIso: iso.endsWith("Z"),
				mtimePos: st.mtimeMs > 1700000000000,
				count: detailed.length,
				totalBytes,
				defaultBlocked,
				parsedStatus: parsed.status,
			};
		`,
		Cwd:     ws,
		Timeout: 10 * time.Second,
	}, &FSSearchBinder{Policy: pol})
	if err != nil {
		t.Fatalf("fs.stat / search.glob detail / fs.read maxBytes test failed: %v", err)
	}
	fm, _ := fsRes.Value.(map[string]any)
	if fm["hasIso"] != true || fm["mtimePos"] != true || fmt.Sprint(fm["count"]) != "2" ||
		fmt.Sprint(fm["totalBytes"]) != "12" || fm["defaultBlocked"] != true || fm["parsedStatus"] != "ok" {
		t.Fatalf("unexpected fs/glob result: %+v", fm)
	}

	// --- 2. replaceSymbol: true on Kotlin expression-bodied function & C# arrow method ---
	ktSrc := strings.Join([]string{
		"package com.example",
		"",
		"class WakeUnlockEngine(private val controller: WakeController) {",
		"    /** Doc kept across expression-body replaceSymbol */",
		"    suspend fun testUnlock(pin: String): Boolean =",
		"        controller.testUnlock(pin)",
		"",
		"    fun other(): Int = 1",
		"}",
		"",
	}, "\n")
	ktPath := filepath.Join(ws, "WakeUnlockEngine.kt")
	if err := os.WriteFile(ktPath, []byte(ktSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	// --- 4. Bare closing delimiter `};` on C struct header + multi-symbol >120-line top-level span ---
	var manyHelperLines []string
	manyHelperLines = append(manyHelperLines,
		"struct vm02_gl_hwc_present {",
		"  union {",
		"    uint32_t raw;",
		"  };",
		"  uint32_t old_field;",
		"};",
		"",
		"static int ensure_stream_locked(int id) {",
	)
	for i := 0; i < 150; i++ {
		manyHelperLines = append(manyHelperLines, fmt.Sprintf("  int step_%d = id + %d;", i, i))
	}
	manyHelperLines = append(manyHelperLines,
		"  return id;",
		"}",
		"",
		"static void helper_two(void) {",
		"  return;",
		"}",
		"",
		"static int present_hwc(const void *hdr, int count) {",
		"  return count;",
		"}",
		"",
	)
	cPath := filepath.Join(ws, "display_host.c")
	if err := os.WriteFile(cPath, []byte(strings.Join(manyHelperLines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	editBinder := &EditTxBinder{Policy: pol}
	_, err = sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			const r = await edit.apply({
				edits: [
					{
						path: "WakeUnlockEngine.kt",
						hunks: [
							{
								symbol: "WakeUnlockEngine.testUnlock",
								replaceSymbol: true,
								replacement: "    suspend fun testUnlock(pin: String): Boolean {\n        return controller.unlockWithRetry(pin)\n    }",
							},
						],
					},
					{
						path: "display_host.c",
						hunks: [
							{
								target: "struct vm02_gl_hwc_present {",
								endTarget: "};",
								replacement: "struct vm02_gl_hwc_present {\n  uint64_t new_field;\n};",
							},
							{
								target: "static int ensure_stream_locked(int id) {",
								endTarget: "static int present_hwc(const void *hdr, int count) {",
								replacement: "static int ensure_stream_locked(int id) {\n  return id * 2;\n}\n\nstatic int present_hwc(const void *hdr, int count) {",
							},
						],
					},
				],
			});
			return r;
		`,
		Cwd:     ws,
		Timeout: 10 * time.Second,
	}, editBinder)
	if err != nil {
		t.Fatalf("edit.apply for Kotlin expression fun + C struct '};' + >120-line top-level span failed: %v", err)
	}
	updatedKt, _ := os.ReadFile(ktPath)
	if !strings.Contains(string(updatedKt), "Doc kept across expression-body") ||
		!strings.Contains(string(updatedKt), "controller.unlockWithRetry(pin)") ||
		!strings.Contains(string(updatedKt), "fun other(): Int = 1") {
		t.Fatalf("unexpected updated Kotlin file:\n%s", string(updatedKt))
	}
	updatedC, _ := os.ReadFile(cPath)
	if !strings.Contains(string(updatedC), "uint64_t new_field;") ||
		strings.Contains(string(updatedC), "old_field") ||
		!strings.Contains(string(updatedC), "return id * 2;") ||
		strings.Contains(string(updatedC), "helper_two") ||
		!strings.Contains(string(updatedC), "return count;") {
		t.Fatalf("unexpected updated C file:\n%s", string(updatedC))
	}

	// --- 3. git.query("for-each-ref ...") & rev-list + canceled vs timed out message ---
	if _, lookErr := exec.LookPath("git"); lookErr == nil {
		repo := t.TempDir()
		runGit := func(args ...string) {
			cmd := exec.Command("git", args...)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
				"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			)
			if out, gErr := cmd.CombinedOutput(); gErr != nil {
				t.Fatalf("git %v failed: %v\n%s", args, gErr, string(out))
			}
		}
		runGit("init")
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "f.txt")
		runGit("commit", "-m", "init")
		runGit("tag", "v0.28.1")

		repoPol := policy.NewEngineWithOptions(repo, policy.Options{StrictWorkspace: true})
		gitRes, gErr := sandbox.Execute(context.Background(), sandbox.ExecRequest{
			Code: `
				const refs = await git.query("for-each-ref --sort=-creatordate --format=%(refname:short) refs/tags/v0.2*");
				const cnt = await git.query("rev-list --count HEAD");
				return { refs: refs.trim(), cnt: cnt.trim() };
			`,
			Cwd:     repo,
			Timeout: 10 * time.Second,
		}, &ShellGitBinder{Policy: repoPol})
		if gErr != nil {
			t.Fatalf("git.query for-each-ref / rev-list failed: %v", gErr)
		}
		gm, _ := gitRes.Value.(map[string]any)
		if gm["refs"] != "v0.28.1" || gm["cnt"] != "1" {
			t.Fatalf("unexpected git plumbing output: %+v", gm)
		}

		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, cErr := runReadOnlyGit(cancelCtx, repo, []string{"log", "-n", "1"})
		if cErr == nil || !strings.Contains(cErr.Error(), "canceled:") || strings.Contains(cErr.Error(), "timed out:") {
			t.Fatalf("expected canceled (not timed out) error on canceled context, got: %v", cErr)
		}
	}
}
