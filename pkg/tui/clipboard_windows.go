//go:build windows

package tui

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	modUser32          = syscall.NewLazyDLL("user32.dll")
	procOpenClipboard  = modUser32.NewProc("OpenClipboard")
	procCloseClipboard = modUser32.NewProc("CloseClipboard")
	procEmptyClipboard = modUser32.NewProc("EmptyClipboard")
	procSetClipData    = modUser32.NewProc("SetClipboardData")

	modKernel32       = syscall.NewLazyDLL("kernel32.dll")
	procGlobalAlloc   = modKernel32.NewProc("GlobalAlloc")
	procGlobalFree    = modKernel32.NewProc("GlobalFree")
	procGlobalLock    = modKernel32.NewProc("GlobalLock")
	procGlobalUnlock  = modKernel32.NewProc("GlobalUnlock")
	procRtlMoveMemory = modKernel32.NewProc("RtlMoveMemory")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

// writeSystemClipboard writes UTF-8 text directly to the Windows clipboard via Win32 CF_UNICODETEXT.
// Zero external dependencies, zero subprocesses, full Unicode/CJK/emoji fidelity.
func writeSystemClipboard(text string) error {
	u16 := utf16.Encode([]rune(text))
	u16 = append(u16, 0)
	byteLen := uintptr(len(u16) * 2)

	var opened bool
	for attempt := 0; attempt < 5; attempt++ {
		r, _, _ := procOpenClipboard.Call(0)
		if r != 0 {
			opened = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !opened {
		return fmt.Errorf("clipboard busy")
	}
	defer procCloseClipboard.Call()

	if r, _, err := procEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("EmptyClipboard: %w", err)
	}

	hMem, _, err := procGlobalAlloc.Call(gmemMoveable, byteLen)
	if hMem == 0 {
		return fmt.Errorf("GlobalAlloc: %w", err)
	}
	ptr, _, err := procGlobalLock.Call(hMem)
	if ptr == 0 {
		procGlobalFree.Call(hMem)
		return fmt.Errorf("GlobalLock: %w", err)
	}
	procRtlMoveMemory.Call(ptr, uintptr(unsafe.Pointer(&u16[0])), byteLen)
	runtime.KeepAlive(u16)
	procGlobalUnlock.Call(hMem)

	if r, _, err := procSetClipData.Call(cfUnicodeText, hMem); r == 0 {
		procGlobalFree.Call(hMem)
		return fmt.Errorf("SetClipboardData: %w", err)
	}
	// Ownership of hMem transfers to Windows on SetClipboardData success; do not GlobalFree.
	return nil
}
