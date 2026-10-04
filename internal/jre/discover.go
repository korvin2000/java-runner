// Package jre finds installed Java runtimes, checks their version and
// architecture, and downloads one when nothing suitable is installed.
package jre

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/ui"
	"github.com/korvin2000/java-runner/internal/version"
)

// Runtime is a usable Java installation.
type Runtime struct {
	Home    string `json:"home"`
	Version string `json:"version"`
	Vendor  string `json:"vendor,omitempty"`
	Arch    string `json:"arch,omitempty"`
	Source  string `json:"source,omitempty"` // system, private, downloaded or bundled
}

// Java returns the path of the java executable.
func (r *Runtime) Java() string { return filepath.Join(r.Home, "bin", exe("java")) }

// Feature returns the feature release, e.g. 17.
func (r *Runtime) Feature() int { return version.JavaFeature(r.Version) }

// IsJDK reports whether the runtime includes the compiler.
func (r *Runtime) IsJDK() bool { return fsutil.IsFile(filepath.Join(r.Home, "bin", exe("javac"))) }

func (r *Runtime) String() string {
	s := "Java " + r.Version
	if r.Vendor != "" {
		s += " (" + r.Vendor + ")"
	}
	return s + " at " + r.Home
}

// Requirement describes acceptable runtimes.
type Requirement struct {
	Min, Max int  // feature releases; Max 0 = no upper bound
	JDK      bool // a full JDK is required
}

func (q Requirement) String() string {
	s := fmt.Sprintf("%d+", q.Min)
	if q.Max == q.Min {
		s = fmt.Sprint(q.Min)
	} else if q.Max > 0 {
		s = fmt.Sprintf("%d-%d", q.Min, q.Max)
	}
	if q.JDK {
		s += " (JDK)"
	}
	return s
}

// AcceptsFeature reports whether a feature release is within range.
func (q Requirement) AcceptsFeature(f int) bool {
	return f >= q.Min && (q.Max == 0 || f <= q.Max)
}

// Check returns why r is not acceptable, or "" if it is.
func (q Requirement) Check(r *Runtime) string {
	switch {
	case !q.AcceptsFeature(r.Feature()):
		return fmt.Sprintf("version %s is outside %s", r.Version, q)
	case r.Arch != "" && normArch(r.Arch) != runtime.GOARCH:
		return fmt.Sprintf("architecture %s does not match this %s system", r.Arch, runtime.GOARCH)
	case q.JDK && !r.IsJDK():
		return "not a JDK"
	}
	return ""
}

// Accepts reports whether r satisfies the requirement.
func (q Requirement) Accepts(r *Runtime) bool { return q.Check(r) == "" }

// Find returns the best installed runtime satisfying req, or nil. The
// preferred homes (the app's private runtime) are tried first, then
// JAVA_HOME, then java on PATH, then well-known install locations (newest
// version first).
func Find(req Requirement, preferred ...string) *Runtime {
	seen := map[string]bool{}
	try := func(home, source string) *Runtime {
		if home == "" {
			return nil
		}
		if real, err := filepath.EvalSymlinks(home); err == nil {
			home = real
		}
		if seen[home] {
			return nil
		}
		seen[home] = true
		r, err := Probe(home)
		if err != nil {
			return nil
		}
		r.Source = source
		if why := req.Check(r); why != "" {
			ui.Debug("skipping %s: %s", r, why)
			return nil
		}
		return r
	}
	for _, h := range preferred {
		if r := try(h, "private"); r != nil {
			return r
		}
	}
	if r := try(os.Getenv("JAVA_HOME"), "system"); r != nil {
		return r
	}
	if p, err := exec.LookPath(exe("java")); err == nil {
		if real, err := filepath.EvalSymlinks(p); err == nil && !(runtime.GOOS == "darwin" && real == "/usr/bin/java") {
			// /usr/bin/java on macOS is a stub that may prompt to install Java.
			home := filepath.Dir(filepath.Dir(real))
			if r := try(home, "system"); r != nil {
				return r
			}
			// Not a Java home (e.g. Oracle's javapath shim on Windows): ask java itself.
			if !fsutil.IsFile(filepath.Join(home, "bin", exe("java"))) {
				if rt, err := probeJava(real); err == nil {
					if r := try(rt.Home, "system"); r != nil {
						return r
					}
				}
			}
		}
	}
	var found []*Runtime
	for _, h := range wellKnownHomes() {
		if r := try(h, "system"); r != nil {
			found = append(found, r)
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return version.Compare(found[i].Version, found[j].Version) > 0 })
	if len(found) > 0 {
		return found[0]
	}
	return nil
}

// Probe inspects the Java installation at home. It reads the "release" file
// (fast); only if that is missing it runs java.
func Probe(home string) (*Runtime, error) {
	if !fsutil.IsFile(filepath.Join(home, "bin", exe("java"))) {
		return nil, fmt.Errorf("%s: no bin/%s", home, exe("java"))
	}
	if rel := ReadRelease(home); rel["JAVA_VERSION"] != "" {
		return &Runtime{Home: home, Version: rel["JAVA_VERSION"], Vendor: rel["IMPLEMENTOR"], Arch: rel["OS_ARCH"]}, nil
	}
	return ProbeExec(home)
}

// ProbeExec runs java to determine its version and architecture. This also
// proves that the runtime actually works on this machine.
func ProbeExec(home string) (*Runtime, error) {
	r, err := probeJava(filepath.Join(home, "bin", exe("java")))
	if err != nil {
		return nil, err
	}
	r.Home = home
	return r, nil
}

// probeJava runs a java executable and reads version, vendor, architecture
// and home from its system properties.
func probeJava(java string) (*Runtime, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, java, "-XshowSettings:properties", "-version")
	cmd.Env = append(os.Environ(), "JAVA_TOOL_OPTIONS=", "_JAVA_OPTIONS=") // keep the output clean
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s does not run: %v %s", java, err, firstLine(out))
	}
	r := &Runtime{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " = ")
		if !ok {
			continue
		}
		switch k {
		case "java.version":
			r.Version = v
		case "java.vendor":
			r.Vendor = v
		case "os.arch":
			r.Arch = v
		case "java.home":
			r.Home = v
		}
	}
	if r.Version == "" || r.Home == "" {
		return nil, fmt.Errorf("%s: cannot determine Java version: %s", java, firstLine(out))
	}
	return r, nil
}

// ReadRelease parses the KEY="value" lines of a runtime's release file.
func ReadRelease(home string) map[string]string {
	m := map[string]string{}
	data, err := os.ReadFile(filepath.Join(home, "release"))
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = strings.Trim(v, `"`)
		}
	}
	return m
}

// FindHome locates the Java home inside an unpacked archive, e.g.
// root/jdk-17.0.12+7-jre or root/zulu.../zulu-17.jre/Contents/Home.
func FindHome(root string) (string, error) {
	var cands []string
	for _, pattern := range []string{"*/*/Contents/Home", "*/Contents/Home", "Contents/Home", "*"} {
		m, _ := filepath.Glob(filepath.Join(root, pattern))
		cands = append(cands, m...)
	}
	cands = append(cands, root)
	for _, c := range cands {
		if fsutil.IsFile(filepath.Join(c, "bin", exe("java"))) {
			return c, nil
		}
	}
	return "", fmt.Errorf("no Java runtime (bin/%s) found in %s", exe("java"), root)
}

// FixPermissions makes the runtime's executables executable; archives created
// on Windows do not carry Unix permissions.
func FixPermissions(home string) {
	if runtime.GOOS == "windows" {
		return
	}
	files, _ := filepath.Glob(filepath.Join(home, "bin", "*"))
	for _, name := range []string{"jspawnhelper", "jexec"} {
		files = append(files, filepath.Join(home, "lib", name))
	}
	for _, f := range files {
		if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() {
			_ = os.Chmod(f, st.Mode().Perm()|0o755)
		}
	}
}

func wellKnownHomes() []string {
	home, _ := os.UserHomeDir()
	var globs []string
	switch runtime.GOOS {
	case "windows":
		roots := []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432"), filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs")}
		vendors := []string{"Java", "Eclipse Adoptium", "Eclipse Foundation", "AdoptOpenJDK", "Zulu", "Microsoft",
			"Amazon Corretto", "BellSoft", "Semeru", "IBM", "RedHat", "OpenJDK", "SapMachine", "GraalVM"}
		for _, root := range roots {
			if root == "" || root == "Programs" {
				continue
			}
			for _, v := range vendors {
				globs = append(globs, filepath.Join(root, v, "*"))
			}
		}
		globs = append(globs, filepath.Join(home, ".jdks", "*"), filepath.Join(home, "scoop", "apps", "*", "current"))
	case "darwin":
		globs = []string{
			"/Library/Java/JavaVirtualMachines/*/Contents/Home",
			filepath.Join(home, "Library/Java/JavaVirtualMachines/*/Contents/Home"),
			"/opt/homebrew/opt/openjdk*/libexec/openjdk.jdk/Contents/Home",
			"/usr/local/opt/openjdk*/libexec/openjdk.jdk/Contents/Home",
			filepath.Join(home, ".sdkman/candidates/java/*"),
			filepath.Join(home, ".jdks/*/Contents/Home"),
			filepath.Join(home, ".jdks/*"),
		}
	default:
		globs = []string{
			"/usr/lib/jvm/*", "/usr/lib64/jvm/*", "/usr/java/*", "/opt/java/*", "/opt/jdk*",
			filepath.Join(home, ".sdkman/candidates/java/*"),
			filepath.Join(home, ".jdks/*"),
		}
	}
	var homes []string
	for _, g := range globs {
		m, _ := filepath.Glob(g)
		homes = append(homes, m...)
	}
	return homes
}

func normArch(a string) string {
	switch strings.ToLower(a) {
	case "x86_64", "amd64", "x64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "x86", "i386", "i486", "i586", "i686":
		return "386"
	}
	return strings.ToLower(a)
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	for _, line := range strings.Split(s, "\n") {
		// Skip noise such as "Picked up JAVA_TOOL_OPTIONS: ...".
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "Picked up") {
			return line
		}
	}
	return s
}
