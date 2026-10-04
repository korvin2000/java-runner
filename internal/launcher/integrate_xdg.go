//go:build !windows && !darwin

package launcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/ui"
)

var iconExts = []string{".png", ".svg", ".xpm"}

// createIntegrations writes freedesktop.org .desktop entries (menu and
// desktop) and the PATH link.
func createIntegrations(in integration) ([]string, error) {
	var created []string
	var errs []error
	entry := desktopEntry(in)
	if in.Menu {
		p := filepath.Join(xdgDataHome(), "applications", in.ID+".desktop")
		if err := writeFile(p, entry, 0o644); err != nil {
			errs = append(errs, err)
		} else {
			created = append(created, p)
			ui.Info("added %s to the applications menu", in.Name)
		}
	}
	if in.Desktop {
		if dir := desktopDir(); dir != "" {
			p := filepath.Join(dir, in.ID+".desktop")
			if err := writeFile(p, entry, 0o755); err != nil {
				errs = append(errs, err)
			} else {
				created = append(created, p)
				run("gio", "set", p, "metadata::trusted", "true") // GNOME: allow launching
				ui.Info("created desktop shortcut %s", p)
			}
		}
	}
	if in.Path {
		if p, err := linkInPath(in); err != nil {
			errs = append(errs, err)
		} else {
			created = append(created, p)
		}
	}
	return created, errors.Join(errs...)
}

func desktopEntry(in integration) string {
	var b strings.Builder
	b.WriteString("[Desktop Entry]\nType=Application\n")
	b.WriteString("Name=" + in.Name + "\n")
	b.WriteString("Exec=" + desktopExec(in.Launcher) + "\n")
	b.WriteString("Path=" + in.InstallDir + "\n")
	if in.Icon != "" {
		b.WriteString("Icon=" + in.Icon + "\n")
	}
	b.WriteString("Terminal=true\nCategories=Utility;\n")
	return b.String()
}

// desktopExec quotes a path for the Exec key (quoting rules of the Desktop
// Entry spec, then the general string escaping, then field-code escaping).
func desktopExec(p string) string {
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(p)
	q = strings.ReplaceAll(`"`+q+`"`, `\`, `\\`)
	return strings.ReplaceAll(q, "%", "%%")
}

func desktopDir() string {
	home, _ := os.UserHomeDir()
	if out, err := output("xdg-user-dir", "DESKTOP"); err == nil {
		if d := strings.TrimSpace(out); d != "" && d != home && fsutil.IsDir(d) {
			return d
		}
	}
	if d := filepath.Join(home, "Desktop"); fsutil.IsDir(d) {
		return d
	}
	return ""
}

func xdgDataHome() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}

func output(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func run(name string, args ...string) { _, _ = output(name, args...) }
