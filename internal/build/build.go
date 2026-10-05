// Package build implements the developer commands: building launchers and
// update packages, generating a configuration and creating jlink runtimes.
package build

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/korvin2000/java-runner/internal/config"
	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/jre"
	"github.com/korvin2000/java-runner/internal/pkg"
	"github.com/korvin2000/java-runner/internal/platform"
	"github.com/korvin2000/java-runner/internal/ui"
	"github.com/korvin2000/java-runner/internal/version"
)

// AllTargets are the platforms launchers can be built for.
var AllTargets = []string{"windows-x64", "windows-aarch64", "linux-x64", "linux-aarch64", "mac-x64", "mac-aarch64"}

// Options for Build.
type Options struct {
	Config  string   // path of jrunner.json
	Targets []string // platform keys; empty = current platform
	Out     string   // output directory
	Stubs   string   // directory with jrunner-<target> stubs for other platforms
	Version string   // overrides the version in the configuration (CI builds)
	Thin    bool     // launchers without the application: it is downloaded from update.url
}

type entry struct{ name, src string } // package entry name and source file

// Build creates one launcher per target and, if updates are configured, the
// update package(s) plus update.json.
func Build(o Options) error {
	cfg, err := config.Load(o.Config)
	if err != nil {
		return err
	}
	if o.Version != "" {
		cfg.Version = strings.TrimSpace(o.Version)
		if cfg.Version == "" {
			return fmt.Errorf("--version must not be empty")
		}
	}
	if u := cfg.Update; u != nil && !strings.HasPrefix(strings.ToLower(u.URL), "https://") {
		ui.Warn("update.url %s is not https: update.json and its checksums could be tampered with in transit", u.URL)
	}
	if o.Thin && cfg.Update == nil {
		return fmt.Errorf("--thin needs update.url: a thin launcher downloads the application from there")
	}
	base, err := filepath.Abs(filepath.Dir(o.Config))
	if err != nil {
		return err
	}
	targets, err := expandTargets(o.Targets)
	if err != nil {
		return err
	}
	files, err := collectFiles(cfg, base)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return err
	}

	ui.Step("Building %s %s", cfg.Name, cfg.Version)
	runtimes := map[string]string{}
	for _, t := range targets {
		if cfg.Java.Runtime != "" {
			if runtimes[t], err = runtimeFor(cfg, base, t); err != nil {
				return err
			}
		}
		stub, err := findStub(t, o.Stubs)
		if err != nil {
			return err
		}
		launcherFiles, launcherRuntime, suffix := files, runtimes[t], ""
		if o.Thin {
			launcherFiles, launcherRuntime, suffix = nil, "", "-thin"
		}
		out := filepath.Join(o.Out, fmt.Sprintf("%s-%s-%s%s%s", cfg.ID, cfg.Version, t, suffix, exeSuffix(t)))
		if err := writeLauncher(out, stub, cfg, launcherFiles, launcherRuntime); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		st, _ := os.Stat(out)
		ui.Success("%-15s %s (%s)", t, out, ui.Size(st.Size()))
	}
	if cfg.Update != nil {
		if err := writeUpdate(o.Out, cfg, files, targets, runtimes); err != nil {
			return err
		}
	}
	return nil
}

func expandTargets(list []string) ([]string, error) {
	if len(list) == 0 {
		return []string{platform.Key()}, nil
	}
	var out []string
	for _, t := range list {
		switch t = strings.TrimSpace(t); {
		case t == "all":
			out = append(out, AllTargets...)
		case t == "":
		default:
			if _, _, ok := platform.GoTarget(t); !ok {
				return nil, fmt.Errorf("unknown target %q (known: %s, all)", t, strings.Join(AllTargets, ", "))
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// collectFiles maps the jar, extra files and icons to package entries below
// app/. Files keep their path relative to the configuration file.
func collectFiles(cfg *config.Config, base string) ([]entry, error) {
	var files []entry
	seen := map[string]string{}
	add := func(name, src string) error {
		if prev, ok := seen[name]; ok {
			if prev != src {
				return fmt.Errorf("%s and %s would both be installed as %s", prev, src, name)
			}
			return nil
		}
		seen[name] = src
		files = append(files, entry{name, src})
		return nil
	}
	if cfg.Jar != "" {
		jar := abs(base, cfg.Jar)
		if !fsutil.IsFile(jar) {
			return nil, fmt.Errorf("jar %s not found (build your application first)", jar)
		}
		if err := add(pkg.AppDir+filepath.Base(jar), jar); err != nil {
			return nil, err
		}
	}
	for _, pattern := range cfg.Files {
		matches, err := filepath.Glob(abs(base, pattern))
		if err != nil || len(matches) == 0 {
			return nil, fmt.Errorf("files: %q matches nothing", pattern)
		}
		for _, m := range matches {
			err := filepath.WalkDir(m, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				rel, rerr := filepath.Rel(base, p)
				if rerr != nil || strings.HasPrefix(rel, "..") {
					// Outside the project: keep the matched name.
					rel, _ = filepath.Rel(filepath.Dir(m), p)
				}
				return add(pkg.AppDir+filepath.ToSlash(rel), p)
			})
			if err != nil {
				return nil, err
			}
		}
	}
	for _, ic := range cfg.Icons {
		p := abs(base, ic)
		if !fsutil.IsFile(p) {
			return nil, fmt.Errorf("icon %s not found", p)
		}
		if err := add(pkg.AppDir+filepath.Base(p), p); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// runtimeFor resolves java.runtime for a target and checks that it is a
// runtime image for that platform.
func runtimeFor(cfg *config.Config, base, target string) (string, error) {
	osName, arch, _ := strings.Cut(target, "-")
	dir := abs(base, strings.NewReplacer("{os}", osName, "{arch}", arch, "{platform}", target).Replace(cfg.Java.Runtime))
	java := filepath.Join(dir, "bin", "java"+exeSuffix(target))
	if !fsutil.IsFile(java) {
		return "", fmt.Errorf("java.runtime: %s is not a Java runtime for %s (missing %s)", dir, target, java)
	}
	rel := jre.ReadRelease(dir)
	if n := strings.ToLower(rel["OS_NAME"]); n != "" {
		want := map[string]string{"windows": "windows", "linux": "linux", "mac": "darwin"}[osName]
		if n != want {
			return "", fmt.Errorf("java.runtime: %s is a %s runtime, not for %s", dir, rel["OS_NAME"], target)
		}
	}
	return dir, nil
}

// findStub returns the jrunner executable for target: this executable for
// the current platform, otherwise jrunner-<target>[.exe] from the stubs
// directory, <tool dir>/stubs or the tool directory.
func findStub(target, dir string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if target == platform.Key() {
		return self, nil
	}
	name := "jrunner-" + target + exeSuffix(target)
	var tried []string
	for _, d := range []string{dir, filepath.Join(filepath.Dir(self), "stubs"), filepath.Dir(self)} {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if fsutil.IsFile(p) {
			return p, nil
		}
		tried = append(tried, d)
	}
	return "", fmt.Errorf("no launcher stub %s for %s (looked in %s); build the stubs with \"make stubs\" or pass --stubs <dir>",
		name, target, strings.Join(tried, ", "))
}

// writeLauncher writes stub + package + trailer.
func writeLauncher(out, stubPath string, cfg *config.Config, files []entry, runtimeDir string) error {
	f, err := os.Open(stubPath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	// A stub that is itself a launcher: use only its executable part.
	if p, err := pkg.OpenExecutable(stubPath); err == nil && p != nil {
		size = p.StubSize
		p.Close()
	}
	return pkg.WriteExecutable(out, io.NewSectionReader(f, 0, size), func(w io.Writer) error {
		return writePackage(w, cfg, files, runtimeDir)
	})
}

// writePackage writes the application package. The build id is a hash of
// the content and the configuration, so identical builds get identical ids.
func writePackage(w io.Writer, cfg *config.Config, files []entry, runtimeDir string) error {
	pw := pkg.NewWriter(w)
	for _, f := range files {
		if err := pw.AddFile(f.name, f.src); err != nil {
			return err
		}
	}
	if runtimeDir != "" {
		if err := pw.AddTree(pkg.RuntimeDir, runtimeDir); err != nil {
			return err
		}
	}
	pc := packagedConfig(cfg, runtimeDir != "")
	plain, err := json.Marshal(pc)
	if err != nil {
		return err
	}
	h := sha256.New()
	h.Write(pw.Digest())
	h.Write(plain)
	pc.Build = &config.Build{
		ID:             hex.EncodeToString(h.Sum(nil))[:16],
		Time:           time.Now().UTC().Format(time.RFC3339),
		Tool:           version.Tool,
		BundledRuntime: runtimeDir != "",
	}
	data, err := pc.Marshal()
	if err != nil {
		return err
	}
	if err := pw.AddBytes(pkg.ConfigName, data); err != nil {
		return err
	}
	return pw.Close()
}

// packagedConfig rewrites build-time paths to paths inside the app directory.
func packagedConfig(cfg *config.Config, bundled bool) *config.Config {
	pc := *cfg
	if cfg.Jar != "" {
		pc.Jar = filepath.Base(cfg.Jar)
	}
	pc.Files = nil
	pc.Icons = nil
	for _, ic := range cfg.Icons {
		pc.Icons = append(pc.Icons, filepath.Base(ic))
	}
	pc.Java.Runtime = ""
	pc.Build = nil
	return &pc
}

// writeUpdate writes the update package(s) and update.json. Without a
// bundled runtime one package serves all platforms.
func writeUpdate(out string, cfg *config.Config, files []entry, targets []string, runtimes map[string]string) error {
	m := pkg.Manifest{Version: cfg.Version}
	write := func(name, runtimeDir string) (string, error) {
		p := filepath.Join(out, name)
		f, err := os.Create(p)
		if err != nil {
			return "", err
		}
		h := sha256.New()
		err = writePackage(io.MultiWriter(f, h), cfg, files, runtimeDir)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return hex.EncodeToString(h.Sum(nil)), err
	}
	var names []string
	if cfg.Java.Runtime == "" {
		name := fmt.Sprintf("%s-%s.zip", cfg.ID, cfg.Version)
		sum, err := write(name, "")
		if err != nil {
			return err
		}
		m.URL, m.SHA256 = name, sum
		names = append(names, name)
	} else {
		m.Platforms = map[string]pkg.Asset{}
		for _, t := range targets {
			name := fmt.Sprintf("%s-%s-%s.zip", cfg.ID, cfg.Version, t)
			sum, err := write(name, runtimes[t])
			if err != nil {
				return err
			}
			m.Platforms[t] = pkg.Asset{URL: name, SHA256: sum}
			names = append(names, name)
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "update.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	ui.Step("Update channel")
	ui.Info("upload update.json and %s so that they are reachable at %s", strings.Join(names, ", "), cfg.Update.URL)
	ui.Info("(add \"notes\" to update.json to show release notes to users)")
	return nil
}

func abs(base, p string) string {
	p = platform.ExpandHome(p)
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, filepath.FromSlash(p))
}

func exeSuffix(target string) string {
	if strings.HasPrefix(target, "windows-") {
		return ".exe"
	}
	return ""
}
