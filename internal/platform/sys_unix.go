//go:build !windows

package platform

import (
	"errors"
	"os"
	"syscall"
)

// OwnConsole reports whether the process got a console window of its own
// (Windows Explorer double-click). Terminals on other systems stay open.
func OwnConsole() bool { return false }

// ProcessAlive reports whether a process with the given pid exists.
func ProcessAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// EnableANSI reports whether ANSI escape sequences can be used on f. Unix
// terminals support them.
func EnableANSI(*os.File) bool { return true }
