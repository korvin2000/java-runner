// jrunner turns a Java application (fat jar, Spring Boot jar or modular app)
// into a native executable that installs itself, provisions a suitable Java
// runtime on first launch, keeps the app up to date and starts it quickly.
//
// The same binary plays two roles:
//   - without an embedded application package it is the builder CLI
//     (jrunner init / build / jlink);
//   - with an embedded package (appended by "jrunner build") it is the
//     application's launcher and installer.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/korvin2000/java-runner/internal/cli"
	"github.com/korvin2000/java-runner/internal/launcher"
	"github.com/korvin2000/java-runner/internal/pkg"
)

func main() {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot locate own executable:", err)
		os.Exit(1)
	}
	p, err := pkg.OpenExecutable(exe)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if p != nil {
		os.Exit(launcher.Run(exe, p, os.Args[1:]))
	}
	os.Exit(cli.Main(os.Args[1:]))
}
