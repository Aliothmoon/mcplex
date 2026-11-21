# MCPlex

[中文文档](README_ZH.md)

**MCPlex** is a single-binary, client-agnostic **Code Mode MCP Tool Proxy** and embeddable Go runtime (`pkg/`) powered by a local TypeScript sandbox (Go + esbuild + Goja).

Give **any MCP-only runtime** (Claude Desktop/Code, Cursor, Codex, Windsurf, Cline, Dify, n8n, OpenWebUI, containers, or custom agents) safe, composable access to local tools and downstream MCP servers — all through **one self-describing `exec` tool** over **stdio** or **Streamable HTTP**.

---

## Why Code Mode Proxy Instead of 30 Flat Tools?

- **One turn instead of 10**: Search with `ripgrep`, inspect AST outlines, filter in memory, run atomic multi-file transactions (`tx.begin()` → `edit` → `verify` → `commit`/auto-rollback), query read-only `git`, stream `fs.hash`, run concurrent `sh.checks`, and call downstream `mcp.*` servers inside ONE TypeScript program.
- **Zero manual Skill setup**: Pure MCP clients automatically receive the API guide via three layers:
  1. `initialize.result.instructions` (MCP `2025-03-26+`)
  2. Self-describing `exec` tool description (works even in minimal clients that ignore `instructions`/`resources`)
  3. `mcplex://skill` MCP Resource (`resources/list` + `resources/read`) & `mcplex skill` CLI command
- **Concurrent-safe & zero disk spills**: Oversized outputs auto-spill to a process-local memory pool (`__result_N`) and paginate cleanly at line boundaries via `state.page('__result_N', 2)`.
- **Embeddable Go Core (`pkg/`)**: Downstream host plugins (such as [`agy-ptc`](https://github.com/aliothmoon/agy-ptc)) import `github.com/aliothmoon/mcplex/pkg/...` directly so there is only ONE core to maintain.

---

## Quick Start

### Build CLI
```bash
go build -o mcplex ./cmd/mcplex
```

### 1. Stdio MCP Proxy (`mcplex serve`)
Add to any MCP client (Claude Desktop, Cursor, Codex, Cline, etc.):
```json
{
  "mcpServers": {
    "mcplex": {
      "command": "/path/to/mcplex",
      "args": [
        "serve",
        "--cwd", "/path/to/your/workspace",
        "--strict-root",
        "--mcp-config", "/path/to/downstream-mcp.json"
      ]
    }
  }
}
```

### 2. Remote / Container Streamable HTTP Proxy (`mcplex serve --http`)
```bash
mcplex serve \
  --http 127.0.0.1:8787 \
  --token "your-secret-token" \
  --cwd /path/to/workspace \
  --strict-root \
  --mcp-config ./downstream-mcp.json
```
- Endpoint: `POST http://127.0.0.1:8787/mcp` (`Authorization: Bearer your-secret-token`)
- Health probe: `GET http://127.0.0.1:8787/healthz`

---

## Built-in Sandbox Capabilities

| Namespace | Capabilities |
| :--- | :--- |
| `fs` | `read` (range/`tail`), `cat` (parallel), `write` (EOL-preserving), `exists` (sync), `bytes` (`<=2MB`), `hash` (streaming `sha256`/`sha512`/`sha1`/`md5` for any file size), `stat`, `list`, `tree`, `mkdir`, `rm`, `rename`, `cp` |
| `search` | `rg` (ripgrep + Go fallback, `filesOnly`, `count`, `contextLines`, `BadArgs` regex hints), `glob` (with `**` & `{a,b}`) |
| `ast` | `outline`, `outlines`, `read` (`Type::method`), `enclosingSymbol` (Go `go/parser`, TS/JS, Python, Rust, C/C++, Java, Kotlin, Markdown) |
| `edit` & `tx` | Atomic multi-file/multi-hunk edits (doc-comment-aware `symbol` scoping, CRLF preservation) + in-memory snapshot `tx.begin()` (`edit`, `write`, `rm`, `rename`, `verify`, `diff`, `commit`, auto-rollback) |
| `git` | Read-only `status`, `diff`, `log`, `show`, `blame`, `query` |
| `sh` | Policy-checked `exec`, `run` (no shell quoting), `checks` (concurrent check pool returning `{ ok, summary, results: [{ name, command, ok, exitCode, timedOut, durationMs, output }], failures? }`) |
| `state` | Sync `get`, `set`, `delete`, `keys`, `clear`, and key-bound `page(key, n, size?)` |
| `tools` & `mcp` | Built-in + active live `tools/list` discovery and pooled stdio/HTTP proxy for downstream MCP servers (`mcp.<server>.<tool>(args)`) |

Print the complete embedded Skill guide anytime with `mcplex skill`.
