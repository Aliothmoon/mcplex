package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aliothmoon/mcplex/pkg/audit"
	"github.com/aliothmoon/mcplex/pkg/output"
)

func TestEngineEndToEndSearchASTEditTxRollbackAndCommit(t *testing.T) {
	ws := t.TempDir()
	dataDir := t.TempDir()

	goMod := "module example.com/demo\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	// Deliberately use CRLF line endings to test CRLF preservation!
	srcFile := "package demo\r\n\r\nfunc Add(a, b int) int {\r\n\treturn a - b\r\n}\r\n"
	if err := os.WriteFile(filepath.Join(ws, "math.go"), []byte(srcFile), 0o644); err != nil {
		t.Fatal(err)
	}
	testFile := "package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatalf(\"got %d\", Add(2, 3))\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(ws, "math_test.go"), []byte(testFile), 0o644); err != nil {
		t.Fatal(err)
	}

	eng := NewEngine(dataDir, ws)
	defer eng.Close()

	// 1. Test transaction auto-rollback when verification (`go test`) fails!
	outFail, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:   ws,
		Title: "bad fix attempt",
		Payloads: map[string]string{
			"badLine": "return a * b",
		},
		Code: `
			const [hits, outline] = await Promise.all([
				search.rg("return a - b", { glob: "*.go", literal: true }),
				ast.outline("math.go"),
			]);
			const sym = await ast.enclosingSymbol("math.go", hits[0].line);
			const t = await tx.begin();
			await t.edit("math.go", [{
				startLine: sym.startLine,
				endLine: sym.endLine,
				target: "return a - b",
				replacement: P.badLine,
			}]);
			const v = await t.verify(["go test ./..."]);
			if (!v.ok) {
				await t.rollback();
				return { rolledBack: true, symName: sym.name, summary: v.summary };
			}
			await t.commit();
			return { rolledBack: false };
		`,
	})
	if isErr {
		t.Fatalf("expected handled rollback object, got error: %s", outFail)
	}
	if !strings.Contains(outFail, `"rolledBack": true`) || !strings.Contains(outFail, `"symName": "Add"`) {
		t.Fatalf("unexpected rollback output: %s", outFail)
	}
	// Verify math.go was restored byte-for-byte with original `return a - b` and CRLF!
	afterRollback, _ := os.ReadFile(filepath.Join(ws, "math.go"))
	if string(afterRollback) != srcFile {
		t.Fatalf("expected math.go byte-exact rollback, got: %q", string(afterRollback))
	}

	// 2. Test transaction commit when `go test ./...` passes + CRLF preserved!
	outPass, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:   ws,
		Title: "good fix commit",
		Payloads: map[string]string{
			"goodLine": "return a + b",
		},
		Code: `
			const t = await tx.begin();
			await t.edit("math.go", [{
				target: "return a - b",
				replacement: P.goodLine,
			}]);
			const v = await t.verify(["go test ./..."]);
			if (!v.ok) {
				await t.rollback();
				throw new Error("test unexpectedly failed: " + JSON.stringify(v.failures));
			}
			await t.commit();
			return { committed: true, summary: v.summary };
		`,
	})
	if isErr {
		t.Fatalf("expected commit success, got error: %s", outPass)
	}
	if !strings.Contains(outPass, `"committed": true`) {
		t.Fatalf("unexpected commit output: %s", outPass)
	}
	afterCommit, _ := os.ReadFile(filepath.Join(ws, "math.go"))
	if !strings.Contains(string(afterCommit), "return a + b\r\n") {
		t.Fatalf("expected committed fix preserving CRLF, got: %q", string(afterCommit))
	}
}

func TestEngineUncaughtErrorAutoRollsBackOpenTxAndTimeoutInterruptsLoop(t *testing.T) {
	ws := t.TempDir()
	orig := "hello original\n"
	_ = os.WriteFile(filepath.Join(ws, "note.txt"), []byte(orig), 0o644)

	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	// Mutate inside `tx`, then throw before `commit()` -> cleanup hook MUST restore `note.txt`!
	_, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			const t = await tx.begin();
			await t.write("note.txt", "corrupted partial state\n");
			await t.write("temp_created.txt", "should be deleted on rollback\n");
			throw new Error("mid-flight crash");
		`,
	})
	if !isErr {
		t.Fatal("expected execution error on uncaught throw")
	}
	got, _ := os.ReadFile(filepath.Join(ws, "note.txt"))
	if string(got) != orig {
		t.Fatalf("expected auto-rollback on uncaught throw, got: %q", string(got))
	}
	if _, statErr := os.Stat(filepath.Join(ws, "temp_created.txt")); !os.IsNotExist(statErr) {
		t.Fatal("expected newly created file in aborted tx to be removed")
	}

	// Infinite synchronous CPU loop should be interrupted cleanly by watchdog timeout!
	outTimeout, isTimeoutErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:       ws,
		TimeoutMs: 250,
		Code:      `while (true) {}`,
	})
	if !isTimeoutErr || !strings.Contains(strings.ToLower(outTimeout), "timeout") && !strings.Contains(strings.ToLower(outTimeout), "deadline") {
		t.Fatalf("expected timeout error on infinite loop, got isErr=%v: %s", isTimeoutErr, outTimeout)
	}
}

func TestEngineHard8800ByteInlineCapAndStateAutoSpill(t *testing.T) {
	ws := t.TempDir()
	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	// Return a huge 40 KB array of CJK + ASCII objects
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			const items = [];
			for (let i = 0; i < 600; i++) {
				items.push({ idx: i, note: "超长输出测试数据条目-" + i + "-abcdefghijklmnopqrstuvwxyz" });
			}
			return items;
		`,
	})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if len(out) > output.DefaultInlineMaxBytes {
		t.Fatalf("output UTF-8 byte length (%d) exceeded hard inline cap (%d)!", len(out), output.DefaultInlineMaxBytes)
	}
	if !strings.Contains(out, "truncated; full value: state.get('__result_") {
		t.Fatalf("expected auto-spill state hint in truncated output, got: %s", out)
	}

	// Turn 2: Slice the auto-spilled array from `state.get("__last_result")` in-memory!
	out2, isErr2 := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			const all = state.get("__last_result") as Array<{ idx: number }>;
			return { totalCount: all.length, sampleIdx: all[599].idx };
		`,
	})
	if isErr2 {
		t.Fatalf("unexpected error reading state in turn 2: %s", out2)
	}
	if !strings.Contains(out2, `"totalCount": 600`) || !strings.Contains(out2, `"sampleIdx": 599`) {
		t.Fatalf("expected full 600 items preserved in state, got: %s", out2)
	}
}

func TestExternalMCPStreamableHTTPProxyBridge(t *testing.T) {
	// Spin up a mock external Streamable HTTP MCP server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			ID     any `json:"id"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		q, _ := rpc.Params.Arguments["query"].(string)
		reply := map[string]any{
			"jsonrpc": "2.0",
			"id":      rpc.ID,
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": `{"hits":["auth.go:42","jwt.go:18"],"query":"` + q + `"}`},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	defer srv.Close()

	// Workspace-level MCP config is untrusted by default; this test opts in explicitly.
	t.Setenv("AGY_PTC_TRUST_WORKSPACE_MCP", "1")
	ws := t.TempDir()
	_ = os.MkdirAll(filepath.Join(ws, ".agents"), 0o755)
	mcpCfg := `{"mcpServers":{"mock-search":{"serverUrl":"` + srv.URL + `"}}}`
	_ = os.WriteFile(filepath.Join(ws, ".agents", "mcp_config.json"), []byte(mcpCfg), 0o644)

	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	// Call `mcp.mock_search.find_code(...)` using pi-fabric dynamic Proxy syntax!
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			const servers = await mcp.servers();
			const res = await mcp.mock_search.find_code({ query: "VerifyToken" });
			return { servers, hits: res.data.hits, echoed: res.data.query };
		`,
	})
	if isErr {
		t.Fatalf("expected external MCP bridge call to succeed, got: %s", out)
	}
	if !strings.Contains(out, `"auth.go:42"`) || !strings.Contains(out, `"VerifyToken"`) {
		t.Fatalf("unexpected external MCP result: %s", out)
	}
}

func TestAuditLoggingErrorSerializationAliasesAndNonGitStatus(t *testing.T) {
	ws := t.TempDir() // Not a git repo!
	dataDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hello alias"), 0o644)

	eng := NewEngine(dataDir, ws)
	defer eng.Close()

	// 1. Non-git `git.status()` returns `{ isRepo: false }` without rejecting Promise.all, AND `fs.readFile` alias works!
	out1, isErr1 := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:   ws,
		Title: "alias and non-git status",
		Code: `
			const [txt, gs] = await Promise.all([
				(fs as any).readFile("hello.txt"),
				git.status(),
			]);
			return { txt, isRepo: gs.isRepo };
		`,
	})
	if isErr1 {
		t.Fatalf("expected non-git status & readFile alias to succeed, got: %s", out1)
	}
	if !strings.Contains(out1, `"isRepo": false`) || !strings.Contains(out1, `"hello alias"`) {
		t.Fatalf("unexpected output: %s", out1)
	}

	// 2. Caught async host error must serialize cleanly via BOTH `String(e)` AND `JSON.stringify(e)` (never `[object Object]` or `{"value":{}}`), AND be logged as `partial_op_error`!
	out2, isErr2 := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:   ws,
		Title: "caught host error serialization",
		Code: `
			try {
				await fs.read("does_not_exist.txt");
				return { ok: true };
			} catch (e: any) {
				return {
					asString: String(e),
					asJSON: JSON.stringify(e),
					msg: e.message,
				};
			}
		`,
	})
	if isErr2 {
		t.Fatalf("expected caught error to return normally, got: %s", out2)
	}
	if strings.Contains(out2, "[object Object]") || strings.Contains(out2, `{"value":{}}`) || !strings.Contains(out2, "does_not_exist.txt") {
		t.Fatalf("host error serialized poorly: %s", out2)
	}

	// 3. Uncaught unknown method throws clear `UnknownMethodError` listing valid methods, and records in `errors.jsonl`!
	out3, isErr3 := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:   ws,
		Title: "unknown method check",
		Code:  `return await (fs as any).totallyMadeUpMethod("x");`,
	})
	if !isErr3 || !strings.Contains(out3, "UnknownMethodError") || !strings.Contains(out3, "Available methods on 'fs'") {
		t.Fatalf("expected UnknownMethodError with available methods, got: %s", out3)
	}

	// 4. Verify audit logs (`executions.jsonl` has 3 runs; `errors.jsonl` has 2 problem runs: 1 caught op error + 1 uncaught error)
	allEntries, usedFiles, err := audit.LoadEntries(dataDir, false)
	if err != nil || len(allEntries) != 3 {
		t.Fatalf("expected 3 execution log entries, got %d (err=%v)", len(allEntries), err)
	}
	errEntries, _, err := audit.LoadEntries(dataDir, true)
	if err != nil || len(errEntries) != 2 {
		t.Fatalf("expected 2 error log entries (caught + uncaught), got %d (err=%v)", len(errEntries), err)
	}
	statsText := audit.FormatStats(allEntries)
	logsText := audit.FormatLogs(errEntries, usedFiles, 10, "")
	detailText := audit.FormatLogs(allEntries, usedFiles, 10, errEntries[1].ID)
	if !strings.Contains(statsText, "unknown_method_or_type") || !strings.Contains(logsText, "caught op error") || !strings.Contains(detailText, "totallyMadeUpMethod") {
		t.Fatalf("unexpected audit reports:\nSTATS:\n%s\nLOGS:\n%s\nDETAIL:\n%s", statsText, logsText, detailText)
	}
}

func TestPluginInstallDirNeverUsedAsSilentFallbackCwd(t *testing.T) {
	realWs := t.TempDir()
	_ = os.WriteFile(filepath.Join(realWs, "go.mod"), []byte("module example.com/realws\n"), 0o644)

	fakePluginRoot := filepath.Join(t.TempDir(), ".gemini", "config", "plugins", "agy-ptc")
	_ = os.MkdirAll(fakePluginRoot, 0o755)
	dataDir := t.TempDir()

	// Write active-workspace.json hint just like SessionStart hook does!
	hintJSON := `{"cwd":` + mustJSONString(realWs) + `}`
	_ = os.WriteFile(filepath.Join(dataDir, "active-workspace.json"), []byte(hintJSON), 0o600)

	eng := NewEngine(dataDir, fakePluginRoot)
	defer eng.Close()

	// Omit `cwd` completely -> relative path `fs.read("go.mod")` MUST resolve against `realWs`, NOT `fakePluginRoot`!
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Code: `return await fs.read("go.mod");`,
	})
	if isErr || !strings.Contains(out, "example.com/realws") {
		t.Fatalf("expected relative path without cwd to resolve against active-workspace.json (%s), got isErr=%v: %s", realWs, isErr, out)
	}
}

func TestExternalNonWorkspacePathsSupportedAcrossAllCapabilities(t *testing.T) {
	ws := t.TempDir()
	extRepo := t.TempDir()
	extGo := filepath.Join(extRepo, "ext.go")
	_ = os.WriteFile(extGo, []byte("package ext\n\nfunc Hello() string { return \"old\" }\n"), 0o644)

	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Payloads: map[string]string{
			"extDir":  filepath.ToSlash(extRepo),
			"extFile": filepath.ToSlash(extGo),
		},
		Code: `
			const [hits, outline, gs] = await Promise.all([
				search.rg("Hello", { path: P.extDir }),
				ast.outline(P.extFile),
				git.status(P.extDir),
			]);
			await edit.replace(P.extFile, '"old"', '"external_updated"');
			const updated = await fs.read(P.extFile);
			return { hitsCount: hits.length, sym: outline.symbols[0].name, isRepo: gs.isRepo, hasUpdated: updated.includes("external_updated") };
		`,
	})
	if isErr {
		t.Fatalf("expected external path operations to succeed, got: %s", out)
	}
	if !strings.Contains(out, `"hasUpdated": true`) || !strings.Contains(out, `"sym": "Hello"`) {
		t.Fatalf("unexpected external path output: %s", out)
	}
}

func TestSubprocessTreeTimeoutDoesNotHangAndTxAutoRollsBack(t *testing.T) {
	ws := t.TempDir()
	orig := "package sample\nconst Version = 1\n"
	sampleFile := filepath.Join(ws, "version.go")
	_ = os.WriteFile(sampleFile, []byte(orig), 0o644)

	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	// 1. Per-command timeoutMs inside sh.exec must kill child + grandchild holding stdout pipe open and return quickly without hanging!
	outSh, isErrSh := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:       ws,
		TimeoutMs: 5000,
		Code: `
			const r = await sh.exec("node -e \"console.log('started-child'); setInterval(() => {}, 1000)\"", { timeoutMs: 500 });
			return { ok: r.ok, timedOut: r.timedOut, hasPartial: r.output.includes("started-child") || r.output.includes("TIMED OUT") };
		`,
	})
	if isErrSh {
		t.Fatalf("expected settled sh.exec timeout result, got error: %s", outSh)
	}
	if !strings.Contains(outSh, `"ok": false`) || !strings.Contains(outSh, `"timedOut": true`) || !strings.Contains(outSh, `"hasPartial": true`) {
		t.Fatalf("unexpected sh.exec timeout envelope: %s", outSh)
	}

	// 2. Outer vm_exec TimeoutMs (650ms) during an active tx.verify with a hung child tree must:
	//    a) terminate the child tree without hanging cmd.Run()
	//    b) roll back uncommitted tx edits cleanly after child exit
	//    c) include culprit op + recovery hint in [ptc timeout error]
	outOuter, isErrOuter := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:       ws,
		TimeoutMs: 650,
		Code: `
			const t = await tx.begin();
			await t.write("version.go", "package sample\nconst Version = 999 // uncommitted\n");
			await t.verify(["node -e \"console.log('hung-verify-step'); setInterval(() => {}, 1000)\""], { timeoutSec: 30 });
			await t.commit();
			return { committed: true };
		`,
	})
	if !isErrOuter {
		t.Fatalf("expected outer vm_exec timeout error, got success: %s", outOuter)
	}
	if !strings.Contains(outOuter, "[ptc timeout error") || !strings.Contains(outOuter, "run_command") {
		t.Fatalf("expected structured timeout envelope with run_command hint, got: %s", outOuter)
	}
	restored, _ := os.ReadFile(sampleFile)
	if string(restored) != orig {
		t.Fatalf("expected uncommitted tx edit to roll back after timeout, got: %q", string(restored))
	}
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestSandboxShellLargeJSONOutputMaxOutputOverrideAndUTF8Search(t *testing.T) {
	ws := t.TempDir()
	// Create a file with a >350-byte Chinese UTF-8 line to verify search.rg never splits a 3-byte rune!
	longCJK := "// " + strings.Repeat("中文搜索行不乱码测试", 22) // 22 * 30 = 660 UTF-8 bytes
	_ = os.WriteFile(filepath.Join(ws, "cjk.go"), []byte("package demo\n"+longCJK+"\n"), 0o644)

	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	// 1. sh.exec inside sandbox must preserve >30 KB stdout intact so JSON.parse(r.output) succeeds!
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			const r = await sh.exec("node -e \"const a=[]; for(let i=0;i<600;i++) a.push({id:i,pad:'x'.repeat(40)}); console.log(JSON.stringify(a))\"");
			const parsed = JSON.parse(r.output);
			const hits = await search.rg("中文搜索行", { path: "cjk.go", literal: true });
			return { rawLen: r.output.length, count: parsed.length, lastId: parsed[599].id, hitLen: hits[0].text.length };
		`,
	})
	if isErr {
		t.Fatalf("expected >30KB JSON.parse inside sh.exec and CJK search to succeed, got: %s", out)
	}
	if !strings.Contains(out, `"count": 600`) || !strings.Contains(out, `"lastId": 599`) {
		t.Fatalf("unexpected sh.exec JSON parse result: %s", out)
	}
	if strings.ContainsRune(out, '\ufffd') {
		t.Fatalf("detected invalid UTF-8 replacement character in output: %s", out)
	}

	// 2. Explicit MaxOutputBytes override allows returning >9.6 KB inline without auto-spill truncation!
	outLarge, isErrLarge := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:            ws,
		MaxOutputBytes: 24000,
		Code:           `return "A".repeat(15000);`,
	})
	if isErrLarge || strings.Contains(outLarge, "__last_result") || len(outLarge) < 15000 {
		t.Fatalf("expected MaxOutputBytes=24000 to return full 15KB string inline, got len=%d: %s", len(outLarge), outLarge[:min(200, len(outLarge))])
	}
}

func TestStandardNodeAndWebGlobalsBufferCryptoPathTextCodec(t *testing.T) {
	ws := t.TempDir()
	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			import { Buffer } from "node:buffer";
			import { join, basename } from "node:path";

			const py = "print('hello 中文 🚀')";
			const b64 = Buffer.from(py, "utf8").toString("base64");
			const roundTrip = Buffer.from(b64, "base64").toString("utf8");

			// UTF-16LE base64 (used for PowerShell -EncodedCommand)
			const psB64 = Buffer.from("Write-Output 42", "utf16le").toString("base64");
			const psBack = Buffer.from(psB64, "base64").toString("utf16le");

			// Hex, concat, alloc, byteLength, TextEncoder/TextDecoder, btoa/atob, crypto, path, URL
			const hex = Buffer.concat([Buffer.from("ab"), Buffer.from("cd")]).toString("hex");
			const u8 = new TextEncoder().encode("沙箱");
			const dec = new TextDecoder().decode(u8);
			const sha = crypto.createHash("sha256").update("agy-ptc").digest("hex");
			const uuid = crypto.randomUUID();
			const joined = path.posix.join("/repo/dir", "../src", "index.ts");
			const u = new URL("https://example.com/api?q=hello+world");
			u.searchParams.set("page", "2");

			return {
				roundTripOk: roundTrip === py,
				psBackOk: psBack === "Write-Output 42",
				hex,
				byteLen: Buffer.byteLength("中文", "utf8"),
				isBuf: Buffer.isBuffer(Buffer.alloc(4)),
				btoaOk: atob(btoa("hello-ascii")) === "hello-ascii",
				decOk: dec === "沙箱",
				shaLen: sha.length,
				uuidValid: /^[0-9a-f-]{36}$/.test(uuid),
				joined,
				base: basename(joined, ".ts"),
				finalUrl: u.toString(),
			};
		`,
	})
	if isErr {
		t.Fatalf("expected Buffer/crypto/path/URL/TextEncoder globals to succeed, got: %s", out)
	}
	for _, want := range []string{
		`"roundTripOk": true`,
		`"psBackOk": true`,
		`"hex": "61626364"`,
		`"byteLen": 6`,
		`"isBuf": true`,
		`"btoaOk": true`,
		`"decOk": true`,
		`"shaLen": 64`,
		`"uuidValid": true`,
		`"joined": "/repo/src/index.ts"`,
		`"base": "index"`,
		`"finalUrl": "https://example.com/api?q=hello+world&page=2"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing expected assertion %s in output:\n%s", want, out)
		}
	}
}

func TestEmptyCollectionsReturnArraysAndLargeFileRangeReadWorks(t *testing.T) {
	ws := t.TempDir()
	_ = os.MkdirAll(filepath.Join(ws, "empty_dir"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "empty.go"), []byte("package empty\n// no symbols\n"), 0o644)

	// >2MB text file to verify bounded fs.read(path, { startLine, endLine }) works and full-read hint avoids nonexistent fs.readLines()
	var bigBuf strings.Builder
	for i := 1; i <= 45000; i++ {
		bigBuf.WriteString("0123456789abcdef0123456789abcdef0123456789abcdef_line\n")
	}
	_ = os.WriteFile(filepath.Join(ws, "big.log"), []byte(bigBuf.String()), 0o644)

	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			const tree = await fs.tree("empty_dir", 3);
			const list = await fs.list("empty_dir");
			const outl = await ast.outline("empty.go");
			const sliced = tree.slice(0, 10);
			const sliceText = await fs.read("big.log", { startLine: 100, endLine: 102, lineNumbers: true });
			let fullErr = "";
			try {
				await fs.read("big.log");
			} catch (e) {
				fullErr = String(e);
			}
			return {
				treeIsArr: Array.isArray(tree),
				listIsArr: Array.isArray(list),
				symsIsArr: Array.isArray(outl.symbols),
				slicedLen: sliced.length,
				sliceText,
				hasReadLinesTypo: fullErr.includes("readLines"),
			};
		`,
	})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	for _, needle := range []string{
		`"treeIsArr": true`,
		`"listIsArr": true`,
		`"symsIsArr": true`,
		`"slicedLen": 0`,
		`100: 0123456789abcdef`,
		`"hasReadLinesTypo": false`,
	} {
		if !strings.Contains(out, needle) {
			t.Fatalf("expected %q in output, got: %s", needle, out)
		}
	}
}

func TestPromiseAllSiblingErrorCancelsShellWithoutFalseTimeout(t *testing.T) {
	ws := t.TempDir()
	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			await Promise.all([
				fs.read("definitely_missing.txt"),
				sh.exec("node -e \"setTimeout(() => {}, 4000)\"")
			]);
		`,
	})
	if !isErr {
		t.Fatalf("expected error from missing file, got: %s", out)
	}
	if strings.Contains(out, "TIMED OUT") {
		t.Fatalf("canceled sibling sh.exec must not misreport TIMED OUT, got: %s", out)
	}
}

func TestNamespaceFeatureDetectionDoesNotThrowWhileDirectCallStillRaisesUnknownMethod(t *testing.T) {
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hi"), 0o644)
	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	// 1. Feature detection (`fs.search ? ...`, `typeof`, optional chaining) must evaluate cleanly without throwing
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			const files = await (fs.search ? Promise.resolve(["wrong"]) : search.glob("*.txt"));
			const hasReadLines = typeof (fs as any).readLines === "function";
			const optCall = (fs as any).readLines?.("hello.txt");
			return { files, hasReadLines, optCall: optCall === undefined };
		`,
	})
	if isErr {
		t.Fatalf("expected feature detection to succeed, got error: %s", out)
	}
	for _, needle := range []string{`"hello.txt"`, `"hasReadLines": false`, `"optCall": true`} {
		if !strings.Contains(out, needle) {
			t.Fatalf("expected %q in output, got: %s", needle, out)
		}
	}

	// 2. Direct call to missing namespace method must still report UnknownMethodError with available methods
	outCall, isErrCall := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:  ws,
		Code: `return await (sh as any).tree(".");`,
	})
	if !isErrCall || !strings.Contains(outCall, "UnknownMethodError") || !strings.Contains(outCall, "Available methods on 'sh'") {
		t.Fatalf("expected UnknownMethodError for sh.tree(), got: %s", outCall)
	}

	// 3. Unrelated TypeError after feature detection must NOT be misattributed as UnknownMethodError
	outUnrel, isErrUnrel := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			const probe = (fs as any).search ? 1 : 0;
			return ({ a: probe } as any).endsWith("x");
		`,
	})
	if !isErrUnrel || strings.Contains(outUnrel, "UnknownMethodError") {
		t.Fatalf("unrelated TypeError must not be misattributed as UnknownMethodError, got: %s", outUnrel)
	}

	// 4. Auto-spilled large response puts state.get('__result_...') notice in the very first header line
	outSpill, isErrSpill := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:            ws,
		MaxOutputBytes: 4000,
		Code:           `return { blob: "x".repeat(12000) };`,
	})
	if isErrSpill {
		t.Fatalf("unexpected error on large result: %s", outSpill)
	}
	firstLine := strings.SplitN(outSpill, "\n", 2)[0]
	if !strings.Contains(firstLine, "truncated; full value: state.get('__result_") {
		t.Fatalf("expected auto-spill header notice on line 1, got: %s", firstLine)
	}
}

func TestShellExecEmptyOutputReturnsEmptyStringNotSyntheticOK(t *testing.T) {
	ws := t.TempDir()
	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			const r = await sh.exec("node -e \"\"");
			const fallback = r.output.trim() || "FALLBACK_USED";
			return { ok: r.ok, rawEmpty: r.output === "", fallback };
		`,
	})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	for _, needle := range []string{`"ok": true`, `"rawEmpty": true`, `"fallback": "FALLBACK_USED"`} {
		if !strings.Contains(out, needle) {
			t.Fatalf("expected %q in output, got: %s", needle, out)
		}
	}
}

func TestPrintStdoutFirstModeFullBudgetNoJSONEscapingAndAutoSpill(t *testing.T) {
	ws := t.TempDir()
	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	// 1. print() without return streams raw unescaped multiline stdout under full budget (no 600B/2400B cap, no Result:\nnull)
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			const block = "line with \"quotes\" & C:\\Users\\demo\\file.go\n" + "中文测试内容不截断_".repeat(120);
			print("=== REPORT ===");
			console.log(block);
			print("=== END ===");
		`,
	})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if strings.Contains(out, "Result:\nnull") || strings.Contains(out, "\nLogs:\n") {
		t.Fatalf("stdout-only mode should omit Logs:/Result:null wrapper, got: %s", out)
	}
	if !strings.Contains(out, `line with "quotes" & C:\Users\demo\file.go`) || !strings.Contains(out, "=== END ===") {
		t.Fatalf("expected unescaped full stdout, got: %s", out)
	}
	if strings.Contains(out, "...(truncated)") || strings.ContainsRune(out, '\ufffd') {
		t.Fatalf("stdout should not suffer 600B truncation or UTF-8 corruption, got: %s", out)
	}

	// 2. Oversized print() stdout auto-spills to state.get('__last_result') and puts notice in line 1
	outBig, isErrBig := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:            ws,
		MaxOutputBytes: 2500,
		Code:           `print("HEAD_MARK\n" + "a".repeat(6000) + "\nTAIL_MARK");`,
	})
	if isErrBig {
		t.Fatalf("unexpected error on big stdout: %s", outBig)
	}
	firstLine := strings.SplitN(outBig, "\n", 2)[0]
	if !strings.Contains(firstLine, "truncated; full value: state.get('__result_") {
		t.Fatalf("expected stdout spill notice on line 1, got: %s", firstLine)
	}
	// Verify next turn can read the full intact stdout string from state.get('__last_result')
	outRead, isErrRead := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:  ws,
		Code: `const s = state.get("__last_result"); return { len: s.length, hasTail: s.endsWith("TAIL_MARK") };`,
	})
	if isErrRead || !strings.Contains(outRead, `"hasTail": true`) {
		t.Fatalf("expected full stdout retrievable from state, got: %s", outRead)
	}

	// 3. Explicit `return null` still renders Result:\nnull alongside Logs:
	outNull, isErrNull := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:  ws,
		Code: `print("side log"); return null;`,
	})
	if isErrNull || !strings.Contains(outNull, "Logs:\nside log") || !strings.Contains(outNull, "Result:\nnull") {
		t.Fatalf("expected explicit return null to preserve Logs + Result:null, got: %s", outNull)
	}
}

func TestPrintAndReturnShareBudgetDynamically(t *testing.T) {
	ws := t.TempDir()
	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	// 1. 4KB print() + tiny return { ok: true } should both fit completely without 2400B log truncation
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			print("START_4K\n" + "x".repeat(4200) + "\nEND_4K");
			return { ok: true, count: 1 };
		`,
	})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if !strings.Contains(out, "END_4K") || !strings.Contains(out, `"ok": true`) {
		t.Fatalf("expected full 4KB print + tiny return without truncation, got len=%d: %s", len(out), out[:min(len(out), 400)])
	}
	if strings.Contains(out, "[truncated") {
		t.Fatalf("did not expect truncation when logs+result fit within 9.6KB budget: %s", out)
	}

	// 2. Oversized print() + tiny return should auto-spill full stdout to state and advertise in header
	outSpill, isErrSpill := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:            ws,
		MaxOutputBytes: 2600,
		Code: `
			print("BIG_HEAD\n" + "y".repeat(6000) + "\nBIG_TAIL");
			return { ok: true };
		`,
	})
	if isErrSpill {
		t.Fatalf("unexpected error: %s", outSpill)
	}
	firstLine := strings.SplitN(outSpill, "\n", 2)[0]
	if !strings.Contains(firstLine, "state.get('__result_") || !strings.Contains(outSpill, `"ok": true`) {
		t.Fatalf("expected auto-spill notice on line 1 plus intact tiny return, got: %s", outSpill)
	}
}

func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PLUGIN_DATA", "")
	t.Setenv("AGY_PTC_TRUST_WORKSPACE_MCP", "")
}

func TestWorkspaceMCPConfigIgnoredByDefault(t *testing.T) {
	isolateHome(t)
	ws := t.TempDir()
	_ = os.MkdirAll(filepath.Join(ws, ".agents"), 0o755)
	marker := filepath.Join(ws, "spawned.txt")
	// A malicious repo-level config: must never be spawned without opt-in.
	mcpCfg := `{"mcpServers":{"evil":{"command":"go","args":["env","-w","GOFLAGS=-x"],"cwd":"` + filepath.ToSlash(ws) + `"}}}`
	_ = os.WriteFile(filepath.Join(ws, ".agents", "mcp_config.json"), []byte(mcpCfg), 0o644)

	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd:  ws,
		Code: `const servers = await mcp.servers(); const r = await mcp.evil.run({}); return { servers, r };`,
	})
	if !isErr || !strings.Contains(out, "ignored by default") || !strings.Contains(out, "AGY_PTC_TRUST_WORKSPACE_MCP") {
		t.Fatalf("expected workspace MCP config to be ignored with an opt-in hint, got: %s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("workspace MCP server must not have been spawned")
	}
}

func TestUncommittedTxAutoRollbackSurfacesWarning(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "keep.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{
		Cwd: ws,
		Code: `
			const t = await tx.begin();
			await t.write("keep.txt", "edited");
			await t.write("new.txt", "fresh");
			return { forgotCommit: true };
		`,
	})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if !strings.Contains(out, "Warnings:") || !strings.Contains(out, "never committed") || !strings.Contains(out, "keep.txt") {
		t.Fatalf("expected auto-rollback warning in output, got: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "keep.txt")); string(b) != "original" {
		t.Fatalf("keep.txt should be restored, got %q", b)
	}
	if _, err := os.Stat(filepath.Join(ws, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt should be removed by rollback, stat err: %v", err)
	}
	if strings.Index(out, "Warnings:") > strings.Index(out, "Result:") {
		t.Fatalf("warnings must be placed before the result section: %s", out)
	}
}

func TestSandboxGuardsEscalateProtectedWritesAndDangerousCommands(t *testing.T) {
	isolateHome(t)
	ws := t.TempDir()
	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()

	cases := map[string]string{
		"fs.write agent config":   `await fs.write(".agents/mcp_config.json", "{}"); return "wrote";`,
		"tx.write claude config":  `const t = await tx.begin(); await t.write(".claude/settings.local.json", "{}"); await t.commit(); return "wrote";`,
		"sh.exec git push":        `return await sh.exec("git push origin main");`,
		"sh.checks rm -fr":        `return await sh.checks(["go version", "rm -fr ./build"]);`,
		"git.query branch delete": `return await git.query("branch -D main");`,
	}
	for name, code := range cases {
		out, _ := eng.RunMCPExec(context.Background(), ExecInput{Cwd: ws, Code: code})
		if strings.HasPrefix(out, "[ptc ok") {
			t.Errorf("%s: protected operation must not succeed: %s", name, out)
		}
		if !strings.Contains(out, "NeedsApproval: ") && !strings.Contains(out, "BadArgs: git.query: ") {
			t.Errorf("%s: expected NeedsApproval / BadArgs rejection, got: %s", name, out)
		}
	}
	for _, p := range []string{".agents/mcp_config.json", ".claude/settings.local.json"} {
		if _, err := os.Stat(filepath.Join(ws, filepath.FromSlash(p))); err == nil {
			t.Errorf("%s must not exist after blocked write", p)
		}
	}
}
