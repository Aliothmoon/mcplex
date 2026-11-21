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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/aliothmoon/mcplex/pkg/audit"
	"github.com/aliothmoon/mcplex/pkg/mcpserver"
	ptcruntime "github.com/aliothmoon/mcplex/pkg/runtime"
	"github.com/aliothmoon/mcplex/pkg/skill"
	"github.com/aliothmoon/mcplex/pkg/tui"
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
	case "ui":
		runUI(args)
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
	fmt.Println(`mcplex — Universal Code Mode MCP Tool Proxy (Local Tools + Downstream MCP Aggregator)

Usage:
  mcplex <command> [flags]

Commands:
  serve [flags]              Start MCP proxy (stdio default; --http or --ui for HTTP)
  mcp [flags]                Alias for 'serve'
  ui [url]                   Attach interactive TUI to a running HTTP server
  run [--code <ts> | file]   Run a TypeScript Code Mode script directly
  skill [--tools all|exec]   Print the Code Mode Skill guide (Markdown)
  logs / errors / stats      Inspect audit logs and execution metrics
  version                    Print version

Core serve flags:
  --cwd <dir>                Workspace root directory (default: current dir)
  --http <addr[/path]>       Serve Streamable HTTP (e.g. 127.0.0.1:8787 or :8787/mcp)
  --ui                       Start HTTP (default 127.0.0.1:8787) with interactive TUI
  --token <secret>           Bearer token for HTTP mode (or MCPLEX_TOKEN env)
  --tools <spec>             Exposed MCP tools: all (default) | exec | read,tasks
  --strict-root              Confine access strictly to --cwd (+ --allow-root)
  --mcp-config <file>        Downstream mcpServers JSON config (repeatable)
  --policy-file <file>       Custom JSON policy rules file (repeatable)

Advanced flags (run 'mcplex serve -h' for full list):
  --allow-root, --data-dir, --max-output, --cors-origin, --compact-schema, --trust-workspace-mcp`)
}

func parseToolSet(spec string) (mcpserver.ToolSet, error) {
	raw := strings.ToLower(strings.TrimSpace(spec))
	if raw == "" || raw == "all" {
		return mcpserver.ToolSet{Read: true, Tasks: true, Docs: true}, nil
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
			ts.Docs = true
		case "read":
			ts.Read = true
		case "tasks", "task", "spawn":
			ts.Tasks = true
		case "docs", "help", "guide":
			ts.Docs = true
		default:
			return mcpserver.ToolSet{}, fmt.Errorf("unknown tool %q (allowed: all, exec, read, tasks, spawn, task, docs)", p)
		}
	}
	return ts, nil
}

func runSkill(args []string) {
	fs := flag.NewFlagSet("skill", flag.ExitOnError)
	toolsFlag := fs.String("tools", "all", "Tool profile to render guide for (all, exec, or comma-separated list)")
	topicFlag := fs.String("topic", "", "Optional docs topic (e.g. fs, tx, sh, tasks, errors, patterns, all)")
	queryFlag := fs.String("query", "", "Optional keyword search query across MCPlex docs")
	_ = fs.Parse(args)
	ts, err := parseToolSet(*toolsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --tools value: %v\n", err)
		os.Exit(1)
	}
	topic := strings.TrimSpace(*topicFlag)
	if topic == "" && fs.NArg() > 0 {
		topic = strings.TrimSpace(fs.Arg(0))
	}
	if topic != "" || strings.TrimSpace(*queryFlag) != "" {
		fmt.Println(skill.Lookup(topic, *queryFlag, ts.Read, ts.Tasks))
		return
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
	uiMode := fs.Bool("ui", false, "Launch interactive TUI console (defaults --http to 127.0.0.1:8787)")

	var extraRoots, mcpConfigs, policyFiles stringListFlag
	fs.Var(&extraRoots, "allow-root", "Additional allowed workspace root (repeatable)")
	fs.Var(&mcpConfigs, "mcp-config", "Downstream mcpServers JSON config path (repeatable)")
	fs.Var(&policyFiles, "policy-file", "Custom JSON policy file (repeatable)")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Allow `mcplex serve --ui` without repeating `--http` (defaults to 127.0.0.1:8787).
	if *uiMode && strings.TrimSpace(*httpAddr) == "" {
		*httpAddr = "127.0.0.1:8787"
	}
	// Support optional path embedded in --http (e.g. `127.0.0.1:8787/mcp` or `http://127.0.0.1:8787/custom`).
	if rawHTTP := strings.TrimSpace(*httpAddr); rawHTTP != "" {
		if strings.HasPrefix(rawHTTP, "http://") || strings.HasPrefix(rawHTTP, "https://") {
			if parsed, pErr := url.Parse(rawHTTP); pErr == nil && parsed.Host != "" {
				*httpAddr = parsed.Host
				if parsed.Path != "" && parsed.Path != "/" && *httpPath == "/mcp" {
					*httpPath = parsed.Path
				}
			}
		} else if slash := strings.IndexByte(rawHTTP, '/'); slash > 0 {
			if *httpPath == "/mcp" {
				*httpPath = rawHTTP[slash:]
			}
			*httpAddr = rawHTTP[:slash]
		}
	}

	toolSet, err := parseToolSet(*toolsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --tools value: %v\n", err)
		os.Exit(1)
	}

	uiHistory := 500
	if rawHist := strings.TrimSpace(os.Getenv("MCPLEX_UI_HISTORY")); rawHist != "" {
		if n, convErr := strconv.Atoi(rawHist); convErr == nil && n > 0 {
			uiHistory = n
		}
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
		EnableEvents:          strings.TrimSpace(*httpAddr) != "",
		EventHistory:          uiHistory,
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
		httpOpts := mcpserver.HTTPOptions{
			Addr:        *httpAddr,
			Path:        *httpPath,
			Token:       *token,
			AllowOrigin: *corsOrigin,
		}
		if *uiMode {
			srvCtx, srvCancel := context.WithCancel(ctx)
			defer srvCancel()

			boundCh := make(chan string, 1)
			srvErrCh := make(chan error, 1)
			go func() {
				srvErrCh <- srv.ListenAndServeHTTP(srvCtx, httpOpts, func(addr string) {
					boundCh <- addr
				})
			}()

			var boundAddr string
			select {
			case boundAddr = <-boundCh:
			case err := <-srvErrCh:
				if err != nil {
					fmt.Fprintf(os.Stderr, "mcplex http error: %v\n", err)
					os.Exit(1)
				}
				return
			}

			endpoint := fmt.Sprintf("http://%s%s", boundAddr, *httpPath)
			src := tui.NewLocalHubSource(eng.EventHub(), "")
			err := tui.Run(ctx, tui.Config{
				Endpoint:    endpoint,
				Version:     version.Version,
				ModeLabel:   "local",
				AuthEnabled: *token != "",
				StrictRoot:  *strictRoot,
			}, src)
			srvCancel()
			<-srvErrCh
			if err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintf(os.Stderr, "mcplex ui error: %v\n", err)
				os.Exit(1)
			}
			return
		}

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
		err := srv.ServeHTTP(ctx, httpOpts)
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

func runUI(args []string) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	targetURL := fs.String("url", "http://127.0.0.1:8787", "Base URL of running 'mcplex serve --http' instance")
	token := fs.String("token", os.Getenv("MCPLEX_TOKEN"), "Optional Bearer token if HTTP server requires auth")
	session := fs.String("session", "", "Optional Mcp-Session-Id filter")
	_ = fs.Parse(args)
	if fs.NArg() > 0 && strings.TrimSpace(fs.Arg(0)) != "" {
		*targetURL = strings.TrimSpace(fs.Arg(0))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	src := tui.NewSSESource(ctx, *targetURL, *token, *session)
	cfg := tui.Config{
		Endpoint:    strings.TrimRight(*targetURL, "/"),
		Version:     version.Version,
		ModeLabel:   "remote",
		AuthEnabled: *token != "",
	}
	if err := tui.Run(ctx, cfg, src); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "mcplex ui error: %v\n", err)
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
