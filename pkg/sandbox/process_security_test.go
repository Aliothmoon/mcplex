package sandbox

import "testing"

func TestRedactURLCredentials(t *testing.T) {
	got := redactURLCredentials("postgres://alice:supersecret@db.internal:5432/prod?sslmode=require")
	if got != "postgres://alice:redacted@db.internal:5432/prod?sslmode=require" {
		t.Fatalf("unexpected redacted URL: %q", got)
	}
	plain := "https://example.com/api/v1"
	if redactURLCredentials(plain) != plain {
		t.Fatalf("URL without password should remain unchanged")
	}

	// Single-token userinfo (PATs in Git/HTTPS URLs)
	singleTok := redactURLCredentials("https://ghp_superSecretToken123@github.com/org/repo.git")
	if singleTok != "https://redacted@github.com/org/repo.git" {
		t.Fatalf("single-token userinfo was not redacted: %q", singleTok)
	}

	// Passwords containing unescaped #, ?, %, and @ where url.Parse fails or misparses authority
	tricky := []struct {
		in   string
		want string
	}{
		{"postgres://alice:p#ssword@db.internal:5432/prod", "postgres://alice:redacted@db.internal:5432/prod"},
		{"postgres://alice:p?ssword@db.internal:5432/prod", "postgres://alice:redacted@db.internal:5432/prod"},
		{"postgres://alice:p%zzword@db.internal:5432/prod", "postgres://alice:redacted@db.internal:5432/prod"},
		{"postgres://alice:p@ss#w%zz@db.internal:5432/prod", "postgres://alice:redacted@db.internal:5432/prod"},
		{"redis://:sec#ret@127.0.0.1:6379/0", "redis://:redacted@127.0.0.1:6379/0"},
		{"https://example.com/api/v1?email=alice@example.com", "https://example.com/api/v1?email=alice@example.com"},
		{"https://example.com?email=alice@example.com", "https://example.com?email=alice@example.com"},
	}
	for _, tc := range tricky {
		if got := redactURLCredentials(tc.in); got != tc.want {
			t.Errorf("redactURLCredentials(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Expanded secret env key patterns: AUTHORIZATION, BEARER, COOKIE, SESSION
	for _, k := range []string{"AUTHORIZATION", "HTTP_AUTHORIZATION", "BEARER", "API_BEARER", "COOKIE", "SESSION", "SESSION_ID"} {
		if !isSecretEnvName(k) {
			t.Errorf("expected %s to be matched as secret env key", k)
		}
	}
}
