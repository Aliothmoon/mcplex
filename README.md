# MCPlex

[中文文档](README_ZH.md)

**MCPlex** is a single-binary, client-agnostic **Code Mode MCP Tool Proxy** and embeddable Go runtime (`pkg/`) powered by a local TypeScript sandbox (Go + esbuild + Goja).

Give **any MCP-only runtime** (Claude Desktop/Code, Cursor, Codex, Windsurf, Cline, Dify, n8n, OpenWebUI, containers, or custom agents) safe, composable access to local tools, multimodal file reads, background tasks/daemons, and downstream MCP servers — over **stdio** or **Streamable HTTP**.

---

## Why Code Mode Proxy Instead of 30 Flat Tools?

- **One turn instead of 10**: Search with `ripgrep`, inspect AST outlines, filter in memory, run atomic multi-file transactions (`tx.begin()` → `edit` → `verify` → `commit`/auto-rollback), query read-only `git`, stream `fs.hash`, run concurrent `sh.checks`, and call downstream `mcp.*` servers inside ONE TypeScript program.
- **Zero manual Skill setup**: Pure MCP clients automatically receive the API guide via four progressive-disclosure layers:
  1. `initialize.result.instructions` (MCP `2025-03-26+`)
  2. Self-describing `exec` tool description (works even in minimal clients that ignore `instructions`/`resources`)
  3. On-demand `docs` MCP tool (`docs()` index, `docs({ topic })`, `docs({ query })`) for exact namespace signatures & recipes
  4. `mcplex://skill` MCP Resource (`resources/list` + `resources/read`) & `mcplex skill` CLI command
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
  --mcp-config ./downstream-mcp.json
```
- Endpoint: `POST http://127.0.0.1:8787/mcp` (`Authorization: Bearer your-secret-token`)
- Health probe: `GET http://127.0.0.1:8787/healthz`
- Live event stream: `GET http://127.0.0.1:8787/events` (SSE) and `GET http://127.0.0.1:8787/api/events`

### 3. Interactive Live TUI (`--ui` / `mcplex ui`)
Launch the GrokNight dark interactive console to watch live colorized CRUD diffs, real-time streaming shell/task output, and inspect full files in a floating `less`-style search modal:
```bash
# Start HTTP server + embedded interactive TUI (defaults to 127.0.0.1:8787)
mcplex serve --ui --cwd /path/to/workspace

# Or attach to a remote/container mcplex HTTP instance
mcplex ui http://127.0.0.1:8787 --token "your-secret-token"
```

### 4. Expose to OAuth-only MCP clients (Gemini custom apps) via Cloudflare Tunnel
Clients such as the Gemini web app only connect to remote MCP servers over HTTPS with **OAuth** (dynamic client registration, no custom headers). `--oauth` turns MCPlex into a standards-based OAuth 2.1 authorization server ([MCP Authorization spec](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization): RFC 9728 / 8414 / 7591 / 8707 / 7009, PKCE S256):
```bash
# 1. Expose 127.0.0.1:8787 at a stable hostname (named tunnel on your own Cloudflare domain)
cloudflared tunnel login   # once: authorize the Cloudflare zone that owns example.com
cloudflared tunnel --name mcplex --hostname mcp.example.com --url http://127.0.0.1:8787

# 2. Serve MCPlex with interactive TUI (auto-generates an owner token if --token is omitted!)
mcplex serve --ui --oauth --public-url https://mcp.example.com \
  --cwd /path/to/workspace

# (Or headless without TUI — pass an explicit --token)
mcplex serve --http 127.0.0.1:8787 --oauth --public-url https://mcp.example.com \
  --token "$(openssl rand -hex 24)" --cwd /path/to/workspace
```
Before adding it to Gemini, verify the public URL end to end with the official MCP SDK client (discovery, DCR, PKCE, consent, tools, refresh, revocation):
```bash
cd scripts/oauth-interop && npm install
MCPLEX_TOKEN="<your --token>" node check.mjs https://mcp.example.com
```

3. In Gemini: **Settings → Connected Apps → Add a custom app**, enter `https://mcp.example.com/mcp` (press `u` or click `URL ⧉` in `--ui` to copy). Your browser opens the MCPlex consent page; press `t` or click `Token ••••xxxx ⧉` in `--ui` to copy the owner secret (or `T` / `↻` to rotate it live), paste it, and click **Approve**.

- The token is the owner secret for the consent page and still works as a plain `Authorization: Bearer` token for header-capable clients (Claude Code, Cursor, `mcplex ui`).
- Only Google's account-linking redirect URIs are accepted by default; add more with `--oauth-redirect-allow <pattern>` (`loopback` allows local native clients).
- Refresh tokens are long-lived for confidential clients (Gemini "persistent linking") and rotate for public clients. Clients/grants persist in `<data-dir>/oauth-server/` (hashed, unreadable from the sandbox). List or revoke them with `mcplex oauth clients` / `mcplex oauth revoke <client-id|all>` — revocation applies to a running server.
- The OAuth issuer is bound to `--public-url`: a changing hostname (e.g. a random `*.trycloudflare.com` quick tunnel) breaks existing links, so prefer a named tunnel. For header-token clients only, `--public-url` without `--oauth` just allows the tunnel hostname.
- Every tool declares MCP annotations (`read`/`docs` read-only; `exec`/`spawn`/`task` destructive) so clients ask for confirmation before running commands. Anyone who completes the consent step can run code on this machine: use a strong random token, and optionally pass `--strict-root` or `--tools` if you want to restrict scope.

---

## Exposed MCP Tools (`--tools`)

By default, `mcplex serve` runs with `--tools all` and exposes **5 orthogonal tools** (pass `--tools exec` for single-tool Code Mode inside hosts that already provide native file/terminal tools):

| MCP Tool | Purpose |
| :--- | :--- |
| `exec` | Fast (`<15s`) multi-step TypeScript Code Mode sandbox composing search, AST, atomic transactions, git, shell checks, `tasks.*`, and downstream `mcp.*` in one turn |
| `read` | Direct parallel file reader (up to 800 lines / 48 KB per call with `startLine`/`endLine`/`tail`/`byteOffset`) + native MCP `image` blocks for `.png`/`.jpg`/`.webp`/`.gif` |
| `spawn` | Start `>15s` builds, full test suites, interactive commands, or persistent `daemon: true` processes (sync `waitMs` window -> automatic background handoff with process-tree / Job Object cleanup) |
| `task` | Manage spawned tasks: `list`, `status`, incremental `logs` (`offset`/`tail`), `wait` (with instant `expect` regex readiness wakeup), stdin `input`, `kill`, `kill_all` |
| `docs` | Instant on-demand API & pattern lookup (`docs()` index, `docs({ topic })` by namespace, `docs({ query })` keyword search) so clients keep upfront prompt overhead minimal |

---

## Built-in Sandbox Capabilities (`exec`)

| Namespace | Capabilities |
| :--- | :--- |
| `fs` | `read` (range/`tail`), `cat` (parallel), `write` (EOL-preserving), `exists` (sync), `bytes` (`<=2MB`), `hash` (streaming `sha256`/`sha512`/`sha1`/`md5` for any file size), `stat`, `list`, `tree`, `mkdir`, `rm`, `rename`, `cp` |
| `search` | `rg` (ripgrep + Go fallback, `filesOnly`, `count`, `contextLines`, `BadArgs` regex hints), `glob` (with `**` & `{a,b}`) |
| `ast` | `outline`, `outlines`, `read` (`Type::method`), `enclosingSymbol` (Go `go/parser`, TS/JS, Python, Rust, C/C++, Java, Kotlin, Markdown) |
| `edit` & `tx` | Atomic multi-file/multi-hunk edits (doc-comment-aware `symbol` scoping, CRLF preservation) + in-memory snapshot `tx.begin()` (`edit`, `write`, `rm`, `rename`, `verify`, `diff`, `commit`, auto-rollback) |
| `git` | Read-only `status`, `diff`, `log`, `show`, `blame`, `query` |
| `sh` | Policy-checked `exec`, `run` (no shell quoting), `checks` (concurrent check pool returning `{ ok, summary, results: [{ name, command, ok, exitCode, timedOut, durationMs, output }], failures? }`) |
| `tasks` | Background task & daemon orchestration from inside scripts: `spawn`, `list`, `status`, `logs`, `wait` (`expect` regex), `input`, `kill`, `killAll` |
| `state` | Sync `get`, `set`, `delete`, `keys`, `clear`, and key-bound `page(key, n, size?)` |
| `tools` & `mcp` | Built-in + active live `tools/list` discovery and pooled stdio/HTTP proxy for downstream MCP servers (`mcp.<server>.<tool>(args)`) |

Print the complete embedded Skill guide anytime with `mcplex skill`.
