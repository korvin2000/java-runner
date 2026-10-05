package launcher

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/korvin2000/java-runner/internal/config"
	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/jre"
	"github.com/korvin2000/java-runner/internal/pkg"
	"github.com/korvin2000/java-runner/internal/ui"
)

// state is persisted in <install>/state.json so that later launches need no
// discovery work.
type state struct {
	Version         string       `json:"version"`
	Build           string       `json:"build,omitempty"`
	InstalledAt     time.Time    `json:"installedAt"`
	Java            *jre.Runtime `json:"java,omitempty"`
	LastUpdateCheck time.Time    `json:"lastUpdateCheck"`
	Integrations    []string     `json:"integrations,omitempty"` // shortcuts/links created outside the install dir
}

// loadState returns the saved state; a missing or damaged file yields an
// empty state, which triggers a (re)installation.
func loadState(name string) *state {
	st := &state{}
	if data, err := os.ReadFile(name); err == nil {
		if json.Unmarshal(data, st) != nil {
			*st = state{}
		}
	}
	return st
}

func (s *state) save(name string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(name, data, 0o644)
}

// install installs the package embedded in this executable (including the
// launcher binary) under the install lock.
func (l *launcher) install(p *pkg.Package) error {
	unlock, err := lock(l.paths.Install)
	if err != nil {
		return err
	}
	defer unlock()
	if l.installedMeanwhile(p.Config) {
		return nil
	}
	return l.installLocked(p, true)
}

// installedMeanwhile reports whether another launcher completed an
// installation of want (nil: of any version) while this one waited for the
// lock, e.g. after a double-click on the first start, and adopts it instead
// of installing again (which on Windows would fail on the running app).
func (l *launcher) installedMeanwhile(want *config.Config) bool {
	if l.opts.reinstall {
		return false
	}
	st := loadState(l.paths.State)
	inst := loadInstalledConfig(l.paths.App)
	switch {
	case inst == nil || st.Version == "":
		return false
	case inst.Jar != "" && !fsutil.IsFile(filepath.Join(l.paths.App, filepath.FromSlash(inst.Jar))):
		return false
	case want != nil && (st.Version != want.Version || st.Build != buildID(want)):
		return false
	}
	ui.Success("installed by another instance")
	l.state, l.cfg = st, inst
	return true
}

// installLocked copies a package into the install directory; the caller
// holds the install lock. fromLauncher is true for the package embedded in
// this executable: then the launcher binary is installed too. Update packages
// only replace the application files.
func (l *launcher) installLocked(p *pkg.Package, fromLauncher bool) error {
	cfg := p.Config
	ui.Info("location: %s", l.paths.Install)
	stop := ui.Spin("copying application files")

	tmp := l.paths.App + ".new"
	_ = os.RemoveAll(tmp)
	if err := p.Extract(pkg.AppDir, tmp); err != nil {
		stop(false, "")
		return fmt.Errorf("extracting application files: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, pkg.ConfigName), p.RawConfig, 0o644); err != nil {
		stop(false, "")
		return err
	}
	if err := fsutil.ReplaceDir(tmp, l.paths.App); err != nil {
		stop(false, "")
		os.RemoveAll(tmp)
		return inUse(cfg.Name, err)
	}
	stop(true, fmt.Sprintf("application files (%s)", ui.Size(fsutil.DirSize(l.paths.App))))
	if p.HasRuntime {
		stop := ui.Spin("installing bundled Java runtime")
		tmp := l.paths.Runtime + ".new"
		_ = os.RemoveAll(tmp)
		if err := p.Extract(pkg.RuntimeDir, tmp); err != nil {
			stop(false, "")
			return fmt.Errorf("extracting Java runtime: %w", err)
		}
		jre.FixPermissions(tmp)
		if err := fsutil.ReplaceDir(tmp, l.paths.Runtime); err != nil {
			stop(false, "")
			os.RemoveAll(tmp)
			return inUse(cfg.Name, err)
		}
		stop(true, fmt.Sprintf("bundled Java runtime (%s)", ui.Size(fsutil.DirSize(l.paths.Runtime))))
	}
	if fromLauncher && !samePath(l.exe, l.paths.Launcher) {
		if err := l.writeLauncher(p); err != nil {
			return fmt.Errorf("installing launcher: %w", err)
		}
	}

	firstInstall := l.state.Version == "" || l.opts.reinstall
	l.cfg = cfg
	l.setup = true
	l.state.Version = cfg.Version
	l.state.Build = buildID(cfg)
	l.state.InstalledAt = time.Now()
	if l.state.Java != nil && (p.HasRuntime || l.state.Java.Source == "bundled") {
		l.state.Java = nil // re-resolve against the new runtime/requirements
	}
	if err := l.state.save(l.paths.State); err != nil {
		return err
	}
	l.integrate(firstInstall)
	ui.Success("installed %s %s", cfg.Name, cfg.Version)
	return nil
}

// writeLauncher installs a copy of this executable that carries only the
// configuration; the application files live in the app directory.
func (l *launcher) writeLauncher(p *pkg.Package) error {
	return pkg.WriteExecutable(l.paths.Launcher, p.Stub(), func(w io.Writer) error {
		pw := pkg.NewWriter(w)
		if err := pw.AddBytes(pkg.ConfigName, p.RawConfig); err != nil {
			return err
		}
		return pw.Close()
	})
}

func inUse(name string, err error) error {
	return fmt.Errorf("cannot replace installed files (%v); if %s is running, close it and try again", err, name)
}

func samePath(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}
