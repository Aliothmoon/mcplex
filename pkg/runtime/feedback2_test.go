package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, ws string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(ws, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFsExistsIsSyncAndStillAwaitable(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, isErr := run(t, eng, ws, `
const e = fs.exists("src/lib.rs");
const n = fs.exists("src") ? (await fs.list("src")).length : -1;
const [a, b] = await Promise.all([fs.exists("src/lib.rs"), fs.exists("nope")]);
return { sync: e === true, n, a, b, awaited: await fs.exists("nope") };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, `"sync": true`, `"n": 1`, `"a": true`, `"b": false`, `"awaited": false`)
}

func TestFsReadTailIncludingHugeFiles(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	var small strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&small, "l%d\r\n", i)
	}
	var big strings.Builder
	for i := 1; big.Len() < 3*1024*1024; i++ {
		fmt.Fprintf(&big, "{\"seq\":%d,\"pad\":\"%s\"}\n", i, strings.Repeat("x", 60))
	}
	writeFiles(t, ws, map[string]string{"small.log": small.String(), "big.jsonl": big.String()})
	total := strings.Count(big.String(), "\n")

	out, isErr := run(t, eng, ws, `
const a = await fs.read("small.log", { tail: 3 });
const b = await fs.read("small.log", { tail: 2, lineNumbers: true });
const c = await fs.read("small.log", { tail: 50 });
const d = await fs.read("big.jsonl", { tail: 2, lineNumbers: true });
return { a, b, c: c.split("\n").length, d: d.split("\n").map((l) => l.split(":")[0]).join(",") };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, `"a": "l8\nl9\nl10"`, `"b": "9: l9\n10: l10"`, `"c": 10`,
		fmt.Sprintf(`"d": "%d,%d"`, total-1, total))
}

func TestProcessEnvRedactsSecrets(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	t.Setenv("MY_API_TOKEN", "s3cret")
	t.Setenv("AGY_TEST_SECRET_KEY", "k")
	t.Setenv("AGY_TEST_AUTH", "a")
	t.Setenv("GIT_AUTHOR_NAME", "bob")
	t.Setenv("AGY_KEYBOARD_LAYOUT", "us")
	out, isErr := run(t, eng, ws, `return [process.env.MY_API_TOKEN, process.env.AGY_TEST_SECRET_KEY, process.env.AGY_TEST_AUTH, process.env.GIT_AUTHOR_NAME, process.env.AGY_KEYBOARD_LAYOUT].join("|");`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, "[redacted]|[redacted]|[redacted]|bob|us")
	if strings.Contains(out, "s3cret") {
		t.Fatalf("secret leaked: %s", out)
	}
}

func TestSearchFilesOnlyAndCount(t *testing.T) {
	eng, ws := newRustWorkspace(t)
	out, isErr := run(t, eng, ws, `
const counts = await search.rg("self", { path: "src", count: true });
const files = await search.rg("let x", { path: "src", filesOnly: true });
return { counts, files };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	flat := strings.Join(strings.Fields(strings.ReplaceAll(out, `\\`, "/")), " ")
	mustContainAll(t, flat, `"src/editor.rs": 6`, `"files": [ "src/lib.rs" ]`)
}

func gitRepo(t *testing.T) (*Engine, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	eng, ws := newErrWorkspace(t)
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	for i := 1; i <= 3; i++ {
		writeFiles(t, ws, map[string]string{"src/lib.rs": fmt.Sprintf("v%d\n", i)})
		git("add", "-A")
		git("commit", "-q", "-m", fmt.Sprintf("commit %d", i))
	}
	writeFiles(t, ws, map[string]string{"src/lib.rs": "v4\n"})
	git("add", "src/lib.rs")
	return eng, ws
}

func TestGitAcceptsStructuredOptions(t *testing.T) {
	eng, ws := gitRepo(t)
	out, isErr := run(t, eng, ws, `
const log2 = await git.log({ n: 2 });
const subj = await git.log({ n: 1, format: "%s" });
const grep = await git.log({ grep: "commit 1", format: "%s" });
const staged = await git.diff({ staged: true, nameOnly: true });
const show = await git.show({ ref: "HEAD~1", nameOnly: true, format: "%s" });
let bad = "";
try { await git.log({ lines: 3 }); } catch (e) { bad = e.name + ": " + e.message; }
return { n2: log2.split("\n").length, oneline: /^[0-9a-f]{7,} commit 3$/.test(log2.split("\n")[0]), subj, grep, staged, show, bad };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, `"n2": 2`, `"oneline": true`, `"subj": "commit 3"`, `"grep": "commit 1"`, `"staged": "src/lib.rs"`,
		`"show": "commit 2\n\nsrc/lib.rs"`, `"bad": "BadArgs: git.log: unknown git option \"lines\"`)
}

const cppSrc = `namespace maa {
class Controller {
public:
    bool connect(const std::string& addr) {
        return true;
    }
    void stop();
};

bool Controller::run(int times)
{
    for (int i = 0; i < times; ++i) {
        if (!connect("x")) {
            return false;
        }
    }
    return true;
}

Controller::~Controller() {
}

static int helper(int a) { return a + 1; }
}
`

const ktSrc = `package app

data class User(val name: String)

class Repo(private val db: Db) {
    fun find(id: Int): User? {
        return db.get(id)
    }

    fun count(): Int = db.size()

    companion object {
        fun create(): Repo = Repo(Db())
    }
}

object Registry {
    fun register(r: Repo) {
        println(r)
    }
}

fun <T> List<T>.second(): T = this[1]
`

func TestOutlineCppAndKotlin(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	writeFiles(t, ws, map[string]string{"a.cpp": cppSrc, "Repo.kt": ktSrc})
	out, isErr := run(t, eng, ws, `
const range = (s: any) => s.name + "@" + s.startLine + "-" + s.endLine;
const cpp = (await ast.outline("a.cpp")).symbols.map((s: any) => s.kind + " " + s.name + " " + s.startLine + "-" + s.endLine);
const kt = (await ast.outline("Repo.kt")).symbols.map((s: any) => s.kind + " " + s.name + " " + s.startLine + "-" + s.endLine);
const reads = [
  await ast.read("a.cpp", "Controller::run"), await ast.read("a.cpp", "Controller::connect"), await ast.readSymbol("a.cpp", "helper"),
  await ast.read("Repo.kt", "Repo.find"), await ast.read("Repo.kt", "Repo.count"), await ast.read("Repo.kt", "Registry.register"), await ast.read("Repo.kt", "second"),
].map(range);
return { cpp: cpp.join(" | "), kt: kt.join(" | "), reads: reads.join(" | ") };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out,
		`namespace maa 1-24`, `class Controller 2-8`, `func connect 4-6`, `func Controller::run 10-18`, `func Controller::~Controller 20-21`, `func helper 23-23`,
		`class User 3-3`, `class Repo 5-15`, `fun find 6-8`, `fun count 10-10`, `object Companion 12-14`, `fun create 13-13`, `object Registry 17-21`, `fun register 18-20`, `fun second 23-23`,
		`Controller::run@10-18 | Controller::connect@4-6 | maa::helper@23-23 | Repo.find@6-8 | Repo.count@10-10 | Registry.register@18-20 | second@23-23`)
	for _, noise := range []string{"func for", "func if", "func stop", "func return"} {
		if strings.Contains(out, noise) {
			t.Errorf("outline contains %q:\n%s", noise, out)
		}
	}
}

func TestFsBytesAndTxRename(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	writeFiles(t, ws, map[string]string{"h.txt": "hello world"})
	out, isErr := run(t, eng, ws, `
const all = await fs.bytes("h.txt");
const mid = await fs.bytes("h.txt", { offset: 6 });
const end = await fs.readBytes("h.txt", { offset: -5, length: 3 });
const hash = crypto.createHash("sha256").update(all).digest("hex");
const t = await tx.begin();
await t.rename("src/lib.rs", "src/core.rs");
const d = await t.diff();
const moved = fs.exists("src/core.rs") && !fs.exists("src/lib.rs");
await t.rollback();
return { isBuf: Buffer.isBuffer(all), mid: mid.toString(), end: end.toString(), hash, moved, restored: fs.exists("src/lib.rs") && !fs.exists("src/core.rs"), d };`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, `"isBuf": true`, `"mid": "world"`, `"end": "wor"`,
		`"hash": "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"`, `"moved": true`, `"restored": true`,
		`rename from src/lib.rs\nrename to src/core.rs`)
}
