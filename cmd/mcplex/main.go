// Command mcplex is a single-binary, client-agnostic Code Mode MCP Tool Proxy:
// it exposes local filesystem, ripgrep, AST outlines, transactional edits, read-only git,
// policy-checked shell checks, session state, and downstream MCP servers through one
// self-describing TypeScript sandbox over stdio or Streamable HTTP.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/aliothmoon/mcplex/pkg/audit"
	"github.com/aliothmoon/mcplex/pkg/mcpserver"
	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
	"github.com/aliothmoon/mcplex/pkg/skill"
	"github.com/aliothmoon/mcplex/pkg/version"
)

const (
	defaultGenericMaxOutputBytes = 24576
	skillResourceURI             = "mcplex://skill"
)

type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ",") }
func (s *stringListFlag) Set(v string) error {
	if t := strings.TrimSpace(v); t != "" {
		*s = append(*s, t)
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	switch sub {
	case "serve", "mcp":
		runServe(args)
	case "run":
		runStandalone(args)
	case "skill":
		runSkill(args)
	case "logs":
		runLogs(args, false)
	case "errors":
		runLogs(args, true)
	case "stats":
		runStats(args)
	case "-h", "--help", "help":
		printUsage()
	case "-v", "--version", "version":
		fmt.Printf("mcplex v%s (%s/%s)\n", version.Version, runtime.GOOS, runtime.GOARCH)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand '%s'\n\n", sub)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`mcplex — Universal Code Mode MCP Tool Proxy (Single-Core Local Tools + Downstream MCP Aggregator)

Usage:
  mcplex <command> [flags]

Commands:
  serve [flags]                          Start the MCP proxy server (stdio default, or --http addr)
  mcp [flags]                            Alias for 'serve'
  run [--cwd <dir>] [--code <ts> | file] Execute a TypeScript Code Mode script directly from CLI
  skill [--tools all|exec]               Print the embedded Code Mode Skill guide (Markdown)
  logs [-e] [--tail N] [--full]          Inspect recorded execution/error JSONL logs
  errors [--tail N]                      Shortcut for 'logs --errors'
  stats                                  Summarize execution success rates and error hotspots
  version                                Print version

Common serve flags:
  --tools <spec>           Exposed MCP tools: all (default: exec,read,spawn,task), exec, or comma list
  --cwd <dir>              Default workspace root directory (defaults to current working dir)
  --strict-root            Confine all file and shell access strictly to --cwd (+ --allow-root)
  --allow-root <dir>       Additional allowed root directory (repeatable)
  --mcp-config <file>      Downstream mcpServers JSON config file to proxy/aggregate (repeatable)
  --policy-file <file>     Custom JSON policy rules file (repeatable)
  --trust-workspace-mcp    Opt in to loading <workspace>/.agents/mcp_config.json
  --data-dir <dir>         Persistent state/logs directory (default: ~/.config/mcplex)
  --max-output <bytes>     Default inline response cap (default: 24576)
  --compact-schema         Use brief exec tool description (rely on initialize.instructions / mcplex://skill)
  --http <addr>            Serve MCP Streamable HTTP instead of stdio (e.g. 127.0.0.1:8787)
  --http-path <path>       HTTP endpoint path (default: /mcp)
  --token <secret>         Require Authorization: Bearer <secret> on HTTP requests (or MCPLEX_TOKEN env)
  --cors-origin <origin>   Optional Access-Control-Allow-Origin header for HTTP mode`)
}

func parseToolSet(spec string) (mcpserver.ToolSet, error) {
	raw := strings.ToLower(strings.TrimSpace(spec))
	if raw == "" || raw == "all" {
		return mcpserver.ToolSet{Read: true, Tasks: true}, nil
	}
	if raw == "exec" || raw == "minimal" {
		return mcpserver.ToolSet{}, nil
	}
	var ts mcpserver.ToolSet
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		switch p {
		case "", "exec":
			// exec is always enabled
		case "all":
			ts.Read = true
			ts.Tasks = true
		case "read":
			ts.Read = true
		case "tasks", "task", "spawn":
			ts.Tasks = true
		default:
			return mcpserver.ToolSet{}, fmt.Errorf("unknown tool %q (allowed: all, exec, read, tasks, spawn, task)", p)
		}
	}
	return ts, nil
}

func runSkill(args []string) {
	fs := flag.NewFlagSet("skill", flag.ExitOnError)
	toolsFlag := fs.String("tools", "all", "Tool profile to render guide for (all, exec, or comma-separated list)")
	_ = fs.Parse(args)
	ts, err := parseToolSet(*toolsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --tools value: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(skill.FullGuideForTools(ts.Read, ts.Tasks))
}

func defaultDataDir() string {
	if env := strings.TrimSpace(os.Getenv("MCPLEX_DATA_DIR")); env != "" {
		return env
	}
	if cfgDir, err := os.UserConfigDir(); err == nil && cfgDir != "" {
		return filepath.Join(cfgDir, "mcplex")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".config", "mcplex")
	}
	return filepath.Join(os.TempDir(), "mcplex")
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	toolsFlag := fs.String("tools", "all", "Exposed MCP tools: all (exec,read,spawn,task), exec, or comma-separated list")
	dataDir := fs.String("data-dir", defaultDataDir(), "Persistent state and audit log directory")
	cwd := fs.String("cwd", "", "Default workspace directory")
	strictRoot := fs.Bool("strict-root", false, "Confine all file/shell access strictly to workspace roots")
	trustWsMCP := fs.Bool("trust-workspace-mcp", false, "Opt in to <workspace>/.agents/mcp_config.json")
	maxOut := fs.Int("max-output", defaultGenericMaxOutputBytes, "Default inline output byte cap")
	compactSchema := fs.Bool("compact-schema", false, "Use concise exec tool description instead of inline API cheat sheet")
	httpAddr := fs.String("http", "", "Serve Streamable HTTP on this address (e.g. 127.0.0.1:8787) instead of stdio")
	httpPath := fs.String("http-path", "/mcp", "HTTP endpoint path for Streamable HTTP mode")
	token := fs.String("token", os.Getenv("MCPLEX_TOKEN"), "Bearer token required in HTTP mode")
	corsOrigin := fs.String("cors-origin", "", "Optional Access-Control-Allow-Origin header in HTTP mode")

	var extraRoots, mcpConfigs, policyFiles stringListFlag
	fs.Var(&extraRoots, "allow-root", "Additional allowed workspace root (repeatable)")
	fs.Var(&mcpConfigs, "mcp-config", "Downstream mcpServers JSON config path (repeatable)")
	fs.Var(&policyFiles, "policy-file", "Custom JSON policy file (repeatable)")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	toolSet, err := parseToolSet(*toolsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --tools value: %v\n", err)
		os.Exit(1)
	}

	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DataDir:               *dataDir,
		DefaultCwd:            *cwd,
		DefaultMaxOutputBytes: *maxOut,
		StrictWorkspace:       *strictRoot,
		ExtraRoots:            extraRoots,
		PolicyFiles:           policyFiles,
		MCPConfigPaths:        mcpConfigs,
		TrustWorkspaceMCP:     *trustWsMCP,
		AntigravityMode:       false,
		EnableTasks:           toolSet.Tasks,
	})
	defer eng.Close()

	execDesc := skill.CompactExecDescriptionForTools(*maxOut, toolSet.Read, toolSet.Tasks)
	if *compactSchema {
		execDesc = "Execute a TypeScript async body in a local Code Mode sandbox composing fs, search, ast, edit, tx, git, sh, state, tools, and downstream mcp servers in ONE turn. Read mcplex://skill for the full API reference."
	}

	srv := mcpserver.NewServerWithOptions(eng, mcpserver.ServerOptions{
		ServerName:         "mcplex",
		Tools:              toolSet,
		Instructions:       skill.InstructionsForTools(toolSet.Read, toolSet.Tasks),
		ExecDescription:    execDesc,
		DefaultMaxOutBytes: *maxOut,
		SkillResourceURI:   skillResourceURI,
		SkillResourceName:  "MCPlex Code Mode Skill",
		SkillResourceText:  skill.FullGuideForTools(toolSet.Read, toolSet.Tasks),
	})

	if strings.TrimSpace(*httpAddr) != "" {
		if *token == "" {
			host, _, splitErr := net.SplitHostPort(*httpAddr)
			if splitErr != nil {
				host = *httpAddr
			}
			h := strings.ToLower(strings.TrimSpace(host))
			isLoopback := h == "localhost" || (net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback())
			if !isLoopback {
				fmt.Fprintf(os.Stderr, "[WARN] mcplex HTTP is binding non-loopback address %q WITHOUT --token authentication! Pass --token <secret> or bind to 127.0.0.1.\n", *httpAddr)
			}
		}
		fmt.Fprintf(os.Stderr, "mcplex v%s listening on http://%s%s (auth=%v, strict-root=%v)\n",
			version.Version, *httpAddr, *httpPath, *token != "", *strictRoot)
		err := srv.ServeHTTP(ctx, mcpserver.HTTPOptions{
			Addr:        *httpAddr,
			Path:        *httpPath,
			Token:       *token,
			AllowOrigin: *corsOrigin,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "mcplex http error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "mcplex stdio error: %v\n", err)
		os.Exit(1)
	}
}

func runStandalone(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cwd := fs.String("cwd", "", "Workspace directory")
	dataDir := fs.String("data-dir", defaultDataDir(), "State and log directory")
	codeFlag := fs.String("code", "", "Inline TypeScript code to execute")
	strictRoot := fs.Bool("strict-root", false, "Confine access strictly to workspace")
	timeoutMs := fs.Int("timeout", 20000, "Timeout in milliseconds")
	maxOut := fs.Int("max-output", 65536, "Max output UTF-8 bytes")
	var mcpConfigs, policyFiles stringListFlag
	fs.Var(&mcpConfigs, "mcp-config", "Downstream mcpServers JSON config path (repeatable)")
	fs.Var(&policyFiles, "policy-file", "Custom JSON policy file (repeatable)")
	_ = fs.Parse(args)

	code := *codeFlag
	if code == "" && fs.NArg() > 0 {
		raw, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed reading %s: %v\n", fs.Arg(0), err)
			os.Exit(1)
		}
		code = string(raw)
	}
	if code == "" {
		raw, _ := io.ReadAll(os.Stdin)
		code = string(raw)
	}
	if strings.TrimSpace(code) == "" {
		fmt.Fprintln(os.Stderr, "error: provide --code '...' or a .ts script file")
		os.Exit(1)
	}

	eng := ptcruntime.NewEngineWithOptions(ptcruntime.EngineOptions{
		DataDir:               *dataDir,
		DefaultCwd:            *cwd,
		DefaultMaxOutputBytes: *maxOut,
		StrictWorkspace:       *strictRoot,
		PolicyFiles:           policyFiles,
		MCPConfigPaths:        mcpConfigs,
		AntigravityMode:       false,
		EnableTasks:           true,
	})
	defer eng.Close()

	out, isErr := eng.RunMCPExec(context.Background(), ptcruntime.ExecInput{
		Code:           code,
		Cwd:            *cwd,
		Title:          "mcplex-run",
		TimeoutMs:      *timeoutMs,
		MaxOutputBytes: *maxOut,
	})
	fmt.Println(out)
	if isErr {
		os.Exit(1)
	}
}

func runLogs(args []string, forceErrors bool) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	errorsOnly := fs.Bool("errors", forceErrors, "Show only runs with errors")
	shortErr := fs.Bool("e", forceErrors, "Alias for --errors")
	full := fs.Bool("full", false, "Print full TypeScript source and output")
	tail := fs.Int("tail", 20, "Number of recent entries")
	runID := fs.String("id", "", "Inspect a specific run ID")
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory")
	asJSON := fs.Bool("json", false, "Output JSON")
	_ = fs.Parse(args)

	entries, files, err := audit.LoadEntries(*dataDir, *errorsOnly || *shortErr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logs error: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(entries, "", "  ")
		fmt.Println(string(raw))
		return
	}
	if *full && *runID == "" {
		start := 0
		if *tail > 0 && len(entries) > *tail {
			start = len(entries) - *tail
		}
		for _, e := range entries[start:] {
			fmt.Print(audit.FormatFullStreamCard(e))
		}
		return
	}
	fmt.Print(audit.FormatLogs(entries, files, *tail, *runID))
}

func runStats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory")
	_ = fs.Parse(args)
	entries, _, err := audit.LoadEntries(*dataDir, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stats error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(audit.FormatStats(entries))
}
