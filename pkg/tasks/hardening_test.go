package tasks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/internal/shellcmd"
	"github.com/aliothmoon/mcplex/pkg/policy"
)

func TestTasksWindowsCmdRawQuotesAndBashExcludesWSL(t *testing.T) {
	if runtime.GOOS == "windows" {
		dir := t.TempDir()
		mgr := NewManager(dir)
		defer mgr.Close()

		pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
		wait := 5000
		res, err := mgr.Start(context.Background(), pol, StartRequest{
			Command: `echo "a b"`,
			Cwd:     dir,
			Shell:   "cmd",
			WaitMs:  &wait,
		})
		if err != nil {
			t.Fatalf("Start(cmd) failed: %v", err)
		}
		if res.Status != StatusCompleted {
			t.Fatalf("expected StatusCompleted, got %s (out=%q)", res.Status, res.Output)
		}
		if strings.TrimSpace(res.Output) != `"a b"` {
			t.Fatalf(`expected raw quotes "a b" from tasks cmd shell, got %q`, strings.TrimSpace(res.Output))
		}

		// Verify configureTaskCmd preserves pre-existing SysProcAttr.CmdLine.
		cmd, err := shellcmd.BuildCommand(context.Background(), "cmd", `echo "x y"`)
		if err != nil {
			t.Fatal(err)
		}
		configureTaskCmd(cmd)
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != `"x y"` {
			t.Fatalf(`expected configureTaskCmd to preserve CmdLine ("x y"), got %q (err=%v)`, string(out), err)
		}

		for _, shName := range []string{"bash", "sh"} {
			if exe, _, rErr := ResolveShell(shName); rErr == nil {
				if shellcmd.IsWSLSystemBash(exe) {
					t.Fatalf("ResolveShell(%q) returned WSL launcher %q", shName, exe)
				}
			}
		}
	}
}

func TestTasksErrWaitDelayWithZeroExitIsCompleted(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Close()

	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	var cmd string
	if runtime.GOOS == "windows" {
		// Spawn a short background child inheriting stdout while parent shell exits 0 immediately;
		// child outlives WaitDelay (500ms) so cmd.Wait() returns exec.ErrWaitDelay with ExitCode()==0.
		cmd = `Start-Process -NoNewWindow powershell -ArgumentList '-NoProfile','-Command','Start-Sleep -Milliseconds 1200'; Write-Output 'MAIN_OK'; exit 0`
	} else {
		cmd = `(sleep 1.2) & echo MAIN_OK; exit 0`
	}

	wait := 4000
	res, err := mgr.Start(context.Background(), pol, StartRequest{
		Command: cmd,
		Cwd:     dir,
		WaitMs:  &wait,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if res.Status != StatusCompleted || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("expected StatusCompleted with exitCode=0 on ErrWaitDelay, got status=%s exit=%v out=%q", res.Status, res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "MAIN_OK") {
		t.Fatalf("expected MAIN_OK in output, got %q", res.Output)
	}
}

func TestProcHandleConcurrentKillAndCloseIdempotent(t *testing.T) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd.exe", "/c", "ping -n 10 127.0.0.1 >nul")
	} else {
		cmd = exec.Command("sh", "-c", "sleep 5")
	}
	configureTaskCmd(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start child failed: %v", err)
	}
	h := attachProcessHandle(cmd)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			h.Kill()
		}()
		go func() {
			defer wg.Done()
			h.Close()
		}()
	}
	killTaskProcessTree(cmd, h)
	_ = cmd.Wait()
	wg.Wait()
	// Second Close must be a safe no-op.
	h.Close()
}

func TestManagerAtomicMaxConcurrentAndLogCleanup(t *testing.T) {
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "tasks")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed an >24h orphan log and a fresh log before NewManager.
	staleLog := filepath.Join(logDir, "orphan-old.log")
	freshLog := filepath.Join(logDir, "recent.log")
	_ = os.WriteFile(staleLog, []byte("old"), 0o600)
	_ = os.WriteFile(freshLog, []byte("new"), 0o600)
	oldTime := time.Now().Add(-26 * time.Hour)
	_ = os.Chtimes(staleLog, oldTime, oldTime)

	mgr := NewManager(dataDir)
	mgr.maxConcurrent = 3
	mgr.maxRetained = 2

	if _, err := os.Stat(staleLog); !os.IsNotExist(err) {
		mgr.Close()
		t.Fatalf("expected >24h orphan log to be deleted on NewManager, stat err=%v", err)
	}
	if _, err := os.Stat(freshLog); err != nil {
		mgr.Close()
		t.Fatalf("expected fresh (<24h) log to remain untouched, got err=%v", err)
	}
	_ = os.Remove(freshLog)

	ws := t.TempDir()
	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})

	// 1. Concurrent Start burst must never exceed maxConcurrent=3.
	var sleepCmd string
	if runtime.GOOS == "windows" {
		sleepCmd = "Start-Sleep -Seconds 15"
	} else {
		sleepCmd = "sleep 15"
	}
	wait0 := 20
	var succeeded atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := mgr.Start(context.Background(), pol, StartRequest{
				Command: sleepCmd,
				Cwd:     ws,
				Daemon:  true,
				WaitMs:  &wait0,
			})
			if err == nil {
				succeeded.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := succeeded.Load(); got != 3 {
		mgr.Close()
		t.Fatalf("expected atomic maxConcurrent=3 to admit exactly 3 concurrent tasks, got %d", got)
	}
	_, _ = mgr.Manage(context.Background(), ManageRequest{Action: "kill_all"})

	// 2. Eviction beyond maxRetained=2 removes old task .log files.
	waitSync := 3000
	var logPaths []string
	for i := 0; i < 4; i++ {
		res, err := mgr.Start(context.Background(), pol, StartRequest{
			Command: "echo done",
			Cwd:     ws,
			WaitMs:  &waitSync,
		})
		if err != nil {
			mgr.Close()
			t.Fatalf("sync task %d failed: %v", i, err)
		}
		logPaths = append(logPaths, res.LogFile)
	}
	// Earlier evicted tasks must have their log files removed.
	evictedRemoved := 0
	for _, lp := range logPaths {
		if lp != "" {
			if _, err := os.Stat(lp); os.IsNotExist(err) {
				evictedRemoved++
			}
		}
	}
	if evictedRemoved == 0 {
		mgr.Close()
		t.Fatal("expected evicted tasks beyond maxRetained to delete their .log files")
	}

	// 3. Close() removes all remaining task .log files.
	mgr.Close()
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected all task .log files removed after mgr.Close(), still have %d entries", len(entries))
	}
}
