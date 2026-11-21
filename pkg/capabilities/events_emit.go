package capabilities

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aliothmoon/mcplex/pkg/diff"
	"github.com/aliothmoon/mcplex/pkg/events"
)

func cleanSlash(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(p))
}

func emitFileDiff(hub *events.Hub, sessionID, runID, txID, txPhase, tool, cwd, relPath, absPath string, before, after []byte, existedBefore, existsAfter bool) {
	if hub == nil || !hub.Active() {
		return
	}
	d := diff.Compute(before, after, existedBefore, existsAfter, diff.DefaultContextLines)
	kind := events.KindFileUpdate
	if !existedBefore && existsAfter {
		kind = events.KindFileCreate
	} else if existedBefore && !existsAfter {
		kind = events.KindFileDelete
	}
	summary := fmt.Sprintf("+%d -%d", d.Added, d.Removed)
	if kind == events.KindFileCreate {
		summary = fmt.Sprintf("+%d lines", d.Added)
	} else if kind == events.KindFileDelete {
		summary = fmt.Sprintf("-%d lines", d.Removed)
	}
	if d.IsBinary {
		summary = "binary"
	}
	hub.Emit(events.Event{
		Kind:      kind,
		SessionID: sessionID,
		RunID:     runID,
		TxID:      txID,
		TxPhase:   txPhase,
		Tool:      tool,
		Cwd:       cleanSlash(cwd),
		Path:      relPath,
		AbsPath:   cleanSlash(absPath),
		Status:    "ok",
		Summary:   summary,
		Diff:      &d,
	})
}

func emitFileRead(hub *events.Hub, sessionID, runID, tool, cwd, relPath, absPath, summary, content string) {
	if hub == nil || !hub.Active() {
		return
	}
	preview, trunc := events.CapPreview(content, events.DefaultReadPreview)
	hub.Emit(events.Event{
		Kind:       events.KindFileRead,
		SessionID:  sessionID,
		RunID:      runID,
		Tool:       tool,
		Cwd:        cleanSlash(cwd),
		Path:       relPath,
		AbsPath:    cleanSlash(absPath),
		Status:     "ok",
		Summary:    summary,
		Detail:     preview,
		DetailKind: "text",
		Truncated:  trunc,
	})
}

func emitFileRename(hub *events.Hub, sessionID, runID, txID, txPhase, tool, cwd, oldRel, oldAbs, newRel, newAbs string) {
	if hub == nil || !hub.Active() {
		return
	}
	hub.Emit(events.Event{
		Kind:       events.KindFileRename,
		SessionID:  sessionID,
		RunID:      runID,
		TxID:       txID,
		TxPhase:    txPhase,
		Tool:       tool,
		Cwd:        cleanSlash(cwd),
		OldPath:    oldRel,
		OldAbsPath: cleanSlash(oldAbs),
		Path:       newRel,
		AbsPath:    cleanSlash(newAbs),
		Status:     "ok",
		Summary:    oldRel + " -> " + newRel,
	})
}

func emitDirCreate(hub *events.Hub, sessionID, runID, tool, cwd, relPath, absPath string) {
	if hub == nil || !hub.Active() {
		return
	}
	hub.Emit(events.Event{
		Kind:      events.KindFileCreate,
		SessionID: sessionID,
		RunID:     runID,
		Tool:      tool,
		Cwd:       cleanSlash(cwd),
		Path:      relPath,
		AbsPath:   cleanSlash(absPath),
		Status:    "ok",
		Summary:   "directory",
	})
}

func emitTxStatus(hub *events.Hub, sessionID, runID, txID, phase string, files []string, note string) {
	if hub == nil || !hub.Active() {
		return
	}
	summary := fmt.Sprintf("%s %s (%d file(s))", txID, phase, len(files))
	if note != "" {
		summary += " · " + note
	}
	hub.Emit(events.Event{
		Kind:      events.KindTxStatus,
		SessionID: sessionID,
		RunID:     runID,
		TxID:      txID,
		TxPhase:   phase,
		Tool:      "tx." + phase,
		Status:    "ok",
		Summary:   summary,
		Detail:    strings.Join(files, "\n"),
	})
}

func formatReadOptsSummary(opts map[string]any, byteLen int) string {
	var parts []string
	if s := optInt(opts, "startLine", optInt(opts, "offset", 0)); s > 0 {
		if e := optInt(opts, "endLine", 0); e > 0 {
			parts = append(parts, fmt.Sprintf("L%d-%d", s, e))
		} else {
			parts = append(parts, fmt.Sprintf("L%d+", s))
		}
	} else if tail := optInt(opts, "tail", 0); tail > 0 {
		parts = append(parts, fmt.Sprintf("tail %d", tail))
	}
	parts = append(parts, formatHumanBytes(int64(byteLen)))
	return strings.Join(parts, " · ")
}

func emitSearchHits(hub *events.Hub, sessionID, runID, tool, searchRoot, pattern string, hits []SearchHit, total int) {
	if hub == nil || !hub.Active() {
		return
	}
	var sb strings.Builder
	for i, h := range hits {
		if i >= 120 {
			fmt.Fprintf(&sb, "... (+%d more hits)\n", len(hits)-i)
			break
		}
		fmt.Fprintf(&sb, "%s:%d: %s\n", h.File, h.Line, h.Text)
	}
	preview, trunc := events.CapPreview(sb.String(), events.DefaultReadPreview)
	countLabel := fmt.Sprintf("%d hits", len(hits))
	if total > len(hits) {
		countLabel = fmt.Sprintf("%d/%d hits", len(hits), total)
	}
	hub.Emit(events.Event{
		Kind:       events.KindSearch,
		SessionID:  sessionID,
		RunID:      runID,
		Tool:       tool,
		Cwd:        cleanSlash(searchRoot),
		AbsPath:    cleanSlash(searchRoot),
		Title:      pattern,
		Status:     "ok",
		Summary:    countLabel,
		Detail:     preview,
		DetailKind: "search",
		Truncated:  trunc,
	})
}

func emitSearchFiles(hub *events.Hub, sessionID, runID, tool, searchRoot, pattern string, files []string, total int) {
	if hub == nil || !hub.Active() {
		return
	}
	preview, trunc := events.CapPreview(strings.Join(files, "\n"), events.DefaultReadPreview)
	countLabel := fmt.Sprintf("%d files", len(files))
	if total > len(files) {
		countLabel = fmt.Sprintf("%d/%d files", len(files), total)
	}
	hub.Emit(events.Event{
		Kind:       events.KindSearch,
		SessionID:  sessionID,
		RunID:      runID,
		Tool:       tool,
		Cwd:        cleanSlash(searchRoot),
		AbsPath:    cleanSlash(searchRoot),
		Title:      pattern,
		Status:     "ok",
		Summary:    countLabel,
		Detail:     preview,
		DetailKind: "search",
		Truncated:  trunc,
	})
}

func emitChecksResult(hub *events.Hub, sessionID, runID, cwd string, res map[string]any) {
	if hub == nil || !hub.Active() || res == nil {
		return
	}
	status := "ok"
	if okVal, _ := res["ok"].(bool); !okVal {
		status = "error"
	}
	summary, _ := res["summary"].(string)
	var sb strings.Builder
	if items, ok := res["results"].([]map[string]any); ok {
		for _, item := range items {
			cmd, _ := item["command"].(string)
			okFlag, _ := item["ok"].(bool)
			out, _ := item["output"].(string)
			tag := "PASS"
			if !okFlag {
				tag = "FAIL"
			}
			fmt.Fprintf(&sb, "[%s] %s\n", tag, cmd)
			if strings.TrimSpace(out) != "" {
				sb.WriteString(strings.TrimSpace(out))
				sb.WriteString("\n\n")
			}
		}
	}
	preview, trunc := events.CapPreview(sb.String(), events.DefaultShellPreview)
	hub.Emit(events.Event{
		Kind:       events.KindShell,
		SessionID:  sessionID,
		RunID:      runID,
		Tool:       "sh.checks",
		Cwd:        cleanSlash(cwd),
		Title:      summary,
		Status:     status,
		Summary:    summary,
		Detail:     preview,
		DetailKind: "shell",
		Truncated:  trunc,
	})
}
