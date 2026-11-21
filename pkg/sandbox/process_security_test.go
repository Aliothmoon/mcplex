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
}
