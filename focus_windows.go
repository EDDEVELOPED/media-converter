//go:build windows

package main

// Bringing windows to the front on Windows.
//
// Windows only lets the program you last clicked put a window in front.
// The app's window, the file pickers and Explorer are opened by other
// processes in the background, so Windows leaves them behind whatever
// was already open. These helpers find those windows once they appear and
// raise them.

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procIsIconic                 = user32.NewProc("IsIconic")
	procShowWindow               = user32.NewProc("ShowWindow")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procBringWindowToTop         = user32.NewProc("BringWindowToTop")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procAttachThreadInput        = user32.NewProc("AttachThreadInput")
	procAllowSetForeground       = user32.NewProc("AllowSetForegroundWindow")
	procKeybdEvent               = user32.NewProc("keybd_event")
	procGetCurrentThreadId       = kernel32.NewProc("GetCurrentThreadId")
)

const (
	swRestore      = 9
	swpNoSize      = 0x0001
	swpNoMove      = 0x0002
	swpShowWindow  = 0x0040
	vkMenu         = 0x12
	keyExtended    = 0x0001
	keyUp          = 0x0002
	asfwAny        = 0xFFFFFFFF
	hwndTopmost    = ^uintptr(0) // (HWND)-1
	hwndNoTopmost  = ^uintptr(1) // (HWND)-2
	appWindowTitle = appName     // the page title, which the browser uses for the window
)

type winInfo struct {
	hwnd  uintptr
	pid   uint32
	title string
	class string
}

var (
	enumMu   sync.Mutex
	enumList []winInfo
	// One callback for the program's lifetime: Windows callbacks made with
	// NewCallback are never freed.
	enumCB = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		if v, _, _ := procIsWindowVisible.Call(hwnd); v == 0 {
			return 1
		}
		var pid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		t := make([]uint16, 512)
		n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&t[0])), uintptr(len(t)))
		c := make([]uint16, 256)
		m, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&c[0])), uintptr(len(c)))
		enumList = append(enumList, winInfo{hwnd, pid, syscall.UTF16ToString(t[:n]), syscall.UTF16ToString(c[:m])})
		return 1
	})
)

// topWindows lists visible top-level windows, front to back.
func topWindows() []winInfo {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumList = nil
	procEnumWindows.Call(enumCB, 0)
	out := enumList
	enumList = nil
	return out
}

// allowForeground lets the process we're about to start (a browser, a file
// picker, Explorer) take the foreground, while we still have the right to.
func allowForeground() { procAllowSetForeground.Call(asfwAny) }

// forceForeground raises hwnd above other windows and gives it focus.
func forceForeground(hwnd uintptr) {
	runtime.LockOSThread() // AttachThreadInput works per OS thread
	defer runtime.UnlockOSThread()
	if ic, _, _ := procIsIconic.Call(hwnd); ic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	}
	fg, _, _ := procGetForegroundWindow.Call()
	if fg != hwnd {
		// Borrow the focus rights of whichever window has them now.
		cur, _, _ := procGetCurrentThreadId.Call()
		fgThread, _, _ := procGetWindowThreadProcessId.Call(fg, 0)
		attached := false
		if fg != 0 && fgThread != 0 && fgThread != cur {
			r, _, _ := procAttachThreadInput.Call(cur, fgThread, 1)
			attached = r != 0
		}
		procSetForegroundWindow.Call(hwnd)
		procBringWindowToTop.Call(hwnd)
		if attached {
			procAttachThreadInput.Call(cur, fgThread, 0)
		}
		if now, _, _ := procGetForegroundWindow.Call(); now != hwnd {
			// Last resort: a tap of the Alt key lifts Windows' focus lock.
			procKeybdEvent.Call(vkMenu, 0, keyExtended, 0)
			procKeybdEvent.Call(vkMenu, 0, keyExtended|keyUp, 0)
			procSetForegroundWindow.Call(hwnd)
		}
	}
	// Put it on top of the stack even if Windows refused to move focus.
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpShowWindow)
	procSetWindowPos.Call(hwnd, hwndNoTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpShowWindow)
}

// waitAndFocus polls for a window matching match and brings it forward.
func waitAndFocus(match func(w winInfo) bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		for _, w := range topWindows() {
			if match(w) {
				forceForeground(w.hwnd)
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func isAppWindow(w winInfo) bool {
	if !strings.HasPrefix(w.title, appWindowTitle) {
		return false
	}
	// Chrome and Edge app windows, or a Firefox tab as a fallback.
	return strings.HasPrefix(w.class, "Chrome_WidgetWin") || w.class == "MozillaWindowClass"
}

// focusAppWindow brings an open app window to the front.
func focusAppWindow() bool { return waitAndFocus(isAppWindow, 0) }

func appWindows() map[uintptr]bool {
	m := map[uintptr]bool{}
	for _, w := range topWindows() {
		if isAppWindow(w) {
			m[w.hwnd] = true
		}
	}
	return m
}

// focusAppWindowSoon waits for the window that's opening to load, then raises
// it. A window left over from an earlier session is only used if no new one shows up.
func focusAppWindowSoon(before map[uintptr]bool) {
	go func() {
		isNew := func(w winInfo) bool { return isAppWindow(w) && !before[w.hwnd] }
		if !waitAndFocus(isNew, 12*time.Second) {
			waitAndFocus(isAppWindow, 0)
		}
	}()
}

// focusDialogOf raises the file picker that process pid opens.
func focusDialogOf(pid int, stop <-chan struct{}) {
	isDialog := func(w winInfo) bool { return int(w.pid) == pid && w.class == "#32770" }
	for {
		select {
		case <-stop:
			return
		default:
		}
		if waitAndFocus(isDialog, 0) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// focusExplorerFor raises the Explorer window that opens on dir: a new one if
// Explorer opened one, otherwise an existing window showing that folder.
func focusExplorerFor(dir string, before map[uintptr]bool) {
	name := filepath.Base(dir)
	isNew := func(w winInfo) bool { return w.class == "CabinetWClass" && !before[w.hwnd] }
	if !waitAndFocus(isNew, 3*time.Second) {
		waitAndFocus(func(w winInfo) bool { return w.class == "CabinetWClass" && w.title == name }, 2*time.Second)
	}
}

func explorerWindows() map[uintptr]bool {
	m := map[uintptr]bool{}
	for _, w := range topWindows() {
		if w.class == "CabinetWClass" {
			m[w.hwnd] = true
		}
	}
	return m
}
