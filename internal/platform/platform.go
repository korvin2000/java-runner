// Package platform hides operating system differences: platform naming,
// standard directories, terminals and opening URLs.
package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// OS returns the operating system name used in platform keys and download
// templates: "windows", "linux" or "mac".
func OS() string {
	if runtime.GOOS == "darwin" {
		return "mac"
	}
	return runtime.GOOS
}

// Arch returns the CPU architecture in Java naming: "x64", "aarch64", "x86".
func Arch() string { return ArchName(runtime.GOARCH) }

// ArchName converts a GOARCH value to Java naming.
func ArchName(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "arm64":
		return "aarch64"
	case "386":
		return "x86"
	}
	return goarch
}

// Key identifies the current platform, e.g. "windows-x64" or "mac-aarch64".
func Key() string { return OS() + "-" + Arch() }

// GoTarget converts a platform key such as "mac-aarch64" to GOOS and GOARCH.
func GoTarget(key string) (goos, goarch string, ok bool) {
	osName, arch, found := strings.Cut(key, "-")
	if !found {
		return "", "", false
	}
	switch osName {
	case "mac":
		goos = "darwin"
	case "windows", "linux":
		goos = osName
	default:
		return "", "", false
	}
	switch arch {
	case "x64":
		goarch = "amd64"
	case "aarch64":
		goarch = "arm64"
	case "x86":
		goarch = "386"
	default:
		return "", "", false
	}
	return goos, goarch, true
}

// ExeName appends ".exe" on Windows.
func ExeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// ProgramsDir is where per-user programs are installed:
// %LOCALAPPDATA%\Programs, ~/Applications or ~/.local/opt.
func ProgramsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "Programs"), nil
		}
		return filepath.Join(home, "AppData", "Local", "Programs"), nil
	case "darwin":
		return filepath.Join(home, "Applications"), nil
	}
	return filepath.Join(home, ".local", "opt"), nil
}

// DataDir is the per-user directory for application data:
// %LOCALAPPDATA%, ~/Library/Application Support or $XDG_DATA_HOME.
func DataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return d, nil
		}
		return filepath.Join(home, "AppData", "Local"), nil
	case "darwin":
		return filepath.Join(home, "Library", "Application Support"), nil
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d, nil
	}
	return filepath.Join(home, ".local", "share"), nil
}

// SafeName strips characters that are not allowed in file names.
func SafeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimRight(strings.TrimSpace(s), ". ")
}

// ExpandHome replaces a leading "~" with the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// IsTerminal reports whether f is an interactive console.
func IsTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// IsMusl reports whether this is a musl-based Linux (e.g. Alpine).
func IsMusl() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	m, _ := filepath.Glob("/lib/ld-musl-*")
	return len(m) > 0
}

// HasDisplay reports whether a graphical session (and thus a browser) is
// likely available.
func HasDisplay() bool {
	switch runtime.GOOS {
	case "windows", "darwin":
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// OpenURL opens u in the default browser without waiting for it.
func OpenURL(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		for _, c := range [][]string{{"xdg-open"}, {"gio", "open"}, {"sensible-browser"}, {"x-www-browser"}} {
			if p, err := exec.LookPath(c[0]); err == nil {
				cmd = exec.Command(p, append(c[1:], u)...)
				break
			}
		}
		if cmd == nil {
			return exec.ErrNotFound
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
