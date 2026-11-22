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

	pubNoTok := runServe([]string{
		"--http", "127.0.0.1:0",
		"--public-url", "https://switched-final-sends-pre.trycloudflare.com",
		"--data-dir", t.TempDir(),
		"--cwd", t.TempDir(),
	})
	if pubNoTok == 0 {
		t.Fatal("expected non-zero exit code when headless --http passes --public-url without --token")
	}
}

func TestShouldAutoGenerateUITokenAndEntropy(t *testing.T) {
	if shouldAutoGenerateUIToken(true, false, false, "", "", "127.0.0.1:8787", "") {
		t.Fatal("plain local loopback --ui must stay passwordless by default")
	}
	if !shouldAutoGenerateUIToken(true, false, true, "", "https://mcp.example.com", "127.0.0.1:8787", "") {
		t.Fatal("--ui --oauth without --token must auto-generate a token")
	}
	if !shouldAutoGenerateUIToken(true, false, false, "", "https://x.trycloudflare.com", "127.0.0.1:8787", "") {
		t.Fatal("--ui --public-url without --token must auto-generate a token")
	}
	if !shouldAutoGenerateUIToken(true, false, false, "", "", "0.0.0.0:8787", "") {
		t.Fatal("--ui on non-loopback bind without --token must auto-generate a token")
	}
	if shouldAutoGenerateUIToken(true, false, true, "explicit-user-secret", "https://mcp.example.com", "127.0.0.1:8787", "") {
		t.Fatal("explicit --token must never be overwritten")
	}
	if shouldAutoGenerateUIToken(false, false, true, "", "https://mcp.example.com", "127.0.0.1:8787", "") {
		t.Fatal("non-UI mode must not silently mint ephemeral tokens")
	}

	t1, err1 := generateRandomToken()
	t2, err2 := generateRandomToken()
	if err1 != nil || err2 != nil || len(t1) != 40 || t1 == t2 {
		t.Fatalf("unexpected generated tokens: %q (%v), %q (%v)", t1, err1, t2, err2)
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
