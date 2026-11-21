//go:build !windows

package tui

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

func writeSystemClipboard(text string) error {
	var candidates [][]string
	if runtime.GOOS == "darwin" {
		candidates = [][]string{{"pbcopy"}}
	} else {
		candidates = [][]string{
			{"wl-copy"},
			{"xclip", "-selection", "clipboard"},
			{"xsel", "--clipboard", "--input"},
		}
	}
	var lastErr error
	for _, argv := range candidates {
		bin, err := exec.LookPath(argv[0])
		if err != nil {
			lastErr = err
			continue
		}
		cmd := exec.Command(bin, argv[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("no clipboard tool: %w", lastErr)
	}
	return fmt.Errorf("no clipboard tool available")
}
