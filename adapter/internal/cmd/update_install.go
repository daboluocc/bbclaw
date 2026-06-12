package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// binaryPath resolves the path of the currently-running executable, following
// symlinks so we replace the real binary rather than a /usr/local/bin symlink
// pointing at it (install-adapter.sh's typical layout). Falling back to the raw
// argv path is correct for `go run` dev usage where there's no install dir to
// rewrite anyway — those callers are blocked earlier by IsNewerVersion when the
// build tag stays "dev".
func binaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

// atomicReplace writes src to dest via a temp file, then renames — avoids
// leaving a half-written binary if the download is interrupted. On macOS we
// also ad-hoc codesign the result so Gatekeeper doesn't SIGKILL the freshly
// downloaded binary on first launch (the com.apple.provenance attribute
// downloads pick up makes spctl reject unsigned executables).
func atomicReplace(w io.Writer, dest string, src io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		if os.IsPermission(err) {
			return permissionDeniedErr(dest, err)
		}
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		if os.IsPermission(err) {
			return permissionDeniedErr(dest, err)
		}
		return err
	}
	if err := codesignIfDarwin(dest); err != nil {
		// Don't fail the update for a signing miss — the user can still run the
		// binary if Gatekeeper happens to accept it as-is, and they always have
		// the manual `codesign --force --sign - <path>` escape hatch.
		fmt.Fprintf(w, "  [warn] codesign step skipped: %v\n", err)
	}
	fmt.Fprintf(w, "  [ok] binary updated → %s\n", dest)
	return nil
}

// permissionDeniedErr wraps a write-permission failure with an actionable hint.
// Common when the binary lives under /usr/local/bin and the user runs the
// upgrade without elevated privileges.
func permissionDeniedErr(dest string, cause error) error {
	return fmt.Errorf("no write permission for %s (%w)\n"+
		"      bbclaw-adapter lives in a directory that needs elevated privileges.\n"+
		"      Re-run with sudo, e.g.:\n"+
		"        sudo bbclaw-adapter update", dest, cause)
}

// codesignIfDarwin ad-hoc signs path on macOS. No-op elsewhere, or when
// codesign is missing (CLT not installed). Failures are non-fatal — caller
// logs and moves on.
func codesignIfDarwin(path string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if _, err := exec.LookPath("codesign"); err != nil {
		return nil
	}
	out, err := exec.Command("codesign", "--force", "--sign", "-", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("codesign: %w\n%s", err, out)
	}
	return nil
}

// IsNewerVersion reports whether `latest` is strictly newer than `current`,
// comparing major.minor.patch only. Leading "v" is optional; any pre-release
// or git-describe suffix (e.g. "-5-g1234abc", "-rc1") is stripped before
// comparison. A dev build whose tag won't parse counts as 0.0.0 — that means
// `update` from a dev binary always downloads, which is what we want when a
// developer asks the admin page to swap their local build for the released one.
func IsNewerVersion(current, latest string) bool {
	c := parseSemverTriple(current)
	l := parseSemverTriple(latest)
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parseSemverTriple(s string) [3]int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if dash := strings.Index(s, "-"); dash >= 0 {
		s = s[:dash]
	}
	parts := strings.SplitN(s, ".", 3)
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		out[i], _ = strconv.Atoi(parts[i])
	}
	return out
}
