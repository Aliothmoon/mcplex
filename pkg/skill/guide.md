# MCPlex — Code Mode Local Tool & MCP Proxy Guide

Use the `exec` tool to compose multi-step file search, AST inspection, atomic transactional edits, git queries, shell checks, and downstream MCP calls inside **ONE TypeScript program turn**. Intermediate data stays in sandbox memory; return only the compact answer you need.

## Core Rules
- Write a **top-level `async` TypeScript body** (`await`, `print()`/`console.log()`, and `return` work directly; do **not** wrap in `async function main() {}`).
- Pass multiline replacement code or templates in `payloads: { key: "..." }` and read via `P.key` / `payloads.key` to avoid JSON/template escaping issues.
- Parallelize independent host calls with `await Promise.all([...])`.
- Always call `await t.commit()` to keep transaction edits; any uncommitted `tx` auto-rolls back when the script ends.
- For multiline text/diffs/logs, prefer `print(...)` without `return` (streams raw unescaped text); use `return { ... }` for compact structured data.
- Oversized output auto-spills in memory to `state.get('__result_N')` (with `'__last_result'` aliasing the latest spill). Continue reading long text with `print(state.page('__result_N', 2))` (`-1` = last page; prefer the explicit `__result_N` key under concurrency).

## Built-in Namespaces

### `fs` — Files, Binary Slices & Streaming Hash
- `await fs.read(path, { startLine?, endLine?, limit?, tail?, lineNumbers? }) -> string` (up to 2 MB; `tail: N` reads last N lines of any size file)
- `await fs.cat(paths, opts?) -> { [path]: string | null }` (parallel multi-read; failed paths return `null` + warning)
- `await fs.write(path, content, { eol?: "lf" | "crlf" })` (preserves existing EOL style by default)
- `fs.exists(path) -> boolean` (**sync**)
- `await fs.bytes(path, { offset?, length? }) -> Buffer` (negative offset = from end; `<= 2 MB`)
- `await fs.hash(path, algoOrOpts?) -> string` (streams any file size; `sha256` default, also `sha512`/`sha1`/`md5`; `hex`/`base64`/`base64url`)
- `await fs.stat(path)` · `await fs.list(path, { detail?, limit? })` (dirs end with `/`) · `await fs.tree(path, depth?)`
- `await fs.mkdir(path)` · `await fs.rm(path, { force? })` · `await fs.rename(from, to)` · `await fs.cp(from, to, { recursive? })` (immediate, not transactional)

### `search` — Ripgrep & Glob
- `await search.rg(pattern, { path?, glob?, literal?, ignoreCase?, contextLines?, maxResults?, filesOnly?, count? })`
  - Default: `Array<{ file, line, column, text, match, before?, after? }>`
  - `{ filesOnly: true }` -> `string[]`; `{ count: true }` -> `{ [file]: n }`
  - In JS strings/templates escape regex backslashes as `\\` or pass `{ literal: true }`.
- `await search.glob(pattern, { path?, maxResults? }) -> string[]` (supports `**` and `{a,b}`)

### `ast` — Symbol Outlines & Symbol Reads
- `await ast.outline(path)` / `await ast.outlines(paths)` -> `{ file, totalLines, symbols: [{ kind, name, signature?, startLine, endLine }] }` (Go `go/parser`, TS/JS, Python, Rust, C/C++, Java, Kotlin, Markdown)
- `await ast.read(path, "Type::method", { lineNumbers? })` -> `{ kind, name, startLine, endLine, text }`
- `await ast.enclosingSymbol(path, line)` -> `{ kind, name, startLine, endLine } | null`

### `edit` & `tx` — Atomic Multi-File Edits & Rollback Transactions
- `await edit.apply({ edits: [{ path, hunks: [{ target, replacement, symbol?, nth?, startLine?, endLine?, replaceAll? }], create?, overwrite? }], dryRun?, verify? })`
  - `symbol` restricts matching to that symbol (including attached leading doc comments & attributes/decorators).
- `await edit.replace(path, target, replacement, { symbol?, nth?, replaceAll? })`
- `const t = await tx.begin();` -> `t.edit(pathOrSpecs, hunks?)`, `t.write(path, content, opts?)`, `t.rm(path)`, `t.rename(from, to)`, `t.diff()`, `t.status()`, `t.verify(commands, opts?)`, `t.commit()`, `t.rollback()`.

### `git` (Read-Only) & `sh` (Policy-Checked Shell)
- `await git.status(dir?)` · `git.diff({ staged?, stat?, nameOnly?, ref?, path? })` · `git.log({ n?, grep?, since?, author?, path? })` · `git.show({ ref?, stat?, nameOnly? })` · `git.blame(path, { startLine?, endLine? })` · `git.query("rev-parse HEAD")`
- `await sh.exec(cmd, { cwd?, env?, shell?, timeoutMs?, timeoutSec?, compact?, throwOnError? }) -> { ok, exitCode, timedOut, durationMs, output, truncated? }` (`sh.shell` = default shell dialect; 15s default timeout + process-tree kill)
- `await sh.run(file, args?, { cwd?, env?, stdin?, timeoutMs? }) -> { ok, exitCode, timedOut, stdout, stderr, durationMs }` (no shell quoting)
- `await sh.checks(commands, { concurrency?, timeoutSec?, compact? }) -> { ok, summary, results: [{ name, command, ok, exitCode, timedOut, durationMs, output }], failures? }` (same shape for `t.verify`)

### `state` (Sync) & `tools` / `mcp` (Downstream MCP Proxy)
- `state.set(k, v)` / `state.get(k)` / `state.delete(k)` / `state.keys()` / `state.clear()` / `state.page(key, n, size?)`
- `await tools.search(query)` · `await tools.describe(ref)` · `await mcp.<server>.<tool>(args)` / `await mcp.call("server.tool", args)`

### Globals & Errors
- Standard globals: `Buffer`, `TextEncoder`/`TextDecoder`, `btoa`/`atob`, `path`, `crypto` (`randomUUID`, `createHash`), `URL`/`URLSearchParams`, `setTimeout`/`clearTimeout`, `await sleep(ms)`, `structuredClone`, read-only `process` (secret env vars redacted) & `os`.
- Semantic error `e.name`: `NotFound` | `NoMatch` | `Ambiguous` | `NeedsApproval` | `Denied` | `Timeout` | `BadArgs` | `CmdFailed` | `Error` (`e.data` holds structured context such as `{ lines, symbols }`).
