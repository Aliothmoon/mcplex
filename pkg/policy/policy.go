// Package policy enforces path resolution, symlink guards, sensitive file protection,
// and Antigravity CLI settings.json permission rules.
package policy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Decision represents the outcome of evaluating a capability operation.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionAsk   Decision = "ask"
	DecisionDeny  Decision = "deny"
)

// PolicyError is returned when an operation is blocked or requires host interactive approval.
type PolicyError struct {
	Decision     Decision
	Action       string
	Target       string
	Reason       string
	NoNativeHint bool
}

func (e *PolicyError) Error() string {
	if e.Decision == DecisionAsk {
		return "needs approval: " + e.Detail()
	}
	return "denied: " + e.Detail()
}

// Kind implements sandbox.KindedError: guest code sees `e.name` "NeedsApproval" or "Denied".
func (e *PolicyError) Kind() string {
	if e.Decision == DecisionAsk {
		return "NeedsApproval"
	}
	return "Denied"
}

// Detail is the one-line message without the kind word: `<target> — <reason>[; <how to proceed>]`.
func (e *PolicyError) Detail() string {
	d := shortTarget(e.Target) + " — " + e.Reason
	if e.Decision == DecisionAsk {
		if e.NoNativeHint {
			d += "; requires host confirmation"
		} else {
			switch e.Action {
			case "command":
				d += "; run it via native run_command"
			case "mcp":
				d += "; call the MCP tool natively"
			default:
				d += "; use native file tools"
			}
		}
	}
	return d
}

func shortTarget(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	if r := []rune(t); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return t
}

// IsApprovalRequired reports whether err indicates an interactive approval gate.
func IsApprovalRequired(err error) bool {
	var pe *PolicyError
	return errors.As(err, &pe) && pe.Decision == DecisionAsk
}

type cliSettings struct {
	AllowNonWorkspaceAccess *bool `json:"allowNonWorkspaceAccess"`
	// TrustWorkspaceMCP opts in to spawning MCP servers declared in `<workspace>/.agents/mcp_config.json`.
	TrustWorkspaceMCP        *bool `json:"agyPtcTrustWorkspaceMcp"`
	GenericTrustWorkspaceMCP *bool `json:"trustWorkspaceMcp"`
	Permissions              struct {
		Allow []string `json:"allow"`
		Ask   []string `json:"ask"`
		Deny  []string `json:"deny"`
	} `json:"permissions"`
}

type parsedRule struct {
	action  string
	target  string
	isRegex bool
	re      *regexp.Regexp
}

// Engine enforces workspace confinement, sensitive file guards, and Antigravity CLI settings.json rules.
// Options configures a policy Engine for either standalone MCPlex or Antigravity plugin mode.
type Options struct {
	ExtraRoots            []string
	StrictWorkspace       bool     // force allowNonWorkspaceAccess = false (confine to workspaceRoots)
	TrustWorkspaceMCP     bool     // allow <workspace>/.agents/mcp_config.json
	PolicyFiles           []string // explicit JSON policy files to load in order
	LoadAntigravityConfig bool     // load ~/.gemini/antigravity-cli/settings.json & ~/.gemini/config/agy-ptc/policy.json & PLUGIN_DATA/policy.json
	NativeEscalationHint  bool     // include "; run it via native run_command" in DecisionAsk messages
}

type Engine struct {
	workspaceRoots          []string
	allowNonWorkspaceAccess bool
	trustWorkspaceMCP       bool
	nativeEscalationHint    bool
	denyRules               []parsedRule
	askRules                []parsedRule
	allowRules              []parsedRule
	sensitiveDirs           []string
}

// NewEngine creates a policy engine rooted at workspaceDir and aligned with ~/.gemini/antigravity-cli/settings.json.
func NewEngine(workspaceDir string, extraRoots ...string) *Engine {
	return NewEngineWithOptions(workspaceDir, Options{
		ExtraRoots:            extraRoots,
		LoadAntigravityConfig: true,
		NativeEscalationHint:  true,
	})
}

// NewEngineWithOptions creates a policy engine with explicit host profile options.
func NewEngineWithOptions(workspaceDir string, opts Options) *Engine {
	strictEnv := os.Getenv("AGY_PTC_STRICT_WORKSPACE") == "1" || os.Getenv("MCPLEX_STRICT_WORKSPACE") == "1"
	trustEnv := os.Getenv("AGY_PTC_TRUST_WORKSPACE_MCP") == "1" || os.Getenv("MCPLEX_TRUST_WORKSPACE_MCP") == "1"
	e := &Engine{
		allowNonWorkspaceAccess: !opts.StrictWorkspace && !strictEnv,
		trustWorkspaceMCP:       opts.TrustWorkspaceMCP || trustEnv,
		nativeEscalationHint:    opts.NativeEscalationHint,
	}
	extraRoots := opts.ExtraRoots
	seen := map[string]bool{}
	addRoot := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return
		}
		real, err := evalSymlinksExisting(abs)
		if err == nil {
			abs = real
		}
		key := normalizeComparePath(abs)
		if !seen[key] {
			seen[key] = true
			e.workspaceRoots = append(e.workspaceRoots, abs)
		}
	}
	addRoot(workspaceDir)
	for _, r := range extraRoots {
		addRoot(r)
	}

	home := userHomeDir()
	if home != "" {
		for _, rel := range []string{
			".ssh",
			".gnupg",
			".aws",
			filepath.Join(".gemini", "antigravity", "mcp_oauth_tokens.json"),
			filepath.Join(".gemini", "antigravity-cli", "mcp_oauth_tokens.json"),
		} {
			e.sensitiveDirs = append(e.sensitiveDirs, filepath.Join(home, rel))
		}
		for _, rel := range homeCredentialPaths {
			e.sensitiveDirs = append(e.sensitiveDirs, filepath.Join(home, rel))
		}
		if opts.LoadAntigravityConfig {
			settingsPath := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
			e.loadSettingsFile(settingsPath)
			e.loadSettingsFile(filepath.Join(home, ".gemini", "config", "agy-ptc", "policy.json"))
		}
	}
	if opts.LoadAntigravityConfig {
		if pd := strings.TrimSpace(os.Getenv("PLUGIN_DATA")); pd != "" {
			e.loadSettingsFile(filepath.Join(pd, "policy.json"))
		}
	}
	for _, pf := range opts.PolicyFiles {
		if strings.TrimSpace(pf) != "" {
			e.loadSettingsFile(pf)
		}
	}
	if opts.StrictWorkspace || strictEnv {
		e.allowNonWorkspaceAccess = false
	}
	return e
}

// LoadSettingsJSON loads CLI or desktop policy JSON directly (useful for testing and custom configs).
// Subsequent calls override AllowNonWorkspaceAccess if set and append permission rules.
func (e *Engine) LoadSettingsJSON(raw []byte) {
	var cfg cliSettings
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return
	}
	if cfg.AllowNonWorkspaceAccess != nil {
		e.allowNonWorkspaceAccess = *cfg.AllowNonWorkspaceAccess
	}
	if cfg.TrustWorkspaceMCP != nil {
		e.trustWorkspaceMCP = *cfg.TrustWorkspaceMCP
	}
	if cfg.GenericTrustWorkspaceMCP != nil {
		e.trustWorkspaceMCP = *cfg.GenericTrustWorkspaceMCP
	}
	e.denyRules = append(e.denyRules, parseRuleList(cfg.Permissions.Deny)...)
	e.askRules = append(e.askRules, parseRuleList(cfg.Permissions.Ask)...)
	e.allowRules = append(e.allowRules, parseRuleList(cfg.Permissions.Allow)...)
}

// TrustWorkspaceMCP reports whether MCP servers declared in `<workspace>/.agents/mcp_config.json` may be spawned.
// Off by default: a cloned repository must not be able to make the sandbox launch arbitrary processes.
// Enable with `AGY_PTC_TRUST_WORKSPACE_MCP=1` or `"agyPtcTrustWorkspaceMcp": true` in an agy-ptc policy.json.
func (e *Engine) TrustWorkspaceMCP() bool {
	return e.trustWorkspaceMCP
}

// NativeEscalationHint reports whether interactive-approval errors should mention native host tools.
func (e *Engine) NativeEscalationHint() bool {
	return e.nativeEscalationHint
}

func (e *Engine) policyErr(dec Decision, action, target, reason string) *PolicyError {
	return &PolicyError{
		Decision:     dec,
		Action:       action,
		Target:       target,
		Reason:       reason,
		NoNativeHint: !e.nativeEscalationHint,
	}
}

func (e *Engine) loadSettingsFile(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	e.LoadSettingsJSON(raw)
}

// ResolveAndCheckRead resolves path against baseDir and checks read permissions.
func (e *Engine) ResolveAndCheckRead(baseDir, targetPath string) (string, error) {
	abs, realPath, err := resolveWithSymlinks(baseDir, targetPath)
	if err != nil {
		return "", err
	}
	if e.isSensitivePath(realPath) {
		return "", &PolicyError{
			Decision: DecisionDeny,
			Action:   "read_file",
			Target:   targetPath,
			Reason:   "credential path",
		}
	}
	if dec, matched := e.evalRules("read_file", realPath); matched {
		if dec != DecisionAllow {
			return "", e.policyErr(dec, "read_file", targetPath, "settings rule")
		}
		return abs, nil
	}
	// Write implies read in Antigravity permission rules.
	if dec, matched := e.evalRules("write_file", realPath); matched && dec == DecisionAllow {
		return abs, nil
	}
	if !e.isInsideWorkspace(realPath) && !e.isTempOrScratch(realPath) && !e.allowNonWorkspaceAccess {
		return "", e.policyErr(DecisionAsk, "read_file", targetPath, "outside workspace")
	}
	return abs, nil
}

// ResolveAndCheckWrite resolves path against baseDir and enforces workspace & .git confinement.
func (e *Engine) ResolveAndCheckWrite(baseDir, targetPath string) (string, error) {
	abs, realPath, err := resolveWithSymlinks(baseDir, targetPath)
	if err != nil {
		return "", err
	}
	if e.isSensitivePath(realPath) || isProtectedGitInternal(realPath) || isProtectedSystemRoot(realPath) {
		return "", &PolicyError{
			Decision: DecisionDeny,
			Action:   "write_file",
			Target:   targetPath,
			Reason:   "protected system/.git/credential path",
		}
	}
	// Deny read implies deny write.
	if dec, matched := e.evalRules("read_file", realPath); matched && dec == DecisionDeny {
		return "", &PolicyError{Decision: DecisionDeny, Action: "write_file", Target: targetPath, Reason: "read deny rule"}
	}
	// Self-escalation guard: agent permission/MCP/hook configs, the plugin itself, shell startup files and
	// global git config are never rewritten from inside the sandbox (user allow rules cannot override this).
	if isProtectedConfigPath(realPath) || isProtectedConfigPath(abs) {
		return "", e.policyErr(DecisionAsk, "write_file", targetPath, "protected agent/plugin/shell/git config")
	}
	if dec, matched := e.evalRules("write_file", realPath); matched {
		if dec != DecisionAllow {
			return "", e.policyErr(dec, "write_file", targetPath, "settings rule")
		}
		return abs, nil
	}
	if !e.isInsideWorkspace(realPath) && !e.allowNonWorkspaceAccess {
		return "", e.policyErr(DecisionAsk, "write_file", targetPath, "outside workspace")
	}
	return abs, nil
}

var hostPrivEscPattern = regexp.MustCompile(`(?i)(?:^|[;&|(\n` + "`" + `]\s*)(?:env\s+(?:[^\s;|&]+\s+)*)?(?:[a-zA-Z0-9_./\\:-]*[/\\])?(?:sudo|su|doas)\b`)

// CheckCommand validates a shell command against CLI permission rules and the built-in guard.
// Order: user deny rules > built-in guard (APPROVAL_REQUIRED) > user ask rules > user allow rules.
// The built-in guard is a best-effort guardrail, not a security boundary (see guards.go).
func (e *Engine) CheckCommand(cmdLine string) error {
	trimmed := strings.TrimSpace(cmdLine)
	if trimmed == "" {
		return errors.New("command cannot be empty")
	}
	dec, matched := e.evalRules("command", trimmed)
	if matched && dec == DecisionDeny {
		return &PolicyError{Decision: DecisionDeny, Action: "command", Target: trimmed, Reason: "settings deny rule"}
	}
	if guard := matchBuiltinCommandGuard(trimmed); guard != "" {
		return e.policyErr(DecisionAsk, "command", trimmed, guard)
	}
	if matched && dec != DecisionAllow {
		return e.policyErr(dec, "command", trimmed, "settings rule")
	}
	return nil
}

// extractHostCommandSurface strips literal quoted arguments of non-shell commands (e.g. `adb shell "...; su -c id"` or `rg "; sudo"`)
// while keeping unquoted command boundaries, quoted command verbs (`"sudo"`, `s"u"do`), `$()` / backtick substitutions,
// and local host-shell wrapper bodies (`sh -c`, `pwsh -Command`, `cmd /c`) intact.
func extractHostCommandSurface(cmdLine string, depth int) string {
	if depth > 6 {
		return cmdLine
	}
	type shellPiece struct {
		text       string
		quoted     bool
		joinBefore bool // true if immediately adjacent to previous piece with no whitespace (e.g. s"u"do or --grep="...")
	}
	var out strings.Builder
	var segPieces []shellPiece
	var substChunks []string

	flushSegment := func(sep string) {
		defer func() {
			segPieces = nil
			substChunks = nil
			if sep != "" {
				out.WriteString(sep)
			}
		}()
		if len(segPieces) == 0 && len(substChunks) == 0 {
			return
		}
		// Group pieces into whitespace-separated words so quoted or partially-quoted command verbs
		// (like `"sudo" id`, `s"u"do id`, or `"sh" -c "sudo id"`) are recognized, while quoted
		// argument parts (like `--grep="; su -"` or `adb shell "...; su -c id"`) are stripped.
		type word struct {
			raw          string
			unquotedOnly string
			hasQuoted    bool
		}
		var words []word
		for _, p := range segPieces {
			if !p.quoted {
				fields := strings.Fields(p.text)
				if len(fields) == 0 {
					continue
				}
				startsWithWS := len(p.text) > 0 && (p.text[0] == ' ' || p.text[0] == '\t')
				for fi, f := range fields {
					if fi == 0 && p.joinBefore && !startsWithWS && len(words) > 0 {
						w := &words[len(words)-1]
						w.raw += f
						w.unquotedOnly += f
					} else {
						words = append(words, word{raw: f, unquotedOnly: f})
					}
				}
			} else {
				if p.joinBefore && len(words) > 0 {
					w := &words[len(words)-1]
					w.raw += p.text
					w.unquotedOnly += `""`
					w.hasQuoted = true
				} else {
					words = append(words, word{raw: p.text, unquotedOnly: `""`, hasQuoted: true})
				}
			}
		}

		verb := ""
		verbWordIdx := -1
		for wi, w := range words {
			if isEnvPrefixToken(w.raw) {
				continue
			}
			verb = normalizeCmdVerb(w.raw)
			verbWordIdx = wi
			break
		}
		isShell := isHostShellWrapper(verb)

		for wi, w := range words {
			if wi > 0 {
				out.WriteByte(' ')
			}
			if wi <= verbWordIdx {
				out.WriteString(w.raw)
			} else if isShell && w.hasQuoted {
				out.WriteString("; ")
				out.WriteString(extractHostCommandSurface(w.raw, depth+1))
			} else if isShell {
				out.WriteString(w.raw)
			} else {
				out.WriteString(w.unquotedOnly)
			}
		}
		for _, sc := range substChunks {
			out.WriteString(" ; ")
			out.WriteString(extractHostCommandSurface(sc, depth+1))
		}
	}

	var cur strings.Builder
	lastEndedWithWS := true
	flushUnquoted := func() {
		if cur.Len() > 0 {
			txt := cur.String()
			segPieces = append(segPieces, shellPiece{text: txt, quoted: false, joinBefore: !lastEndedWithWS})
			last := txt[len(txt)-1]
			lastEndedWithWS = last == ' ' || last == '\t'
			cur.Reset()
		}
	}

	n := len(cmdLine)
	for i := 0; i < n; i++ {
		ch := cmdLine[i]
		switch ch {
		case '\'':
			flushUnquoted()
			joinBefore := !lastEndedWithWS
			start := i + 1
			i++
			for i < n && cmdLine[i] != '\'' {
				i++
			}
			segPieces = append(segPieces, shellPiece{text: cmdLine[start:i], quoted: true, joinBefore: joinBefore})
			lastEndedWithWS = false
		case '"':
			flushUnquoted()
			joinBefore := !lastEndedWithWS
			var q strings.Builder
			i++
			for i < n && cmdLine[i] != '"' {
				if cmdLine[i] == '\\' && i+1 < n && (cmdLine[i+1] == '"' || cmdLine[i+1] == '\\' || cmdLine[i+1] == '$' || cmdLine[i+1] == '`') {
					q.WriteByte(cmdLine[i+1])
					i += 2
					continue
				}
				if cmdLine[i] == '$' && i+1 < n && cmdLine[i+1] == '(' {
					sub, nextIdx := scanCommandSubst(cmdLine, i+2)
					substChunks = append(substChunks, sub)
					i = nextIdx + 1
					continue
				}
				if cmdLine[i] == '`' {
					if sub, nextIdx, closed := scanBacktickSubst(cmdLine, i+1); closed {
						substChunks = append(substChunks, sub)
						i = nextIdx + 1
						continue
					}
				}
				q.WriteByte(cmdLine[i])
				i++
			}
			segPieces = append(segPieces, shellPiece{text: q.String(), quoted: true, joinBefore: joinBefore})
			lastEndedWithWS = false
		case '$':
			if i+1 < n && cmdLine[i+1] == '(' {
				sub, nextIdx := scanCommandSubst(cmdLine, i+2)
				substChunks = append(substChunks, sub)
				i = nextIdx
				continue
			}
			cur.WriteByte(ch)
		case '`':
			if sub, nextIdx, closed := scanBacktickSubst(cmdLine, i+1); closed {
				substChunks = append(substChunks, sub)
				i = nextIdx
				continue
			}
			cur.WriteByte(ch)
		case ';', '\n', '(', ')':
			flushUnquoted()
			flushSegment(string(ch))
			lastEndedWithWS = true
		case '&', '|':
			flushUnquoted()
			if i+1 < n && cmdLine[i+1] == ch {
				flushSegment(cmdLine[i : i+2])
				i++
			} else {
				flushSegment(string(ch))
			}
			lastEndedWithWS = true
		default:
			cur.WriteByte(ch)
		}
	}
	flushUnquoted()
	flushSegment("")
	return out.String()
}

func scanCommandSubst(s string, start int) (string, int) {
	depth := 1
	var b strings.Builder
	i := start
	for i < len(s) && depth > 0 {
		c := s[i]
		if c == '(' {
			depth++
		} else if c == ')' {
			depth--
			if depth == 0 {
				return b.String(), i
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), len(s) - 1
}

func scanBacktickSubst(s string, start int) (string, int, bool) {
	var b strings.Builder
	i := start
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i += 2
			continue
		}
		if s[i] == '`' {
			return b.String(), i, true
		}
		b.WriteByte(s[i])
		i++
	}
	return "", start, false
}

func isEnvPrefixToken(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	name := tok[:eq]
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9' && i > 0) || c == '_') {
			return false
		}
	}
	return true
}

func normalizeCmdVerb(tok string) string {
	tok = strings.TrimSpace(tok)
	if idx := strings.LastIndexAny(tok, `/\`); idx >= 0 {
		tok = tok[idx+1:]
	}
	tok = strings.ToLower(tok)
	for _, ext := range []string{".exe", ".cmd", ".bat"} {
		tok = strings.TrimSuffix(tok, ext)
	}
	return tok
}

func isHostShellWrapper(verb string) bool {
	switch verb {
	case "sh", "bash", "zsh", "dash", "ksh", "ash", "pwsh", "powershell", "cmd":
		return true
	}
	return false
}

// CheckMCP validates external MCP bridge invocations against mcp(server/tool) rules.
func (e *Engine) CheckMCP(server, tool string) error {
	target := server + "/" + tool
	if dec, matched := e.evalRules("mcp", target); matched && dec != DecisionAllow {
		reason := "settings deny rule"
		if dec == DecisionAsk {
			reason = "settings ask rule"
		}
		return e.policyErr(dec, "mcp", target, reason)
	}
	return nil
}

func (e *Engine) evalRules(action, target string) (Decision, bool) {
	if matchRuleSet(e.denyRules, action, target, e.workspaceRoots) {
		return DecisionDeny, true
	}
	if matchRuleSet(e.askRules, action, target, e.workspaceRoots) {
		return DecisionAsk, true
	}
	if matchRuleSet(e.allowRules, action, target, e.workspaceRoots) {
		return DecisionAllow, true
	}
	return "", false
}

func parseRuleList(rawList []string) []parsedRule {
	var out []parsedRule
	for _, item := range rawList {
		item = strings.TrimSpace(item)
		openIdx := strings.IndexByte(item, '(')
		if openIdx <= 0 || !strings.HasSuffix(item, ")") {
			continue
		}
		action := strings.TrimSpace(item[:openIdx])
		target := strings.TrimSpace(item[openIdx+1 : len(item)-1])
		if action == "" || target == "" {
			continue
		}
		r := parsedRule{action: action, target: target}
		if strings.HasPrefix(target, "regex:") {
			pat := strings.TrimPrefix(target, "regex:")
			if compiled, err := regexp.Compile("^(?:" + pat + ")$"); err == nil {
				r.isRegex = true
				r.re = compiled
			} else {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

func matchRuleSet(rules []parsedRule, action, target string, roots []string) bool {
	for _, r := range rules {
		if r.action != action {
			continue
		}
		if r.target == "*" {
			return true
		}
		switch action {
		case "command":
			if r.isRegex {
				if r.re.MatchString(target) {
					return true
				}
				continue
			}
			if target == r.target || strings.HasPrefix(target, r.target+" ") {
				return true
			}
		case "mcp":
			if r.target == target {
				return true
			}
			if strings.HasSuffix(r.target, "/*") {
				prefix := strings.TrimSuffix(r.target, "*")
				if strings.HasPrefix(target, prefix) {
					return true
				}
			}
		case "read_file", "write_file":
			normTarget := normalizeRulePath(target)
			rulePath := r.target
			if !strings.HasPrefix(rulePath, "/") && len(roots) > 0 {
				rulePath = filepath.Join(roots[0], filepath.FromSlash(rulePath))
			}
			normRule := strings.TrimSuffix(normalizeRulePath(rulePath), "/")
			if normTarget == normRule || strings.HasPrefix(normTarget, normRule+"/") {
				return true
			}
		}
	}
	return false
}

func (e *Engine) isInsideWorkspace(realPath string) bool {
	norm := normalizeComparePath(realPath)
	for _, root := range e.workspaceRoots {
		normRoot := strings.TrimSuffix(normalizeComparePath(root), "/")
		if norm == normRoot || strings.HasPrefix(norm, normRoot+"/") {
			return true
		}
	}
	return false
}

func (e *Engine) isTempOrScratch(realPath string) bool {
	norm := normalizeComparePath(realPath)
	tmp := strings.TrimSuffix(normalizeComparePath(os.TempDir()), "/")
	if tmp != "" && (norm == tmp || strings.HasPrefix(norm, tmp+"/")) {
		return true
	}
	home := userHomeDir()
	if home != "" {
		brain := strings.TrimSuffix(normalizeComparePath(filepath.Join(home, ".gemini")), "/")
		if norm == brain || strings.HasPrefix(norm, brain+"/") {
			return true
		}
	}
	return false
}

func (e *Engine) isSensitivePath(realPath string) bool {
	norm := normalizeComparePath(realPath)
	for _, s := range e.sensitiveDirs {
		ns := strings.TrimSuffix(normalizeComparePath(s), "/")
		if norm == ns || strings.HasPrefix(norm, ns+"/") {
			return true
		}
	}
	return false
}

func isProtectedGitInternal(realPath string) bool {
	norm := "/" + strings.Trim(normalizeRulePath(realPath), "/") + "/"
	if strings.Contains(norm, "/.git/hooks/") || strings.HasSuffix(norm, "/.git/config/") || strings.Contains(norm, "/.git/objects/") {
		return true
	}
	return false
}

func isProtectedSystemRoot(realPath string) bool {
	norm := strings.TrimSuffix(normalizeRulePath(realPath), "/")
	for _, sys := range []string{"/windows", "/program files", "/program files (x86)", "/etc", "/usr", "/bin", "/sbin", "/boot", "/system"} {
		if norm == sys || strings.HasPrefix(norm, sys+"/") {
			return true
		}
	}
	return false
}

func resolveWithSymlinks(baseDir, p string) (absPath string, realPath string, err error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", "", errors.New("path cannot be empty")
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) || p == "~" {
		if h := userHomeDir(); h != "" {
			if len(p) == 1 {
				p = h
			} else {
				p = filepath.Join(h, p[2:])
			}
		}
	}
	if filepath.IsAbs(p) {
		absPath = filepath.Clean(p)
	} else {
		if baseDir == "" {
			baseDir, _ = os.Getwd()
		}
		absPath = filepath.Clean(filepath.Join(baseDir, p))
	}
	realPath, err = evalSymlinksExisting(absPath)
	if err != nil {
		return absPath, absPath, nil
	}
	return absPath, realPath, nil
}

// evalSymlinksExisting walks up to the deepest existing ancestor, resolves symlinks, and re-joins the non-existent tail.
func evalSymlinksExisting(absPath string) (string, error) {
	cur := absPath
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return absPath, err
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

func normalizeComparePath(p string) string {
	s := filepath.ToSlash(filepath.Clean(p))
	if runtime.GOOS == "windows" {
		s = strings.ToLower(s)
	}
	return s
}

// normalizeRulePath strips Windows drive letters and converts backslashes to forward slashes per Antigravity CLI docs.
func normalizeRulePath(p string) string {
	s := filepath.ToSlash(filepath.Clean(p))
	if len(s) >= 2 && s[1] == ':' {
		s = s[2:]
	}
	if runtime.GOOS == "windows" {
		s = strings.ToLower(s)
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return s
}

func userHomeDir() string {
	if runtime.GOOS == "windows" {
		if up := os.Getenv("USERPROFILE"); up != "" {
			return up
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}
