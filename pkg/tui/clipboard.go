package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// writeClipboard is injectable for unit tests so tests never clobber the developer's clipboard.
var writeClipboard = writeSystemClipboard

type copyToastClearMsg struct {
	seq int
}

func copyToastTimerCmd(seq int) tea.Cmd {
	return tea.Tick(2500*time.Millisecond, func(time.Time) tea.Msg {
		return copyToastClearMsg{seq: seq}
	})
}

func formatCopyToast(text string) string {
	lines := strings.Count(text, "\n") + 1
	if lines > 1 {
		return fmt.Sprintf("✓ Copied %d lines (%d B)", lines, len(text))
	}
	trimmed := strings.TrimSpace(text)
	n := utf8.RuneCountInString(trimmed)
	if n > 0 && n <= 22 {
		return fmt.Sprintf("✓ Copied %q", trimmed)
	}
	return fmt.Sprintf("✓ Copied %d chars", utf8.RuneCountInString(text))
}
