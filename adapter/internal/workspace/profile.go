package workspace

import (
	"os"
	"strings"
)

// Profile STATUS marker values carried by MEMORY/profile.md (ADR-026). The
// butler flips the marker during first-run onboarding; the adapter reads it
// each turn to decide whether the onboarding directive still needs to be
// injected into the system prompt.
const (
	ProfileStatusUninitialized = "uninitialized"
	ProfileStatusInitialized   = "initialized"
	ProfileStatusSkipped       = "skipped"
)

// profileStatusMarker is the opening of the HTML-comment STATUS marker the
// profile.md skeleton ships with: `<!-- STATUS: uninitialized -->`.
const profileStatusMarker = "<!-- STATUS:"

// ProfileStatus reads MEMORY/profile.md and returns its STATUS marker value
// (one of the ProfileStatus* constants). It returns "" when the file or the
// marker is missing, malformed, or unreadable — callers MUST treat unknown as
// "inject nothing" so a broken profile never spams the user with onboarding.
func ProfileStatus() string {
	path, err := MemoryFilePath("profile.md")
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return ParseProfileStatus(string(raw))
}

// ParseProfileStatus extracts the value of the `<!-- STATUS: xxx -->` marker
// from profile.md content. Returns "" when the marker is absent or malformed.
func ParseProfileStatus(content string) string {
	_, rest, ok := strings.Cut(content, profileStatusMarker)
	if !ok {
		return ""
	}
	value, _, ok := strings.Cut(rest, "-->")
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}
