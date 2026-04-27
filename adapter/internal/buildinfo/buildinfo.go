package buildinfo

import (
	"fmt"
	"strings"
)

var (
	Tag       = "dev"
	BuildTime = "unknown"
)

func ShouldPrintVersion(args []string) bool {
	for _, arg := range args {
		switch strings.TrimSpace(arg) {
		case "-v", "--version", "-version", "version":
			return true
		}
	}
	return false
}

func String(name string) string {
	return fmt.Sprintf("%s version tag=%s build=%s", name, Tag, BuildTime)
}
