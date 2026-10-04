// Package cli is the developer command line (the jrunner binary without an
// embedded application).
package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/korvin2000/java-runner/internal/build"
	"github.com/korvin2000/java-runner/internal/config"
	"github.com/korvin2000/java-runner/internal/platform"
	"github.com/korvin2000/java-runner/internal/ui"
	"github.com/korvin2000/java-runner/internal/version"
)

const usage = `jrunner %s - native launcher, installer and updater for Java applications

Usage:
  jrunner init  [--force] [app.jar]       create jrunner.json from a jar (detects Spring Boot)
  jrunner build [options]                 build launchers (and update packages)
      -c, --config FILE    configuration file (default jrunner.json)
      -t, --target LIST    comma-separated targets or "all" (default %s)
                           targets: %s
      -o, --out DIR        output directory (default dist)
      --version VERSION    override the version in the configuration (CI builds)
      --stubs DIR          directory with jrunner-<target> stubs for other platforms
  jrunner jlink [options]                 create a minimal Java runtime with jlink
      --jar FILE           application jar (modules are detected with jdeps)
      --out DIR            output directory (default runtime)
      --jdk DIR            JDK to use (default JAVA_HOME or jlink on PATH)
      --jmods DIR          jmods of a target-platform JDK (cross-platform images)
      --modules LIST       use exactly these modules
      --add-modules LIST   add modules to the detected ones
  jrunner version

A built launcher accepts --jrunner-help for its own options.
`

// Main runs the CLI and returns the exit code.
func Main(args []string) int {
	if len(args) == 0 {
		printUsage()
		ui.PauseIfOwnConsole() // started by double-click
		return 2
	}
	var err error
	switch args[0] {
	case "init":
		err = cmdInit(args[1:])
	case "build":
		err = cmdBuild(args[1:])
	case "jlink":
		err = cmdJlink(args[1:])
	case "version", "--version", "-v":
		fmt.Println("jrunner", version.Tool)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		printUsage()
		return 2
	}
	if err != nil {
		ui.Error(err)
		return 1
	}
	return 0
}

func printUsage() {
	fmt.Printf(usage, version.Tool, platform.Key(), strings.Join(build.AllTargets, ", "))
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing jrunner.json")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	jar := ""
	if len(pos) > 0 {
		jar = pos[0]
	}
	return build.Init(jar, *force)
}

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	var o build.Options
	var targets string
	fs.StringVar(&o.Config, "config", config.FileName, "configuration file")
	fs.StringVar(&o.Config, "c", config.FileName, "configuration file")
	fs.StringVar(&targets, "target", "", "targets")
	fs.StringVar(&targets, "t", "", "targets")
	fs.StringVar(&o.Out, "out", "dist", "output directory")
	fs.StringVar(&o.Out, "o", "dist", "output directory")
	fs.StringVar(&o.Stubs, "stubs", "", "stub directory")
	fs.StringVar(&o.Version, "version", "", "version override")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	o.Targets = split(targets)
	return build.Build(o)
}

func cmdJlink(args []string) error {
	fs := flag.NewFlagSet("jlink", flag.ContinueOnError)
	var o build.JlinkOptions
	var mods, add string
	fs.StringVar(&o.Jar, "jar", "", "application jar")
	fs.StringVar(&o.Out, "out", "runtime", "output directory")
	fs.StringVar(&o.JDK, "jdk", "", "JDK home")
	fs.StringVar(&o.JMods, "jmods", "", "target jmods directory")
	fs.StringVar(&mods, "modules", "", "module list")
	fs.StringVar(&add, "add-modules", "", "additional modules")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	o.Modules, o.AddModules = split(mods), split(add)
	return build.Jlink(o)
}

// parse parses flags that may be interleaved with positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "see \"jrunner help\"\n") }
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
