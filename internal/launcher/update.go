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
	u := l.cfg.Update
	return u != nil && time.Since(l.state.LastUpdateCheck) >= u.Interval()
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
	p, done, err := l.downloadPackage(m)
	if err != nil {
		return false, err
	}
	defer done()
	switch {
	case version.Compare(p.Config.Version, m.Version) != 0:
		return false, fmt.Errorf("update package contains version %s, but update.json announces %s", p.Config.Version, m.Version)
	case p.Config.Build != nil && p.Config.Build.BundledRuntime != p.HasRuntime:
		return false, errors.New("update package is inconsistent (bundled runtime)")
	}
	ui.Step("Installing %s %s", p.Config.Name, p.Config.Version)
	if err := l.install(p, false); err != nil {
		return false, err
	}
	return true, nil
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

// downloadPackage fetches the package announced by m for this platform,
// verifies it and opens it. done removes the temporary file.
func (l *launcher) downloadPackage(m *pkg.Manifest) (p *pkg.Package, done func(), err error) {
	asset := pkg.Asset{URL: m.URL, SHA256: m.SHA256}
	if a, ok := m.Platforms[platform.Key()]; ok {
		asset = a
	}
	if asset.URL == "" {
		return nil, nil, fmt.Errorf("version %s has no package for %s", m.Version, platform.Key())
	}
	pkgURL, err := resolveRef(l.cfg.Update.URL, asset.URL)
	if err != nil {
		return nil, nil, err
	}
	if asset.SHA256 == "" {
		ui.Warn("update.json has no sha256 for the package, integrity not verified")
	}
	file := filepath.Join(l.paths.Install, updateFile)
	if err := os.MkdirAll(l.paths.Install, 0o755); err != nil {
		return nil, nil, err
	}
	ui.Info("downloading %s", pkgURL)
	if err := fetch.File(context.Background(), pkgURL, file, asset.SHA256); err != nil {
		os.Remove(file)
		return nil, nil, err
	}
	p, err = pkg.OpenFile(file)
	if err != nil {
		os.Remove(file)
		return nil, nil, err
	}
	done = func() { p.Close(); os.Remove(file) }
	switch {
	case p.Config.ID != l.cfg.ID:
		done()
		return nil, nil, fmt.Errorf("package is for %q, not %q", p.Config.ID, l.cfg.ID)
	case !p.HasApp:
		done()
		return nil, nil, errors.New("package contains no application files")
	}
	return p, done, nil
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
	p, done, err := l.downloadPackage(m)
	if err != nil {
		return err
	}
	defer done()
	if !samePath(l.exe, l.paths.Launcher) {
		if err := fsutil.CopyFile(l.exe, l.paths.Launcher, 0o755); err != nil {
			return fmt.Errorf("installing launcher: %w", err)
		}
	}
	if err := l.install(p, false); err != nil {
		return err
	}
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
