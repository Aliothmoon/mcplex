package tasks

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/aliothmoon/mcplex/pkg/policy"
)

func TestManagerSyncCompletion(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Close()

	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	wait := 5000
	res, err := mgr.Start(context.Background(), pol, StartRequest{
		Command: "echo hello-mcplex-sync",
		Cwd:     dir,
		WaitMs:  &wait,
		Name:    "greet",
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if res.Backgrounded {
		t.Fatalf("expected fast echo to complete synchronously, got backgrounded: %+v", res)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %s (output=%q)", res.Status, res.Output)
	}
	if !strings.Contains(res.Output, "hello-mcplex-sync") {
		t.Fatalf("expected output to contain hello-mcplex-sync, got %q", res.Output)
	}
}

func TestManagerBackgroundWaitExpectAndKill(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Close()

	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = `Start-Sleep -Milliseconds 300; Write-Output "SERVER_READY http://127.0.0.1:9999"; Start-Sleep -Seconds 30`
	} else {
		cmd = `sleep 0.3; echo "SERVER_READY http://127.0.0.1:9999"; sleep 30`
	}

	wait0 := 50
	startRes, err := mgr.Start(context.Background(), pol, StartRequest{
		Command: cmd,
		Cwd:     dir,
		Daemon:  true,
		WaitMs:  &wait0,
		Name:    "dev-srv",
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if !startRes.Backgrounded || startRes.Status != StatusRunning {
		t.Fatalf("expected background running daemon, got %+v", startRes)
	}

	// Wait with expect regex should wake up quickly once SERVER_READY appears.
	waitRes, err := mgr.Manage(context.Background(), ManageRequest{
		Action: "wait",
		TaskID: startRes.TaskID,
		Expect: `SERVER_READY\s+http://`,
		WaitMs: 8000,
	})
	if err != nil {
		t.Fatalf("wait failed: %v", err)
	}
	if !waitRes.Matched || !strings.Contains(waitRes.MatchLine, "SERVER_READY") {
		t.Fatalf("expected expect-regex match, got %+v", waitRes)
	}

	// List should show the running task.
	listRes, err := mgr.Manage(context.Background(), ManageRequest{Action: "list"})
	if err != nil || len(listRes.Tasks) != 1 || listRes.Tasks[0].Status != StatusRunning {
		t.Fatalf("unexpected list result: %+v err=%v", listRes, err)
	}

	// Kill should terminate the daemon cleanly.
	killRes, err := mgr.Manage(context.Background(), ManageRequest{Action: "kill", TaskID: startRes.TaskID})
	if err != nil {
		t.Fatalf("kill failed: %v", err)
	}
	if killRes.KilledCount != 1 || killRes.Task.Status != StatusKilled {
		t.Fatalf("expected killed status, got %+v", killRes)
	}
}

func TestManagerInteractiveStdinAndOffsetLogs(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Close()

	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = `$line = [Console]::In.ReadLine(); Write-Output "GOT:$line"`
	} else {
		cmd = `read line; echo "GOT:$line"`
	}

	wait0 := 100
	startRes, err := mgr.Start(context.Background(), pol, StartRequest{
		Command: cmd,
		Cwd:     dir,
		WaitMs:  &wait0,
		Name:    "stdin-echo",
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if !startRes.Backgrounded {
		t.Fatalf("expected task waiting on stdin to be backgrounded, got %+v", startRes)
	}

	inRes, err := mgr.Manage(context.Background(), ManageRequest{
		Action:     "input",
		TaskID:     startRes.TaskID,
		Input:      "ping-42",
		CloseStdin: true,
	})
	if err != nil || inRes.BytesWritten == 0 {
		t.Fatalf("input failed: res=%+v err=%v", inRes, err)
	}

	waitRes, err := mgr.Manage(context.Background(), ManageRequest{
		Action: "wait",
		TaskID: startRes.TaskID,
		WaitMs: 5000,
	})
	if err != nil {
		t.Fatalf("wait failed: %v", err)
	}
	if waitRes.Task.Status != StatusCompleted || !strings.Contains(waitRes.Output, "GOT:ping-42") {
		t.Fatalf("expected GOT:ping-42 completed, got %+v", waitRes)
	}

	off := int64(0)
	logRes, err := mgr.Manage(context.Background(), ManageRequest{
		Action: "logs",
		TaskID: startRes.TaskID,
		Offset: &off,
	})
	if err != nil || !strings.Contains(logRes.Output, "GOT:ping-42") || logRes.NextOffset == 0 {
		t.Fatalf("offset logs mismatch: %+v err=%v", logRes, err)
	}
	// Second incremental read at NextOffset should yield empty output.
	next := logRes.NextOffset
	logRes2, err := mgr.Manage(context.Background(), ManageRequest{
		Action: "logs",
		TaskID: startRes.TaskID,
		Offset: &next,
	})
	if err != nil || logRes2.Output != "" {
		t.Fatalf("expected empty incremental log at EOF, got %+v err=%v", logRes2, err)
	}
}

func TestManagerPolicyBlocksDangerousCommand(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Close()

	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	_, err := mgr.Start(context.Background(), pol, StartRequest{
		Command: "rm -rf /",
		Cwd:     dir,
	})
	if err == nil {
		t.Fatal("expected policy to block rm -rf /")
	}
}

func TestManagerSessionIsolationAndCloseSession(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Close()

	pol := policy.NewEngineWithOptions(dir, policy.Options{StrictWorkspace: true})
	var sleepCmd string
	if runtime.GOOS == "windows" {
		sleepCmd = "Start-Sleep -Seconds 30"
	} else {
		sleepCmd = "sleep 30"
	}

	wait0 := 50
	r1, err := mgr.Start(context.Background(), pol, StartRequest{
		Command:   sleepCmd,
		Cwd:       dir,
		Daemon:    true,
		WaitMs:    &wait0,
		SessionID: "sess-A",
	})
	if err != nil {
		t.Fatalf("Start sess-A failed: %v", err)
	}

	// sess-B must not see or kill sess-A's task.
	listB, err := mgr.Manage(context.Background(), ManageRequest{Action: "list", SessionID: "sess-B"})
	if err != nil || len(listB.Tasks) != 0 {
		t.Fatalf("sess-B should not see sess-A tasks, got %+v", listB)
	}
	if _, err := mgr.Manage(context.Background(), ManageRequest{Action: "kill", TaskID: r1.TaskID, SessionID: "sess-B"}); err == nil {
		t.Fatal("sess-B should not be allowed to kill sess-A task")
	}

	// Closing sess-A must terminate its running task.
	if n := mgr.CloseSession("sess-A"); n != 1 {
		t.Fatalf("expected CloseSession(sess-A) to kill 1 task, got %d", n)
	}
}
