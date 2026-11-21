package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFsListReturnsNamesByDefault(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	writeFiles(t, ws, map[string]string{"src/sub/x.go": "package sub\n"})
	out, isErr := run(t, eng, ws, `
const names = await fs.list("src");
const detail = await fs.list("src", { detail: true });
return { names: names.join(","), endsWith: names.filter((n) => n.endsWith(".rs")).length, dir: detail.find((d) => d.isDir).name };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, `"names": "lib.rs,sub/"`, `"endsWith": 1`, `"dir": "sub/"`)
}

func TestFsExistsThrowsOnBlockedPath(t *testing.T) {
	isolateHome(t)
	home := os.Getenv("USERPROFILE")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	eng := NewEngine(t.TempDir(), ws)
	defer eng.Close()
	out, _ := eng.RunMCPExec(context.Background(), ExecInput{Cwd: ws, Code: `try { fs.exists("~/.ssh/id_rsa"); return "no throw"; } catch (e) { return e.name; }`})
	mustContainAll(t, out, "Denied")
}

func TestTimeoutUnitHeuristicIsAnnounced(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `
await sh.exec("go version", { timeout: 30 });
await sh.run("go", ["version"], { timeout: 5000 });
await sh.checks(["go version"], { timeout: 20 });
return "ok";`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, "sh.exec: timeout 30 read as seconds; use timeoutMs or timeoutSec to be explicit",
		"sh.checks: timeout 20 read as seconds")
	if strings.Contains(out, "sh.run: timeout") {
		t.Errorf("millisecond-sized timeout must not warn:\n%s", out)
	}
}

func TestStatePersistsOncePerExec(t *testing.T) {
	ws := t.TempDir()
	dataDir := t.TempDir()
	eng := NewEngine(dataDir, ws)
	out, isErr := eng.RunMCPExec(context.Background(), ExecInput{Cwd: ws, Code: `for (let i = 0; i < 50; i++) state.set("k" + i, i); return state.keys().length;`})
	eng.Close()
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	// A fresh engine (new MCP process) must see the values written by the previous exec.
	eng2 := NewEngine(dataDir, ws)
	defer eng2.Close()
	out, isErr = eng2.RunMCPExec(context.Background(), ExecInput{Cwd: ws, Code: `return [state.get("k0"), state.get("k49")];`})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, "0,", "49")
}
