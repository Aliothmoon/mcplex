package policy

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Built-in command guard.
//
// This is a best-effort, defense-in-depth GUARDRAIL against accidental destructive or outward-facing
// commands issued by the model. It is NOT a security boundary: `sh.exec` hands the command line to a
// real shell, so interpreters (`node -e`, `python -c`), obfuscation, or indirection can always bypass
// regex filtering. Matching commands are not executed; they return APPROVAL_REQUIRED so the model
// escalates to the host's native `run_command`, where the user confirms interactively.
//
// Rule categories follow the shell-family grouping popularized by Netcatty's AI command blocklist
// (common / POSIX / PowerShell / cmd) but the patterns are written independently for Go's RE2 engine
// (no look-around; linear-time matching, so no ReDoS).

type commandRule struct {
	name string
	re   *regexp.Regexp
	// veto, when set, receives the matched text and may let it through (RE2 has no negative look-ahead).
	veto func(match string) bool
}

// seg matches characters inside one shell segment (stops at separators and newlines).
const seg = `[^\n;&|]`

// gitPrefix anchors a rule to the git SUBCOMMAND position, skipping global options such as
// `-C <path>`, `-c k=v`, `--git-dir <dir>`, `--no-pager`, so `git log --grep="push"` never matches `push`.
const gitPrefix = `(?i:\bgit(?:\.exe)?)(?:\s+(?:-[Cc]\s+(?:"[^"]*"|'[^']*'|\S+)|--[a-zA-Z][a-zA-Z-]*(?:[= ](?:"[^"]*"|'[^']*'|\S+))?|-[a-zA-Z]))*\s+`

func rule(name, pattern string) commandRule {
	return commandRule{name: name, re: regexp.MustCompile(pattern)}
}

func ruleVeto(name, pattern string, veto func(string) bool) commandRule {
	return commandRule{name: name, re: regexp.MustCompile(pattern), veto: veto}
}

var (
	gitRestoreStagedOnly = regexp.MustCompile(`\s(?:--staged|-S)\b`)
	gitRestoreWorktree   = regexp.MustCompile(`\s(?:--worktree|-W)\b`)
	gitConfigReadOnly    = regexp.MustCompile(`\s(?:--get(?:-all|-regexp|-urlmatch)?|--list|-l|--show-origin|--show-scope)\b`)
	gitStashPush         = regexp.MustCompile(`(?i)\bstash\s+push\b`)
)

// rawCommandRules run against the FULL raw command text (including quoted arguments) so remote/nested
// shells such as `adb shell "reboot"`, `ssh host "rm -rf /"` or `sh -c "..."` are still caught.
var rawCommandRules = []commandRule{
	// --- common: recursive + force deletion (any flag order / spelling) ---
	rule("recursive delete",
		`(?i)\brm\s+(?:`+seg+`*\s)?(?:-[a-z]*(?:r[a-z]*f|f[a-z]*r)[a-z]*|(?:-[a-z]*r[a-z]*|--recursive)\s+(?:`+seg+`*\s)?(?:-[a-z]*f[a-z]*|--force)|(?:-[a-z]*f[a-z]*|--force)\s+(?:`+seg+`*\s)?(?:-[a-z]*r[a-z]*|--recursive))\b`),
	rule("recursive delete",
		`(?i)\b(?:remove-item|ri|rmdir|rd|del|erase)\b`+seg+`*\s-(?:r|re|rec[a-z]*)\b`+seg+`*\s-(?:f|fo[a-z]*)\b`),
	rule("recursive delete",
		`(?i)\b(?:remove-item|ri|rmdir|rd|del|erase)\b`+seg+`*\s-(?:f|fo[a-z]*)\b`+seg+`*\s-(?:r|re|rec[a-z]*)\b`),
	rule("recursive delete", `(?i)\b(?:rd|rmdir|del|erase)\b`+seg+`*\s/s\b`),
	rule("recursive delete", `(?i)\bfind\b`+seg+`*\s-delete\b`),
	rule("power off/reboot", `(?i)\b(?:shutdown|reboot|poweroff|halt|stop-computer|restart-computer)\b`),
	rule("power off/reboot", `(?i)\binit\s+[06]\b`),
	rule("kills all processes", `(?i)\bkill\s+(?:-[a-z0-9]+\s+)*-1\s*(?:$|[;&|\n])`),

	// --- POSIX native: disks, devices, system files, remote code ---
	rule("disk tool", `(?i)\b(?:mkfs(?:\.[a-z0-9]+)?|fdisk|sfdisk|parted|wipefs|diskpart)\b`),
	rule("raw device write", `(?i)\bdd\b`+seg+`*\bof=["']?/dev/`),
	rule("raw device write", `>\s*["']?/dev/(?:sd|hd|vd|xvd|nvme|mmcblk|disk)`),
	rule("system file write", `(?:>|\btee\b`+seg+`*\s)\s*["']?/etc/`),
	rule("recursive chmod/chown on / or ~", `(?i)\b(?:chmod|chown|chgrp)\s+(?:`+seg+`*\s)?(?:-[a-z]*R[a-z]*|--recursive)\s+(?:`+seg+`*\s)?["']?(?:/|~/?)["']?(?:\s|$)`),
	rule("moves /", `(?i)\bmv\s+(?:-[a-z]+\s+)*["']?/["']?(?:\s|$)`),
	rule("fork bomb", `:\s*\(\s*\)\s*\{`+`[^}]*:\s*\|\s*:`),
	rule("runs remote code", `(?i)\b(?:curl|wget|fetch|irm|iwr|invoke-restmethod|invoke-webrequest)\b[^\n;&]*\|\s*(?:sudo\s+)?(?:sh|bash|zsh|dash|ksh|fish|pwsh|powershell|python[0-9.]*|perl|ruby|node|iex|invoke-expression)\b`),
	rule("runs remote code", `(?i)\b(?:sh|bash|zsh|dash|ksh)\b`+seg+`*(?:<\(|\$\()\s*(?:curl|wget)\b`),
	rule("runs encoded code", `(?i)\bbase64\b[^\n;&]*\|\s*(?:sudo\s+)?(?:sh|bash|zsh|dash|ksh|pwsh|powershell|python[0-9.]*|perl|iex)\b`),

	// --- PowerShell / cmd ---
	rule("runs encoded code", `(?i)\b(?:pwsh|powershell)(?:\.exe)?\b`+seg+`*\s-(?:e|ec|en|enc|encodedcommand)\s`),
	rule("execution policy bypass", `(?i)\bset-executionpolicy\b`+seg+`*\b(?:bypass|unrestricted)\b`),
	rule("disk wipe", `(?i)\b(?:format-volume|clear-disk|initialize-disk|remove-partition)\b`),
	rule("disk format", `(?i)\bformat(?:\.com)?\s+[a-z]:`),
	rule("system destruction", `(?i)\b(?:reg(?:\.exe)?\s+delete|bcdedit|vssadmin\s+delete|wmic\s+shadowcopy\s+delete|cipher\s+/w)\b`),

	// --- git: outward-facing or work-discarding operations ---
	ruleVeto("publishes to remote", gitPrefix+`push\b`, func(m string) bool { return gitStashPush.MatchString(m) }),
	rule("discards work", gitPrefix+`reset\b`+seg+`*\s--(?:hard|merge|keep)\b`),
	rule("discards work", gitPrefix+`clean\b`+seg+`*\s(?:-[a-zA-Z]*f[a-zA-Z]*|--force)\b`),
	rule("discards work", gitPrefix+`checkout\b`+seg+`*\s(?:-f|--force|--|\.|\./\S*|:/\S*)(?:\s|$)`),
	ruleVeto("discards work", gitPrefix+`restore\b`+seg+`*`, func(m string) bool {
		return gitRestoreStagedOnly.MatchString(m) && !gitRestoreWorktree.MatchString(m)
	}),
	rule("discards work", gitPrefix+`switch\b`+seg+`*\s(?:-f|--force|--discard-changes)\b`),
	rule("discards work", gitPrefix+`stash\s+(?:drop|clear)\b`),
	rule("discards work", gitPrefix+`branch\b`+seg+`*\s(?:-[a-zA-Z]*D[a-zA-Z]*\b|--delete\s(?:`+seg+`*\s)?(?:-f|--force)\b|(?:-f|--force)\s(?:`+seg+`*\s)?(?:-d|--delete)\b)`),
	rule("rewrites history", gitPrefix+`(?:filter-branch|filter-repo|reflog\s+(?:expire|delete)|update-ref\s+(?:`+seg+`*\s)?-d)\b`),
	rule("rewrites history", gitPrefix+`gc\b`+seg+`*\s--prune\b`),
	ruleVeto("global git config", gitPrefix+`config\b`+seg+`*\s--(?:global|system)\b`+seg+`*`, func(m string) bool {
		return gitConfigReadOnly.MatchString(m)
	}),

	// --- outward-facing publish / infra mutation ---
	rule("publishes package", `(?i)\b(?:(?:npm|pnpm|yarn|bun)\s+(?:un)?publish|cargo\s+publish|twine\s+upload|gem\s+push|dotnet\s+nuget\s+push|docker\s+push|podman\s+push)(?:\s|$)`),
	rule("GitHub write", `(?i)\bgh\s+(?:repo\s+(?:delete|archive|rename)|release\s+(?:create|delete|upload)|pr\s+merge)\b`),
	rule("infra change", `(?i)\b(?:kubectl\s+delete|terraform\s+(?:apply|destroy)|helm\s+uninstall)\b`),

	// --- credentials & self-escalation via shell ---
	rule("credential access", `(?i)(?:^|[\s"'=:/\\])\.(?:ssh|gnupg|aws|kube)(?:[/\\]|["']|\s|$)|\.git-credentials\b|(?:^|[\s"'/\\])[._]netrc\b|\.docker[/\\]config\.json|\b(?:oauth_creds|mcp_oauth_tokens)\.json\b`),
	rule("shell startup file", `(?i)(?:>>?\s*|\btee\s+(?:-a\s+)?|\b(?:cp|mv|ln|copy|move|set-content|add-content|out-file)\b`+seg+`*\s)["']?\S*\.(?:bashrc|bash_profile|bash_login|zshrc|zprofile|zshenv|zlogin|profile)\b`),
	rule("shell startup file", `(?i)(?:>>?\s*|\b(?:set-content|add-content|out-file|copy-item|move-item)\b`+seg+`*\s)["']?\$profile\b`),
	rule("agent config", `(?i)(?:>>?\s*|\btee\s+(?:-a\s+)?|\b(?:cp|mv|ln|copy|move|set-content|add-content|out-file|copy-item|move-item)\b`+seg+`*\s)["']?\S*[/\\]\.(?:gemini|claude|agents|codex|cursor)[/\\]\S*(?:settings(?:\.local)?\.json|mcp_config\.json|mcp\.json|policy\.json|hooks\.json|config\.toml)`),
}

// surfaceCommandRules run against the host-command SURFACE (quoted arguments of non-shell commands stripped),
// so `rg "eval"` or `git log --grep="iex"` are not flagged while `eval "$X"` / `... | iex` are.
var surfaceCommandRules = []commandRule{
	rule("dynamic eval", `(?i)(?:^|[;&|(\n])\s*(?:eval|iex|invoke-expression)(?:\s|$)`),
}

// matchBuiltinCommandGuard returns the name of the first built-in guard matched by cmdLine, or "".
func matchBuiltinCommandGuard(cmdLine string) string {
	surface := extractHostCommandSurface(cmdLine, 0)
	if hostPrivEscPattern.MatchString(surface) {
		return "privilege escalation"
	}
	for _, r := range rawCommandRules {
		loc := r.re.FindStringIndex(cmdLine)
		if loc == nil {
			continue
		}
		if r.veto != nil && r.veto(cmdLine[loc[0]:loc[1]]) {
			continue
		}
		return r.name
	}
	for _, r := range surfaceCommandRules {
		if r.re.MatchString(surface) {
			return r.name
		}
	}
	return ""
}

// ---------------------------------------------------------------------------------------------
// Protected configuration paths (self-escalation guard for fs.write / edit.* / tx.*).
// ---------------------------------------------------------------------------------------------

// Agent harness directories whose permission / MCP / hook config files must not be rewritten from inside
// the sandbox: doing so would let the model grant itself new permissions or spawn arbitrary MCP servers.
var agentConfigDirs = map[string]bool{".gemini": true, ".claude": true, ".agents": true, ".codex": true, ".cursor": true}

var agentConfigFiles = map[string]bool{
	"settings.json":       true,
	"settings.local.json": true,
	"mcp_config.json":     true,
	"mcp.json":            true,
	"policy.json":         true,
	"hooks.json":          true,
	"config.toml":         true,
}

// Home-anchored files sourced or honored automatically by shells / git / agent CLIs.
var homeStartupFiles = []string{
	".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".profile",
	".zshrc", ".zprofile", ".zshenv", ".zlogin", ".zlogout",
	".gitconfig", ".claude.json",
	filepath.Join(".config", "fish", "config.fish"),
	filepath.Join(".config", "git", "config"),
}

// Home-anchored credential stores: blocked for BOTH read and write (in addition to ~/.ssh, ~/.gnupg, ~/.aws).
var homeCredentialPaths = []string{
	".git-credentials",
	".netrc",
	"_netrc",
	".npmrc",
	".pypirc",
	".kube",
	filepath.Join(".docker", "config.json"),
	filepath.Join(".config", "gh"),
	filepath.Join(".config", "gcloud"),
	".azure",
	filepath.Join(".cargo", "credentials"),
	filepath.Join(".cargo", "credentials.toml"),
	filepath.Join(".gemini", "oauth_creds.json"),
	filepath.Join(".gemini", ".env"),
	filepath.Join(".claude", ".credentials.json"),
	filepath.Join(".codex", "auth.json"),
	filepath.Join("AppData", "Roaming", "GitHub CLI"),
	filepath.Join("AppData", "Roaming", "gcloud"),
}

// isProtectedConfigPath reports whether writing p could escalate privileges of the agent/plugin or
// persist code execution (agent permission/MCP/hook configs, plugin install dir, shell startup files, git config).
func isProtectedConfigPath(p string) bool {
	norm := strings.ToLower(filepath.ToSlash(filepath.Clean(p)))
	base := path.Base(norm)
	parts := strings.Split(norm, "/")

	if agentConfigFiles[base] {
		for _, seg := range parts[:len(parts)-1] {
			if agentConfigDirs[seg] {
				return true
			}
		}
	}
	if strings.Contains(norm, "/.gemini/config/plugins/") || strings.Contains(norm, "/.agents/plugins/") {
		return true
	}
	for _, env := range []string{"PLUGIN_ROOT"} {
		if root := strings.TrimSpace(os.Getenv(env)); root != "" {
			r := strings.TrimSuffix(strings.ToLower(filepath.ToSlash(filepath.Clean(root))), "/")
			if norm == r || strings.HasPrefix(norm, r+"/") {
				return true
			}
		}
	}
	if pd := strings.TrimSpace(os.Getenv("PLUGIN_DATA")); pd != "" {
		if norm == strings.ToLower(filepath.ToSlash(filepath.Join(pd, "policy.json"))) {
			return true
		}
	}
	// PowerShell profiles: Documents/PowerShell/*profile.ps1, Documents/WindowsPowerShell/*profile.ps1 (incl. OneDrive redirection).
	if strings.HasSuffix(base, "profile.ps1") {
		for _, seg := range parts {
			if seg == "powershell" || seg == "windowspowershell" {
				return true
			}
		}
	}
	if home := userHomeDir(); home != "" {
		h := strings.TrimSuffix(strings.ToLower(filepath.ToSlash(filepath.Clean(home))), "/")
		for _, rel := range homeStartupFiles {
			if norm == h+"/"+strings.ToLower(filepath.ToSlash(rel)) {
				return true
			}
		}
	}
	return false
}
