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
	"github.com/korvin2000/java-runner/internal/pkg"
	"github.com/korvin2000/java-runner/internal/platform"
	"github.com/korvin2000/java-runner/internal/ui"
	"github.com/korvin2000/java-runner/internal/version"
)

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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var m pkg.Manifest
	err := fetch.JSON(ctx, u.URL, &m)
	l.state.LastUpdateCheck = time.Now()
	_ = l.state.save(l.paths.State)
	if err != nil {
		return false, err
	}
	if m.Version == "" {
		return false, fmt.Errorf("%s: no version in update manifest", u.URL)
	}
	if version.Compare(m.Version, l.cfg.Version) <= 0 {
		if forced {
			ui.Info("%s %s is up to date", l.cfg.Name, l.cfg.Version)
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
	asset := pkg.Asset{URL: m.URL, SHA256: m.SHA256}
	if a, ok := m.Platforms[platform.Key()]; ok {
		asset = a
	}
	if asset.URL == "" {
		return false, fmt.Errorf("update %s has no package for %s", m.Version, platform.Key())
	}
	pkgURL, err := resolveRef(u.URL, asset.URL)
	if err != nil {
		return false, err
	}

	file := filepath.Join(l.paths.Install, ".update.zip")
	defer os.Remove(file)
	ui.Info("downloading %s", pkgURL)
	if err := fetch.File(context.Background(), pkgURL, file, asset.SHA256); err != nil {
		return false, err
	}
	p, err := pkg.OpenFile(file)
	if err != nil {
		return false, err
	}
	defer p.Close()
	switch {
	case p.Config.ID != l.cfg.ID:
		return false, fmt.Errorf("update package is for %q, not %q", p.Config.ID, l.cfg.ID)
	case !p.HasApp:
		return false, errors.New("update package contains no application files")
	}
	ui.Step("Installing %s %s", p.Config.Name, p.Config.Version)
	if err := l.install(p, false); err != nil {
		return false, err
	}
	return true, nil
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
