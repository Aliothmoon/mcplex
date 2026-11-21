# MCPlex

[English](README.md)

**MCPlex** 是一个单二进制、宿主无关的 **Code Mode MCP Tool Proxy** 与可嵌入 Go 核心库（`pkg/`），底层基于本地 TypeScript 沙箱（Go + esbuild + Goja）。

它可以让**任意只支持 MCP 的运行环境**（Claude Desktop/Code、Cursor、Codex、Windsurf、Cline、Dify、n8n、OpenWebUI、容器或自研 Agent）通过**一个自描述的 `exec` 工具**，经由 **stdio** 或 **Streamable HTTP** 安全地组合调用本地工具与下游 MCP 服务器。

---

## 为什么是 Code Mode Proxy？

- **一轮完成多步编排**：在一次 TypeScript 调用里并行执行 `ripgrep` 检索、AST 符号读取、内存过滤、带自动回滚的多文件事务编辑（`tx.begin()`）、只读 `git`、流式 `fs.hash`、并发 `sh.checks` 以及下游 `mcp.*` 调用。
- **纯 MCP 宿主零配置即可上手**：通过三层协议能力自动把 Skill 交给模型：
  1. `initialize.result.instructions`（MCP `2025-03-26+` 自动注入）
  2. `exec` 工具描述自带精简 API 速查（即使宿主忽略 `instructions` 和 `resources` 也能一把写对）
  3. `mcplex://skill` MCP Resource（`resources/list` + `resources/read`）及 `mcplex skill` 命令
- **并发安全 & 零落盘溢出**：超长输出自动存入进程内独立内存池（`__result_N`），按行分页并通过 `state.page('__result_N', 2)` 绑定键翻页，并发不串数据。
- **单核复用（Go Module `pkg/`）**：下游宿主插件（如 [`agy-ptc`](https://github.com/aliothmoon/agy-ptc)）直接通过 `go.mod` 引用 `github.com/aliothmoon/mcplex/pkg/...`，全生态只维护一套 Core。

---

## 快速开始

### 编译 CLI
```bash
go build -o mcplex ./cmd/mcplex
```

### 1. Stdio MCP Proxy（`mcplex serve`）
配进任意支持 MCP 的客户端：
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

### 2. 远程 / 容器 Streamable HTTP Proxy（`mcplex serve --http`）
```bash
mcplex serve \
  --http 127.0.0.1:8787 \
  --token "your-secret-token" \
  --cwd /path/to/workspace \
  --strict-root \
  --mcp-config ./downstream-mcp.json
```
- MCP 端点：`POST http://127.0.0.1:8787/mcp`（请求头携带 `Authorization: Bearer your-secret-token`）
- 健康检查：`GET http://127.0.0.1:8787/healthz`

---

## 内置沙箱能力一览

| 命名空间 | 核心能力 |
| :--- | :--- |
| `fs` | `read`（行范围/`tail`）、`cat`（并行）、`write`（保留换行符）、`exists`（同步）、`bytes`（`<=2MB`）、`hash`（流式 `sha256`/`sha512`/`sha1`/`md5`，支持任意大文件）、`stat`、`list`、`tree`、`mkdir`、`rm`、`rename`、`cp` |
| `search` | `rg`（ripgrep + Go 回退，支持 `filesOnly`/`count`/`contextLines` 及无效正则 `BadArgs` 转义提示）、`glob`（支持 `**` 与 `{a,b}`） |
| `ast` | `outline`、`outlines`、`read`（`Type::method`）、`enclosingSymbol`（支持 Go `go/parser`、TS/JS、Python、Rust、C/C++、Java、Kotlin、Markdown） |
| `edit` & `tx` | 原子多文件替换（`symbol` 作用域自动兼容前置 doc comment 与属性/装饰器）+ 内存快照事务 `tx.begin()`（`edit`/`write`/`rm`/`rename`/`verify`/`diff`/`commit`/自动回滚） |
| `git` | 只读 `status`、`diff`、`log`、`show`、`blame`、`query` |
| `sh` | 策略受控的 `exec`、`run`（无 shell 引号问题）、`checks`（并发校验池，返回 `{ ok, summary, results: [{ name, command, ok, exitCode, timedOut, durationMs, output }], failures? }`） |
| `state` | 同步 `get`/`set`/`delete`/`keys`/`clear` 及键绑定分页 `page(key, n, size?)` |
| `tools` & `mcp` | 内置能力目录 + 下游 MCP 服务器主动 `tools/list` 发现与 stdio/HTTP 代理调用（`mcp.<server>.<tool>(args)`） |

随时运行 `mcplex skill` 查看完整内置 Skill 文档。
