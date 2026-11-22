package capabilities

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/policy"
)

func TestSearchGlobPrefixPushdownBuildArtifactsGitignoreAndParallelScale(t *testing.T) {
	ws := t.TempDir()
	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Simulate an Android/C++ repo with .ignore (honored by rg), build/ (with '*' .ignore inside),
	// denied vault/, and 600 generated files across subdirectories.
	mustWrite(".ignore", "out/\nbuild/\n")
	mustWrite("build/.ignore", "*\n")
	mustWrite("native/src/version.map", "V1 {};\n")
	mustWrite("scripts/staging/pack.py", "# pack\n")
	mustWrite("scripts/guest/init.sh", "#!/bin/sh\n")
	mustWrite("build/native/modern/libvm02_worker.so", "ELF")
	mustWrite("build/native/modern/vm02_probe", "ELF")
	mustWrite("out/staging/system.img", "IMG")
	mustWrite("vault/secret.map", "SECRET")
	for i := 0; i < 600; i++ {
		mustWrite(fmt.Sprintf("app/build/generated/d%02d/meta_%03d.json", i%12, i), "{}")
	}

	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	pol.LoadSettingsJSON([]byte(`{"permissions":{"deny":["read_file(vault/)"]}}`))
	binder := &FSSearchBinder{Policy: pol}

	// 1. Prefix pushdown (`native/**/*.map`) + deny rule (`vault/secret.map` excluded)
	maps, _, _, err := binder.runGlobSearch(context.Background(), ws, ws, "native/**/*.map", 50)
	if err != nil || !slices.Equal(maps, []string{"native/src/version.map"}) {
		t.Fatalf("unexpected native/**/*.map result: %v (err=%v)", maps, err)
	}

	// 2. Multi-branch brace prefix pushdown (`{scripts/staging,scripts/guest,nonexistent/dir}/**/*`)
	scripts, _, _, err := binder.runGlobSearch(context.Background(), ws, ws, "{scripts/staging,scripts/guest,nonexistent/dir}/**/*", 50)
	if err != nil || !slices.Equal(scripts, []string{"scripts/guest/init.sh", "scripts/staging/pack.py"}) {
		t.Fatalf("unexpected brace prefix result: %v (err=%v)", scripts, err)
	}

	// 3. Explicit `build/native/**/*vm02*` MUST find build artifacts even though `build` is in skippedWalkDirs and .ignore!
	bins, _, _, err := binder.runGlobSearch(context.Background(), ws, ws, "build/native/**/*vm02*", 50)
	if err != nil || !slices.Equal(bins, []string{"build/native/modern/libvm02_worker.so", "build/native/modern/vm02_probe"}) {
		t.Fatalf("expected build/native artifacts to be found, got: %v (err=%v)", bins, err)
	}

	// 4. Root wildcard `**/*` skips gitignored `out/` and `build/` (and `vault/`), while explicit `out/staging/**/*` falls back and finds `system.img`!
	rootImgs, _, _, err := binder.runGlobSearch(context.Background(), ws, ws, "**/*.img", 50)
	if err != nil {
		t.Fatalf("root *.img failed: %v", err)
	}
	if len(rootImgs) != 0 && binder.resolveRg() != "" {
		t.Fatalf("expected rg --files to honor .ignore on root **/*.img, got: %v", rootImgs)
	}
	explicitOut, _, _, err := binder.runGlobSearch(context.Background(), ws, ws, "out/staging/**/*", 50)
	if err != nil || !slices.Equal(explicitOut, []string{"out/staging/system.img"}) {
		t.Fatalf("expected explicit prefix out/staging/**/* to fallback and find system.img, got: %v (err=%v)", explicitOut, err)
	}

	// 5. Explicit searchRoot `{ path: "app/build/generated" }` walks 600 files in parallel in well under 2s!
	t0 := time.Now()
	genDir := filepath.Join(ws, "app", "build", "generated")
	jsons, total, _, err := binder.runGlobSearch(context.Background(), ws, genDir, "**/*.json", 100)
	if err != nil || total != 600 || len(jsons) != 100 {
		t.Fatalf("expected 100 of 600 json files, got len=%d total=%d err=%v", len(jsons), total, err)
	}
	if dur := time.Since(t0); dur > 2*time.Second {
		t.Fatalf("parallel glob walk over 600 files took too long: %v", dur)
	}

	// 6. Canceled context returns immediately without hanging
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = walkFilesParallel(cctx, pol, ws, []string{genDir}, false)
}

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
