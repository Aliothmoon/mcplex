// Package skill embeds the client-agnostic MCPlex Code Mode guide delivered via
// MCP initialize.instructions, exec tool description, and the mcplex://skill resource.
package skill

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed guide.md
var fullGuide string

// FullGuide returns the complete Markdown Code Mode skill manual (served via mcplex://skill and `mcplex skill`).
func FullGuide() string {
	return FullGuideForTools(true, true)
}

// FullGuideForTools returns the Markdown skill guide annotated with the active MCP tool profile.
func FullGuideForTools(hasRead, hasTasks bool) string {
	var enabled []string
	enabled = append(enabled, "`exec`")
	if hasRead {
		enabled = append(enabled, "`read`")
	}
	if hasTasks {
		enabled = append(enabled, "`spawn`", "`task`")
	}
	banner := fmt.Sprintf("<!-- Active MCPlex tools: %s -->\n", strings.Join(enabled, ", "))
	return banner + strings.TrimSpace(fullGuide) + "\n"
}

// Instructions returns the concise system-level instructions returned in MCP initialize.result.instructions.
func Instructions() string {
	return InstructionsForTools(true, true)
}

// InstructionsForTools tailors MCP initialize.result.instructions to the active toolset.
func InstructionsForTools(hasRead, hasTasks bool) string {
	var extra []string
	if hasRead {
		extra = append(extra, "Use `read` for whole files, paginated ranges, tail logs, or screenshots/images.")
	}
	if hasTasks {
		extra = append(extra, "Use `spawn` + `task` for >15s builds, full test suites, interactive commands, and daemons (with `task({ action: 'wait', taskId, expect: 'regex' })`).")
	}
	if len(extra) == 0 {
		return strings.TrimSpace(fullGuide)
	}
	return strings.Join(extra, " ") + "\n\n" + strings.TrimSpace(fullGuide)
}

// CompactExecDescription returns a self-describing tool description for `exec` assuming default tools.
func CompactExecDescription(defaultMaxOutBytes int) string {
	return CompactExecDescriptionForTools(defaultMaxOutBytes, true, true)
}

// CompactExecDescriptionForTools returns a self-describing `exec` tool description tailored to enabled sibling tools.
func CompactExecDescriptionForTools(defaultMaxOutBytes int, hasRead, hasTasks bool) string {
	if defaultMaxOutBytes <= 0 {
		defaultMaxOutBytes = 24576
	}
	tasksSnippet := ""
	if hasTasks {
		tasksSnippet = "tasks.{spawn(cmd,{daemon?,waitMs?,name?,cwd?}),list(),status(id),logs(id,{tail?,offset?}),wait(id,{waitMs?,expect?}),input(id,text),kill(id),killAll()}; "
	}
	siblingHint := ""
	if hasRead && hasTasks {
		siblingHint = " Use sibling `read` for whole files/images and `spawn`+`task` for >15s builds or daemons."
	} else if hasRead {
		siblingHint = " Use sibling `read` for whole files/images."
	} else if hasTasks {
		siblingHint = " Use sibling `spawn`+`task` for >15s builds or daemons."
	}
	return fmt.Sprintf(
		"Execute a TypeScript async body (top-level await, print()/console.log(), return; do not wrap in async function) inside a fast local Code Mode sandbox that composes local tools and external MCP servers in ONE turn. "+
			"Globals: "+
			"fs.{read(p,{startLine?,endLine?,tail?,lineNumbers?}),cat(paths),write(p,s,{eol?}),exists(p) [sync],bytes(p,{offset?,length?}),hash(p,algo?),stat(p),list(p,{detail?}),tree(p,depth?),mkdir(p),rm(p,{force?}),rename(a,b),cp(a,b,{recursive?})}; "+
			"search.{rg(pat,{path?,glob?,literal?,ignoreCase?,contextLines?,maxResults?,filesOnly?,count?}),glob(pat,{path?,maxResults?})}; "+
			"ast.{outline(p),outlines(paths),read(p,sym),enclosingSymbol(p,line)}; "+
			"edit.{apply({edits:[{path,hunks:[{target,replacement,symbol?,nth?,startLine?,endLine?,replaceAll?}]}],dryRun?}),replace(p,old,new,opts?)}; "+
			"tx.begin()->t.{edit,write,rm,rename,verify(cmds),diff,status,commit,rollback} (uncommitted tx auto-rolls back); "+
			"git.{status,diff,log,show,blame,query} (read-only); "+
			"sh.{exec(cmd,{cwd?,env?,shell?,timeoutMs?,compact?,throwOnError?}),run(file,args,opts?),checks(cmds)->{ok,summary,results:[{name,command,ok,exitCode,timedOut,durationMs,output}],failures?}}; "+
			"%s"+
			"state.{get,set,delete,keys,clear,page(key,n,size?)} [sync]; "+
			"tools.{search(q),describe(ref)} & mcp.<server>.<tool>(args); "+
			"P/payloads, Buffer, crypto, path, URL, setTimeout, sleep(ms), structuredClone, read-only process/os. "+
			"Output cap: %dB inline (oversized output auto-spills in memory to state.get('__result_N') and pages via print(state.page('__result_N', 2))).%s "+
			"Read resource mcplex://skill for full guide.",
		tasksSnippet,
		defaultMaxOutBytes,
		siblingHint,
	)
}
