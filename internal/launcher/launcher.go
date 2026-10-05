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
	"sync"
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
	verbose, update, reinstall, uninstall, info, help, noBrowser bool
}

type launcher struct {
	exe      string
	embedded *pkg.Package
	opts     options
	cfg      *config.Config // effective configuration (installed or embedded)
	paths    paths
	state    *state
	override *jre.Runtime // <ID>_JAVA_HOME runtime for this launch (never saved)
	setup    bool         // something was installed or downloaded during this launch
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
		case "no-browser":
			o.noBrowser = true
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
	l.recoverInterrupted()
	l.state = loadState(l.paths.State)
	installed := loadInstalledConfig(l.paths.App)
	if l.opts.info {
		l.printInfo(installed)
		return 0, nil
	}

	forceCheck := l.opts.update
	if reason := l.installReason(installed); reason != "" {
		ui.Title(l.cfg.Name, l.cfg.Version, reason)
		if l.opts.reinstall {
			l.state.Java = nil // look for Java again
		}
		if l.embedded.HasApp {
			if err := l.install(l.embedded); err != nil {
				return 1, err
			}
		} else {
			// A thin launcher carries only the configuration: fetch the
			// application from the update channel.
			if l.cfg.Update == nil {
				return 1, fmt.Errorf("%s is not installed correctly in %s; please run the original installer again", l.cfg.Name, l.paths.Install)
			}
			if err := l.bootstrap(); err != nil {
				return 1, err
			}
		}
	} else {
		switch c := version.Compare(l.cfg.Version, installed.Version); {
		case c < 0:
			ui.Debug("installed version %s is newer than this launcher's %s, using it", installed.Version, l.cfg.Version)
		case c > 0 && !l.embedded.HasApp:
			// A newer thin launcher: look for the matching package now.
			forceCheck = true
		}
		l.cfg = installed
	}
	l.cleanup()

	if err := l.ensureJava(); err != nil {
		return 1, err
	}
	if forceCheck || l.updateDue() {
		updated, err := l.checkUpdate(forceCheck)
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
// case costs one or two stat calls. <ID>_JAVA_HOME overrides discovery for
// this launch without being remembered.
func (l *launcher) ensureJava() error {
	req := requirement(l.cfg)
	if home := os.Getenv(envJavaHomeName(l.cfg.ID)); home != "" {
		rt, err := jre.Probe(platform.ExpandHome(home))
		if err != nil {
			return fmt.Errorf("%s: %v", envJavaHomeName(l.cfg.ID), err)
		}
		if why := req.Check(rt); why != "" {
			return fmt.Errorf("%s: %s", envJavaHomeName(l.cfg.ID), why)
		}
		rt.Source = "override"
		ui.Debug("using %s from %s", rt, envJavaHomeName(l.cfg.ID))
		l.override = rt
		return nil
	}
	if usable(l.state.Java, req) {
		return nil
	}
	unlock, err := lock(l.paths.Install)
	if err != nil {
		if !fsutil.NotWritable(err) {
			return err
		}
		// A read-only installation (e.g. deployed by an administrator):
		// discovery still works, only a download would fail.
		ui.Debug("no install lock: %v", err)
		unlock = func() {}
	}
	defer unlock()
	// Another launcher may have resolved Java while this one waited.
	if st := loadState(l.paths.State); usable(st.Java, req) {
		l.state.Java = st.Java
		return nil
	}
	rt, err := l.resolveJava(l.cfg, req)
	if err != nil {
		return err
	}
	l.setup = true
	l.state.Java = rt
	if err := l.state.save(l.paths.State); err != nil {
		ui.Warn("cannot save %s (%v); Java will be searched again next time", l.paths.State, err)
	}
	return nil
}

// usable reports whether a remembered runtime can still be used: it exists,
// is not damaged (own runtimes) and satisfies req. "override" entries were
// saved by older versions and are not trusted.
func usable(rt *jre.Runtime, req jre.Requirement) bool {
	switch {
	case rt == nil || rt.Source == "override":
		return false
	case rt.Source == "system":
		return fsutil.IsFile(rt.Java()) && req.Accepts(rt)
	}
	return jre.Intact(rt) && req.Accepts(rt)
}

// javaRuntime is the runtime used for this launch.
func (l *launcher) javaRuntime() *jre.Runtime {
	if l.override != nil {
		return l.override
	}
	return l.state.Java
}

// resolveJava finds or downloads a runtime for cfg; the caller holds the
// install lock.
func (l *launcher) resolveJava(cfg *config.Config, req jre.Requirement) (*jre.Runtime, error) {
	if cfg.Build != nil && cfg.Build.BundledRuntime {
		rt, err := jre.Probe(l.paths.Runtime)
		if err == nil && !jre.Intact(rt) {
			err = errors.New("class library missing")
		}
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
		ui.Success("using %s", rt)
		return rt, nil
	}
	feature := cfg.DownloadFeature()
	if !req.AcceptsFeature(feature) {
		feature = req.Min
	}
	ui.Info("no suitable Java installation found on this computer")
	ui.Step("Downloading Java %d (%s, %s)", feature, cfg.Java.Image, platform.Key())
	rt, err := jre.Download(context.Background(), cfg.Java.Download, feature, req, l.paths.Runtime)
	if err != nil {
		return nil, err
	}
	ui.Success("installed %s", rt)
	return rt, nil
}

func requirement(c *config.Config) jre.Requirement {
	return jre.Requirement{Min: c.Java.MinVersion, Max: c.Java.MaxVersion, JDK: c.Java.Image == "jdk"}
}

// keepDownloads is how long partial (or complete but not yet installed)
// downloads are kept so that a later launch can resume or reuse them.
const keepDownloads = 72 * time.Hour

// lockFile is the install lock; see lock.
func (l *launcher) lockFile() string { return filepath.Join(l.paths.Install, ".lock") }

// recoverInterrupted moves a directory back whose replacement was
// interrupted between its two renames (only the ".old" copy exists), so that
// a crash or power loss at that moment does not force a reinstallation.
func (l *launcher) recoverInterrupted() {
	if _, held := lockHolder(l.lockFile()); held {
		return
	}
	for _, d := range []string{l.paths.App, l.paths.Runtime} {
		if !fsutil.Exists(d) && fsutil.IsDir(d+".old") {
			ui.Debug("restoring %s from an interrupted update", d)
			_ = fsutil.Rename(d+".old", d)
		}
	}
}

// cleanup removes leftovers of replaced files (Windows keeps running
// executables and open jars locked until the process exits), of interrupted
// extractions and of downloads older than keepDownloads. It is skipped while
// another launcher holds the install lock: the leftovers may be its work in
// progress.
func (l *launcher) cleanup() {
	if _, held := lockHolder(l.lockFile()); held {
		return
	}
	for _, p := range []string{
		l.paths.Launcher + ".old", l.paths.Launcher + ".tmp", l.paths.App + ".old", l.paths.App + ".new",
		l.paths.Runtime + ".old", l.paths.Runtime + ".new",
	} {
		if fsutil.Exists(p) {
			_ = os.RemoveAll(p)
		}
	}
	removeOlder := func(age time.Duration, files ...string) {
		for _, p := range files {
			if st, err := os.Lstat(p); err == nil && time.Since(st.ModTime()) > age {
				_ = os.Remove(p)
			}
		}
	}
	for _, f := range []string{l.paths.Runtime + ".download", filepath.Join(l.paths.Install, updateFile)} {
		removeOlder(keepDownloads, f, f+".part", f+".part.meta")
	}
	tmps, _ := filepath.Glob(l.paths.State + ".*.tmp") // from an interrupted state save
	removeOlder(time.Minute, tmps...)
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

const (
	lockRefresh = 10 * time.Second // the holder touches the lock file this often
	lockStale   = time.Minute      // a lock not touched for this long is abandoned
)

// lock serializes installation, updates and runtime downloads between
// concurrently started launchers. The holder keeps the lock file fresh, so
// a waiting launcher waits as long as needed (a slow download may take many
// minutes) and takes over the lock of a crashed or hung process.
func lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create %s: %w", dir, err)
	}
	name := filepath.Join(dir, ".lock")
	waiting, denied := false, 0
	for {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprint(f, os.Getpid())
			f.Close()
			stop := make(chan struct{})
			go func() {
				t := time.NewTicker(lockRefresh)
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case now := <-t.C:
						_ = os.Chtimes(name, now, now)
					}
				}
			}()
			var once sync.Once
			return func() { once.Do(func() { close(stop); os.Remove(name) }) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			// Windows denies access to a file whose deletion is pending
			// (the previous holder just released it): try again shortly.
			if runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission) && fsutil.Exists(name) && denied < 20 {
				denied++
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return nil, err
		}
		pid, held := lockHolder(name)
		if !held {
			os.Remove(name)
			continue
		}
		if !waiting {
			ui.Info("waiting for another instance (process %d) to finish installing...", pid)
			waiting = true
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// lockHolder reports the process owning the lock file name and whether the
// lock is held by another live process. A lock that has not been refreshed
// for lockStale is abandoned; one without a pid is still being written by
// its owner unless it is older than a few seconds.
func lockHolder(name string) (pid int, held bool) {
	st, err := os.Stat(name)
	if err != nil {
		return 0, false
	}
	age := time.Since(st.ModTime())
	if age > lockStale {
		return 0, false
	}
	if data, err := os.ReadFile(name); err == nil {
		fmt.Sscan(string(data), &pid)
	}
	if pid <= 0 {
		return 0, age < 10*time.Second
	}
	return pid, pid != os.Getpid() && platform.ProcessAlive(pid)
}

func (l *launcher) printHelp() {
	fmt.Printf(`%s %s (jrunner %s launcher)

Usage: %s [application arguments] [launcher options]

Launcher options:
  %[5]supdate      check for an update now and install it
  %[5]sreinstall   reinstall from this launcher and look for Java again
  %[5]suninstall   remove the application (your data is kept)
  %[5]sinfo        show installation details
  %[5]sno-browser  do not open the browser (web applications)
  %[5]sverbose     print diagnostic output
  %[5]shelp        show this help

All other arguments are passed to the application. Extra JVM options can be
put into %[6]s (one per line) or the environment variable %[7]s.
%[8]s selects the Java installation to use for this launch.
`, l.cfg.Name, l.cfg.Version, version.Tool, filepath.Base(l.exe), flagPrefix, l.vmOptionsFile(), envOptsName(l.cfg.ID), envJavaHomeName(l.cfg.ID))
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
	if opts := readVMOptions(l.vmOptionsFile()); len(opts) > 0 {
		fmt.Printf("JVM options:   %s (%s)\n", strings.Join(opts, " "), l.vmOptionsFile())
	}
	if c := installed; c != nil && c.Update != nil {
		last := "never"
		if !l.state.LastUpdateCheck.IsZero() {
			last = l.state.LastUpdateCheck.Format(time.RFC1123)
		}
		fmt.Printf("Updates:       %s (last check: %s)\n", c.Update.URL, last)
	}
}
