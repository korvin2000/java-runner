package build

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/korvin2000/java-runner/internal/archive"
	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/jre"
	"github.com/korvin2000/java-runner/internal/ui"
)

// JlinkOptions for Jlink.
type JlinkOptions struct {
	Jar        string   // application jar (for module detection)
	Out        string   // output directory
	JDK        string   // JDK home (default JAVA_HOME / java on PATH)
	JMods      string   // jmods directory of a target-platform JDK (cross-platform images)
	Modules    []string // explicit module list (skips detection)
	AddModules []string // modules added to the detected ones
}

// springBootModules is used when jdeps cannot analyse a Spring Boot jar.
var springBootModules = []string{"java.base", "java.compiler", "java.desktop", "java.instrument", "java.management",
	"java.naming", "java.net.http", "java.prefs", "java.rmi", "java.scripting", "java.security.jgss",
	"java.sql", "jdk.jfr", "jdk.management", "jdk.unsupported"}

// Jlink creates a minimal Java runtime image for the application with jdeps
// and jlink. Bundle it with "java.runtime" in jrunner.json.
func Jlink(o JlinkOptions) error {
	jdk, err := findJDK(o.JDK)
	if err != nil {
		return err
	}
	ui.Step("Creating a Java runtime image with %s", jdk)
	available := listModules(jdk, o.JMods)

	mods := o.Modules
	if len(mods) == 0 {
		if o.Jar == "" {
			return fmt.Errorf("pass --jar <app.jar> to detect the required modules, or --modules")
		}
		if mods, err = detectModules(jdk, o.Jar); err != nil {
			return err
		}
	}
	mods = append(mods, o.AddModules...)
	// Needed at runtime but invisible to static analysis: elliptic-curve TLS.
	if available["jdk.crypto.ec"] {
		mods = append(mods, "jdk.crypto.ec")
	}
	mods = dedupe(mods, available)
	ui.Info("modules: %s", strings.Join(mods, ","))

	if fsutil.Exists(o.Out) {
		if !fsutil.IsFile(filepath.Join(o.Out, "release")) {
			return fmt.Errorf("%s exists and is not a runtime image; choose another --out", o.Out)
		}
		if err := os.RemoveAll(o.Out); err != nil {
			return err
		}
	}
	compress := "zip-6"
	if jdk.Feature() < 21 {
		compress = "2"
	}
	args := []string{}
	if o.JMods != "" {
		args = append(args, "--module-path", o.JMods)
	}
	args = append(args, "--add-modules", strings.Join(mods, ","), "--strip-debug", "--no-man-pages",
		"--no-header-files", "--compress="+compress, "--output", o.Out)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool(jdk, "jlink"), args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("jlink failed: %w", err)
	}
	ui.Step("Runtime image created: %s (%s)", o.Out, ui.Size(fsutil.DirSize(o.Out)))
	ui.Info("bundle it by adding to jrunner.json:  \"java\": { \"runtime\": %q }", filepath.ToSlash(o.Out))
	if o.JMods == "" {
		ui.Info("note: the image is for %s/%s only; use --jmods <target JDK>/jmods for other platforms", runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

func findJDK(home string) (*jre.Runtime, error) {
	var cands []string
	if home != "" {
		cands = append(cands, home)
	} else {
		cands = append(cands, os.Getenv("JAVA_HOME"))
		if p, err := exec.LookPath("jlink"); err == nil {
			if real, err := filepath.EvalSymlinks(p); err == nil {
				cands = append(cands, filepath.Dir(filepath.Dir(real)))
			}
		}
	}
	for _, h := range cands {
		if h == "" {
			continue
		}
		if rt, err := jre.Probe(h); err == nil && fsutil.IsFile(tool(rt, "jlink")) && fsutil.IsFile(tool(rt, "jdeps")) {
			return rt, nil
		}
	}
	return nil, fmt.Errorf("a JDK (with jlink and jdeps) is required: pass --jdk <path> or set JAVA_HOME")
}

func tool(rt *jre.Runtime, name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(rt.Home, "bin", name)
}

// detectModules runs jdeps on the jar. Spring Boot jars are unpacked first
// because jdeps does not look into nested jars.
func detectModules(jdk *jre.Runtime, jar string) ([]string, error) {
	ui.Info("analysing %s with jdeps", jar)
	tmp, err := os.MkdirTemp("", "jrunner-jdeps-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	zr, err := zip.OpenReader(jar)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	springBoot := false
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "BOOT-INF/") {
			springBoot = true
			break
		}
	}
	targets := []string{jar}
	if springBoot {
		classes, lib := filepath.Join(tmp, "classes"), filepath.Join(tmp, "lib")
		if err := archive.ExtractZip(&zr.Reader, "BOOT-INF/classes/", classes); err != nil {
			return nil, err
		}
		if err := archive.ExtractZip(&zr.Reader, "BOOT-INF/lib/", lib); err != nil {
			return nil, err
		}
		jars, _ := filepath.Glob(filepath.Join(lib, "*.jar"))
		targets = append([]string{classes}, jars...)
	}

	var argfile strings.Builder
	for _, a := range []string{"--ignore-missing-deps", "-q", "--multi-release", strconv.Itoa(jdk.Feature()), "--print-module-deps"} {
		argfile.WriteString(a + "\n")
	}
	for _, t := range targets {
		argfile.WriteString(strconv.Quote(filepath.ToSlash(t)) + "\n")
	}
	argPath := filepath.Join(tmp, "jdeps.args")
	if err := os.WriteFile(argPath, []byte(argfile.String()), 0o644); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, tool(jdk, "jdeps"), "@"+argPath)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String() + "\n" + stdout.String())
		if springBoot {
			ui.Warn("jdeps could not analyse the jar (%v), using a module set that fits typical Spring Boot apps", err)
			ui.Debug("%s", msg)
			return springBootModules, nil
		}
		return nil, fmt.Errorf("jdeps failed: %v\n%s\nPass the modules explicitly with --modules", err, msg)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		return []string{"java.base"}, nil
	}
	return strings.Split(last, ","), nil
}

// listModules returns the modules the JDK (or jmods dir) provides.
func listModules(jdk *jre.Runtime, jmods string) map[string]bool {
	m := map[string]bool{}
	if jmods != "" {
		files, _ := filepath.Glob(filepath.Join(jmods, "*.jmod"))
		for _, f := range files {
			m[strings.TrimSuffix(filepath.Base(f), ".jmod")] = true
		}
		return m
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, jdk.Java(), "--list-modules").Output()
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(out), "\n") {
		if name, _, _ := strings.Cut(strings.TrimSpace(line), "@"); name != "" {
			m[name] = true
		}
	}
	return m
}

func dedupe(mods []string, available map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range mods {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		if len(available) > 0 && !available[m] {
			ui.Warn("module %s is not available in this JDK, skipping it", m)
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
