package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/platform"
	"github.com/korvin2000/java-runner/internal/ui"
)

// integration holds what the OS-specific code needs to create shortcuts,
// menu entries, PATH entries and (Windows) the "Apps" list entry.
type integration struct {
	Name, ID, Version, Publisher string
	Launcher, InstallDir, Icon   string
	Desktop, Menu, Path          bool
}

// fileName is the base name for shortcuts: the display name without
// characters that are not allowed in file names.
func (in integration) fileName() string {
	if n := platform.SafeName(in.Name); n != "" {
		return n
	}
	return in.ID
}

// integration describes the current app. Shortcuts and PATH entries are only
// requested when withShortcuts is set (first install or reinstall), so that
// shortcuts a user deleted do not come back with every update.
func (l *launcher) integration(withShortcuts bool, icons ...string) integration {
	c := l.cfg
	in := integration{
		Name: c.Name, ID: c.ID, Version: c.Version, Publisher: c.Publisher,
		Launcher: l.paths.Launcher, InstallDir: l.paths.Install,
	}
	if withShortcuts {
		in.Desktop, in.Menu, in.Path = c.Install.DesktopShortcut, c.Install.MenuShortcut, c.Install.AddToPath
	}
	for _, ext := range icons {
		for _, ic := range c.Icons {
			if strings.EqualFold(filepath.Ext(ic), ext) {
				if p := filepath.Join(l.paths.App, filepath.FromSlash(ic)); fsutil.IsFile(p) {
					in.Icon = p
					return in
				}
			}
		}
	}
	return in
}

// integrate creates (or refreshes) desktop integration. Failures are only
// warnings: the application itself is installed and works.
func (l *launcher) integrate(withShortcuts bool) {
	created, err := createIntegrations(l.integration(withShortcuts, iconExts...))
	for _, c := range created {
		if !contains(l.state.Integrations, c) {
			l.state.Integrations = append(l.state.Integrations, c)
		}
	}
	if err != nil {
		ui.Warn("desktop integration incomplete: %v", err)
	}
	if len(created) > 0 {
		_ = l.state.save(l.paths.State)
	}
}

func (l *launcher) uninstall() error {
	st := loadState(l.paths.State)
	if !fsutil.Exists(l.paths.State) && !fsutil.Exists(l.paths.App) {
		return fmt.Errorf("%s is not installed (%s)", l.cfg.Name, l.paths.Install)
	}
	if sharedDir(l.paths.Install) {
		return fmt.Errorf("refusing to delete %s: it is not a directory of its own (check install.dir); remove the application files manually", l.paths.Install)
	}
	if ui.Interactive() && !ui.Confirm("Uninstall "+l.cfg.Name+" from "+l.paths.Install+"?", false) {
		ui.Info("cancelled")
		return nil
	}
	ui.Step("Uninstalling %s", l.cfg.Name)
	if installed := loadInstalledConfig(l.paths.App); installed != nil {
		l.cfg = installed
	}
	if err := removeIntegrations(l.integration(false), st.Integrations); err != nil {
		ui.Warn("%v", err)
	}
	if err := removeInstallDir(l.paths.Install, l.exe); err != nil {
		return fmt.Errorf("cannot remove %s: %v", l.paths.Install, err)
	}
	ui.Info("removed %s", l.paths.Install)
	if fsutil.Exists(l.paths.Data) {
		ui.Info("your data in %s was kept", l.paths.Data)
	}
	return nil
}

// sharedDir reports whether dir is the home directory, contains it, or is one
// of the per-user program or data folders: a misconfigured install.dir must
// never make uninstall delete those.
func sharedDir(dir string) bool {
	dir = filepath.Clean(dir)
	if filepath.Dir(dir) == dir {
		return true // file system root
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Dir(home) != home {
		if rel, err := filepath.Rel(dir, home); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	for _, f := range []func() (string, error){platform.ProgramsDir, platform.DataDir} {
		if d, err := f(); err == nil && filepath.Clean(d) == dir {
			return true
		}
	}
	return false
}

// removeFiles deletes recorded shortcut files and links.
func removeFiles(files []string) {
	for _, f := range files {
		if strings.HasSuffix(f, ".app") {
			_ = os.RemoveAll(f)
		} else {
			_ = os.Remove(f)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
