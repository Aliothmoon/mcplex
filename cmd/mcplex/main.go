// Command mcplex is a single-binary, client-agnostic Code Mode MCP Tool Proxy:
// it exposes local filesystem, ripgrep, AST outlines, transactional edits, read-only git,
// policy-checked shell checks, session state, and downstream MCP servers through one
// self-describing TypeScript sandbox over stdio or Streamable HTTP.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aliothmoon/mcplex/pkg/audit"
	"github.com/aliothmoon/mcplex/pkg/mcpserver"
	"github.com/aliothmoon/mcplex/pkg/oauthserver"
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
	os.Exit(runMain(os.Args[1:]))
}

func runMain(argv []string) int {
	if len(argv) < 1 {
		printUsage()
		return 1
	}
	sub := argv[0]
	args := argv[1:]

	switch sub {
	case "serve", "mcp":
		return runServe(args)
	case "ui":
		return runUI(args)
	case "run":
		return runStandalone(args)
	case "skill":
		return runSkill(args)
	case "logs":
		return runLogs(args, false)
	case "errors":
		return runLogs(args, true)
	case "stats":
		return runStats(args)
	case "oauth":
		return runOAuth(args)
	case "-h", "--help", "help":
		printUsage()
		return 0
	case "-v", "--version", "version":
		fmt.Printf("mcplex v%s (%s/%s)\n", version.Version, runtime.GOOS, runtime.GOARCH)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand '%s'\n\n", sub)
		printUsage()
		return 1
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
  oauth clients|revoke       List or revoke OAuth clients registered via serve --oauth
  version                    Print version

Core serve flags:
  --cwd <dir>                Workspace root directory (default: current dir)
  --http <addr[/path]>       Serve Streamable HTTP (e.g. 127.0.0.1:8787 or :8787/mcp)
  --ui                       Start HTTP (default 127.0.0.1:8787) with interactive TUI
  --token <secret>           Bearer token for HTTP mode (or MCPLEX_TOKEN env)
  --tools <spec>             Exposed MCP tools: all (default) | exec | read,tasks,docs
  --strict-root              Confine host file, git, and cwd access strictly to --cwd (+ --allow-root)
  --mcp-config <file>        Downstream mcpServers JSON config (repeatable)
  --policy-file <file>       Custom JSON policy rules file (repeatable)
  --public-url <url>         Public HTTPS origin when exposed via a tunnel (allowed Host; OAuth issuer)
  --oauth                    Act as OAuth 2.1 server for OAuth-only clients (needs --public-url, --token)

Advanced flags (run 'mcplex serve -h' for full list):
  --allow-root, --data-dir, --max-output, --cors-origin, --insecure-no-token, --session-idle-ttl, --oauth-redirect-allow, --compact-schema, --trust-workspace-mcp`)
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

func runSkill(args []string) int {
	fs := flag.NewFlagSet("skill", flag.ContinueOnError)
	toolsFlag := fs.String("tools", "all", "Tool profile to render guide for (all, exec, or comma-separated list)")
	topicFlag := fs.String("topic", "", "Optional docs topic (e.g. fs, tx, sh, tasks, errors, patterns, all)")
	queryFlag := fs.String("query", "", "Optional keyword search query across MCPlex docs")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	ts, err := parseToolSet(*toolsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --tools value: %v\n", err)
		return 1
	}
	topic := strings.TrimSpace(*topicFlag)
	if topic == "" && fs.NArg() > 0 {
		topic = strings.TrimSpace(fs.Arg(0))
	}
	if topic != "" || strings.TrimSpace(*queryFlag) != "" {
		fmt.Println(skill.Lookup(topic, *queryFlag, ts.Read, ts.Tasks))
		return 0
	}
	fmt.Print(skill.FullGuideForToolset(ts.Read, ts.Tasks, ts.Docs))
	return 0
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

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	toolsFlag := fs.String("tools", "all", "Exposed MCP tools: all (exec,read,spawn,task,docs), exec, or comma-separated list")
	dataDir := fs.String("data-dir", defaultDataDir(), "Persistent state and audit log directory")
	cwd := fs.String("cwd", "", "Default workspace directory")
	strictRoot := fs.Bool("strict-root", false, "Confine host file, git, and cwd access strictly to --cwd (+ --allow-root)")
	trustWsMCP := fs.Bool("trust-workspace-mcp", false, "Opt in to <workspace>/.agents/mcp_config.json")
	maxOut := fs.Int("max-output", defaultGenericMaxOutputBytes, "Default inline output byte cap")
	compactSchema := fs.Bool("compact-schema", false, "Use minimal one-line exec tool description")
	httpAddr := fs.String("http", "", "Serve Streamable HTTP on this address (e.g. 127.0.0.1:8787) instead of stdio")
	httpPath := fs.String("http-path", "/mcp", "HTTP endpoint path for Streamable HTTP mode")
	token := fs.String("token", os.Getenv("MCPLEX_TOKEN"), "Bearer token required in HTTP mode")
	corsOrigin := fs.String("cors-origin", "", "Optional Access-Control-Allow-Origin header in HTTP mode")
	insecureNoToken := fs.Bool("insecure-no-token", false, "Allow non-loopback HTTP bind or wildcard CORS without --token")
	sessionIdleTTL := fs.Duration("session-idle-ttl", 30*time.Minute, "HTTP mode: close sessions idle this long (kills their background tasks incl. daemons; 0 disables)")
	uiMode := fs.Bool("ui", false, "Launch interactive TUI console (defaults --http to 127.0.0.1:8787)")
	publicURL := fs.String("public-url", "", "Public HTTPS origin of this server when exposed through a tunnel (e.g. https://mcp.example.com); allowed as Host and used as OAuth issuer")
	oauthEnabled := fs.Bool("oauth", false, "HTTP mode: act as an OAuth 2.1 authorization server so OAuth-only MCP clients (e.g. Gemini custom apps) can connect; needs --public-url (--token is auto-generated in --ui if omitted)")

	var extraRoots, mcpConfigs, policyFiles stringListFlag
	fs.Var(&extraRoots, "allow-root", "Additional allowed workspace root (repeatable)")
	fs.Var(&mcpConfigs, "mcp-config", "Downstream mcpServers JSON config path (repeatable)")
	fs.Var(&policyFiles, "policy-file", "Custom JSON policy file (repeatable)")
	var oauthRedirects stringListFlag
	fs.Var(&oauthRedirects, "oauth-redirect-allow", "Extra allowed OAuth redirect URI pattern (repeatable; trailing * = prefix, \"loopback\" = local native clients). Google redirect URIs are always allowed")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *uiMode && strings.TrimSpace(*httpAddr) == "" {
		*httpAddr = "127.0.0.1:8787"
	}
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
		return 1
	}

	autoGeneratedToken := false
	if shouldAutoGenerateUIToken(*uiMode, *insecureNoToken, *oauthEnabled, *token, *publicURL, *httpAddr, *corsOrigin) {
		genTok, genErr := generateRandomToken()
		if genErr != nil {
			fmt.Fprintf(os.Stderr, "mcplex token generation error: %v\n", genErr)
			return 1
		}
		*token = genTok
		autoGeneratedToken = true
	}

	allowedHosts, oauthSrv, oErr := buildPublicExposure(*httpAddr, *httpPath, *publicURL, *oauthEnabled, *token, *dataDir, oauthRedirects)
	if oErr != nil {
		fmt.Fprintf(os.Stderr, "mcplex oauth error: %v\n", oErr)
		return 1
	}

	tokStore := newLiveTokenStore(*token)

	if strings.TrimSpace(*httpAddr) != "" {
		preOpts := mcpserver.HTTPOptions{
			Addr:                 *httpAddr,
			Path:                 *httpPath,
			Token:                tokStore.Get(),
			TokenFunc:            tokStore.Get,
			AllowOrigin:          *corsOrigin,
			AllowInsecureNoToken: *insecureNoToken,
			SessionIdleTTL:       httpSessionTTL(*sessionIdleTTL),
			AllowedHosts:         allowedHosts,
			OAuth:                oauthSrv,
		}
		if vErr := mcpserver.ValidateHTTPOptions(preOpts); vErr != nil {
			fmt.Fprintf(os.Stderr, "mcplex http security error: %v\n", vErr)
			return 1
		}
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

	execDesc := skill.CompactExecDescriptionForToolset(*maxOut, toolSet.Read, toolSet.Tasks, toolSet.Docs)
	if *compactSchema {
		if toolSet.Docs {
			execDesc = "Execute a TypeScript async body in a local Code Mode sandbox composing fs, search, ast, edit, tx, git, sh, state, tools, and downstream mcp servers in ONE turn. Call docs({ topic }) or read mcplex://skill for signatures."
		} else {
			execDesc = "Execute a TypeScript async body in a local Code Mode sandbox composing fs, search, ast, edit, tx, git, sh, state, tools, and downstream mcp servers in ONE turn. Read mcplex://skill for the full API reference."
		}
	}

	srv := mcpserver.NewServerWithOptions(eng, mcpserver.ServerOptions{
		ServerName:         "mcplex",
		Tools:              toolSet,
		Instructions:       skill.InstructionsForToolset(toolSet.Read, toolSet.Tasks, toolSet.Docs),
		ExecDescription:    execDesc,
		DefaultMaxOutBytes: *maxOut,
		SkillResourceURI:   skillResourceURI,
		SkillResourceName:  "MCPlex Code Mode Skill",
		SkillResourceText:  skill.FullGuideForToolset(toolSet.Read, toolSet.Tasks, toolSet.Docs),
	})

	if strings.TrimSpace(*httpAddr) != "" {
		httpOpts := mcpserver.HTTPOptions{
			Addr:                 *httpAddr,
			Path:                 *httpPath,
			Token:                tokStore.Get(),
			TokenFunc:            tokStore.Get,
			AllowOrigin:          *corsOrigin,
			AllowInsecureNoToken: *insecureNoToken,
			SessionIdleTTL:       httpSessionTTL(*sessionIdleTTL),
			AllowedHosts:         allowedHosts,
			OAuth:                oauthSrv,
		}
		if oauthSrv != nil && !*uiMode {
			fmt.Fprintf(os.Stderr, "mcplex OAuth enabled: add %s as a custom MCP app in your client and approve it with your --token on the consent page\n", oauthSrv.Resource())
			if len(*token) < 16 {
				fmt.Fprintln(os.Stderr, "[WARN] --token is the OAuth owner secret; use at least 16 random characters")
			}
		}
		if tokStore.Get() == "" && !mcpserver.IsLoopbackBindAddr(*httpAddr) && !*uiMode {
			fmt.Fprintf(os.Stderr, "[WARN] mcplex HTTP is binding non-loopback address %q WITHOUT --token (--insecure-no-token enabled)!\n", *httpAddr)
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
					return 1
				}
				return 0
			}

			endpoint := fmt.Sprintf("http://%s%s", boundAddr, *httpPath)
			pubEndpoint := ""
			if oauthSrv != nil {
				pubEndpoint = oauthSrv.Resource()
			} else if rawPub := strings.TrimSpace(*publicURL); rawPub != "" {
				p := *httpPath
				if !strings.HasPrefix(p, "/") {
					p = "/" + p
				}
				pubEndpoint = strings.TrimRight(rawPub, "/") + p
			}
			src := tui.NewLocalHubSource(eng.EventHub(), "")
			err := tui.Run(ctx, tui.Config{
				Endpoint:       endpoint,
				PublicEndpoint: pubEndpoint,
				Version:        version.Version,
				ModeLabel:      "local",
				AuthEnabled:    tokStore.Get() != "",
				OAuthEnabled:   oauthSrv != nil,
				StrictRoot:     *strictRoot,
				InitialToken:   tokStore.Get(),
				AutoGenerated:  autoGeneratedToken,
				RotateToken: func() (string, error) {
					next, genErr := generateRandomToken()
					if genErr != nil {
						return "", genErr
					}
					if oauthSrv != nil {
						if sErr := oauthSrv.SetOwnerSecret(next); sErr != nil {
							return "", sErr
						}
					}
					tokStore.Set(next)
					return next, nil
				},
			}, src)
			srvCancel()
			<-srvErrCh
			if err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintf(os.Stderr, "mcplex ui error: %v\n", err)
				return 1
			}
			return 0
		}

		fmt.Fprintf(os.Stderr, "mcplex v%s listening on http://%s%s (auth=%v, strict-root=%v)\n",
			version.Version, *httpAddr, *httpPath, *token != "", *strictRoot)
		err := srv.ServeHTTP(ctx, httpOpts)
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "mcplex http error: %v\n", err)
			return 1
		}
		return 0
	}

	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "mcplex stdio error: %v\n", err)
		return 1
	}
	return 0
}

func runUI(args []string) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	targetURL := fs.String("url", "http://127.0.0.1:8787", "Base URL of running 'mcplex serve --http' instance")
	token := fs.String("token", os.Getenv("MCPLEX_TOKEN"), "Optional Bearer token if HTTP server requires auth")
	session := fs.String("session", "", "Optional Mcp-Session-Id filter")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
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
		return 1
	}
	return 0
}

func runStandalone(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cwd := fs.String("cwd", "", "Workspace directory")
	dataDir := fs.String("data-dir", defaultDataDir(), "State and log directory")
	codeFlag := fs.String("code", "", "Inline TypeScript code to execute")
	strictRoot := fs.Bool("strict-root", false, "Confine host file, git, and cwd access strictly to workspace")
	trustWsMCP := fs.Bool("trust-workspace-mcp", false, "Opt in to <workspace>/.agents/mcp_config.json")
	timeoutMs := fs.Int("timeout", 20000, "Timeout in milliseconds")
	maxOut := fs.Int("max-output", 65536, "Max output UTF-8 bytes")
	var extraRoots, mcpConfigs, policyFiles stringListFlag
	fs.Var(&extraRoots, "allow-root", "Additional allowed workspace root (repeatable)")
	fs.Var(&mcpConfigs, "mcp-config", "Downstream mcpServers JSON config path (repeatable)")
	fs.Var(&policyFiles, "policy-file", "Custom JSON policy file (repeatable)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	code := *codeFlag
	if code == "" && fs.NArg() > 0 {
		raw, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed reading %s: %v\n", fs.Arg(0), err)
			return 1
		}
		code = string(raw)
	}
	if code == "" {
		raw, _ := io.ReadAll(os.Stdin)
		code = string(raw)
	}
	if strings.TrimSpace(code) == "" {
		fmt.Fprintln(os.Stderr, "error: provide --code '...' or a .ts script file")
		return 1
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
		return 1
	}
	return 0
}

func runLogs(args []string, forceErrors bool) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	errorsOnly := fs.Bool("errors", forceErrors, "Show only runs with errors")
	shortErr := fs.Bool("e", forceErrors, "Alias for --errors")
	full := fs.Bool("full", false, "Print full TypeScript source and output")
	tail := fs.Int("tail", 20, "Number of recent entries")
	runID := fs.String("id", "", "Inspect a specific run ID")
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory")
	asJSON := fs.Bool("json", false, "Output JSON")
	compact := fs.Bool("compact", false, "Omit full source/result bodies in JSON output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	wantErrors := *errorsOnly || *shortErr
	if *runID != "" {
		wantErrors = false
	}
	entries, files, err := audit.LoadEntries(*dataDir, wantErrors)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logs error: %v\n", err)
		return 1
	}
	if *runID != "" && len(audit.FilterEntries(entries, 0, *runID)) == 0 && !wantErrors {
		if errEntries, errFiles, err2 := audit.LoadEntries(*dataDir, true); err2 == nil && len(audit.FilterEntries(errEntries, 0, *runID)) > 0 {
			entries, files = errEntries, errFiles
		}
	}
	if *asJSON {
		filtered := audit.FilterEntries(entries, *tail, *runID)
		raw, _ := audit.MarshalEntriesJSON(filtered, *full || *runID != "", *compact)
		fmt.Println(string(raw))
		return 0
	}
	if *full && *runID == "" {
		start := 0
		if *tail > 0 && len(entries) > *tail {
			start = len(entries) - *tail
		} else if *tail == 0 {
			start = len(entries)
		}
		for _, e := range entries[start:] {
			fmt.Print(audit.FormatFullStreamCard(e))
		}
		return 0
	}
	fmt.Print(audit.FormatLogs(entries, files, *tail, *runID))
	return 0
}

func runStats(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	entries, _, err := audit.LoadEntries(*dataDir, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stats error: %v\n", err)
		return 1
	}
	fmt.Print(audit.FormatStats(entries))
	return 0
}

// httpSessionTTL maps the --session-idle-ttl flag to HTTPOptions.SessionIdleTTL, where 0 means "use the default"
// and a negative value disables reaping; on the CLI, 0 (or any non-positive value) disables reaping.
func httpSessionTTL(flagVal time.Duration) time.Duration {
	if flagVal <= 0 {
		return -1
	}
	return flagVal
}

type liveTokenStore struct {
	mu  sync.RWMutex
	val string
}

func newLiveTokenStore(initial string) *liveTokenStore {
	return &liveTokenStore{val: strings.TrimSpace(initial)}
}

func (s *liveTokenStore) Get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.val
}

func (s *liveTokenStore) Set(v string) {
	s.mu.Lock()
	s.val = strings.TrimSpace(v)
	s.mu.Unlock()
}

func generateRandomToken() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// shouldAutoGenerateUIToken returns true when interactive `--ui` mode should mint a fresh random secret
// automatically because `--token` was omitted for a tunnel/OAuth/non-loopback/wildcard-CORS exposure.
// Plain local `mcplex serve --ui` on loopback remains passwordless by default (users can press `t` in UI anytime).
func shouldAutoGenerateUIToken(uiMode, insecureNoToken, oauthOn bool, token, publicURL, httpAddr, corsOrigin string) bool {
	if !uiMode || insecureNoToken || strings.TrimSpace(token) != "" {
		return false
	}
	if oauthOn || strings.TrimSpace(publicURL) != "" || strings.TrimSpace(corsOrigin) == "*" {
		return true
	}
	return strings.TrimSpace(httpAddr) != "" && !mcpserver.IsLoopbackBindAddr(httpAddr)
}

// buildPublicExposure validates --public-url / --oauth and returns the extra allowed Host header(s) plus the OAuth
// authorization server (nil when --oauth is off).
func buildPublicExposure(httpAddr, httpPath, publicURL string, oauthOn bool, token, dataDir string, redirects []string) ([]string, *oauthserver.Server, error) {
	publicURL = strings.TrimSpace(publicURL)
	if !oauthOn && publicURL == "" {
		return nil, nil, nil
	}
	if strings.TrimSpace(httpAddr) == "" {
		return nil, nil, errors.New("--public-url / --oauth require HTTP mode (--http or --ui)")
	}
	if !oauthOn {
		u, err := url.Parse(publicURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, nil, fmt.Errorf("invalid --public-url %q (expected e.g. https://mcp.example.com)", publicURL)
		}
		return []string{u.Host}, nil, nil
	}
	if strings.TrimSpace(token) == "" {
		return nil, nil, errors.New("--oauth requires --token (or MCPLEX_TOKEN): it is the owner secret typed on the consent page")
	}
	srv, err := oauthserver.New(oauthserver.Config{
		PublicURL:     publicURL,
		MCPPath:       httpPath,
		OwnerSecret:   token,
		DataDir:       dataDir,
		RedirectAllow: redirects,
	})
	if err != nil {
		return nil, nil, err
	}
	return []string{srv.PublicHost()}, srv, nil
}

// runOAuth manages OAuth clients registered by `serve --oauth`: list them or revoke their grants/tokens.
func runOAuth(args []string) int {
	if len(args) == 0 || (args[0] != "clients" && args[0] != "revoke") {
		fmt.Fprintln(os.Stderr, "usage: mcplex oauth clients [--data-dir dir] [--json]\n       mcplex oauth revoke <client-id...|all> [--data-dir dir]")
		return 1
	}
	action := args[0]
	fs := flag.NewFlagSet("oauth "+action, flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory used by `mcplex serve`")
	asJSON := fs.Bool("json", false, "Output JSON")
	positional, perr := parseInterspersed(fs, args[1:])
	if perr != nil {
		if errors.Is(perr, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	switch action {
	case "clients":
		list, err := oauthserver.ListClients(*dataDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "oauth clients error: %v\n", err)
			return 1
		}
		if *asJSON {
			raw, _ := json.MarshalIndent(list, "", "  ")
			fmt.Println(string(raw))
			return 0
		}
		if len(list) == 0 {
			fmt.Println("No OAuth clients registered.")
			return 0
		}
		for _, c := range list {
			name := c.Name
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Printf("%s  %-20s  grants=%d  auth=%s  last-used=%s\n    redirect: %s\n",
				c.ID, name, c.Grants, c.AuthMethod, c.LastUsedAt.Local().Format("2006-01-02 15:04"), strings.Join(c.Redirects, ", "))
		}
		return 0
	default: // revoke
		ids := positional
		if len(ids) == 0 {
			fmt.Fprintln(os.Stderr, "mcplex oauth revoke: pass client IDs (see `mcplex oauth clients`) or `all`")
			return 1
		}
		n, err := oauthserver.RevokeClients(*dataDir, ids)
		if err != nil {
			fmt.Fprintf(os.Stderr, "oauth revoke error: %v\n", err)
			return 1
		}
		fmt.Printf("Revoked %d client(s); their access and refresh tokens no longer work.\n", n)
		return 0
	}
}

// parseInterspersed parses flags that may appear before or after positional arguments
// (`mcplex oauth revoke all --data-dir X`); the standard flag package stops at the first positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		if rest[0] == "--" {
			return append(positional, rest[1:]...), nil
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
}
