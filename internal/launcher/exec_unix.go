//go:build !windows

package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// execJava replaces the launcher process with Java: no extra process stays
// around and signals go straight to the JVM.
func execJava(cmd *exec.Cmd) (int, error) {
	if err := os.Chdir(cmd.Dir); err != nil {
		return 1, err
	}
	err := syscall.Exec(cmd.Path, cmd.Args, os.Environ())
	return 1, fmt.Errorf("starting Java: %w", err)
}

// handleSignals forwards termination signals to the child process.
func handleSignals(p *os.Process) (stop func()) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range ch {
			_ = p.Signal(s)
		}
	}()
	return func() { signal.Stop(ch); close(ch) }
}

func removeInstallDir(dir, _ string) error { return os.RemoveAll(dir) }
