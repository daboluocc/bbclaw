package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/daboluocc/bbclaw/adapter/internal/buildinfo"
	"github.com/spf13/cobra"
)

// githubRepo is the source for `bbclaw-adapter update` downloads. The unified
// release.yml workflow ships an adapter binary for every v* tag alongside the
// firmware (ADR-011), so /releases/latest always carries the binary we need.
const githubRepo = "daboluocc/bbclaw"

// NewUpdateCmd builds the `bbclaw-adapter update` subcommand. Downloads the
// latest release binary for the current OS/arch and atomically replaces the
// running executable in place — whether it lives in $HOME/bbclaw-adapter,
// /usr/local/bin, or ~/.local/bin (install-adapter.sh keeps a symlink in the
// PATH dir pointing at the real binary; EvalSymlinks lets us update the real
// file so the symlink stays valid).
func NewUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Update bbclaw-adapter to the latest GitHub release",
		Long: `Fetches the latest release binary from ` + githubRepo + ` and replaces the
currently-running bbclaw-adapter binary in place. Pair with the admin page's
"一键升级" button (POST /v1/admin/update) for a one-click flow that also
self-restarts the service when the swap is done.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return DownloadLatestBinary(os.Stdout)
		},
	}
}

// DownloadLatestBinary is the shared core used by both the CLI subcommand and
// the admin HTTP endpoint. It short-circuits when the running build is already
// at (or ahead of) the latest tag, so calling it on every page load is cheap.
func DownloadLatestBinary(w io.Writer) error {
	tag, assetURL, err := latestReleaseAsset()
	if err != nil {
		return err
	}
	if !IsNewerVersion(buildinfo.Tag, tag) {
		fmt.Fprintf(w, "  [ok] already at %s — nothing to do\n", buildinfo.Tag)
		return nil
	}
	fmt.Fprintf(w, "  [dl] downloading %s (%s)...\n", tag, assetURL)
	resp, err := http.Get(assetURL) //nolint:gosec
	if err != nil {
		return fmt.Errorf("HTTP get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return atomicReplace(w, binaryPath(), resp.Body)
}
