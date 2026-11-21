package skill

import (
	"strings"
	"testing"
)

func TestProgressiveDisclosureSizesAndLookup(t *testing.T) {
	instr := InstructionsForTools(true, true)
	if len(instr) > 900 {
		t.Fatalf("InstructionsForTools should stay slim (<900B), got %d bytes", len(instr))
	}
	if !strings.Contains(instr, "docs({ topic })") {
		t.Fatalf("InstructionsForTools missing docs pointer:\n%s", instr)
	}

	desc := CompactExecDescriptionForTools(24576, true, true)
	if len(desc) > 850 {
		t.Fatalf("CompactExecDescriptionForTools should stay slim (<850B), got %d bytes", len(desc))
	}
	if !strings.Contains(desc, "docs({ topic })") {
		t.Fatalf("CompactExecDescriptionForTools missing docs pointer:\n%s", desc)
	}

	// Empty lookup returns compact topic index
	idx := Lookup("", "", true, true)
	if !strings.Contains(idx, "MCPlex Docs Index") || !strings.Contains(idx, "`tx`") || !strings.Contains(idx, "`tasks`") {
		t.Fatalf("unexpected topic index:\n%s", idx)
	}

	// Tasks topic omitted when hasTasks == false
	idxNoTasks := Lookup("", "", true, false)
	if strings.Contains(idxNoTasks, "**`tasks`**") {
		t.Fatalf("expected tasks topic hidden when hasTasks=false:\n%s", idxNoTasks)
	}

	// Exact topic & alias resolution
	txDoc := Lookup("transaction", "", true, true)
	if !strings.Contains(txDoc, "tx.begin()") || !strings.Contains(txDoc, "t.commit()") {
		t.Fatalf("alias 'transaction' failed to resolve tx topic:\n%s", txDoc)
	}
	shDoc := Lookup("shell", "", true, true)
	if !strings.Contains(shDoc, "sh.checks") || !strings.Contains(shDoc, "sh.run") {
		t.Fatalf("alias 'shell' failed to resolve sh topic:\n%s", shDoc)
	}

	// Keyword query search
	qRes := Lookup("", "rollback verify", true, true)
	if !strings.Contains(qRes, "tx.begin()") {
		t.Fatalf("keyword search 'rollback verify' should match tx topic:\n%s", qRes)
	}

	// Unknown topic falls back to keyword search instead of dead-ending
	fb := Lookup("ripgrep", "", true, true)
	if !strings.Contains(fb, "search.rg") {
		t.Fatalf("alias/fallback 'ripgrep' should resolve search docs:\n%s", fb)
	}

	// FullGuide assembles all sections from the same registry
	full := FullGuideForTools(true, true)
	if !strings.Contains(full, "fs.hash") || !strings.Contains(full, "state.page") || !strings.Contains(full, "tasks.wait") {
		t.Fatalf("FullGuideForTools missing expected sections")
	}
}
