package platform

import (
	"os"
	"syscall"
	"unsafe"
)

var procGetConsoleProcessList = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// OwnConsole reports whether the process got a console window of its own,
// i.e. it was started by double-click rather than from a command prompt. In
// that case the window closes on exit, so errors should be confirmed first.
func OwnConsole() bool {
	if procGetConsoleProcessList.Find() != nil {
		return false
	}
	var ids [8]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	return n == 1
}

// ProcessAlive reports whether a process with the given pid exists.
func ProcessAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
