// Package prewarm seeds the butler's long-term memory the moment a user adds a
// project through the local admin page. Instead of the butler meeting a new
// directory cold on the first voice turn, a lightweight scan distils a few
// durable facts — language/stack, the README's opening lines, the most recent
// git commits — and appends them as a section into the workspace's
// MEMORY/projects.md, the file the persona already reads on demand.
//
// Everything here is best-effort and non-fatal: a scan failure must never block
// the admin Add (the project is registered regardless). Writes are idempotent —
// re-adding the same project name replaces its section rather than duplicating —
// and target the plural projects.md the persona indexes, which is decoupled from
// the singular project.md the auto-consolidator owns, so the two never collide.
package prewarm

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// inflight tracks RecordAsync goroutines so callers (graceful shutdown, tests)
// can await outstanding scans via Wait — otherwise an async projects.md write
// can land after a test's temp dir is torn down ("directory not empty").
var inflight sync.WaitGroup

// Wait blocks until all in-flight RecordAsync scans have finished.
func Wait() { inflight.Wait() }

// Logger is the minimal log surface; *obs.Logger satisfies it. nil is tolerated.
type Logger interface {
	Infof(string, ...any)
	Warnf(string, ...any)
}

// maxReadmeLines caps how much of the README we lift into the summary.
const maxReadmeLines = 8

// gitLogTimeout bounds the `git log` probe so a huge or pathological repo can't
// stall the scan.
const gitLogTimeout = 5 * time.Second

// stackMarkers maps a sentinel file to the stack label it implies. Order is
// stable for deterministic output; multiple hits are all reported.
var stackMarkers = []struct{ file, label string }{
	{"go.mod", "Go"},
	{"package.json", "Node.js"},
	{"Cargo.toml", "Rust"},
	{"pyproject.toml", "Python"},
	{"requirements.txt", "Python"},
	{"pom.xml", "Java (Maven)"},
	{"build.gradle", "Java/Kotlin (Gradle)"},
	{"Gemfile", "Ruby"},
	{"composer.json", "PHP"},
	{"CMakeLists.txt", "C/C++ (CMake)"},
	{"Dockerfile", "Docker"},
}

// Marker prefixes a project's managed section so a re-add can replace it in place.
func marker(name string) string { return "<!-- prewarm:" + name + " -->" }

// Record scans dir for a quick profile of the project and upserts a summary
// section (keyed by name) into projectsMDPath. It is safe to call in a goroutine.
// Errors are returned for the caller to log; they are never fatal to the add.
func Record(name, dir, projectsMDPath string) error {
	section := buildSection(name, dir)
	return upsertSection(projectsMDPath, name, section)
}

// RecordAsync runs Record in a goroutine, logging the outcome. This is the entry
// point the admin handler uses so the HTTP response isn't blocked on disk/git.
func RecordAsync(name, dir, projectsMDPath string, log Logger) {
	inflight.Add(1)
	go func() {
		defer inflight.Done()
		if err := Record(name, dir, projectsMDPath); err != nil {
			if log != nil {
				log.Warnf("prewarm: record %q failed (non-fatal): %v", name, err)
			}
			return
		}
		if log != nil {
			log.Infof("prewarm: seeded MEMORY/projects.md for %q", name)
		}
	}()
}

// buildSection renders the managed markdown block for one project.
func buildSection(name, dir string) string {
	var b strings.Builder
	b.WriteString(marker(name))
	b.WriteString("\n## ")
	b.WriteString(name)
	b.WriteString("\n- 路径：")
	b.WriteString(dir)
	b.WriteString("\n")

	if stack := detectStack(dir); stack != "" {
		b.WriteString("- 技术栈：")
		b.WriteString(stack)
		b.WriteString("\n")
	}
	if readme := readReadme(dir); readme != "" {
		b.WriteString("- 简介（取自 README）：")
		b.WriteString(readme)
		b.WriteString("\n")
	}
	if commits := recentCommits(dir); len(commits) > 0 {
		b.WriteString("- 近期提交：")
		b.WriteString(strings.Join(commits, "；"))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// detectStack returns a comma-joined list of detected stacks, or "".
func detectStack(dir string) string {
	var hits []string
	seen := map[string]struct{}{}
	for _, m := range stackMarkers {
		if _, err := os.Stat(filepath.Join(dir, m.file)); err == nil {
			if _, dup := seen[m.label]; dup {
				continue
			}
			seen[m.label] = struct{}{}
			hits = append(hits, m.label)
		}
	}
	return strings.Join(hits, ", ")
}

// readReadme returns a one-line condensation of the first non-heading prose lines
// of the project's README (case-insensitive match on common names), or "".
func readReadme(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var path string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(e.Name())
		if lower == "readme.md" || lower == "readme" || lower == "readme.txt" || lower == "readme.rst" {
			path = filepath.Join(dir, e.Name())
			break
		}
	}
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for sc.Scan() && len(lines) < maxReadmeLines {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "![") || strings.HasPrefix(line, "<") {
			continue // skip blanks, headings, badge images, HTML
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil && len(lines) == 0 {
		return "" // unreadable README → no summary
	}
	summary := strings.Join(lines, " ")
	summary = strings.Join(strings.Fields(summary), " ") // collapse whitespace
	const cap = 280
	if len(summary) > cap {
		summary = strings.TrimSpace(summary[:cap]) + "…"
	}
	return summary
}

// recentCommits returns up to 3 recent commit subjects via git, or nil when the
// directory is not a git repo / git is unavailable.
func recentCommits(dir string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), gitLogTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log", "--no-merges", "-3", "--pretty=format:%s")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var commits []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			commits = append(commits, line)
		}
	}
	return commits
}

// upsertSection writes section into projectsMDPath, replacing any existing block
// for the same project (matched by its marker) or appending when absent. The file
// is created with a header when missing. Writes are atomic (tmp + rename, 0600).
func upsertSection(path, name, section string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("prewarm: read %s: %w", path, err)
	}
	content := string(existing)
	if content == "" {
		content = "# 最近在做的项目\n\n<!-- 由 BBClaw 自动维护：用户近期关注的项目与进展线索。 -->\n"
	}

	blocks := splitSections(content)
	blocks[name] = section
	rebuilt := renderSections(content, blocks)

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("prewarm: mkdir %s: %w", dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(rebuilt), 0o600); err != nil {
		return fmt.Errorf("prewarm: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("prewarm: rename: %w", err)
	}
	return nil
}

// splitSections extracts existing prewarm-managed blocks keyed by project name.
// Non-managed prose (the header, anything the user wrote) is left out here and
// preserved by renderSections via the head slice.
func splitSections(content string) map[string]string {
	out := map[string]string{}
	const open = "<!-- prewarm:"
	idx := strings.Index(content, open)
	for idx >= 0 {
		rest := content[idx:]
		_, after, ok := strings.Cut(rest, open)
		if !ok {
			break
		}
		name, _, ok := strings.Cut(after, " -->")
		if !ok {
			break
		}
		name = strings.TrimSpace(name)
		// The block runs until the next prewarm marker or end of content. Search
		// past the current marker (after `open`) so it doesn't match itself.
		next := strings.Index(after, open)
		if next < 0 {
			out[name] = strings.TrimRight(rest, "\n")
			break
		}
		blockEnd := len(open) + next
		out[name] = strings.TrimRight(rest[:blockEnd], "\n")
		idx += blockEnd
	}
	return out
}

// renderSections rebuilds the file: the head (everything before the first managed
// block) followed by every managed block sorted by name for deterministic output.
func renderSections(content string, blocks map[string]string) string {
	head := content
	if i := strings.Index(content, "<!-- prewarm:"); i >= 0 {
		head = content[:i]
	}
	head = strings.TrimRight(head, "\n") + "\n"

	names := make([]string, 0, len(blocks))
	for n := range blocks {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString(head)
	for _, n := range names {
		b.WriteString("\n")
		b.WriteString(blocks[n])
		b.WriteString("\n")
	}
	return b.String()
}
