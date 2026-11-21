package runtime

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/aliothmoon/mcplex/pkg/output"
)

var (
	footerRe   = regexp.MustCompile(`\[page (\d+)/(\d+) · lines (\d+)-(\d+) of (\d+)`)
	spillKeyRe = regexp.MustCompile(`state\.get\('(__result_\d+)'\)`)
)

func TestLongPrintIsPagedNotCutInTheMiddle(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	var sb strings.Builder
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&sb, "fn line_%04d() { body(); }\n", i)
	}
	writeFiles(t, ws, map[string]string{"src/huge.rs": sb.String()})

	out, isErr := run(t, eng, ws, `print(await fs.read("src/huge.rs"));`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if len(out) > output.DefaultInlineMaxBytes {
		t.Fatalf("response %d bytes exceeds the inline budget", len(out))
	}
	if strings.Contains(out, "[omitted ") {
		t.Fatalf("long print must be paged, not cut in the middle:\n%s", out[len(out)-300:])
	}
	m := footerRe.FindStringSubmatch(out)
	if m == nil || m[1] != "1" || m[3] != "1" || m[5] != "2000" || !strings.Contains(out, "next: print(state.page('__result_1', 2)) · last: state.page('__result_1', -1)") {
		t.Fatalf("missing keyed page-1 footer:\n%s", out[len(out)-300:])
	}
	lastOnPage1 := m[4]
	mustContainAll(t, out, "fn line_0001()", "Largest results: fs.read(src/huge.rs)")

	// Explicit keyed paging (recommended for concurrent safety)
	out2, _ := run(t, eng, ws, `print(state.page('__result_1', 2));`)
	m2 := footerRe.FindStringSubmatch(out2)
	if m2 == nil || m2[1] != "2" {
		t.Fatalf("page 2 footer missing:\n%s", out2)
	}
	var first, prevLast int
	fmt.Sscan(m2[3], &first)
	fmt.Sscan(lastOnPage1, &prevLast)
	if first != prevLast+1 {
		t.Fatalf("page 2 starts at line %d, page 1 ended at %d", first, prevLast)
	}
	mustContainAll(t, out2, fmt.Sprintf("fn line_%04d()", first), "state.page('__result_1', 3)")

	// Legacy keyless state.page(-1) still works and resolves __last_result
	out3, _ := run(t, eng, ws, `print(state.page(-1));`)
	mustContainAll(t, out3, "fn line_2000()", "of 2000]")
	if strings.Contains(out3, "next:") {
		t.Fatalf("last page must not offer a next page:\n%s", out3)
	}

	out4, _ := run(t, eng, ws, `try { state.page(99); } catch (e) { return e.name + ": " + e.message; }`)
	mustContainAll(t, out4, "BadArgs: state.page: page out of range (1..")
}

func TestLongStringResultIsPagedAndObjectsPageAsJSON(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	out, _ := run(t, eng, ws, `return Array.from({ length: 2500 }, (_, i) => "row " + i).join("\n");`)
	mustContainAll(t, out, "Result:\nrow 0\nrow 1\n", "[page 1/", "next: print(state.page('__result_1', 2))")

	// A second spill does not clobber paging of __result_1 when addressed by key.
	_, _ = run(t, eng, ws, `return { items: Array.from({ length: 800 }, (_, i) => ({ id: i, name: "item-" + i })) };`)
	outObj, _ := run(t, eng, ws, `print(state.page(1));`)
	mustContainAll(t, outObj, "{\n  \"items\": [\n    {\n      \"id\": 0,", "[page 1/", "state.page('__result_2', 2)")

	// Still able to page the first result cleanly via key in all supported overloads!
	for _, code := range []string{
		`print(state.page('__result_1', 2));`,
		`print(state.page(2, '__result_1'));`,
		`print(state.page(2, { key: '__result_1' }));`,
		`print(state.page({ key: '__result_1', page: 2 }));`,
	} {
		got, isErr := run(t, eng, ws, code)
		if isErr {
			t.Fatalf("%s failed: %s", code, got)
		}
		mustContainAll(t, got, "row ", "[page 2/", "state.page('__result_1', 3)")
		if strings.Contains(got, `"item-`) {
			t.Fatalf("%s leaked __result_2 data:\n%s", code, got)
		}
	}

	// Missing explicit key gives a targeted NotFound message.
	miss, _ := run(t, eng, ws, `try { state.page('__result_999', 1); } catch (e) { return e.name + ": " + e.message; }`)
	mustContainAll(t, miss, "NotFound: state.page: key '__result_999' not found")
}

func TestConcurrentAutoSpillAndKeyedPagingNeverMixData(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	const workers = 12

	type spillOutcome struct {
		worker int
		key    string
		out    string
		isErr  bool
	}
	results := make([]spillOutcome, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			code := fmt.Sprintf(`
const lines = [];
for (let j = 1; j <= 600; j++) lines.push("W%02d-LINE-" + String(j).padStart(4, "0") + "-PAYLOAD-PADDING");
print(lines.join("\n"));`, id)
			out, isErr := run(t, eng, ws, code)
			m := spillKeyRe.FindStringSubmatch(out)
			key := ""
			if m != nil {
				key = m[1]
			}
			results[id] = spillOutcome{worker: id, key: key, out: out, isErr: isErr}
		}(i)
	}
	wg.Wait()

	seenKeys := map[string]int{}
	for _, r := range results {
		if r.isErr {
			t.Fatalf("worker %d failed: %s", r.worker, r.out)
		}
		if r.key == "" {
			t.Fatalf("worker %d missing spill key in output:\n%s", r.worker, r.out)
		}
		if prev, dup := seenKeys[r.key]; dup {
			t.Fatalf("duplicate spill key %s for workers %d and %d", r.key, prev, r.worker)
		}
		seenKeys[r.key] = r.worker
		wantNext := fmt.Sprintf("next: print(state.page('%s', 2))", r.key)
		if !strings.Contains(r.out, wantNext) {
			t.Fatalf("worker %d footer missing %q:\n%s", r.worker, wantNext, r.out[max(0, len(r.out)-300):])
		}
	}

	// Concurrently page page-2 and last page for every worker using its own spill key and verify zero data mixing.
	var wg2 sync.WaitGroup
	wg2.Add(workers)
	errs := make(chan string, workers)
	for _, r := range results {
		go func(o spillOutcome) {
			defer wg2.Done()
			prefix := fmt.Sprintf("W%02d-LINE-", o.worker)
			p2, isErr := run(t, eng, ws, fmt.Sprintf(`print(state.page('%s', 2));`, o.key))
			if isErr {
				errs <- fmt.Sprintf("worker %d page 2 error: %s", o.worker, p2)
				return
			}
			if !strings.Contains(p2, prefix) {
				errs <- fmt.Sprintf("worker %d page 2 missing own prefix %s:\n%s", o.worker, prefix, p2)
				return
			}
			pLast, isErr := run(t, eng, ws, fmt.Sprintf(`print(state.page('%s', -1));`, o.key))
			if isErr {
				errs <- fmt.Sprintf("worker %d last page error: %s", o.worker, pLast)
				return
			}
			wantTail := fmt.Sprintf("W%02d-LINE-0600-PAYLOAD-PADDING", o.worker)
			if !strings.Contains(pLast, wantTail) {
				errs <- fmt.Sprintf("worker %d last page missing %s:\n%s", o.worker, wantTail, pLast)
				return
			}
			for other := 0; other < workers; other++ {
				if other == o.worker {
					continue
				}
				bad := fmt.Sprintf("W%02d-LINE-", other)
				if strings.Contains(p2, bad) || strings.Contains(pLast, bad) {
					errs <- fmt.Sprintf("worker %d contaminated with worker %d data!", o.worker, other)
					return
				}
			}
		}(r)
	}
	wg2.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

func TestCapWarningsReportTotals(t *testing.T) {
	eng, ws := newErrWorkspace(t)
	files := map[string]string{}
	for i := 0; i < 30; i++ {
		files[fmt.Sprintf("data/f%02d.txt", i)] = "x\n"
	}
	writeFiles(t, ws, files)
	out, isErr := run(t, eng, ws, `
const g = await search.glob("**/*.txt", { path: "data", maxResults: 10 });
const l = await fs.list("data", { limit: 5 });
return [g.length, l.length];`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	mustContainAll(t, out, "search.glob: capped at 10 of 30 files", "fs.list: capped at 5 of 30 entries")
}
