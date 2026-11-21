package capabilities

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aliothmoon/mcplex/pkg/policy"
)

func TestSearchRgAndGlobRespectPerFileReadPolicy(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "public.txt"), []byte("TOKEN_MARKER=public-ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secretDir := filepath.Join(ws, "vault")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "creds.txt"), []byte("TOKEN_MARKER=super-secret-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	pol.LoadSettingsJSON([]byte(`{"permissions":{"deny":["read_file(vault/)"]}}`))
	binder := &FSSearchBinder{Policy: pol}

	q := searchQuery{pattern: "TOKEN_MARKER", path: ".", maxResults: 50, caseSensitive: true}

	// 1. Default search path (rg if present, else fallback)
	hits, _, err := binder.runTextOrRegexSearch(context.Background(), ws, ws, q)
	if err != nil {
		t.Fatalf("runTextOrRegexSearch failed: %v", err)
	}
	if len(hits) != 1 || hits[0].File != "public.txt" {
		t.Fatalf("expected only public.txt hit, got: %+v", hits)
	}

	// 2. Explicit Go fallback path
	fbHits, err := runGoFallbackSearch(context.Background(), pol, ws, ws, q)
	if err != nil {
		t.Fatalf("runGoFallbackSearch failed: %v", err)
	}
	if len(fbHits) != 1 || fbHits[0].File != "public.txt" {
		t.Fatalf("expected only public.txt hit in Go fallback, got: %+v", fbHits)
	}

	// 3. rg --count path (when rg is installed)
	if rgBin := binder.resolveRg(); rgBin != "" {
		counts, _, err := runRgCounts(context.Background(), pol, rgBin, ws, ws, q)
		if err != nil {
			t.Fatalf("runRgCounts failed: %v", err)
		}
		if len(counts) != 1 || counts[0].file != "public.txt" {
			t.Fatalf("expected only public.txt in rg counts, got: %+v", counts)
		}
	}

	// 4. glob search must also skip denied paths
	files, _, _, err := binder.runGlobSearch(context.Background(), ws, ws, "**/*.txt", 50)
	if err != nil {
		t.Fatalf("runGlobSearch failed: %v", err)
	}
	if len(files) != 1 || files[0] != "public.txt" {
		t.Fatalf("expected only public.txt in glob results, got: %+v", files)
	}
}
