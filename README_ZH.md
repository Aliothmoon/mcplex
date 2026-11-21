# MCPlex

[English](README.md)

**MCPlex** 是一个单二进制、宿主无关的 **Code Mode MCP Tool Proxy** 与可嵌入 Go 核心库（`pkg/`），底层基于本地 TypeScript 沙箱（Go + esbuild + Goja）。

它可以让**任意只支持 MCP 的运行环境**（Claude Desktop/Code、Cursor、Codex、Windsurf、Cline、Dify、n8n、OpenWebUI、容器或自研 Agent）经由 **stdio** 或 **Streamable HTTP** 安全地获得本地 Code Mode 编排、多模态文件直读、后台长任务/常驻服务管控与下游 MCP 代理能力。

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
- 实时事件流：`GET http://127.0.0.1:8787/events`（SSE）与 `GET http://127.0.0.1:8787/api/events`

### 3. 交互式实时 TUI（`--ui` / `mcplex ui`）
开启 GrokNight 暗色交互终端，实时查看带颜色行内高亮的 CRUD Diff、`shell`/`task` 流式日志输出，并支持鼠标点击与 `less` 风格悬浮弹窗全文检索：
```bash
# 一键启动 HTTP 服务并进入交互式 TUI（默认监听 127.0.0.1:8787）
mcplex serve --ui --cwd /path/to/workspace

# 或远程挂载到正在运行的 mcplex HTTP 服务
mcplex ui http://127.0.0.1:8787 --token "your-secret-token"
```

---

## 对外暴露的 MCP 工具集（`--tools`）

`mcplex serve` 默认启用 `--tools all`，暴露 **5 个正交工具**（若宿主已自带文件与终端工具，可传 `--tools exec` 切换为单工具纯 Code Mode）：

| MCP 工具 | 职责 |
| :--- | :--- |
| `exec` | `<15s` 短平快多步 TypeScript 沙箱，一轮完成搜索、AST、事务改码、只读 git、并发校验、`tasks.*` 与下游 `mcp.*` 编排 |
| `read` | 独立大预算并发文件直读（单次最高 800 行 / 48 KB，支持 `startLine`/`endLine`/`tail`/`byteOffset`）及 `.png`/`.jpg`/`.webp`/`.gif` 原生 MCP 多模态图片返回 |
| `spawn` | 启动 `>15s` 重构建、全量测试、交互式命令或常驻服务（`daemon: true`）；支持 `waitMs` 同步等待窗口，超时自动转后台并配备进程树/Windows Job Object 兜底清理 |
| `task` | 后台任务统一管理：`list`、`status`、按字节 `offset`/`tail` 增量拉取 `logs`、带正则 `expect` 毫秒级唤醒的 `wait`、交互式 `input`、`kill` 与 `kill_all` |
| `docs` | 零开销按需文档查询（无参查目录、`{ topic }` 查命名空间精确签名与示例、`{ query }` 关键词检索），大幅降低握手期上下文占用 |

---

## 内置沙箱能力一览（`exec`）

| 命名空间 | 核心能力 |
| :--- | :--- |
| `fs` | `read`（行范围/`tail`）、`cat`（并行）、`write`（保留换行符）、`exists`（同步）、`bytes`（`<=2MB`）、`hash`（流式 `sha256`/`sha512`/`sha1`/`md5`，支持任意大文件）、`stat`、`list`、`tree`、`mkdir`、`rm`、`rename`、`cp` |
| `search` | `rg`（ripgrep + Go 回退，支持 `filesOnly`/`count`/`contextLines` 及无效正则 `BadArgs` 转义提示）、`glob`（支持 `**` 与 `{a,b}`） |
| `ast` | `outline`、`outlines`、`read`（`Type::method`）、`enclosingSymbol`（支持 Go `go/parser`、TS/JS、Python、Rust、C/C++、Java、Kotlin、Markdown） |
| `edit` & `tx` | 原子多文件替换（`symbol` 作用域自动兼容前置 doc comment 与属性/装饰器）+ 内存快照事务 `tx.begin()`（`edit`/`write`/`rm`/`rename`/`verify`/`diff`/`commit`/自动回滚） |
| `git` | 只读 `status`、`diff`、`log`、`show`、`blame`、`query` |
| `sh` | 策略受控的 `exec`、`run`（无 shell 引号问题）、`checks`（并发校验池，返回 `{ ok, summary, results: [{ name, command, ok, exitCode, timedOut, durationMs, output }], failures? }`） |
| `tasks` | 沙箱内后台任务与常驻进程联动：`spawn`、`list`、`status`、`logs`、`wait`（支持 `expect` 正则）、`input`、`kill`、`killAll` |
| `state` | 同步 `get`/`set`/`delete`/`keys`/`clear` 及键绑定分页 `page(key, n, size?)` |
| `tools` & `mcp` | 内置能力目录 + 下游 MCP 服务器主动 `tools/list` 发现与 stdio/HTTP 代理调用（`mcp.<server>.<tool>(args)`） |

随时运行 `mcplex skill` 查看完整内置 Skill 文档。
