//go:build windows

package capabilities

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
)

func TestWindowsJobPreservesAdbDaemonAndCleansOrphans(t *testing.T) {
	ws := t.TempDir()
	pol := policy.NewEngineWithOptions(ws, policy.Options{StrictWorkspace: true})
	binder := &ShellGitBinder{Policy: pol}

	// 1. Verify `{ env: { ...process.env, ... } }` never clobbers redacted host keys (like ADB_VENDOR_KEYS),
	//    and custom `PATH` in `sh.run` resolves executables outside the initial process PATH!
	t.Setenv("ADB_VENDOR_KEYS", "C:\\real\\adb_keys")
	customBinDir := filepath.Join(ws, "custom-bin")
	if err := os.MkdirAll(customBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	batPath := filepath.Join(customBinDir, "my_custom_tool.bat")
	if err := os.WriteFile(batPath, []byte("@echo off\r\necho KEYS=%ADB_VENDOR_KEYS%\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	envRes, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			const redactedInSandbox = process.env.ADB_VENDOR_KEYS;
			const r = await sh.run("my_custom_tool.bat", [], {
				env: {
					...process.env,
					PATH: P.customBin + ";" + (process.env.PATH || ""),
				},
			});
			return { redactedInSandbox, stdout: r.stdout.trim() };
		`,
		Payloads: map[string]string{"customBin": customBinDir},
		Cwd:      ws,
		Timeout:  10 * time.Second,
	}, binder)
	if err != nil {
		t.Fatalf("custom PATH + ...process.env test failed: %v", err)
	}
	em, _ := envRes.Value.(map[string]any)
	if em["redactedInSandbox"] != "[redacted]" || em["stdout"] != `KEYS=C:\real\adb_keys` {
		t.Fatalf("expected sandbox [redacted] but real host ADB_VENDOR_KEYS in child, got: %+v", em)
	}

	// 2. Verify normal non-daemon background orphans ARE still terminated when sh.exec exits!
	orphanRes, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			const r = await sh.exec("$p = Start-Process ping.exe -ArgumentList '-n','60','127.0.0.1' -PassThru; Write-Output $p.Id", {
				shell: "pwsh",
			});
			return r.output.trim();
		`,
		Cwd:     ws,
		Timeout: 10 * time.Second,
	}, binder)
	if err != nil {
		t.Fatalf("orphan spawn test failed: %v", err)
	}
	var orphanPID int
	_, _ = fmt.Sscanf(fmt.Sprint(orphanRes.Value), "%d", &orphanPID)
	if orphanPID > 0 {
		time.Sleep(150 * time.Millisecond)
		checkCmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", orphanPID), "/NH")
		out, _ := checkCmd.CombinedOutput()
		if strings.Contains(strings.ToLower(string(out)), "ping.exe") {
			_ = exec.Command("taskkill", "/F", "/PID", fmt.Sprint(orphanPID)).Run()
			t.Fatalf("expected non-daemon orphan ping.exe (PID %d) to be killed on Job close, got:\n%s", orphanPID, string(out))
		}
	}

	// 3. If `adb.exe` is installed on this Windows host, start an isolated ADB server on a random free local port
	//    via `sh.run("adb", ...)` inside PTC and prove the `adb.exe` daemon SURVIVES after PTC exits!
	adbPath, lookErr := exec.LookPath("adb")
	if lookErr != nil {
		t.Log("adb.exe not on PATH; skipping live isolated adb daemon survival check")
		return
	}
	ln, lErr := net.Listen("tcp", "127.0.0.1:0")
	if lErr != nil {
		t.Fatal(lErr)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	portStr := fmt.Sprint(port)

	defer func() {
		killCmd := exec.Command(adbPath, "-P", portStr, "kill-server")
		_ = killCmd.Run()
	}()

	adbRun1, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			const r = await sh.run("adb", ["-P", P.port, "start-server"]);
			return { ok: r.ok, out: r.stdout, err: r.stderr };
		`,
		Payloads: map[string]string{"port": portStr},
		Cwd:      ws,
		Timeout:  15 * time.Second,
	}, binder)
	if err != nil {
		t.Fatalf("isolated adb start-server inside PTC failed: %v", err)
	}
	_ = adbRun1

	// Immediately after `sandbox.Execute` returns (and its Job Object has closed!), `127.0.0.1:<port>` MUST still be listening!
	conn, dialErr := net.DialTimeout("tcp", "127.0.0.1:"+portStr, 2*time.Second)
	if dialErr != nil {
		t.Fatalf("adb.exe daemon on isolated port %s was killed when PTC sh.run exited: %v", portStr, dialErr)
	}
	_ = conn.Close()

	// A second PTC turn must connect to the warm daemon WITHOUT restarting it!
	adbRun2, err := sandbox.Execute(context.Background(), sandbox.ExecRequest{
		Code: `
			const r = await sh.run("adb", ["-P", P.port, "devices"]);
			return { ok: r.ok, out: r.stdout, err: r.stderr };
		`,
		Payloads: map[string]string{"port": portStr},
		Cwd:      ws,
		Timeout:  10 * time.Second,
	}, binder)
	if err != nil {
		t.Fatalf("second adb devices inside PTC failed: %v", err)
	}
	m2, _ := adbRun2.Value.(map[string]any)
	if strings.Contains(fmt.Sprint(m2["err"]), "daemon not running") || strings.Contains(fmt.Sprint(m2["out"]), "daemon not running") {
		t.Fatalf("expected warm adb daemon to stay alive across PTC turns, but it restarted: %+v", m2)
	}
}
