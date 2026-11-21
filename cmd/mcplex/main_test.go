package main

import (
	"flag"
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

func TestBuildPublicExposureValidation(t *testing.T) {
	data := t.TempDir()
	if hosts, srv, err := buildPublicExposure("127.0.0.1:8787", "/mcp", "", false, "", data, nil); err != nil || hosts != nil || srv != nil {
		t.Fatalf("no exposure flags should be a no-op: %v %v %v", hosts, srv, err)
	}
	if hosts, srv, err := buildPublicExposure("127.0.0.1:8787", "/mcp", "https://x.trycloudflare.com", false, "", data, nil); err != nil || srv != nil || len(hosts) != 1 || hosts[0] != "x.trycloudflare.com" {
		t.Fatalf("--public-url only: %v %v %v", hosts, srv, err)
	}
	for name, c := range map[string]struct {
		addr, pub, token string
	}{
		"oauth without http":  {"", "https://mcp.example.com", "secret-secret-secret"},
		"oauth without token": {"127.0.0.1:8787", "https://mcp.example.com", ""},
		"oauth without url":   {"127.0.0.1:8787", "", "secret-secret-secret"},
		"oauth plain http":    {"127.0.0.1:8787", "http://mcp.example.com", "secret-secret-secret"},
	} {
		if _, _, err := buildPublicExposure(c.addr, "/mcp", c.pub, true, c.token, data, nil); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	hosts, srv, err := buildPublicExposure("127.0.0.1:8787", "/mcp", "https://mcp.example.com", true, "secret-secret-secret", data, []string{"loopback"})
	if err != nil || srv == nil || srv.Resource() != "https://mcp.example.com/mcp" || len(hosts) != 1 || hosts[0] != "mcp.example.com" {
		t.Fatalf("valid oauth exposure: %v %v %v", hosts, srv, err)
	}
}

func TestParseInterspersedFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	dir := fs.String("data-dir", "default", "")
	js := fs.Bool("json", false, "")
	pos, err := parseInterspersed(fs, []string{"all", "--data-dir", "X", "id2", "--json", "--", "--literal"})
	if err != nil || *dir != "X" || !*js || len(pos) != 3 || pos[0] != "all" || pos[1] != "id2" || pos[2] != "--literal" {
		t.Fatalf("got pos=%v dir=%q json=%v err=%v", pos, *dir, *js, err)
	}
}
