//go:build !windows

package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/korvin2000/java-runner/internal/ui"
)

// linkInPath creates ~/.local/bin/<id> pointing to the launcher.
func linkInPath(in integration) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	link := filepath.Join(dir, in.ID)
	if st, err := os.Lstat(link); err == nil {
		if st.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("%s exists and is not a link, leaving it alone", link)
		}
		_ = os.Remove(link)
	}
	if err := os.Symlink(in.Launcher, link); err != nil {
		return "", err
	}
	ui.Info("linked %s -> %s", link, in.Launcher)
	if !onPath(dir) {
		ui.Info("add %s to your PATH to start %q from any terminal", dir, in.ID)
	}
	return link, nil
}

func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == dir {
			return true
		}
	}
	return false
}

func removeIntegrations(_ integration, files []string) error {
	removeFiles(files)
	return nil
}

func writeFile(name string, data string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(name, []byte(data), perm); err != nil {
		return err
	}
	return os.Chmod(name, perm)
}

// shQuote quotes s for a POSIX shell.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
