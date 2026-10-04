// Package ui prints progress to the console in a compact, readable form:
//
//	==> step
//	    detail
//	  ! warning
package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/korvin2000/java-runner/internal/platform"
)

var (
	out     io.Writer = os.Stderr
	tty               = platform.IsTerminal(os.Stderr)
	verbose bool
)

// SetVerbose enables Debug output.
func SetVerbose(v bool) { verbose = v }

// Interactive reports whether the user can answer questions.
func Interactive() bool { return tty && platform.IsTerminal(os.Stdin) }

// Step announces a major action.
func Step(format string, a ...any) { fmt.Fprintf(out, "==> "+format+"\n", a...) }

// Info prints a detail line under the current step.
func Info(format string, a ...any) { fmt.Fprintf(out, "    "+format+"\n", a...) }

// Warn prints a non-fatal problem.
func Warn(format string, a ...any) { fmt.Fprintf(out, "  ! "+format+"\n", a...) }

// Debug prints only in verbose mode.
func Debug(format string, a ...any) {
	if verbose {
		fmt.Fprintf(out, "    . "+format+"\n", a...)
	}
}

// Error prints a fatal error.
func Error(err error) { fmt.Fprintf(out, "error: %v\n", err) }

// Confirm asks a yes/no question; without a terminal it returns def.
func Confirm(question string, def bool) bool {
	if !Interactive() {
		return def
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	fmt.Fprintf(out, "==> %s %s ", question, hint)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

// PauseIfOwnConsole keeps a console window that was opened just for this
// process (Windows double-click) open until the user presses Enter, so error
// messages can be read.
func PauseIfOwnConsole() {
	if !platform.OwnConsole() {
		return
	}
	fmt.Fprint(out, "\nPress Enter to close this window...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// Size formats a byte count, e.g. "42.1 MB".
func Size(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	f, i := float64(n), -1
	for f >= 1024 && i < 3 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %cB", f, "KMGT"[i])
}

// Bar is a download progress bar. It implements io.Writer, so it can be fed
// through io.MultiWriter. On a terminal it redraws one line; otherwise it
// prints a line every 25%.
type Bar struct {
	total, done int64
	start, last time.Time
	lastPct     int64
}

// NewBar creates a progress bar; total may be -1 if unknown.
func NewBar(total int64) *Bar {
	now := time.Now()
	return &Bar{total: total, start: now, last: now}
}

func (b *Bar) Write(p []byte) (int, error) {
	b.done += int64(len(p))
	b.render(false)
	return len(p), nil
}

// Finish draws the final state and ends the line.
func (b *Bar) Finish() {
	b.render(true)
	if tty {
		fmt.Fprintln(out)
	}
}

func (b *Bar) render(final bool) {
	now := time.Now()
	if !final && now.Sub(b.last) < 120*time.Millisecond {
		return
	}
	b.last = now
	secs := now.Sub(b.start).Seconds()
	if secs < 0.001 {
		secs = 0.001
	}
	speed := Size(int64(float64(b.done)/secs)) + "/s"
	if !tty {
		if b.total > 0 {
			pct := b.done * 100 / b.total
			if pct/25 > b.lastPct/25 || final {
				b.lastPct = pct
				Info("%3d%%  %s / %s", pct, Size(b.done), Size(b.total))
			}
		} else if final {
			Info("%s", Size(b.done))
		}
		return
	}
	var line string
	if b.total > 0 {
		const width = 30
		pct := b.done * 100 / b.total
		n := int(b.done * width / b.total)
		if n > width {
			n = width
		}
		line = fmt.Sprintf("    [%s%s] %3d%%  %s / %s  %s",
			strings.Repeat("#", n), strings.Repeat("-", width-n), pct, Size(b.done), Size(b.total), speed)
	} else {
		line = fmt.Sprintf("    %s  %s", Size(b.done), speed)
	}
	fmt.Fprintf(out, "\r%-78s", line)
}
