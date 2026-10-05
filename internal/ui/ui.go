// Package ui prints progress to the console in a compact, readable form:
//
//	▸ step
//	  detail
//	  ✓ success
//	  ⚠ warning
//	  [████████░░░░░░░░░░░░]  41%  21.3 MB / 51.8 MB  6.2 MB/s  ETA 5s
//
// Colors and Unicode glyphs are used on terminals that support them (Windows
// Terminal, every modern Unix terminal); legacy Windows consoles get ASCII.
// NO_COLOR disables colors.
package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/korvin2000/java-runner/internal/platform"
)

var (
	out     io.Writer = os.Stderr
	tty               = platform.IsTerminal(os.Stderr)
	ansi              = tty && os.Getenv("TERM") != "dumb" && platform.EnableANSI(os.Stderr) // cursor control: spinner, progress bar
	color             = ansi && os.Getenv("NO_COLOR") == ""
	unicode           = runtime.GOOS != "windows" || os.Getenv("WT_SESSION") != "" || os.Getenv("TERM_PROGRAM") != ""
	verbose bool
	mu      sync.Mutex // serializes writes from the spinner and other output
)

const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
)

type glyphs struct{ step, ok, warn, err, dot, full, empty string }

var g = func() glyphs {
	if unicode {
		return glyphs{"▸", "✓", "⚠", "✗", "·", "█", "░"}
	}
	return glyphs{">", "+", "!", "x", ".", "#", "-"}
}()

var spinFrames = func() []string {
	if unicode {
		return []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	}
	return []string{"-", "\\", "|", "/"}
}()

func paint(c, s string) string {
	if !color {
		return s
	}
	return c + s + reset
}

// SetVerbose enables Debug output.
func SetVerbose(v bool) { verbose = v }

// Colors reports whether colored output is active.
func Colors() bool { return color }

// took formats an elapsed time for a result line; short durations are omitted.
func took(start time.Time) string {
	if d := time.Since(start); d >= 500*time.Millisecond {
		return " " + paint(dim, "("+Duration(d)+")")
	}
	return ""
}

// Interactive reports whether the user can answer questions.
func Interactive() bool { return tty && platform.IsTerminal(os.Stdin) }

func line(prefix, format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	clearSpinner()
	fmt.Fprintf(out, prefix+format+"\n", a...)
}

// Title prints the application banner shown on first launch.
func Title(name, version, note string) {
	line("", "%s %s  %s", paint(bold, name), paint(dim, version), paint(dim, note))
}

// Step announces a major action.
func Step(format string, a ...any) { line(paint(cyan+bold, g.step)+" ", format, a...) }

// Info prints a detail line under the current step.
func Info(format string, a ...any) { line("  ", format, a...) }

// Success prints a completed detail.
func Success(format string, a ...any) { line("  "+paint(green, g.ok)+" ", format, a...) }

// Warn prints a non-fatal problem.
func Warn(format string, a ...any) { line("  "+paint(yellow, g.warn)+" ", format, a...) }

// Debug prints only in verbose mode.
func Debug(format string, a ...any) {
	if verbose {
		line("  "+paint(dim, g.dot)+" ", "%s", paint(dim, fmt.Sprintf(format, a...)))
	}
}

// Error prints a fatal error.
func Error(err error) { line(paint(red+bold, g.err)+" ", "%v", err) }

// Confirm asks a yes/no question; without a terminal it returns def.
func Confirm(question string, def bool) bool {
	if !Interactive() {
		return def
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	mu.Lock()
	clearSpinner()
	fmt.Fprintf(out, "%s %s %s ", paint(cyan+bold, "?"), question, paint(dim, hint))
	mu.Unlock()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "д", "да":
		return true
	case "n", "no", "н", "нет":
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
	fmt.Fprint(out, "\n"+paint(dim, "Press Enter to close this window..."))
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

// Duration formats d compactly: "0.8s", "12s", "2m05s".
func Duration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// ---- spinner -------------------------------------------------------------

var spinning string // text of the running spinner, "" if none

// clearSpinner erases the spinner line; the caller holds mu.
func clearSpinner() {
	if spinning != "" && ansi {
		fmt.Fprint(out, "\r\033[K")
	}
}

// Spin shows an animated indicator for an action without measurable
// progress. The returned function ends it with a success or failure line.
func Spin(format string, a ...any) func(ok bool, result string) {
	text := fmt.Sprintf(format, a...)
	start := time.Now()
	if !ansi {
		Info("%s...", text)
		return func(ok bool, result string) {
			if ok {
				Success("%s%s", result, took(start))
			}
		}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; ; i++ {
			mu.Lock()
			spinning = text
			fmt.Fprintf(out, "\r\033[K  %s %s", paint(cyan, spinFrames[i%len(spinFrames)]), text)
			mu.Unlock()
			select {
			case <-done:
				return
			case <-time.After(80 * time.Millisecond):
			}
		}
	}()
	return func(ok bool, result string) {
		close(done)
		<-finished
		mu.Lock()
		clearSpinner()
		spinning = ""
		mu.Unlock()
		if ok {
			Success("%s%s", result, took(start))
		}
	}
}

// ---- progress bar --------------------------------------------------------

// Bar is a download progress bar. It implements io.Writer, so it can be fed
// through io.MultiWriter. On a terminal it redraws one line; otherwise it
// prints a line every 25%.
type Bar struct {
	total, done, base int64 // base: bytes already present (resumed download)
	start, last       time.Time
	lastPct           int64
}

// NewBar creates a progress bar; total may be -1 if unknown, done is the
// number of bytes already downloaded earlier.
func NewBar(total, done int64) *Bar {
	now := time.Now()
	return &Bar{total: total, done: done, base: done, start: now, last: now}
}

func (b *Bar) Write(p []byte) (int, error) {
	b.done += int64(len(p))
	b.render(false)
	return len(p), nil
}

// Finish draws the final state and ends the line; ok tells whether the
// download completed.
func (b *Bar) Finish(ok bool) {
	if !ansi {
		if ok {
			b.render(true)
		}
		return
	}
	mu.Lock()
	fmt.Fprint(out, "\r\033[K")
	mu.Unlock()
	if ok {
		Success("downloaded %s%s", Size(b.done), took(b.start))
	}
}

func (b *Bar) render(final bool) {
	now := time.Now()
	if !final && now.Sub(b.last) < 100*time.Millisecond {
		return
	}
	b.last = now
	elapsed := now.Sub(b.start).Seconds()
	if elapsed < 0.001 {
		elapsed = 0.001
	}
	rate := float64(b.done-b.base) / elapsed
	speed := Size(int64(rate)) + "/s"
	if !ansi {
		if b.total > 0 {
			pct := b.done * 100 / b.total
			if pct/25 > b.lastPct/25 || (final && pct != b.lastPct) {
				b.lastPct = pct
				Info("%3d%%  %s / %s  %s", pct, Size(b.done), Size(b.total), speed)
			}
		} else if final {
			Info("%s  %s", Size(b.done), speed)
		}
		return
	}
	var text string
	if b.total > 0 {
		const width = 24
		pct := b.done * 100 / b.total
		n := int(b.done * width / b.total)
		if n > width {
			n = width
		}
		eta := ""
		if rate > 0 && b.done < b.total && elapsed > 1 {
			eta = "  ETA " + Duration(time.Duration(float64(b.total-b.done)/rate*float64(time.Second)))
		}
		bar := paint(cyan, strings.Repeat(g.full, n)) + paint(dim, strings.Repeat(g.empty, width-n))
		text = fmt.Sprintf("  [%s] %3d%%  %s / %s  %s%s", bar, pct, Size(b.done), Size(b.total), paint(dim, speed), paint(dim, eta))
	} else {
		text = fmt.Sprintf("  %s %s  %s", paint(cyan, spinFrames[int(elapsed*10)%len(spinFrames)]), Size(b.done), paint(dim, speed))
	}
	mu.Lock()
	fmt.Fprintf(out, "\r\033[K%s", text)
	mu.Unlock()
}
