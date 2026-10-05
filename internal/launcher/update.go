package launcher

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/korvin2000/java-runner/internal/fetch"
	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/pkg"
	"github.com/korvin2000/java-runner/internal/platform"
	"github.com/korvin2000/java-runner/internal/ui"
	"github.com/korvin2000/java-runner/internal/version"
)

// updateFile is the temporary name of a downloaded update package.
const updateFile = ".update.zip"

func (l *launcher) updateDue() bool {
	u, last := l.cfg.Update, l.state.LastUpdateCheck
	// A last check in the future means the clock was wrong: check now.
	return u != nil && (last.After(time.Now()) || time.Since(last) >= u.Interval())
}

// checkUpdate fetches update.json and, if a newer version is published and
// the user agrees (or update.auto is set), downloads and installs it.
func (l *launcher) checkUpdate(forced bool) (bool, error) {
	u := l.cfg.Update
	if u == nil {
		if forced {
			return false, errors.New("no update URL is configured for this application")
		}
		return false, nil
	}
	if forced {
		ui.Step("Checking for updates")
	} else {
		ui.Debug("checking for updates at %s", u.URL)
	}
	m, err := l.fetchManifest(10 * time.Second)
	l.state.LastUpdateCheck = time.Now()
	_ = l.state.save(l.paths.State)
	if err != nil {
		return false, err
	}
	if version.Compare(m.Version, l.cfg.Version) <= 0 {
		if forced {
			ui.Success("%s %s is up to date", l.cfg.Name, l.cfg.Version)
		}
		return false, nil
	}

	ui.Step("Update available: %s %s (installed: %s)", l.cfg.Name, m.Version, l.cfg.Version)
	if m.Notes != "" {
		ui.Info("%s", m.Notes)
	}
	if !forced && !u.Auto && !(ui.Interactive() && ui.Confirm("Install the update now?", true)) {
		ui.Info("not installed; run \"%s %supdate\" to install it later", l.paths.Launcher, flagPrefix)
		return false, nil
	}
	unlock, err := lock(l.paths.Install)
	if err != nil {
		return false, err
	}
	defer unlock()
	// Another launcher may have installed this update while this one waited.
	if inst := loadInstalledConfig(l.paths.App); inst != nil &&
		version.Compare(inst.Version, l.cfg.Version) > 0 && version.Compare(inst.Version, m.Version) >= 0 {
		ui.Success("%s %s was installed by another instance", inst.Name, inst.Version)
		l.cfg, l.state = inst, loadState(l.paths.State)
		return true, nil
	}
	p, file, err := l.downloadPackage(m)
	if err != nil {
		return false, err
	}
	// The verified package is kept when it cannot be installed right now
	// (files in use, Java not available offline): the next attempt reuses it.
	keep := false
	defer func() {
		p.Close()
		if !keep {
			os.Remove(file)
		}
	}()
	switch {
	case version.Compare(p.Config.Version, m.Version) != 0:
		return false, fmt.Errorf("update package contains version %s, but update.json announces %s", p.Config.Version, m.Version)
	case p.Config.Build != nil && p.Config.Build.BundledRuntime != p.HasRuntime:
		return false, errors.New("update package is inconsistent (bundled runtime)")
	}
	if err := l.prepareJava(p); err != nil {
		keep = true
		return false, fmt.Errorf("%s %s needs Java %s: %w; %s stays installed", p.Config.Name, p.Config.Version, requirement(p.Config), err, l.cfg.Version)
	}
	ui.Step("Installing %s %s", p.Config.Name, p.Config.Version)
	if err := l.installLocked(p, false); err != nil {
		keep = true
		return false, err
	}
	return true, nil
}

// prepareJava makes sure that the Java an update requires is available
// before the update replaces the installed version, so that a failing
// download (e.g. offline) leaves the current version usable. The caller holds
// the install lock.
func (l *launcher) prepareJava(p *pkg.Package) error {
	if p.HasRuntime || l.override != nil {
		return nil // brings its own runtime, or the user chose one
	}
	req := requirement(p.Config)
	if usable(l.state.Java, req) {
		return nil
	}
	rt, err := l.resolveJava(p.Config, req)
	if err != nil {
		return err
	}
	l.state.Java = rt
	return nil
}

// fetchManifest downloads and validates update.json.
func (l *launcher) fetchManifest(timeout time.Duration) (*pkg.Manifest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var m pkg.Manifest
	if err := fetch.JSON(ctx, l.cfg.Update.URL, &m); err != nil {
		return nil, err
	}
	if m.Version == "" {
		return nil, fmt.Errorf("%s: no version in update manifest", l.cfg.Update.URL)
	}
	return &m, nil
}

// downloadPackage fetches the package announced by m for this platform
// (resuming an interrupted download, reusing a complete one), verifies it
// and opens it. The caller closes p and decides whether to remove file.
func (l *launcher) downloadPackage(m *pkg.Manifest) (p *pkg.Package, file string, err error) {
	asset := pkg.Asset{URL: m.URL, SHA256: m.SHA256}
	if a, ok := m.Platforms[platform.Key()]; ok {
		asset = a
	}
	if asset.URL == "" {
		return nil, "", fmt.Errorf("version %s has no package for %s", m.Version, platform.Key())
	}
	pkgURL, err := resolveRef(l.cfg.Update.URL, asset.URL)
	if err != nil {
		return nil, "", err
	}
	if asset.SHA256 == "" {
		ui.Warn("update.json has no sha256 for the package, integrity not verified")
	}
	file = filepath.Join(l.paths.Install, updateFile)
	if err := os.MkdirAll(l.paths.Install, 0o755); err != nil {
		return nil, "", err
	}
	ui.Info("downloading %s", pkgURL)
	if err := fetch.File(context.Background(), pkgURL, file, asset.SHA256); err != nil {
		return nil, "", err // a partial download stays for the next attempt
	}
	if p, err = pkg.OpenFile(file); err != nil {
		os.Remove(file)
		return nil, "", err
	}
	switch {
	case p.Config.ID != l.cfg.ID:
		err = fmt.Errorf("package is for %q, not %q", p.Config.ID, l.cfg.ID)
	case !p.HasApp:
		err = errors.New("package contains no application files")
	}
	if err != nil {
		p.Close()
		os.Remove(file)
		return nil, "", err
	}
	return p, file, nil
}

// bootstrap installs a thin launcher: the application package is downloaded
// from the update channel and the launcher binary itself is installed.
func (l *launcher) bootstrap() error {
	ui.Step("Downloading %s from %s", l.cfg.Name, l.cfg.Update.URL)
	m, err := l.fetchManifest(30 * time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach the download server: %w", err)
	}
	ui.Info("latest version: %s", m.Version)
	unlock, err := lock(l.paths.Install)
	if err != nil {
		return err
	}
	defer unlock()
	if l.installedMeanwhile(nil) {
		return nil
	}
	p, file, err := l.downloadPackage(m)
	if err != nil {
		return err
	}
	keep := true // kept for a retry unless installed
	defer func() {
		p.Close()
		if !keep {
			os.Remove(file)
		}
	}()
	if !samePath(l.exe, l.paths.Launcher) {
		if err := fsutil.CopyFileAtomic(l.exe, l.paths.Launcher, 0o755); err != nil {
			return fmt.Errorf("installing launcher: %w", err)
		}
	}
	if err := l.installLocked(p, false); err != nil {
		return err
	}
	keep = false
	l.state.LastUpdateCheck = time.Now()
	return l.state.save(l.paths.State)
}

// resolveRef resolves a package URL relative to the manifest URL, so that
// update.json and the packages can simply be uploaded to the same folder.
func resolveRef(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}
