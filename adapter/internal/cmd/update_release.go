package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"time"
)

// latestReleaseAsset finds the download URL for the current OS/arch binary in
// the latest release. The unified release.yml ships every tag with both the
// firmware and adapter binaries, so /releases/latest always carries the asset
// we want — no need to scan releases.atom for "the latest tag that has an
// adapter binary".
func latestReleaseAsset() (tag, url string, err error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(apiURL) //nolint:gosec
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("github releases/latest: status %d", resp.StatusCode)
	}
	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", "", err
	}
	want := binaryAssetName()
	for _, a := range release.Assets {
		if a.Name == want {
			return release.TagName, a.BrowserDownloadURL, nil
		}
	}
	return "", "", fmt.Errorf("no %s asset in release %s", want, release.TagName)
}

// binaryAssetName mirrors the flat name produced by adapter/scripts/release_build.sh
// and consumed by scripts/install-adapter.sh — keeping all three in sync means
// the upgrade flow lands the same artifact a fresh install would download.
func binaryAssetName() string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("bbclaw-adapter-%s-%s.exe", runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf("bbclaw-adapter-%s-%s", runtime.GOOS, runtime.GOARCH)
}

// FetchLatestTag returns the latest release tag, or empty on any error / timeout.
// Used by the admin page's "check for updates" call so a flaky network never
// blocks the page from rendering.
func FetchLatestTag() string {
	tag, _, err := latestReleaseAsset()
	if err != nil {
		return ""
	}
	return tag
}
