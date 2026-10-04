// Package launcher is the runtime side of jrunner: it installs the embedded
// application, provisions Java, applies updates and starts the app.
//
// First launch:  install files -> find or download Java -> start.
// Later launches: read state.json, check that Java still exists -> start.
// Network access happens only for Java downloads and (rate limited) update
// checks, so normal launches are fast and work offline.
package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

const flagPrefix = "--jrunner-"

type options struct {
	verbose, update, reinstall, uninstall, info, help bool
}

type launcher struct {
	exe      string
	embedded *pkg.Package
	opts     options
	cfg      *config.Config // effective configuration (installed or embedded)
	paths    paths
	state    *state
	setup    bool // something was installed or downloaded during this launch
}

// Run executes the launcher and returns the process exit code.
func Run(exe string, embedded *pkg.Package, args []string) int {
	opts, appArgs, err := parseArgs(args)
	code := 1
	if err == nil {
		ui.SetVerbose(opts.verbose || os.Getenv("JRUNNER_VERBOSE") != "")
		l := &launcher{exe: exe, embedded: embedded, opts: opts, cfg: embedded.Config}
		code, err = l.run(appArgs)
	}
	if err != nil {
		ui.Error(err)
		code = 1
	}
	// 130/143: stopped with Ctrl+C or a termination signal, nothing to read.
	if code != 0 && code != 130 && code != 143 {
		ui.PauseIfOwnConsole()
	}
	return code
}

func parseArgs(args []string) (options, []string, error) {
	var o options
	var rest []string
	for _, a := range args {
		if !strings.HasPrefix(a, flagPrefix) {
			rest = append(rest, a)
			continue
		}
		switch strings.TrimPrefix(a, flagPrefix) {
		case "verbose":
			o.verbose = true
		case "update":
			o.update = true
		case "reinstall":
			o.reinstall = true
		case "uninstall":
			o.uninstall = true
		case "info":
			o.info = true
		case "help":
			o.help = true
		default:
			return o, nil, fmt.Errorf("unknown launcher option %s (see %shelp)", a, flagPrefix)
		}
	}
	return o, rest, nil
}

func (l *launcher) run(appArgs []string) (int, error) {
	if l.opts.help {
		l.printHelp()
		return 0, nil
	}
	var err error
	if l.paths, err = resolvePaths(l.cfg); err != nil {
		return 1, err
	}
	if l.opts.uninstall {
		return 0, l.uninstall()
	}
	l.state = loadState(l.paths.State)
	installed := loadInstalledConfig(l.paths.App)
	if l.opts.info {
		l.printInfo(installed)
		return 0, nil
	}

	if reason := l.installReason(installed); reason != "" {
		if !l.embedded.HasApp {
			return 1, fmt.Errorf("%s is not installed correctly in %s; please run the original installer again", l.cfg.Name, l.paths.Install)
		}
		ui.Step("%s %s: %s", l.cfg.Name, l.cfg.Version, reason)
		if l.opts.reinstall {
			l.state.Java = nil // look for Java again
		}
		if err := l.install(l.embedded, true); err != nil {
			return 1, err
		}
	} else {
		if c := version.Compare(l.cfg.Version, installed.Version); c < 0 {
			ui.Debug("installed version %s is newer than this launcher's %s, using it", installed.Version, l.cfg.Version)
		}
		l.cfg = installed
	}
	l.cleanup()

	if err := l.ensureJava(); err != nil {
		return 1, err
	}
	if l.opts.update || l.updateDue() {
		updated, err := l.checkUpdate(l.opts.update)
		if err != nil {
			if l.opts.update {
				return 1, err
			}
			ui.Warn("update check failed: %v", err)
		}
		if updated {
			if err := l.ensureJava(); err != nil {
				return 1, err
			}
		}
	}
	if l.opts.update {
		return 0, nil
	}
	return l.launch(appArgs)
}

// installReason tells whether the embedded package must be installed.
func (l *launcher) installReason(installed *config.Config) string {
	e := l.embedded.Config
	switch {
	case l.opts.reinstall:
		return "reinstalling"
	case installed == nil || l.state.Version == "":
		return "first launch, installing"
	case installed.Jar != "" && !fsutil.IsFile(filepath.Join(l.paths.App, filepath.FromSlash(installed.Jar))):
		return "repairing installation"
	case !l.embedded.HasApp:
		return ""
	}
	switch c := version.Compare(e.Version, l.state.Version); {
	case c > 0:
		return "upgrading from " + l.state.Version
	case c == 0 && buildID(e) != l.state.Build:
		return "installing new build"
	}
	return ""
}

// ensureJava makes sure state.Java points to a usable runtime. The common
// case costs one stat call.
func (l *launcher) ensureJava() error {
	req := requirement(l.cfg)
	if rt := l.state.Java; rt != nil && fsutil.IsFile(rt.Java()) && req.Accepts(rt) {
		return nil
	}
	unlock, err := lock(l.paths.Install)
	if err != nil {
		return err
	}
	defer unlock()
	rt, err := l.resolveJava(req)
	if err != nil {
		return err
	}
	l.setup = true
	l.state.Java = rt
	return l.state.save(l.paths.State)
}

func (l *launcher) resolveJava(req jre.Requirement) (*jre.Runtime, error) {
	if l.cfg.Build != nil && l.cfg.Build.BundledRuntime {
		rt, err := jre.Probe(l.paths.Runtime)
		if err != nil {
			return nil, fmt.Errorf("bundled Java runtime is missing or damaged (%v); run with %sreinstall", err, flagPrefix)
		}
		rt.Source = "bundled"
		return rt, nil
	}
	ui.Step("Looking for Java %s", req)
	var private []string
	if home, err := jre.FindHome(l.paths.Runtime); err == nil {
		private = append(private, home)
	}
	if rt := jre.Find(req, private...); rt != nil {
		ui.Info("using %s", rt)
		return rt, nil
	}
	feature := l.cfg.DownloadFeature()
	if !req.AcceptsFeature(feature) {
		feature = req.Min
	}
	ui.Step("Java %s not found, downloading Java %d", req, feature)
	rt, err := jre.Download(context.Background(), l.cfg.Java.Download, feature, req, l.paths.Runtime)
	if err != nil {
		return nil, err
	}
	ui.Info("installed %s", rt)
	return rt, nil
}

func requirement(c *config.Config) jre.Requirement {
	return jre.Requirement{Min: c.Java.MinVersion, Max: c.Java.MaxVersion, JDK: c.Java.Image == "jdk"}
}

// cleanup removes leftovers of replaced files (Windows keeps running
// executables and open jars locked until the process exits).
func (l *launcher) cleanup() {
	for _, p := range []string{l.paths.Launcher + ".old", l.paths.App + ".old", l.paths.App + ".new", l.paths.Runtime + ".old"} {
		if fsutil.Exists(p) {
			_ = os.RemoveAll(p)
		}
	}
}

func buildID(c *config.Config) string {
	if c.Build == nil {
		return ""
	}
	return c.Build.ID
}

// paths are the locations used by an installation.
type paths struct {
	Install  string // program directory
	App      string // application files
	Runtime  string // private (downloaded or bundled) Java runtime
	Data     string // per-user data, default working directory
	Launcher string // installed launcher executable
	State    string // state.json
}

func resolvePaths(c *config.Config) (paths, error) {
	name := c.ID
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		if n := platform.SafeName(c.Name); n != "" {
			name = n
		}
	}
	dir := c.Install.Dir
	if dir != "" {
		var err error
		if dir, err = filepath.Abs(platform.ExpandHome(os.ExpandEnv(dir))); err != nil {
			return paths{}, err
		}
	} else {
		root, err := platform.ProgramsDir()
		if err != nil {
			return paths{}, err
		}
		dir = filepath.Join(root, name)
	}
	dataRoot, err := platform.DataDir()
	if err != nil {
		return paths{}, err
	}
	return paths{
		Install:  dir,
		App:      filepath.Join(dir, "app"),
		Runtime:  filepath.Join(dir, "runtime"),
		Data:     filepath.Join(dataRoot, name),
		Launcher: filepath.Join(dir, platform.ExeName(c.ID)),
		State:    filepath.Join(dir, "state.json"),
	}, nil
}

func loadInstalledConfig(appDir string) *config.Config {
	data, err := os.ReadFile(filepath.Join(appDir, pkg.ConfigName))
	if err != nil {
		return nil
	}
	c, err := config.Parse(data, false)
	if err != nil {
		ui.Warn("installed configuration is damaged: %v", err)
		return nil
	}
	return c
}

// lock serializes installation and runtime downloads between concurrently
// started launchers. Locks of dead processes are taken over.
func lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create %s: %w", dir, err)
	}
	name := filepath.Join(dir, ".lock")
	deadline := time.Now().Add(5 * time.Minute)
	waiting := false
	for {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprint(f, os.Getpid())
			f.Close()
			return func() { os.Remove(name) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		var pid int
		if data, err := os.ReadFile(name); err == nil {
			fmt.Sscan(string(data), &pid)
		}
		if pid <= 0 || pid == os.Getpid() || !platform.ProcessAlive(pid) {
			os.Remove(name)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another installation (process %d) is still running; remove %s if that is not the case", pid, name)
		}
		if !waiting {
			ui.Info("waiting for another instance to finish installing...")
			waiting = true
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (l *launcher) printHelp() {
	fmt.Printf(`%s %s (jrunner %s launcher)

Usage: %s [application arguments] [launcher options]

Launcher options:
  %[5]supdate      check for an update now and install it
  %[5]sreinstall   reinstall from this launcher and look for Java again
  %[5]suninstall   remove the application (your data is kept)
  %[5]sinfo        show installation details
  %[5]sverbose     print diagnostic output
  %[5]shelp        show this help

All other arguments are passed to the application. Extra JVM options can be
given in the environment variable %[6]s.
`, l.cfg.Name, l.cfg.Version, version.Tool, filepath.Base(l.exe), flagPrefix, envOptsName(l.cfg.ID))
}

func (l *launcher) printInfo(installed *config.Config) {
	fmt.Printf("Application:   %s (%s)\n", l.cfg.Name, l.cfg.ID)
	fmt.Printf("This launcher: version %s, build %s\n", l.cfg.Version, buildID(l.cfg))
	if installed != nil {
		fmt.Printf("Installed:     version %s, build %s, %s\n", l.state.Version, l.state.Build, l.state.InstalledAt.Format(time.RFC1123))
	} else {
		fmt.Printf("Installed:     no\n")
	}
	fmt.Printf("Install dir:   %s\n", l.paths.Install)
	fmt.Printf("Data dir:      %s\n", l.paths.Data)
	if rt := l.state.Java; rt != nil {
		fmt.Printf("Java:          %s [%s]\n", rt, rt.Source)
	}
	if c := installed; c != nil && c.Update != nil {
		last := "never"
		if !l.state.LastUpdateCheck.IsZero() {
			last = l.state.LastUpdateCheck.Format(time.RFC1123)
		}
		fmt.Printf("Updates:       %s (last check: %s)\n", c.Update.URL, last)
	}
}
