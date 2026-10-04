package launcher

import (
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"

	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/platform"
	"github.com/korvin2000/java-runner/internal/ui"
)

var iconExts = []string{".icns"}

// createIntegrations creates ~/Applications/<Name>.app (Launchpad/Spotlight),
// a ~/Desktop/<Name>.command shortcut and the PATH link. Both open the
// launcher in Terminal so its output stays visible.
func createIntegrations(in integration) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var created []string
	var errs []error
	name := platform.SafeName(in.Name)
	if in.Menu {
		app := filepath.Join(home, "Applications", name+".app")
		if err := writeAppBundle(app, in); err != nil {
			errs = append(errs, err)
		} else {
			created = append(created, app)
			ui.Info("created %s", app)
		}
	}
	if in.Desktop {
		p := filepath.Join(home, "Desktop", name+".command")
		if err := writeFile(p, "#!/bin/sh\nexec "+shQuote(in.Launcher)+" \"$@\"\n", 0o755); err != nil {
			errs = append(errs, err)
		} else {
			created = append(created, p)
			ui.Info("created desktop shortcut %s", p)
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

func writeAppBundle(app string, in integration) error {
	_ = os.RemoveAll(app)
	contents := filepath.Join(app, "Contents")
	script := "#!/bin/sh\nexec open -a Terminal " + shQuote(in.Launcher) + "\n"
	if err := writeFile(filepath.Join(contents, "MacOS", in.ID), script, 0o755); err != nil {
		return err
	}
	iconKey := ""
	if in.Icon != "" && fsutil.CopyFile(in.Icon, filepath.Join(contents, "Resources", "app.icns"), 0o644) == nil {
		iconKey = "<key>CFBundleIconFile</key><string>app</string>"
	}
	x := html.EscapeString
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>%s</string>
  <key>CFBundleDisplayName</key><string>%s</string>
  <key>CFBundleIdentifier</key><string>%s</string>
  <key>CFBundleVersion</key><string>%s</string>
  <key>CFBundleShortVersionString</key><string>%s</string>
  <key>CFBundleExecutable</key><string>%s</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  %s
</dict>
</plist>
`, x(in.Name), x(in.Name), x("jrunner."+in.ID), x(in.Version), x(in.Version), x(in.ID), iconKey)
	return writeFile(filepath.Join(contents, "Info.plist"), plist, 0o644)
}
