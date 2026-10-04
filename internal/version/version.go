// Package version compares application and Java version strings.
package version

import (
	"strconv"
	"strings"
)

// Tool is the jrunner version, set at build time with
// -ldflags "-X github.com/korvin2000/java-runner/internal/version.Tool=1.0.0".
var Tool = "dev"

// Compare compares two version strings such as "1.2.10", "17.0.2+8",
// "1.8.0_392" or "2.0.0-rc.1" and returns -1, 0 or +1. Numeric parts are
// compared as numbers, build metadata after '+' is ignored and a pre-release
// sorts before the corresponding release.
func Compare(a, b string) int {
	am, ap := split(a)
	bm, bp := split(b)
	if c := compareParts(am, bm); c != 0 {
		return c
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	return compareParts(fields(ap), fields(bp))
}

// JavaFeature returns the feature (major) release of a Java version string:
// "1.8.0_392" -> 8, "17.0.2+8" -> 17, "21" -> 21. It returns 0 if unparsable.
func JavaFeature(v string) int {
	parts, _ := split(v)
	if len(parts) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(parts[0])
	if n == 1 && len(parts) > 1 {
		n, _ = strconv.Atoi(parts[1])
	}
	return n
}

func split(v string) (main []string, pre string) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v, pre = v[:i], v[i+1:]
	}
	return fields(v), pre
}

func fields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '_' || r == '-' })
}

func compareParts(a, b []string) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := "0", "0"
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				if xn < yn {
					return -1
				}
				return 1
			}
		case xerr == nil: // numeric identifiers sort before alphanumeric ones
			return -1
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return 0
}
