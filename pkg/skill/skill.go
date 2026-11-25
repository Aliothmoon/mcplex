// Package skill provides the single-source MCPlex Code Mode documentation registry,
// slim progressive-disclosure instructions, and on-demand topic/keyword lookup for the `docs` MCP tool.
package skill

import (
	"fmt"
	"sort"
	"strings"
)

// DocTopic represents one structured documentation section queryable via `docs({ topic })` or `docs({ query })`.
type DocTopic struct {
	ID            string
	Title         string
	Summary       string
	Aliases       []string
	RequiresTasks bool
	Body          string
}

var docTopics = []DocTopic{
	{
		ID:      "overview",
		Title:   "Overview & Core Rules",
		Summary: "Tool routing (read vs exec vs spawn/task vs docs) and TypeScript sandbox rules",
		Aliases: []string{"index", "start", "rules", "basics", "intro"},
		Body: `### When to Use Which Tool
- ` + "`read`" + `: Read whole files, large line ranges, tail logs, or images (` + "`.png`/`.jpg`/`.webp`/`.gif`" + ` returned as native image blocks). Each parallel ` + "`read`" + ` gets its own 48 KB / 800-line budget.
- ` + "`exec`" + `: Any ` + "`>=2`" + `-step or data-dependent chain (search -> AST -> read -> filter -> transactional edit -> fast verify), loops over files, downstream ` + "`mcp.*`" + ` calls, and ` + "`<15s`" + ` checks.
- ` + "`spawn` + `task`" + `: Long-running builds/tests (` + "`>15s`" + `), interactive stdin commands, and persistent daemons (` + "`daemon: true`" + `). Use ` + "`task({ action: \"wait\", taskId, expect: \"regex\" })`" + ` for instant readiness wakeup.
- ` + "`docs`" + `: Look up exact API signatures, options, error fixes, and examples on demand.

### Core Rules for ` + "`exec`" + `
- Write a **top-level ` + "`async`" + ` TypeScript body** (` + "`await`" + `, ` + "`print()`/`console.log()`" + `, ` + "`return`" + ` work directly; do **not** wrap in ` + "`async function main() {}`" + `).
- Put multiline replacement text/templates in ` + "`payloads: { key: \"...\" }`" + ` and reference ` + "`P.key`" + ` / ` + "`payloads.key`" + ` to avoid JSON escaping issues.
- Parallelize independent calls with ` + "`await Promise.all([...])`" + ` — every async host op runs in its own Go goroutine in true parallel.
- Always call ` + "`await t.commit()`" + ` to keep transaction edits; uncommitted ` + "`tx`" + ` auto-rolls back when the script exits.
- For multiline diffs/logs/code, prefer ` + "`print(...)`" + ` without ` + "`return`" + ` (raw unescaped output); use ` + "`return { ... }`" + ` for compact JSON objects.
- Oversized output auto-spills in memory to ` + "`state.get('__result_N')`" + ` (` + "`'__last_result'`" + ` aliases latest). Page with ` + "`print(state.page('__result_N', 2))`" + ` (` + "`-1`" + ` = last page).`,
	},
	{
		ID:      "fs",
		Title:   "fs — Files, Binary Slices & Streaming Hash",
		Summary: "read, cat, write, exists [sync], bytes, hash, stat, list, tree, mkdir, rm, rename, cp",
		Aliases: []string{"file", "files", "read", "write", "hash", "tree"},
		Body: `- ` + "`await fs.read(path, { startLine?, endLine?, limit?, tail?, lineNumbers? }) -> string`" + ` (up to 2 MB; ` + "`tail: N`" + ` reads last N lines of any file size; ` + "`.gz`" + ` text files auto-decompressed)
- ` + "`await fs.cat(paths, opts?) -> { [path]: string | null }`" + ` (parallel multi-read; failed paths return ` + "`null`" + ` + warning)
- ` + "`await fs.write(path, content, { eol?: \"lf\" | \"crlf\" })`" + ` (preserves existing EOL style by default; new ` + "`.bat`/`.cmd`" + ` -> CRLF)
- ` + "`fs.exists(path) -> boolean`" + ` (**sync**, no ` + "`await`" + ` needed)
- ` + "`await fs.bytes(path, { offset?, length? }) -> Buffer`" + ` (negative offset = from end; ` + "`<= 2 MB`" + `)
- ` + "`await fs.hash(path, algoOrOpts?) -> string`" + ` (streams any size; ` + "`sha256`" + ` default, also ` + "`sha512`/`sha1`/`md5`" + `; encoding ` + "`hex`/`base64`/`base64url`" + `)
- ` + "`await fs.stat(path)`" + ` · ` + "`await fs.list(path, { detail?, limit? })`" + ` (dirs end with ` + "`/`" + `) · ` + "`await fs.tree(path, depth?)`" + `
- ` + "`await fs.mkdir(path)`" + ` (` + "`-p`" + `) · ` + "`await fs.rm(path, { force? })`" + ` (file/empty dir) · ` + "`await fs.rename(from, to)`" + ` · ` + "`await fs.cp(from, to, { recursive? })`" + ` (immediate, not transactional)

Example:
` + "```ts\nconst [pkg, files] = await Promise.all([fs.read(\"package.json\"), fs.list(\"src\", { detail: true })]);\nprint(files.map(f => `${f.name} (${f.size}B)`).join(\"\\n\"));\n```",
	},
	{
		ID:      "search",
		Title:   "search — Ripgrep & Glob",
		Summary: "search.rg (regex/literal/filesOnly/count/context) and search.glob (**, {a,b})",
		Aliases: []string{"rg", "ripgrep", "grep", "glob", "find"},
		Body: `- ` + "`await search.rg(pattern, { path?, glob?, literal?, ignoreCase?, contextLines?, maxResults?, filesOnly?, count? })`" + `
  - Default: ` + "`Array<{ file, line, column, text, match, before?, after? }>`" + `
  - ` + "`{ filesOnly: true }`" + ` -> ` + "`string[]`" + `; ` + "`{ count: true }`" + ` -> ` + "`{ [file]: n }`" + `
  - In JS strings escape regex backslashes as ` + "`\\\\`" + ` (e.g. ` + "`\"foo\\\\(\"`" + `) or pass ` + "`{ literal: true }`" + `.
- ` + "`await search.glob(pattern, { path?, maxResults? }) -> string[]`" + ` (use ` + "`*`" + `, ` + "`**`" + `, and ` + "`{a,b}`" + ` — not regex ` + "`|`" + `)

Example:
` + "```ts\nconst hits = await search.rg(\"VerifyToken\", { glob: \"*.go\", contextLines: 2 });\nprint(hits.map(h => `${h.file}:${h.line}: ${h.text}`).join(\"\\n\"));\n```",
	},
	{
		ID:      "ast",
		Title:   "ast — Symbol Outlines & Targeted Symbol Reads",
		Summary: "ast.outline, ast.outlines, ast.read(\"Type::method\"), ast.enclosingSymbol",
		Aliases: []string{"outline", "symbol", "symbols", "parser"},
		Body: `- ` + "`await ast.outline(path)`" + ` / ` + "`await ast.outlines(paths)`" + ` -> ` + "`{ file, totalLines, symbols: [{ kind, name, signature?, startLine, endLine }] }`" + `
  - Exact ` + "`go/parser`" + ` AST for Go + Tree-sitter AST (including ` + "`Type::method`" + ` / ` + "`Type.method`" + ` members) for C/C++, Rust, Python, TS/TSX/JS, Java, Kotlin, C#, plus Markdown.
- ` + "`await ast.read(path, \"Type::method\", { lineNumbers? })`" + ` -> ` + "`{ kind, name, startLine, endLine, text }`" + ` (also accepts ` + "`Type.method`" + ` or bare ` + "`name`" + `)
- ` + "`await ast.enclosingSymbol(path, line)`" + ` -> ` + "`{ kind, name, startLine, endLine } | null`" + `

Example:
` + "```ts\nconst sym = await ast.read(\"pkg/auth/jwt.go\", \"Verifier::Check\", { lineNumbers: true });\nprint(sym.text);\n```",
	},
	{
		ID:      "edit",
		Title:   "edit — Scoped Multi-File Atomic Edits",
		Summary: "edit.replaceSymbol / insertSymbol / append (0 target), endTarget span, edit.apply",
		Aliases: []string{"replace", "replaceSymbol", "endTarget", "patch", "hunk", "hunks"},
		Body: `- **Low-token whole-symbol rewrite (recommended for full functions/methods/structs)**:
  - ` + "`await edit.replaceSymbol(path, \"Type::method\", P.newCode)`" + ` (0 ` + "`target`" + ` tokens; auto-preserves leading doc comments/decorators unless ` + "`P.newCode`" + ` includes them).
- **Zero-` + "`target`" + ` function/method insertion & EOF append**:
  - ` + "`await edit.insertSymbol(path, \"Type::fn\", \"after\" | \"before\" | \"inside\", P.newFn)`" + ` (or hunk ` + "`{ symbol, insert: \"after\" | \"before\" | \"inside\", replacement: P.newFn }`" + ` — ` + "`inside`" + ` appends a member before a ` + "`class`/`struct`/`impl`/`namespace`" + ` closing brace with auto-indent).
  - ` + "`await edit.append(path, P.newFn)`" + ` (or ` + "`{ path, append: P.newFn }`" + ` / hunk ` + "`{ insert: \"eof\", replacement: P.newFn }`" + `).
- **Low-token boundary-anchored span (` + "`endTarget`" + `, recommended for 10–120 line blocks)**:
  - ` + "`{ symbol?: \"fn\", target: \"<start line>\", endTarget: \"<end line>\", replacement: P.newBlock }`" + ` (replaces closed span ` + "`[target .. endTarget]`" + ` without copying middle lines).
- **Scoped multi-file / micro-hunk edits**:
  - ` + "`await edit.apply({ edits: [{ path, hunks: [{ target, endTarget?, replacement, symbol?, replaceSymbol?, nth?, startLine?, endLine?, replaceAll? }], create?, overwrite? }], dryRun?, bestEffort?, verify? })`" + `
  - Default for multi-file edits (and new files via ` + "`{ path, create: P.content, overwrite: true }`" + `): every hunk across all files is validated before any write, then run ` + "`sh.checks`" + `; if a check fails, keep edits on disk and send only the fix next turn.
  - ` + "`symbol: \"Type::method\"`" + ` restricts matching to that symbol; ` + "`nth: 2`" + ` targets the Nth occurrence; ` + "`dryRun: true`" + ` previews ` + "`.diff`" + `.
- Single hunk shortcut: ` + "`await edit.replace(path, target, replacement, { symbol?, endTarget?, nth?, replaceAll? })`" + `
- Always pass multiline replacement code in ` + "`payloads`" + ` (` + "`P.newCode`" + `) to avoid JS backslash escaping issues.

Example:
` + "```ts\nawait edit.replaceSymbol(\"pkg/config.go\", \"LoadConfig\", P.newLoadConfig);\nawait edit.replace(\"pkg/config.go\", \"Timeout = 5\", \"Timeout = 15\", { symbol: \"DefaultConfig\" });\n```",
	},
	{
		ID:      "tx",
		Title:   "tx — Opt-In All-or-Nothing Transactions with Rollback",
		Summary: "tx.begin() -> replaceSymbol, edit, write, rm, rename, verify, diff, status, commit, rollback",
		Aliases: []string{"transaction", "transactions", "rollback", "commit", "diff", "verify"},
		Body: `- **Opt-in only when a failed check must leave the tree untouched** (rollback discards every edit, so a retry must resend the whole payload; for normal iterative edits use ` + "`edit.apply` + `sh.checks`" + `).
- ` + "`const t = await tx.begin();`" + `
- Operations: ` + "`t.replaceSymbol(path, symbol, replacement)`" + `, ` + "`t.edit(pathOrSpecs, hunks?)`" + `, ` + "`t.write(path, content, { eol? })`" + `, ` + "`t.rm(path)`" + `, ` + "`t.rename(from, to)`" + `
  - Prefer ` + "`t.replaceSymbol`" + ` (0 ` + "`target`" + ` tokens) for whole functions/methods, ` + "`{ symbol, target, endTarget, replacement }`" + ` for 10–120 line blocks, and ` + "`{ symbol, target, replacement }`" + ` for 1–5 line tweaks.
  - Pass all files to one ` + "`await t.edit([{ path, hunks }, ...])`" + ` — every hunk across every file is validated upfront before anything is written.
- Inspection & checks: ` + "`await t.diff()`" + `, ` + "`await t.status()`" + `, ` + "`await t.verify(commands, { cwd?, concurrency?, timeoutSec?, compact? })`" + ` (items in ` + "`commands`" + ` can be strings or ` + "`{ cmd, cwd?, name?, timeoutMs? }`" + `)
- Finalize: ` + "`await t.commit()`" + ` (MUST call to keep changes; uncommitted ` + "`tx`" + ` auto-rolls back on exit!) or ` + "`await t.rollback()`" + `.

Example:
` + "```ts\nconst t = await tx.begin();\nawait t.replaceSymbol(\"pkg/a.go\", \"Service.Run\", P.newRun);\nawait t.edit(\"pkg/b.go\", [{ symbol: \"Init\", target: \"timeout := 5\", replacement: \"timeout := 15\" }]);\nconst v = await t.verify([\"go vet ./...\", \"go test ./pkg/...\"]);\nif (!v.ok) { await t.rollback(); print(v.failures); return { ok: false }; }\nawait t.commit();\nreturn { ok: true, summary: v.summary };\n```",
	},
	{
		ID:      "sh",
		Title:   "sh — Fast Policy-Checked Shell & Concurrent Checks",
		Summary: "sh.exec, sh.run (no shell quoting), sh.checks concurrent batch runner",
		Aliases: []string{"shell", "cmd", "command", "exec", "checks", "run"},
		Body: `- ` + "`await sh.exec(cmd, { cwd?, env?, shell?: \"pwsh\"|\"cmd\"|\"bash\"|\"sh\", timeoutMs?, timeoutSec?, compact?, throwOnError? })`" + `
  - Returns ` + "`{ ok, exitCode, timedOut, durationMs, output, truncated? }`" + ` (non-zero exit returns ` + "`ok: false`" + ` by default; pass ` + "`throwOnError: true`" + ` to throw ` + "`CmdFailed`" + `).
  - ` + "`sh.shell`" + ` reports the active default shell dialect; default timeout is 15s with process-tree / Job Object kill.
  - ` + "`compact: true`" + ` keeps only summary/failure lines — omit it when you need full stdout.
- ` + "`await sh.run(file, args?, { cwd?, env?, stdin?, timeoutMs? }) -> { ok, exitCode, timedOut, stdout, stderr, durationMs }`" + ` (direct argv execution, zero shell quoting issues).
- ` + "`await sh.checks(commands, { cwd?, concurrency?, timeoutSec?, compact? })`" + ` -> ` + "`{ ok, summary, results: [{ name, command, cwd?, ok, exitCode, timedOut, durationMs, output }], failures? }`" + ` (` + "`commands`" + ` accepts strings or ` + "`{ cmd, cwd?, name?, timeoutMs? }`" + `; same shape for ` + "`t.verify`" + `).`,
	},
	{
		ID:      "git",
		Title:   "git — Read-Only Repository Queries",
		Summary: "git.status, diff, log, show, blame, query",
		Aliases: []string{"repo", "blame", "log"},
		Body: `- ` + "`await git.status(repoDir?) -> { isRepo, branch, clean, staged, modified, untracked }`" + `
- ` + "`await git.diff({ staged?, stat?, nameOnly?, ref?, path?, cwd? }) -> string`" + `
- ` + "`await git.log({ n?, grep?, since?, author?, format?, path?, cwd? }) -> string`" + ` (string flags also accepted)
- ` + "`await git.show({ ref?, stat?, nameOnly?, cwd? }) -> string`" + `
- ` + "`await git.blame(path, { cwd?, startLine?, endLine? }) -> string`" + `
- ` + "`await git.query(\"rev-parse HEAD\", { cwd? }) -> string`" + ` (read-only git subcommands incl. ` + "`worktree list`" + `, ` + "`for-each-ref`, `rev-list`, `stash list`, `stash show`" + `, ` + "`branch`" + `, ` + "`tag`" + `, ` + "`remote -v`" + `; destructive git commands are blocked by policy)`,
	},
	{
		ID:            "tasks",
		Title:         "tasks — Background Processes & Daemons",
		Summary:       "spawn/task tools and tasks.* namespace for >15s builds, interactive commands, and daemons",
		Aliases:       []string{"task", "spawn", "daemon", "background", "bg", "logs", "wait"},
		RequiresTasks: true,
		Body: `Use standalone MCP tools ` + "`spawn`" + ` + ` + "`task`" + ` (or ` + "`tasks.*`" + ` inside ` + "`exec`" + `) for ` + "`>15s`" + ` builds/tests, interactive stdin commands, and persistent daemons:
- ` + "`await tasks.spawn(cmd, { cwd?, shell?, env?, daemon?, waitMs?, timeoutSec?, name? })`" + ` -> ` + "`{ taskId, pid, status, daemon, backgrounded, exitCode?, durationMs, output, logFile, nextOffset }`" + `
- ` + "`await tasks.list()`" + ` · ` + "`await tasks.status(taskId, { tail? })`" + `
- ` + "`await tasks.logs(taskId, { tail?, offset? })`" + ` (incremental byte-offset streaming via ` + "`nextOffset`" + `)
- ` + "`await tasks.wait(taskId, { waitMs?, expect?, tail? })`" + ` -> ` + "`{ task, matched, matchLine?, timedOut, output, nextOffset }`" + ` (regex ` + "`expect`" + ` wakes immediately when banner appears!)
- ` + "`await tasks.input(taskId, text, { appendNewline?, closeStdin? })`" + ` · ` + "`await tasks.kill(taskId)`" + ` · ` + "`await tasks.killAll()`" + `

Example (start dev server and wait for ready):
` + "```ts\nconst t = await tasks.spawn(\"npm run dev\", { daemon: true, name: \"web\", waitMs: 1000 });\nconst ready = await tasks.wait(t.taskId, { expect: \"Local:\\\\s+http\", waitMs: 15000 });\nreturn { taskId: t.taskId, matched: ready.matched, banner: ready.matchLine };\n```",
	},
	{
		ID:      "state",
		Title:   "state — Session Memory & Auto-Spill Paging",
		Summary: "Sync state.get/set/delete/keys/clear and state.page(key, n, size?)",
		Aliases: []string{"page", "paging", "spill", "memory", "session"},
		Body: `- All ` + "`state.*`" + ` calls are **synchronous** (no ` + "`await`" + ` needed) and persist across turns within the same MCP session:
  - ` + "`state.set(key, val)`" + ` · ` + "`state.get(key)`" + ` · ` + "`state.delete(key)`" + ` · ` + "`state.keys()`" + ` · ` + "`state.clear()`" + `
- **Auto-spill paging**: When an ` + "`exec`" + ` response exceeds the inline byte cap, the full text is saved in memory under ` + "`'__result_N'`" + ` (` + "`'__last_result'`" + ` aliases the latest spill).
  - Read next page cleanly on line boundaries: ` + "`print(state.page('__result_N', 2))`" + ` (` + "`-1`" + ` = last page; optional 3rd arg ` + "`size`" + `).
  - Prefer explicit ` + "`'__result_N'`" + ` over ` + "`'__last_result'`" + ` when running concurrent ` + "`exec`" + ` calls.`,
	},
	{
		ID:      "mcp",
		Title:   "tools & mcp — Downstream MCP Server Proxy",
		Summary: "Discover and call downstream stdio/HTTP MCP servers via tools.search & mcp.<server>.<tool>",
		Aliases: []string{"tools", "proxy", "downstream", "servers"},
		Body: `- ` + "`await tools.search(query)`" + ` -> discover available built-in and downstream MCP tools
- ` + "`await tools.describe(ref)`" + ` -> inspect exact JSON input schema for a tool
- ` + "`await mcp.servers()`" + ` -> list connected downstream MCP servers
- ` + "`await mcp.<server>.<tool>(args)`" + ` or ` + "`await mcp.call(\"server.tool\", args)`" + ` -> ` + "`{ text, data? }`" + ` (` + "`res.data`" + ` holds auto-parsed JSON or ` + "`structuredContent`" + ` when the tool outputs JSON; ` + "`res.text`" + ` holds raw text; safe for ` + "`Promise.all`" + ` multiplexing!).`,
	},
	{
		ID:      "globals",
		Title:   "Built-in JS/Node Utility Globals",
		Summary: "P/payloads, Buffer, zlib, util, assert, events, crypto, path, URL, timers, process, os",
		Aliases: []string{"global", "stdlib", "buffer", "crypto", "zlib", "util", "assert", "payloads", "sleep", "env"},
		Body: `Available globally in ` + "`exec`" + ` (and via ` + "`import ... from \"node:<mod>\"`" + `):
- ` + "`P`" + ` / ` + "`payloads`" + `: named multiline text/code blocks passed in the ` + "`payloads`" + ` argument
- ` + "`Buffer`" + ` (` + "`utf8`/`base64`/`base64url`/`hex`/`utf16le`" + `, full ` + "`read/write[U]Int{8,16,32}LE/BE`" + `, ` + "`read/writeBig[U]Int64LE/BE`" + `, ` + "`read/writeFloat/DoubleLE/BE`" + `, ` + "`indexOf`/`includes`/`copy`/`swap*`" + `), ` + "`btoa`/`atob`" + `, ` + "`TextEncoder`/`TextDecoder`" + `
- ` + "`zlib`" + ` (` + "`gzipSync`/`gunzipSync`/`deflateSync`/`inflateSync`/`deflateRawSync`/`inflateRawSync`/`unzipSync`" + ` + async), ` + "`util`" + ` (` + "`format`/`inspect`/`promisify`/`parseArgs`/`stripVTControlCharacters`/`types`" + `), ` + "`assert`" + ` (` + "`deepStrictEqual`/`throws`/`rejects`" + `), ` + "`events`" + ` (` + "`EventEmitter`" + `), ` + "`querystring`" + `, ` + "`string_decoder`" + `
- ` + "`path`" + ` (` + "`join`/`resolve`/`relative`/`dirname`/`basename`/`extname`/`parse`/`format`" + `)
- ` + "`crypto`" + ` (` + "`randomUUID`/`randomBytes`/`randomInt`/`createHash`/`createHmac`/`hash`/`timingSafeEqual`" + `), ` + "`URL`/`URLSearchParams`" + `, ` + "`structuredClone`" + `
- ` + "`performance.now()`" + `, ` + "`setTimeout`/`clearTimeout`" + `, ` + "`setInterval`/`clearInterval`" + `, ` + "`setImmediate`" + `, ` + "`queueMicrotask`" + `, ` + "`await sleep(ms)`" + `
- Read-only ` + "`process`" + ` (` + "`platform`" + `, ` + "`arch`" + `, ` + "`pid`" + `, ` + "`hrtime`" + `, ` + "`stdout.write`" + `, ` + "`cwd()`" + `, ` + "`env`" + ` with secret-looking values redacted) and ` + "`os`" + ` (` + "`cpus()`" + `, ` + "`availableParallelism()`" + `, ` + "`endianness()`" + `, ` + "`homedir()`" + `, ` + "`tmpdir()`" + `, ` + "`EOL`" + `)`,
	},
	{
		ID:      "errors",
		Title:   "Semantic Errors & Recovery",
		Summary: "Error names (NotFound, NoMatch, Ambiguous, NeedsApproval, BadArgs, CmdFailed, Timeout) and e.data",
		Aliases: []string{"error", "troubleshoot", "approval", "policy"},
		Body: `Every host error has ` + "`e.name`" + `, ` + "`e.message`" + ` (` + "`\"op: detail\"`" + `), and optional structured ` + "`e.data`" + `:
- ` + "`NotFound`" + `: path or symbol missing — check ` + "`fs.list`" + ` / ` + "`ast.outline`" + `
- ` + "`NoMatch`" + `: edit target not found — re-read target lines (` + "`fs.read`" + ` or ` + "`ast.read`" + `) for exact whitespace
- ` + "`Ambiguous`" + `: multiple edit matches — ` + "`e.data`" + ` includes ` + "`{ lines, symbols }`" + `; pass ` + "`symbol`" + `, ` + "`nth`" + `, or ` + "`startLine`/`endLine`" + `
- ` + "`NeedsApproval`" + ` / ` + "`Denied`" + `: destructive commands (` + "`rm -rf`" + `, ` + "`git push`" + `, ` + "`reset --hard`" + `), credential paths, or out-of-root writes blocked by policy — do NOT rephrase to bypass
- ` + "`BadArgs`" + `: invalid argument shape or regex syntax — escape ` + "`\\\\`" + ` or pass ` + "`literal: true`" + `
- ` + "`Timeout`" + ` / ` + "`CmdFailed`" + `: shell command exceeded deadline or exited non-zero — move ` + "`>15s`" + ` commands to ` + "`spawn` + `task`" + ``,
	},
	{
		ID:      "patterns",
		Title:   "Common One-Turn Recipes",
		Summary: "Parallel research, AST-scoped refactor + verify, and background daemon startup",
		Aliases: []string{"pattern", "examples", "example", "recipes", "recipe"},
		Body: `### 1. Parallel Codebase Survey (` + "`Promise.all`" + `)
` + "```ts\nconst [hits, status, files] = await Promise.all([\n  search.rg(\"HandleAuth\", { glob: \"*.go\" }),\n  git.status(),\n  search.glob(\"pkg/**/*.go\"),\n]);\nreturn { branch: status.branch, files: files.length, hits: hits.slice(0, 10) };\n```" + `

### 2. Low-Token Multi-File Edit + Fast Checks (Default — Keeps Edits on Check Failure)
` + "```ts\nawait edit.apply({\n  edits: [\n    { path: \"pkg/auth.go\", hunks: [{ symbol: \"CheckToken\", replaceSymbol: true, replacement: P.newFn }] },\n    { path: \"pkg/auth_test.go\", hunks: [{ symbol: \"TestCheckToken\", target: \"want := false\", replacement: \"want := true\" }] },\n  ],\n});\nconst check = await sh.checks([\"go vet ./...\", \"go test ./pkg/...\"]);\nreturn check.ok ? { ok: true, summary: check.summary } : { ok: false, failures: check.failures };\n```",
	},
}

func activeTopics(hasTasks bool) []DocTopic {
	out := make([]DocTopic, 0, len(docTopics))
	for _, t := range docTopics {
		if t.RequiresTasks && !hasTasks {
			continue
		}
		out = append(out, t)
	}
	return out
}

func activeToolNames(hasRead, hasTasks, hasDocs bool) []string {
	names := []string{"`exec`"}
	if hasRead {
		names = append(names, "`read`")
	}
	if hasTasks {
		names = append(names, "`spawn`", "`task`")
	}
	if hasDocs {
		names = append(names, "`docs`")
	}
	return names
}

// TopicIndex renders a compact table of contents returned by `docs()` with no arguments.
func TopicIndex(hasRead, hasTasks bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# MCPlex Docs Index\nActive tools: %s\n\n", strings.Join(activeToolNames(hasRead, hasTasks, true), ", "))
	b.WriteString("Call `docs({ topic })` for exact signatures/examples, `docs({ query })` to search by keyword, or `docs({ topic: \"all\" })` for the full manual.\n\n")
	for _, t := range activeTopics(hasTasks) {
		fmt.Fprintf(&b, "- **`%s`** — %s\n", t.ID, t.Summary)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Lookup resolves an on-demand `docs` tool request by topic (exact ID or alias) and/or keyword query.
func Lookup(topic, query string, hasRead, hasTasks bool) string {
	tp := strings.ToLower(strings.TrimSpace(topic))
	q := strings.TrimSpace(query)

	if tp == "" && q == "" {
		return TopicIndex(hasRead, hasTasks)
	}
	if tp == "all" || tp == "full" {
		return strings.TrimRight(FullGuideForTools(hasRead, hasTasks), "\n")
	}

	topics := activeTopics(hasTasks)
	if tp != "" {
		for _, t := range topics {
			if t.ID == tp {
				if q != "" {
					if filtered := searchTopics([]DocTopic{t}, q); filtered != "" {
						return filtered
					}
				}
				return formatTopicSection(t)
			}
			for _, a := range t.Aliases {
				if a == tp {
					if q != "" {
						if filtered := searchTopics([]DocTopic{t}, q); filtered != "" {
							return filtered
						}
					}
					return formatTopicSection(t)
				}
			}
		}
		// Unknown topic name: seamlessly fall back to keyword search so the model never wastes a turn.
		fallbackQuery := tp
		if q != "" {
			fallbackQuery = tp + " " + q
		}
		if hits := searchTopics(topics, fallbackQuery); hits != "" {
			return fmt.Sprintf("No exact topic %q — showing keyword matches:\n\n%s", topic, hits)
		}
		return fmt.Sprintf("Unknown topic %q.\n\n%s", topic, TopicIndex(hasRead, hasTasks))
	}

	if hits := searchTopics(topics, q); hits != "" {
		return hits
	}
	return fmt.Sprintf("No docs matched query %q.\n\n%s", q, TopicIndex(hasRead, hasTasks))
}

func formatTopicSection(t DocTopic) string {
	return fmt.Sprintf("## %s (`topic: %q`)\n\n%s", t.Title, t.ID, strings.TrimSpace(t.Body))
}

func searchTopics(topics []DocTopic, query string) string {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return ""
	}
	type scoredTopic struct {
		topic DocTopic
		score int
	}
	var matched []scoredTopic
	for _, t := range topics {
		hay := strings.ToLower(t.ID + " " + t.Title + " " + t.Summary + " " + strings.Join(t.Aliases, " ") + "\n" + t.Body)
		score := 0
		for _, term := range terms {
			if strings.Contains(hay, term) {
				score += strings.Count(hay, term)
				if t.ID == term {
					score += 20
				}
				if strings.Contains(strings.ToLower(t.Title+" "+t.Summary), term) {
					score += 5
				}
			}
		}
		if score > 0 {
			matched = append(matched, scoredTopic{topic: t, score: score})
		}
	}
	if len(matched) == 0 {
		return ""
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].score > matched[j].score
	})
	if len(matched) > 3 {
		matched = matched[:3]
	}
	var parts []string
	for _, m := range matched {
		parts = append(parts, formatTopicSection(m.topic))
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// FullGuide returns the complete Markdown Code Mode skill manual (served via mcplex://skill and `mcplex skill`).
func FullGuide() string {
	return FullGuideForTools(true, true)
}

// FullGuideForTools dynamically assembles the complete Markdown manual from the single-source topic registry.
func FullGuideForTools(hasRead, hasTasks bool) string {
	return FullGuideForToolset(hasRead, hasTasks, true)
}

// FullGuideForToolset dynamically assembles the complete Markdown manual for the exact active toolset.
func FullGuideForToolset(hasRead, hasTasks, hasDocs bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- Active MCPlex tools: %s -->\n", strings.Join(activeToolNames(hasRead, hasTasks, hasDocs), ", "))
	b.WriteString("# MCPlex — Code Mode Local Tool & MCP Proxy Guide\n\n")
	for i, t := range activeTopics(hasTasks) {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## %s\n\n%s", t.Title, strings.TrimSpace(t.Body))
	}
	b.WriteByte('\n')
	return b.String()
}

// Instructions returns the slim progressive-disclosure instructions returned in MCP initialize.result.instructions.
func Instructions() string {
	return InstructionsForToolset(true, true, true)
}

// InstructionsForTools returns a compact (~600B) routing guide tailored to hasRead and hasTasks (assuming docs is enabled).
func InstructionsForTools(hasRead, hasTasks bool) string {
	return InstructionsForToolset(hasRead, hasTasks, true)
}

// InstructionsForToolset returns a compact (~600B) routing guide tailored to the exact active toolset,
// omitting references to `docs` when `hasDocs` is false (e.g. `--tools exec`).
func InstructionsForToolset(hasRead, hasTasks, hasDocs bool) string {
	var routes []string
	if hasRead {
		routes = append(routes, "- `read`: read whole files, line ranges, tail logs, or images (`.png`/`.jpg`/`.webp`/`.gif`).")
	}
	routes = append(routes, "- `exec`: compose multi-step TypeScript (`top-level await`, `Promise.all`, `P`/`payloads`) with local tools & downstream `mcp.*` in ONE turn.")
	if hasTasks {
		routes = append(routes, "- `spawn` + `task`: run >15s builds/tests, interactive commands, or `daemon: true` services (use `task({ action: \"wait\", taskId, expect })`).")
	}
	if hasDocs {
		routes = append(routes, "- `docs`: instant on-demand reference (`docs()` index, `docs({ topic })`, or `docs({ query })`).")
	}

	ns := "fs, search, ast, edit, tx, git, sh, state, tools/mcp"
	if hasTasks {
		ns = "fs, search, ast, edit, tx, git, sh, tasks, state, tools/mcp"
	}
	docHint := "Read resource `mcplex://skill` if you need full API signatures or examples."
	if hasDocs {
		docHint = "Call `docs({ topic })` (e.g. `edit`, `sh`, `search`, `ast`, `tx`, `errors`, `patterns`) or `docs({ query })` whenever you need exact signatures or examples."
	}
	return fmt.Sprintf(
		"MCPlex Code Mode Proxy:\n%s\n\nInside `exec`, globals include: %s (plus Buffer, crypto, path, URL, sleep, P/payloads). "+
			"Prefer `edit.apply` + `sh.checks`; with `tx`, call `await t.commit()`. %s",
		strings.Join(routes, "\n"),
		ns,
		docHint,
	)
}

// CompactExecDescription returns a concise self-describing tool description for `exec`.
func CompactExecDescription(defaultMaxOutBytes int) string {
	return CompactExecDescriptionForToolset(defaultMaxOutBytes, true, true, true)
}

// CompactExecDescriptionForTools returns a slim (~520B) `exec` tool description (assuming `docs` is enabled).
func CompactExecDescriptionForTools(defaultMaxOutBytes int, hasRead, hasTasks bool) string {
	return CompactExecDescriptionForToolset(defaultMaxOutBytes, hasRead, hasTasks, true)
}

// CompactExecDescriptionForToolset returns a slim (~520B) `exec` tool description tailored to the exact active toolset.
func CompactExecDescriptionForToolset(defaultMaxOutBytes int, hasRead, hasTasks, hasDocs bool) string {
	if defaultMaxOutBytes <= 0 {
		defaultMaxOutBytes = 24576
	}
	ns := "fs, search, ast, edit, tx, git, sh, state, tools/mcp"
	if hasTasks {
		ns = "fs, search, ast, edit, tx, git, sh, tasks, state, tools/mcp"
	}
	siblingHint := ""
	if hasRead && hasTasks {
		siblingHint = " Use `read` for whole files/images and `spawn`+`task` for >15s builds or daemons."
	} else if hasRead {
		siblingHint = " Use `read` for whole files/images."
	} else if hasTasks {
		siblingHint = " Use `spawn`+`task` for >15s builds or daemons."
	}
	docTail := "Read `mcplex://skill` for full API signatures and examples."
	if hasDocs {
		docTail = "Call `docs({ topic })` or `docs({ query })` for exact API signatures, options, and examples."
	}
	return fmt.Sprintf(
		"Execute a top-level async TypeScript body (`await`, `Promise.all`, `print()`, `return`; do not wrap in `async function`) in a fast local Code Mode sandbox. "+
			"Namespaces: %s, plus `P`/`payloads`, Buffer, crypto, path, URL, sleep. "+
			"Inline output cap: %dB (oversized output spills to memory `state.page('__result_N', 2)`).%s "+
			"%s",
		ns,
		defaultMaxOutBytes,
		siblingHint,
		docTail,
	)
}
