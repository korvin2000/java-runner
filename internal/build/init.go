package build

import (
	"archive/zip"
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/korvin2000/java-runner/internal/config"
	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/ui"
	"github.com/korvin2000/java-runner/internal/version"
)

// Init writes a starter jrunner.json, pre-filled from the jar's manifest
// (name, version, Java release) and, for Spring Boot, the server port.
func Init(jar string, force bool) error {
	if fsutil.Exists(config.FileName) && !force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", config.FileName)
	}
	if jar == "" {
		jar = guessJar()
		if jar == "" {
			return fmt.Errorf("no jar found in target/ or build/libs/; pass it explicitly: jrunner init path/to/app.jar")
		}
	}
	info, err := inspectJar(jar)
	if err != nil {
		return err
	}
	cfg := &config.Config{
		Name:    info.name,
		ID:      config.MakeID(info.name),
		Version: info.version,
		Jar:     filepath.ToSlash(jar),
		Java: config.Java{
			MinVersion: info.java,
			Options:    []string{"-Xmx512m"},
			Download:   []config.Source{{Provider: "adoptium"}, {Provider: "zulu"}},
		},
		Install: config.Install{DesktopShortcut: true, MenuShortcut: true},
	}
	if info.springBoot && info.port > 0 {
		cfg.Browser = &config.Browser{URL: fmt.Sprintf("http://localhost:%d%s/", info.port, strings.TrimSuffix(info.contextPath, "/"))}
	}
	data, err := cfg.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(config.FileName, data, 0o644); err != nil {
		return err
	}
	ui.Step("Created %s for %s %s", config.FileName, cfg.Name, cfg.Version)
	ui.Info("jar:   %s", jar)
	ui.Info("java:  %d+", cfg.Java.MinVersion)
	if cfg.Browser != nil {
		ui.Info("web:   Spring Boot app, opens %s", cfg.Browser.URL)
	}
	ui.Info("Review the file, then run: jrunner build --target all")
	ui.Info("Updates: add \"update\": {\"url\": \"https://example.com/myapp/update.json\"}")
	return nil
}

type jarInfo struct {
	name, version, contextPath string
	java, port                 int
	springBoot                 bool
}

func inspectJar(jar string) (*jarInfo, error) {
	zr, err := zip.OpenReader(jar)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", jar, err)
	}
	defer zr.Close()
	mf := manifest(readEntry(&zr.Reader, "META-INF/MANIFEST.MF"))
	info := &jarInfo{name: mf["Implementation-Title"], version: mf["Implementation-Version"], java: 17}
	if info.name == "" {
		info.name = regexp.MustCompile(`-\d.*$`).ReplaceAllString(strings.TrimSuffix(filepath.Base(jar), ".jar"), "")
	}
	if info.version == "" {
		info.version = "1.0.0"
	}
	if spec := version.JavaFeature(mf["Build-Jdk-Spec"]); spec > 0 {
		info.java = spec
	} else if spec := version.JavaFeature(mf["Build-Jdk"]); spec > 0 {
		info.java = spec
	}
	if boot := mf["Spring-Boot-Version"]; boot != "" || mf["Start-Class"] != "" {
		info.springBoot = true
		if strings.HasPrefix(boot, "3.") && info.java < 17 {
			info.java = 17
		}
		props := map[string]string{}
		for _, name := range []string{"application.properties", "application.yml", "application.yaml"} {
			text := readEntry(&zr.Reader, "BOOT-INF/classes/"+name)
			if strings.HasSuffix(name, ".properties") {
				parseProperties(text, props)
			} else {
				parseYAML(text, props)
			}
		}
		info.port = 8080
		if p, err := strconv.Atoi(propDefault(props["server.port"])); err == nil {
			info.port = p // 0 means a random port: no browser
		}
		info.contextPath = propDefault(props["server.servlet.context-path"])
	}
	return info, nil
}

func guessJar() string {
	var best string
	var bestSize int64
	for _, pattern := range []string{"target/*.jar", "build/libs/*.jar", "*.jar"} {
		m, _ := filepath.Glob(pattern)
		sort.Strings(m)
		for _, f := range m {
			if regexp.MustCompile(`-(sources|javadoc|plain|tests)\.jar$`).MatchString(f) {
				continue
			}
			if st, err := os.Stat(f); err == nil && st.Size() > bestSize {
				best, bestSize = f, st.Size()
			}
		}
		if best != "" {
			return best
		}
	}
	return ""
}

func readEntry(zr *zip.Reader, name string) string {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return ""
			}
			defer rc.Close()
			b, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
			return string(b)
		}
	}
	return ""
}

// manifest parses META-INF/MANIFEST.MF (with continuation lines).
func manifest(text string) map[string]string {
	m := map[string]string{}
	var key string
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.HasPrefix(line, " ") && key != "" {
			m[key] += line[1:]
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			key = strings.TrimSpace(k)
			m[key] = strings.TrimSpace(v)
		}
	}
	return m
}

func parseProperties(text string, props map[string]string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		if i := strings.IndexAny(line, "=:"); i > 0 {
			props[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
}

// parseYAML flattens simple nested "key: value" YAML into dotted keys; this
// is enough to find server.port in typical Spring Boot configurations.
func parseYAML(text string, props map[string]string) {
	type level struct {
		indent int
		key    string
	}
	var stack []level
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r ")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		if trimmed == "---" {
			break // only the first document (default profile)
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		key := strings.TrimSpace(k)
		if v = strings.Trim(strings.TrimSpace(v), `"'`); v == "" {
			stack = append(stack, level{indent, key})
			continue
		}
		var parts []string
		for _, l := range stack {
			parts = append(parts, l.key)
		}
		props[strings.Join(append(parts, key), ".")] = v
	}
}

// propDefault resolves "${PORT:8080}" to its default value.
func propDefault(v string) string {
	if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
		if _, def, ok := strings.Cut(v[2:len(v)-1], ":"); ok {
			return def
		}
		return ""
	}
	return v
}
