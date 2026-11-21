package main

import (
	"testing"
)

func TestParseToolSetProfiles(t *testing.T) {
	all, err := parseToolSet("all")
	if err != nil || !all.Read || !all.Tasks || !all.Docs {
		t.Fatalf("expected all tools enabled, got %+v (err=%v)", all, err)
	}
	execOnly, err := parseToolSet("exec")
	if err != nil || execOnly.Read || execOnly.Tasks || execOnly.Docs {
		t.Fatalf("expected exec-only to disable read/tasks/docs, got %+v (err=%v)", execOnly, err)
	}
}

func TestRunServeRejectsUnauthenticatedWildcardCORSWithoutExitingProcess(t *testing.T) {
	code := runServe([]string{
		"--http", "127.0.0.1:0",
		"--cors-origin", "*",
		"--data-dir", t.TempDir(),
		"--cwd", t.TempDir(),
	})
	if code == 0 {
		t.Fatal("expected non-zero exit code when --cors-origin '*' is passed without --token")
	}
}
