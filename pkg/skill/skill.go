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
	return strings.TrimSpace(fullGuide) + "\n"
}

// Instructions returns the concise system-level instructions returned in MCP initialize.result.instructions.
func Instructions() string {
	return strings.TrimSpace(fullGuide)
}

// CompactExecDescription returns a self-describing tool description so even minimal MCP clients
// that ignore initialize.instructions and resources can immediately compose local tools via `exec`.
func CompactExecDescription(defaultMaxOutBytes int) string {
	if defaultMaxOutBytes <= 0 {
		defaultMaxOutBytes = 24576
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
			"state.{get,set,delete,keys,clear,page(key,n,size?)} [sync]; "+
			"tools.{search(q),describe(ref)} & mcp.<server>.<tool>(args); "+
			"P/payloads, Buffer, crypto, path, URL, setTimeout, sleep(ms), structuredClone, read-only process/os. "+
			"Output cap: %dB inline (oversized output auto-spills in memory to state.get('__result_N') and pages via print(state.page('__result_N', 2))). "+
			"Read resource mcplex://skill for full guide.",
		defaultMaxOutBytes,
	)
}
