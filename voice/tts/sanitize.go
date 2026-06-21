package tts

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	// Fenced code blocks: ```lang\n...\n``` → keep inner content, drop fences.
	reCodeFence = regexp.MustCompile("(?s)```[^\n]*\n?(.*?)```")

	// Inline code: `code` → keep inner content.
	reInlineCode = regexp.MustCompile("`([^`\n]*)`")

	// Markdown image: ![alt](url) → keep alt only.
	reMarkdownImage = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)

	// Markdown link: [text](url) → keep text only.
	reMarkdownLink = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)

	// HTML tags: <tag ...> or </tag> → drop.
	reHTMLTag = regexp.MustCompile(`<[^>]+>`)

	// Bold / italic emphasis (***x***, **x**, *x*, ___x___, __x__, _x_) and
	// strikethrough (~~x~~). Run greedy-first to peel paired markers without
	// matching unpaired strays.
	reEmphasisStars  = regexp.MustCompile(`\*{1,3}([^\*\n]+?)\*{1,3}`)
	reEmphasisUnders = regexp.MustCompile(`_{1,3}([^_\n]+?)_{1,3}`)
	reStrike         = regexp.MustCompile(`~~([^~\n]+?)~~`)

	// Leading line markers: ATX headers, list bullets, blockquotes.
	reLineHeader = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]+`)
	reLineBullet = regexp.MustCompile(`(?m)^[ \t]{0,3}([-*+]|\d+[.)])[ \t]+`)
	reLineQuote  = regexp.MustCompile(`(?m)^[ \t]{0,3}>+[ \t]?`)
	reHorizontal = regexp.MustCompile(`(?m)^[ \t]{0,3}(?:-[ \t]*){3,}$|^[ \t]{0,3}(?:\*[ \t]*){3,}$|^[ \t]{0,3}(?:_[ \t]*){3,}$`)

	// Multiple newlines and tabs collapse to a single space so TTS gets a
	// natural pause without literal "newline" pronunciation.
	reWhitespaceRun = regexp.MustCompile(`[ \t\f\v]{2,}`)
	reNewlineRun    = regexp.MustCompile(`[\r\n]+`)
)

// Sanitize cleans LLM/markdown output so a TTS engine can speak the prose
// end-to-end. It strips formatting tokens (bold/italic/code fences/headers/
// list markers/links/HTML), removes invisible and control characters, and
// collapses runs of whitespace. The remaining content — including paths,
// numbers, punctuation — is preserved so the speaker still hears it.
//
// Returns an empty string only when the input is empty or contains nothing
// but formatting/whitespace.
func Sanitize(input string) string {
	if input == "" {
		return ""
	}
	s := input

	s = reCodeFence.ReplaceAllString(s, "$1")
	s = reMarkdownImage.ReplaceAllString(s, "$1")
	s = reMarkdownLink.ReplaceAllString(s, "$1")
	s = reInlineCode.ReplaceAllString(s, "$1")
	s = reHTMLTag.ReplaceAllString(s, "")

	s = reHorizontal.ReplaceAllString(s, "")
	s = reLineHeader.ReplaceAllString(s, "")
	s = reLineBullet.ReplaceAllString(s, "")
	s = reLineQuote.ReplaceAllString(s, "")

	s = reStrike.ReplaceAllString(s, "$1")
	s = reEmphasisStars.ReplaceAllString(s, "$1")
	s = reEmphasisUnders.ReplaceAllString(s, "$1")

	s = stripUnspeakableRunes(s)

	s = reNewlineRun.ReplaceAllString(s, " ")
	s = reWhitespaceRun.ReplaceAllString(s, " ")

	return strings.TrimSpace(s)
}

// stripUnspeakableRunes removes BOM, zero-width joiners, and other control
// characters that the TTS engine either chokes on or reads as a glitch.
// Printable text, CJK, emoji, and ordinary punctuation are kept.
func stripUnspeakableRunes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\n', '\t':
			b.WriteRune(r)
			continue
		case '\uFEFF', '\u200B', '\u200C', '\u200D', '\u2060':
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
