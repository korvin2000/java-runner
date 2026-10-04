package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// execJava runs Java as a child in the same console and returns its exit code.
func execJava(cmd *exec.Cmd) (int, error) {
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("starting Java: %w", err)
	}
	stop := handleSignals(cmd.Process)
	defer stop()
	return exitCode(cmd.Wait())
}

// handleSignals keeps the launcher alive on Ctrl+C / window close: the console
// delivers these events to Java itself, which then shuts down gracefully.
func handleSignals(*os.Process) (stop func()) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range ch {
		}
	}()
	return func() { signal.Stop(ch); close(ch) }
}

// removeInstallDir deletes the installation. A running executable cannot
// delete itself on Windows, so what remains is removed by a detached
// cmd.exe once this process has exited.
func removeInstallDir(dir, exe string) error {
	err := os.RemoveAll(dir)
	if err == nil {
		return nil
	}
	if rel, rerr := filepath.Rel(dir, exe); rerr != nil || strings.HasPrefix(rel, "..") {
		return err
	}
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       fmt.Sprintf(`cmd.exe /D /S /C "ping -n 3 127.0.0.1 >NUL & rmdir /S /Q "%s""`, dir),
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
	return cmd.Start()
}
