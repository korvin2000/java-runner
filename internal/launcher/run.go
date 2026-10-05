package launcher

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/platform"
	"github.com/korvin2000/java-runner/internal/ui"
)

// launch starts the application. Plain apps replace the launcher process
// (Unix) or run as its child (Windows); web apps run as a child so that the
// launcher can open the browser once the port accepts connections.
func (l *launcher) launch(appArgs []string) (int, error) {
	cmd, err := l.command(appArgs)
	if err != nil {
		return 1, err
	}
	if l.setup {
		ui.Step("Starting %s", l.cfg.Name)
	}
	ui.Debug("working directory: %s", cmd.Dir)
	ui.Debug("command: %s", strings.Join(cmd.Args, " "))
	if l.cfg.Browser == nil || l.opts.noBrowser {
		return execJava(cmd)
	}
	if l.cfg.Browser.URL == "" {
		return l.launchWebDetect(cmd)
	}
	return l.launchWeb(cmd)
}

func (l *launcher) command(appArgs []string) (*exec.Cmd, error) {
	c := l.cfg
	vars := map[string]string{
		"APP_DIR":     l.paths.App,
		"INSTALL_DIR": l.paths.Install,
		"DATA_DIR":    l.paths.Data,
		"VERSION":     c.Version,
		"ID":          c.ID,
	}
	if home, err := os.UserHomeDir(); err == nil {
		vars["HOME"] = home
	}
	if cwd, err := os.Getwd(); err == nil {
		vars["CWD"] = cwd
	}
	inApp := func(p string) string { return filepath.Join(l.paths.App, filepath.FromSlash(p)) }

	args := expandAll(c.Java.Options, vars)
	args = append(args, expandAll(readVMOptions(l.vmOptionsFile()), vars)...)
	if env := os.Getenv(envOptsName(c.ID)); env != "" {
		args = append(args, strings.Fields(env)...)
	}
	switch {
	case c.Java.Module != "":
		mp := c.Java.ModulePath
		if len(mp) == 0 && c.Jar != "" {
			mp = []string{c.Jar}
		}
		if len(mp) > 0 {
			args = append(args, "-p", joinPaths(mp, inApp))
		}
		args = append(args, "-m", c.Java.Module)
	case c.Java.MainClass != "":
		cp := c.Java.ClassPath
		if c.Jar != "" {
			cp = append([]string{c.Jar}, cp...)
		}
		if len(cp) > 0 {
			args = append(args, "-cp", joinPaths(cp, inApp))
		}
		args = append(args, c.Java.MainClass)
	default:
		jar := inApp(c.Jar)
		if !fsutil.IsFile(jar) {
			return nil, fmt.Errorf("application jar %s is missing; run with %sreinstall", jar, flagPrefix)
		}
		args = append(args, "-jar", jar)
	}
	args = append(args, expandAll(c.Java.Args, vars)...)
	args = append(args, appArgs...)

	dir := l.paths.Data
	if c.WorkDir != "" {
		dir = platform.ExpandHome(expand(c.WorkDir, vars))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create working directory: %w", err)
	}
	cmd := exec.Command(l.javaRuntime().Java(), args...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	for k, v := range c.Java.Env {
		cmd.Env = setEnv(cmd.Env, k, expand(v, vars))
	}
	return cmd, nil
}

// setEnv sets key in env, replacing an inherited value. Duplicates must be
// avoided: the JVM uses the first occurrence, so an appended value would lose
// against the inherited one when Java is exec'ed directly.
func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !envKeyIs(kv, key) {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}

// hasEnv reports whether env contains key.
func hasEnv(env []string, key string) bool {
	for _, kv := range env {
		if envKeyIs(kv, key) {
			return true
		}
	}
	return false
}

func envKeyIs(kv, key string) bool {
	k, _, _ := strings.Cut(kv, "=")
	if runtime.GOOS == "windows" {
		return strings.EqualFold(k, key)
	}
	return k == key
}

// vmOptionsFile is a user-editable file with one JVM option per line, kept
// next to the launcher (it survives updates, uninstall removes it).
func (l *launcher) vmOptionsFile() string {
	return filepath.Join(l.paths.Install, l.cfg.ID+".vmoptions")
}

// readVMOptions reads a .vmoptions file: one option per line, blank lines and
// lines starting with '#' are ignored.
func readVMOptions(name string) []string {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil
	}
	var opts []string
	text := strings.TrimPrefix(string(data), "\ufeff") // byte order mark written by some Windows editors
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			opts = append(opts, line)
		}
	}
	return opts
}

func (l *launcher) launchWeb(cmd *exec.Cmd) (int, error) {
	target := l.cfg.Browser.URL
	addr, err := hostPort(target)
	if err != nil {
		return 1, err
	}
	if portOpen(addr) {
		// Typically a second double-click while the app is running.
		ui.Step("Port %s is already in use, %s seems to be running already", addr, l.cfg.Name)
		openBrowser(target)
		return 0, nil
	}
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("starting Java: %w", err)
	}
	stop := handleSignals(cmd.Process)
	defer stop()

	// Large applications (e.g. Spring Boot with many beans) may need minutes:
	// keep polling as long as the JVM runs; the timeout only adds a hint.
	exited := make(chan struct{})
	go func() {
		timeout := time.Duration(l.cfg.Browser.Timeout) * time.Second
		start, interval, hinted := time.Now(), 300*time.Millisecond, false
		for {
			select {
			case <-exited:
				return
			case <-time.After(interval):
			}
			if portOpen(addr) {
				ui.Step("%s is ready at %s", l.cfg.Name, target)
				openBrowser(target)
				return
			}
			if !hinted && time.Since(start) >= timeout {
				hinted, interval = true, time.Second
				ui.Info("%s is still starting (port %s not open after %s); the browser opens as soon as it is ready", l.cfg.Name, addr, ui.Duration(timeout))
			}
		}
	}()
	err = cmd.Wait()
	close(exited)
	return exitCode(err)
}

// launchWebDetect runs the application with its output watched for the URL
// it listens on (see detect.go); once the port accepts connections the
// browser is opened.
func (l *launcher) launchWebDetect(cmd *exec.Cmd) (int, error) {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	cmd.Stdout, cmd.Stderr = outW, errW
	// The app writes to a pipe now and would turn its log colors off; tell
	// Spring Boot that the output still ends up on a color terminal.
	if ui.Colors() && platform.IsTerminal(os.Stdout) && !hasEnv(cmd.Env, "SPRING_OUTPUT_ANSI_ENABLED") {
		cmd.Env = append(cmd.Env, "SPRING_OUTPUT_ANSI_ENABLED=ALWAYS")
	}
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("starting Java: %w", err)
	}
	stop := handleSignals(cmd.Process)
	defer stop()

	found := make(chan string, 2)
	report := func(u string) {
		select {
		case found <- u:
		default:
		}
	}
	var lastOutput atomic.Int64 // UnixNano of the latest output: the app is making progress
	lastOutput.Store(time.Now().UnixNano())
	var wg sync.WaitGroup
	wg.Add(2)
	go watchOutput(outR, os.Stdout, report, &lastOutput, &wg)
	go watchOutput(errR, os.Stderr, report, &lastOutput, &wg)

	// The URL usually appears at the very end of the startup log, which can
	// take minutes. Wait as long as the JVM runs; the timeout counts from the
	// latest output line and only adds a hint, a URL seen later still counts.
	exited := make(chan struct{})
	timeout := time.Duration(l.cfg.Browser.Timeout) * time.Second
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		hinted := false
		for {
			select {
			case <-exited:
				return
			case <-tick.C:
				if !hinted && time.Since(time.Unix(0, lastOutput.Load())) >= timeout {
					hinted = true
					ui.Warn("no local URL in the output of %s yet (no output for %s); if it does not announce one, set browser.url in the configuration", l.cfg.Name, ui.Duration(timeout))
				}
			case target := <-found:
				if addr, err := hostPort(target); err == nil {
					deadline := time.Now().Add(15 * time.Second)
					for !portOpen(addr) && time.Now().Before(deadline) {
						select {
						case <-exited:
							return
						case <-time.After(200 * time.Millisecond):
						}
					}
				}
				ui.Step("%s is ready at %s", l.cfg.Name, target)
				openBrowser(target)
				return
			}
		}
	}()
	err := cmd.Wait()
	outW.Close()
	errW.Close()
	wg.Wait()
	close(exited)
	return exitCode(err)
}

func openBrowser(u string) {
	if !platform.HasDisplay() {
		ui.Info("open %s in your browser", u)
		return
	}
	if err := platform.OpenURL(u); err != nil {
		ui.Warn("cannot open a browser (%v); open %s manually", err, u)
	}
}

func hostPort(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	host, port := u.Hostname(), u.Port()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(host, port), nil
}

func portOpen(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func exitCode(err error) (int, error) {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		if code := ee.ExitCode(); code >= 0 {
			return code, nil
		}
		return 1, nil // killed by a signal
	}
	return 1, err
}

var varPattern = regexp.MustCompile(`\$\{([A-Za-z0-9_.]+)\}`)

// expand replaces ${NAME} with launcher variables (APP_DIR, INSTALL_DIR,
// DATA_DIR, CWD, HOME, VERSION, ID) or environment variables.
func expand(s string, vars map[string]string) string {
	return varPattern.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		if v, ok := vars[name]; ok {
			return v
		}
		return os.Getenv(name)
	})
}

func expandAll(list []string, vars map[string]string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, expand(s, vars))
	}
	return out
}

func joinPaths(list []string, resolve func(string) string) string {
	parts := make([]string, len(list))
	for i, p := range list {
		parts[i] = resolve(p)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// envOptsName is the environment variable with extra JVM options, e.g.
// DEMO_APP_JAVA_OPTS for id "demo-app".
func envOptsName(id string) string { return envPrefix(id) + "_JAVA_OPTS" }

// envJavaHomeName is the environment variable that overrides the Java
// installation, e.g. DEMO_APP_JAVA_HOME.
func envJavaHomeName(id string) string { return envPrefix(id) + "_JAVA_HOME" }

func envPrefix(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, id)
}
