//go:build !windows

package platform

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

// SystemProxy returns the proxy configured in macOS' network settings. Other
// Unix systems use the environment variables only.
func SystemProxy() Proxy {
	if runtime.GOOS != "darwin" {
		return Proxy{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "scutil", "--proxy").Output()
	if err != nil {
		return Proxy{}
	}
	return parseScutil(string(out))
}
