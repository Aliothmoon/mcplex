package tasks

import (
	"context"
	"runtime"
	"testing"

	"github.com/aliothmoon/mcplex/pkg/policy"
)

func TestEmptySessionCannotAccessOrKillNamedSessionTasks(t *testing.T) {
	ws := t.TempDir()
	pol := policy.NewEngine(ws)
	mgr := NewManager("")
	defer mgr.Close()

	sleepCmd := "sleep 10"
	if runtime.GOOS == "windows" {
		sleepCmd = "Start-Sleep -Seconds 10"
	}

	w := 100
	started, err := mgr.Start(context.Background(), pol, StartRequest{
		Command:   sleepCmd,
		Cwd:       ws,
		Daemon:    true,
		WaitMs:    &w,
		SessionID: "sess-A",
	})
	if err != nil {
		t.Fatalf("start daemon in sess-A: %v", err)
	}

	listed, err := mgr.Manage(context.Background(), ManageRequest{Action: "list", SessionID: ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tasks) != 0 {
		t.Fatalf("empty session leaked %d task(s) from sess-A", len(listed.Tasks))
	}

	if _, err := mgr.Manage(context.Background(), ManageRequest{Action: "status", TaskID: started.TaskID, SessionID: ""}); err == nil {
		t.Fatal("empty session was able to inspect sess-A's task")
	}
	killRes, err := mgr.Manage(context.Background(), ManageRequest{Action: "kill_all", SessionID: ""})
	if err != nil {
		t.Fatal(err)
	}
	if killRes.KilledCount != 0 {
		t.Fatalf("empty session killed %d task(s) from sess-A", killRes.KilledCount)
	}

	ownList, err := mgr.Manage(context.Background(), ManageRequest{Action: "list", SessionID: "sess-A"})
	if err != nil || len(ownList.Tasks) != 1 || ownList.Tasks[0].Status != StatusRunning {
		t.Fatalf("expected sess-A task to remain running, got err=%v tasks=%+v", err, ownList)
	}
}
