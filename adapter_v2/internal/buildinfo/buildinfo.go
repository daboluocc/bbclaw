// Package buildinfo carries the version/build stamps injected at link time via
// -ldflags -X (see the Makefile and .github/workflows/release.yml). Mirrors v1's
// adapter/internal/buildinfo so both binaries stamp versions the same way.
package buildinfo

import (
	"fmt"
	"strings"
)

var (
	Tag       = "dev"
	BuildTime = "unknown"
)

// ShouldPrintVersion reports whether argv asks for the version (so main can print
// and exit before doing anything else).
func ShouldPrintVersion(args []string) bool {
	for _, arg := range args {
		switch strings.TrimSpace(arg) {
		case "-v", "--version", "-version", "version":
			return true
		}
	}
	return false
}

// String renders the one-line version banner.
func String(name string) string {
	return fmt.Sprintf("%s version tag=%s build=%s", name, Tag, BuildTime)
}
