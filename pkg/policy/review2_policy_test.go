package policy

import (
	"runtime"
	"testing"
)

func TestUnixProtectedSystemRootRejectsRootAndEtcWorkspace(t *testing.T) {
	eRoot := &Engine{workspaceRoots: []string{"/"}}
	if !eRoot.isProtectedUnixSystemRoot("/etc/passwd") {
		t.Fatal("cwd='/' must NOT exempt /etc/passwd")
	}
	eEtc := &Engine{workspaceRoots: []string{"/etc"}}
	if !eEtc.isProtectedUnixSystemRoot("/etc/passwd") {
		t.Fatal("cwd='/etc' must NOT exempt /etc/passwd")
	}
	eUsrBin := &Engine{workspaceRoots: []string{"/usr/bin"}}
	if !eUsrBin.isProtectedUnixSystemRoot("/usr/bin/sh") {
		t.Fatal("cwd='/usr/bin' must NOT exempt /usr/bin/sh")
	}
	eDocker := &Engine{workspaceRoots: []string{"/usr/src/app"}}
	if eDocker.isProtectedUnixSystemRoot("/usr/src/app/main.go") {
		t.Fatal("cwd='/usr/src/app' SHOULD allow /usr/src/app/main.go")
	}
	if !eDocker.isProtectedUnixSystemRoot("/usr/bin/sh") {
		t.Fatal("cwd='/usr/src/app' must still protect /usr/bin/sh")
	}
	if runtime.GOOS != "windows" {
		if _, err := NewEngine("/").ResolveAndCheckWrite("/", "/etc/passwd"); err == nil {
			t.Fatal("cwd='/' must not disable /etc write protection")
		}
		if _, err := NewEngine("/etc").ResolveAndCheckWrite("/etc", "/etc/hosts"); err == nil {
			t.Fatal("cwd='/etc' must not disable /etc write protection")
		}
	}
}

func TestWindowsExtendedLengthPathAllowedWhileADSAndDevicesBlocked(t *testing.T) {
	if clean, blocked := normalizeWindowsExtendedPath(`\\?\C:\Project\foo.go`); blocked || clean != `C:\Project\foo.go` {
		t.Fatalf("expected \\\\?\\C:\\Project\\foo.go to strip cleanly, got clean=%q blocked=%v", clean, blocked)
	}
	if clean, blocked := normalizeWindowsExtendedPath(`\\?\UNC\server\share\file.txt`); blocked || clean != `\\server\share\file.txt` {
		t.Fatalf("expected \\\\?\\UNC path to convert cleanly, got clean=%q blocked=%v", clean, blocked)
	}
	if _, blocked := normalizeWindowsExtendedPath(`\\.\PhysicalDrive0`); !blocked {
		t.Fatal("expected \\\\.\\PhysicalDrive0 to be blocked")
	}
	if _, blocked := normalizeWindowsExtendedPath(`\\?\GLOBALROOT\Device\Harddisk0`); !blocked {
		t.Fatal("expected \\\\?\\GLOBALROOT to be blocked")
	}

	if runtime.GOOS == "windows" {
		ws := t.TempDir()
		e := NewEngine(ws)
		longPath := `\\?\` + ws + `\ok.txt`
		if _, err := e.ResolveAndCheckWrite(ws, longPath); err != nil {
			t.Fatalf("expected valid \\\\?\\ extended path inside workspace to be allowed, got: %v", err)
		}
		if _, err := e.ResolveAndCheckWrite(ws, `\\?\C:\Windows\win.ini`); err == nil {
			t.Fatal("expected \\\\?\\C:\\Windows\\win.ini to still be blocked by system-root policy")
		}
		adsLongPath := `\\?\` + ws + `\.git\config::$DATA`
		if _, err := e.ResolveAndCheckWrite(ws, adsLongPath); err == nil {
			t.Fatal("expected \\\\?\\ extended path with ::$DATA ADS to still be denied")
		}
	}
}
