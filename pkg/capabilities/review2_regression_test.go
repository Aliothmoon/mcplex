package capabilities

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func TestSameExecTxAndFsWriteOrEditReplaceNoSelfDeadlock(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "x.txt")
	if err := os.WriteFile(target, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pol := policy.NewEngine(ws)
	locks := NewFileLockTable()
	code := `
		const t = await tx.begin();
		await t.edit("x.txt", [{ target: "hello", replacement: "hi" }]);
		await fs.write("x.txt", "hi from fs.write\n");
		await edit.replace("x.txt", "fs.write", "edit.replace");
		await fs.cp("x.txt", "x_copy.txt");
		await fs.rename("x_copy.txt", "x_moved.txt");
		await fs.rm("x_moved.txt");
		await t.commit();
		return await fs.read("x.txt");
	`
	res, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code:    code,
		Cwd:     ws,
		Timeout: 3 * time.Second,
	}, &FSSearchBinder{Policy: pol, Locks: locks}, &EditTxBinder{Policy: pol, Locks: locks})
	if err != nil {
		t.Fatalf("same-exec tx + fs.write/edit.replace/fs.ops deadlocked or failed: %v", err)
	}
	if got, _ := res.Value.(string); !strings.Contains(got, "hi from edit.replace") {
		t.Fatalf("unexpected final content: %q", got)
	}
}

func TestCrossExecDeadlockDetectedImmediately(t *testing.T) {
	locks := NewFileLockTable()
	relA, err := locks.Acquire(context.Background(), "exec:1", "/ws/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer relA()
	relB, err := locks.Acquire(context.Background(), "exec:2", "/ws/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer relB()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		rel, e := locks.Acquire(ctx, "exec:1", "/ws/b.txt")
		if rel != nil {
			rel()
		}
		errCh <- e
	}()
	time.Sleep(30 * time.Millisecond)
	start := time.Now()
	go func() {
		defer wg.Done()
		rel, e := locks.Acquire(ctx, "exec:2", "/ws/a.txt")
		if rel != nil {
			rel()
		}
		errCh <- e
	}()

	firstErr := <-errCh
	elapsed := time.Since(start)
	if firstErr == nil || !strings.Contains(firstErr.Error(), "deadlock detected") {
		t.Fatalf("expected immediate deadlock error, got %v", firstErr)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("deadlock detection took too long: %v", elapsed)
	}
	cancel()
	wg.Wait()
}

func TestFileLockTimeoutReportsHolder(t *testing.T) {
	locks := NewFileLockTable()
	rel, err := locks.Acquire(context.Background(), "exec:holder", "/ws/shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = locks.AcquireIn(ctx, "/ws", "exec:waiter", "/ws/SharedFile.txt")
	if err == nil || !strings.Contains(err.Error(), "exec:holder") || !strings.Contains(err.Error(), "shared.txt") {
		// Re-test with exact matching path preserving case in display output.
		_, err = locks.AcquireIn(ctx, "/ws", "exec:waiter", "/ws/shared.txt")
	}
	if err == nil || !strings.Contains(err.Error(), "exec:holder") || !strings.Contains(err.Error(), "shared.txt") {
		t.Fatalf("expected timeout error naming path and holder, got: %v", err)
	}
}

func TestFileLockPreservesRelativeCaseInErrorsAndTracksMultiEdges(t *testing.T) {
	locks := NewFileLockTable()
	ws := t.TempDir()
	mixed := filepath.Join(ws, "MixedCase.go")
	rel, err := locks.AcquireIn(context.Background(), ws, "exec:holder", mixed)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = locks.AcquireIn(ctx, ws, "exec:waiter", mixed)
	if err == nil {
		t.Fatal("expected lock timeout error")
	}
	if !strings.Contains(err.Error(), "MixedCase.go") {
		t.Fatalf("expected original-case relative path MixedCase.go in error, got: %v", err)
	}
}

func TestStdioReadLoopScannerErrorMarksClosedAndWakesPendingImmediately(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()

	c := &stdioClient{
		pending: map[int64]chan []byte{},
	}
	waiter := make(chan []byte, 1)
	c.pending[1] = waiter

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.readLoop(pr)
	}()

	// Write a >8MB single line (exceeds bufio.Scanner max token size) WITHOUT closing pw,
	// simulating an alive subprocess returning an oversized payload.
	big := make([]byte, 8*1024*1024+128)
	for i := range big {
		big[i] = 'a'
	}
	go func() {
		_, _ = pw.Write(big)
	}()

	select {
	case _, ok := <-waiter:
		if ok {
			t.Fatal("expected pending channel to be closed on scanner error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending RPC was not woken when scanner exited on oversized line")
	}
	if !c.closed.Load() {
		t.Fatal("expected stdioClient to be marked closed immediately after scanner exit")
	}
	<-done
}

func TestLimitedSyncBufferChineseUTF8HeadPreserved(t *testing.T) {
	b := newLimitedSyncBuffer(64 * 1024) // 32 KB head + 32 KB tail
	chunk := []byte(strings.Repeat("中", 1000))
	for i := 0; i < 300; i++ {
		_, _ = b.Write(chunk)
	}
	out := b.String()
	markerIdx := strings.Index(out, "bytes omitted from middle")
	if markerIdx < 0 {
		t.Fatalf("expected inline omission marker in output")
	}
	headPart := out[:markerIdx]
	tailPart := out[markerIdx:]
	if len(headPart) < 30*1024 {
		t.Fatalf("Chinese head was wiped during UTF-8 trim: got only %d bytes, expected ~32KB", len(headPart))
	}
	if len(tailPart) < 30*1024 {
		t.Fatalf("Chinese tail too short: got %d bytes, expected ~32KB", len(tailPart))
	}
	if !strings.HasPrefix(headPart, "中中中中") {
		t.Fatalf("unexpected head prefix: %q", headPart[:min(24, len(headPart))])
	}
}

func TestReadFileDirectAllowsGBKTextFile(t *testing.T) {
	ws := t.TempDir()
	gbkBytes := []byte{0xD6, 0xD0, 0xCE, 0xC4, 0xB2, 0xE2, 0xCA, 0xD4, ' ', 'h', 'e', 'l', 'l', 'o', '\n'}
	p := filepath.Join(ws, "gbk.txt")
	if err := os.WriteFile(p, gbkBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.NewEngine(ws)
	res, err := ReadFileDirect(pol, ws, DirectReadRequest{Path: "gbk.txt"})
	if err != nil {
		t.Fatalf("expected GBK text file to be readable (not falsely flagged binary), got: %v", err)
	}
	if !strings.Contains(res.Text, "hello") {
		t.Fatalf("expected readable ASCII content in GBK file, got: %q", res.Text)
	}
}

func TestMCPBridgeHTTPRetriesFailedInitAndReinitializesOn404(t *testing.T) {
	var initCount atomic.Int32
	var expiredOnce atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			n := initCount.Add(1)
			if n == 1 {
				http.Error(w, `{"error":"booting"}`, http.StatusServiceUnavailable)
				return
			}
			sid := "sess-" + string(rune('0'+n))
			w.Header().Set("Mcp-Session-Id", sid)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"protocolVersion": "2025-11-25"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			sid := r.Header.Get("Mcp-Session-Id")
			if sid == "sess-2" && !expiredOnce.Swap(true) {
				http.Error(w, `{"error":"session expired"}`, http.StatusNotFound)
				return
			}
			if sid != "sess-3" {
				http.Error(w, `{"error":"missing fresh session"}`, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": `{"ok":true,"sid":"` + sid + `"}`}},
				},
			})
		}
	}))
	defer srv.Close()

	ws := t.TempDir()
	br := NewMCPBridgeWithOptions(policy.NewEngine(ws), ws, MCPBridgeOptions{})
	if _, err := br.callStreamableHTTP(context.Background(), "demo", srv.URL, nil, "ping", map[string]any{}); err == nil {
		t.Fatal("expected first call to fail when initial handshake returned 503")
	}
	res, err := br.callStreamableHTTP(context.Background(), "demo", srv.URL, nil, "ping", map[string]any{})
	if err != nil {
		t.Fatalf("expected bridge to retry init and recover from 404 expired session, got: %v", err)
	}
	m, _ := res.(map[string]any)
	data, _ := m["data"].(map[string]any)
	if data["sid"] != "sess-3" {
		t.Fatalf("expected recovered session sess-3, got %+v", res)
	}
	if initCount.Load() != 3 {
		t.Fatalf("expected 3 initialize attempts (503, sess-2, sess-3), got %d", initCount.Load())
	}
}
