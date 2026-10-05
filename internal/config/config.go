// Package config defines jrunner.json, the single file that describes how an
// application is packaged, installed, launched and updated.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// FileName is the conventional name of the configuration file.
const FileName = "jrunner.json"

// Config is the application description. Paths (jar, files, icons,
// java.runtime) are relative to the configuration file at build time; in the
// packaged copy they are relative to the installed "app" directory.
type Config struct {
	Name      string   `json:"name"`                // display name, e.g. "Demo App"
	ID        string   `json:"id,omitempty"`        // file-system id, e.g. "demo-app" (derived from name)
	Version   string   `json:"version"`             // application version, compared on install/update
	Publisher string   `json:"publisher,omitempty"` // shown in the Windows "Apps" list
	Jar       string   `json:"jar,omitempty"`       // main (fat) jar, started with "java -jar"
	Files     []string `json:"files,omitempty"`     // extra files/dirs/globs copied into the app dir
	Icons     []string `json:"icons,omitempty"`     // .ico (Windows), .icns (macOS), .png (Linux)
	WorkDir   string   `json:"workDir,omitempty"`   // working directory (default ${DATA_DIR})
	Java      Java     `json:"java"`
	Browser   *Browser `json:"browser,omitempty"` // open a browser once the app listens (web apps)
	Update    *Update  `json:"update,omitempty"`  // self-update channel
	Install   Install  `json:"install"`
	Build     *Build   `json:"build,omitempty"` // written by "jrunner build"
}

// Java describes the required runtime and how to start the application.
type Java struct {
	MinVersion      int               `json:"minVersion"`                // minimum feature release, e.g. 17
	MaxVersion      int               `json:"maxVersion,omitempty"`      // maximum feature release (0 = any)
	Image           string            `json:"image,omitempty"`           // "jre" (default) or "jdk" (requires javac)
	DownloadVersion int               `json:"downloadVersion,omitempty"` // release to download if missing (default minVersion)
	Download        []Source          `json:"download,omitempty"`        // where to download a runtime from, in order
	Runtime         string            `json:"runtime,omitempty"`         // build time: jlink image to bundle; may use {os},{arch},{platform}
	Options         []string          `json:"options,omitempty"`         // JVM options, e.g. "-Xmx512m"
	Env             map[string]string `json:"env,omitempty"`             // environment variables for the app, e.g. SPRING_PROFILES_ACTIVE
	Args            []string          `json:"args,omitempty"`            // default application arguments
	MainClass       string            `json:"mainClass,omitempty"`       // start a class instead of "-jar"
	ClassPath       []string          `json:"classPath,omitempty"`       // extra class path entries (with mainClass)
	Module          string            `json:"module,omitempty"`          // modular app: "module/main.Class"
	ModulePath      []string          `json:"modulePath,omitempty"`      // module path entries (with module)
}

// Providers are the built-in Java download sources (in default order) with
// their labels for progress messages.
var Providers = []struct{ Name, Label string }{
	{"adoptium", "Eclipse Temurin (adoptium.net)"},
	{"zulu", "Azul Zulu (azul.com)"},
	{"corretto", "Amazon Corretto (corretto.aws)"},
	{"liberica", "BellSoft Liberica (bell-sw.com)"},
	{"microsoft", "Microsoft Build of OpenJDK (microsoft.com)"},
}

// DefaultSources returns the default download sources: all providers in order.
func DefaultSources() []Source {
	out := make([]Source, len(Providers))
	for i, p := range Providers {
		out[i] = Source{Provider: p.Name}
	}
	return out
}

// IsProvider reports whether name is a built-in provider.
func IsProvider(name string) bool {
	for _, p := range Providers {
		if p.Name == name {
			return true
		}
	}
	return false
}

// Source is a place to download a Java runtime from. In JSON it may be an
// object or a shorthand string: a provider name or a URL template.
type Source struct {
	Provider string `json:"provider,omitempty"` // see Providers
	URL      string `json:"url,omitempty"`      // template with {version} {os} {arch} {image} {ext}
	SHA256   string `json:"sha256,omitempty"`   // hex digest, or URL (template) of a checksum file
}

// UnmarshalJSON accepts the string shorthand.
func (s *Source) UnmarshalJSON(b []byte) error {
	var str string
	if json.Unmarshal(b, &str) == nil {
		*s = Source{}
		if strings.Contains(str, "://") {
			s.URL = str
		} else {
			s.Provider = strings.ToLower(strings.TrimSpace(str))
		}
		return nil
	}
	type plain Source
	return json.Unmarshal(b, (*plain)(s))
}

// Label describes the source for progress messages.
func (s Source) Label() string {
	for _, p := range Providers {
		if p.Name == s.Provider {
			return p.Label
		}
	}
	if u, err := url.Parse(s.URL); err == nil && u.Host != "" {
		return u.Host
	}
	return s.URL
}

// Browser configures opening the application's web UI. With an empty URL the
// launcher detects the address from the application's log output (Spring
// Boot's "Tomcat started on port 8080 (http) with context path '/'" or any
// printed http://localhost... URL).
type Browser struct {
	URL     string `json:"url,omitempty"`     // e.g. "http://localhost:8080/"; empty = detect from the log
	Timeout int    `json:"timeout,omitempty"` // seconds until a "still starting" hint (default 120); waiting continues while the app runs
}

// Update configures the update channel.
type Update struct {
	URL           string `json:"url"`                     // URL of update.json (generated by "jrunner build")
	IntervalHours *int   `json:"intervalHours,omitempty"` // hours between checks (default 24, 0 = every launch)
	Auto          bool   `json:"auto,omitempty"`          // install without asking
}

// Interval returns the time between update checks.
func (u *Update) Interval() time.Duration {
	if u.IntervalHours == nil {
		return 24 * time.Hour
	}
	return time.Duration(*u.IntervalHours) * time.Hour
}

// Install configures the installation.
type Install struct {
	Dir             string `json:"dir,omitempty"`             // override the install directory
	DesktopShortcut bool   `json:"desktopShortcut,omitempty"` // create a desktop shortcut
	MenuShortcut    bool   `json:"menuShortcut,omitempty"`    // Start menu / applications menu entry
	AddToPath       bool   `json:"addToPath,omitempty"`       // make the launcher available on PATH
}

// Build is metadata stamped into the package by the builder.
type Build struct {
	ID             string `json:"id"`                       // content hash, detects rebuilds of a version
	Time           string `json:"time,omitempty"`           // build time (RFC 3339)
	Tool           string `json:"tool,omitempty"`           // jrunner version used
	BundledRuntime bool   `json:"bundledRuntime,omitempty"` // package contains runtime/
}

// Load reads and validates a configuration file strictly (unknown fields are
// errors, catching typos).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data, true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes, completes and validates a configuration. Packaged configs
// are parsed leniently so that newer builders stay compatible.
func Parse(data []byte, strict bool) (*Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if strict && c.Build != nil {
		return nil, errors.New("\"build\" is written by jrunner build and must not be set")
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.ID == "" {
		c.ID = MakeID(c.Name)
	}
	if c.Java.Image == "" {
		c.Java.Image = "jre"
	}
	if c.Java.MinVersion == 0 {
		c.Java.MinVersion = 17
	}
	if len(c.Java.Download) == 0 {
		c.Java.Download = DefaultSources()
	}
	if c.Browser != nil && c.Browser.Timeout <= 0 {
		c.Browser.Timeout = 120
	}
}

// Validate reports all configuration problems at once.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	if strings.TrimSpace(c.Name) == "" {
		add("name is required")
	}
	if strings.TrimSpace(c.Version) == "" {
		add("version is required")
	}
	switch {
	case c.ID == "":
		add("id is required (it cannot be derived from name %q)", c.Name)
	case strings.Trim(c.ID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_") != "":
		add("id %q may only contain letters, digits, '.', '-' and '_'", c.ID)
	case strings.Trim(c.ID, ".") == "":
		add("id %q is not a valid directory name", c.ID) // "." or ".." would escape the programs folder
	}
	if c.Jar == "" && c.Java.MainClass == "" && c.Java.Module == "" {
		add("one of jar, java.mainClass or java.module is required")
	}
	if c.Java.Image != "jre" && c.Java.Image != "jdk" {
		add("java.image must be \"jre\" or \"jdk\"")
	}
	if c.Java.MaxVersion != 0 && c.Java.MaxVersion < c.Java.MinVersion {
		add("java.maxVersion (%d) is lower than java.minVersion (%d)", c.Java.MaxVersion, c.Java.MinVersion)
	}
	if d := c.Java.DownloadVersion; d != 0 && (d < c.Java.MinVersion || (c.Java.MaxVersion != 0 && d > c.Java.MaxVersion)) {
		add("java.downloadVersion (%d) is outside java.minVersion..maxVersion", d)
	}
	for _, s := range c.Java.Download {
		if s.URL == "" && !IsProvider(s.Provider) {
			names := make([]string, len(Providers))
			for i, p := range Providers {
				names[i] = p.Name
			}
			add("java.download: unknown source %q (use %s or a URL)", s.Provider, strings.Join(names, ", "))
		}
	}
	if c.Browser != nil && c.Browser.URL != "" {
		if u, err := url.Parse(c.Browser.URL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			add("browser.url must be an absolute http(s) URL such as http://localhost:8080/, or empty to detect it from the log")
		}
	}
	if c.Update != nil {
		if u, err := url.Parse(c.Update.URL); err != nil || u.Host == "" {
			add("update.url must be an absolute URL of update.json")
		}
		if c.Update.IntervalHours != nil && *c.Update.IntervalHours < 0 {
			add("update.intervalHours must not be negative")
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// DownloadFeature is the Java feature release to download when none is found.
func (c *Config) DownloadFeature() int {
	if c.Java.DownloadVersion > 0 {
		return c.Java.DownloadVersion
	}
	return c.Java.MinVersion
}

// Marshal returns the configuration as indented JSON.
func (c *Config) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// MakeID derives a file-system friendly id from a display name.
func MakeID(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-.")
}
