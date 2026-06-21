package extract

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// NormalizeReply turns the raw text scraped from claude's vt grid into clean,
// speakable prose for the device/voice line. It is a SEPARATE, swappable step
// (the "TTS rendering" component): when we only have the rendered 80-column
// window — not claude's original token stream — the extracted text carries grid
// artifacts that read badly aloud. Replace/skip this layer if a future transport
// yields the raw text directly.
//
// It fixes the artifacts observed on real devices:
//   - hard wraps that split a word/sentence across grid rows
//     ("工作目\n录现在是…" → "工作目录现在是…")
//   - runs of spaces from wide-char / alignment padding
//     ("我是   Claude Code … 命令行       AI" → "我是 Claude Code … 命令行 AI")
//   - the 2-space continuation indent claude aligns under "⏺ "
//
// A blank line is a real paragraph break and is preserved (as a single newline);
// every other newline inside a paragraph is treated as a wrap and rejoined. CJK
// boundaries rejoin with no space (Chinese has no inter-word spaces); an
// ASCII-word boundary rejoins with one space (the wrap ate the original space).
func NormalizeReply(raw string) string {
	var paragraphs []string
	var cur string
	flush := func() {
		if s := collapseSpaces(strings.TrimSpace(cur)); s != "" {
			paragraphs = append(paragraphs, s)
		}
		cur = ""
	}
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			flush() // blank line → paragraph break
			continue
		}
		cur = rejoinWrap(cur, t)
	}
	flush()
	return strings.Join(paragraphs, "\n")
}

// rejoinWrap appends next to cur across a wrap boundary. Script-based: if either
// side of the boundary is CJK, rejoin with NO space (Chinese/Japanese have no
// inter-word spaces, and a long line wrapped between two CJK runes); otherwise
// the text is Latin and the wrap dropped a real inter-word space, so rejoin with
// one space ("…is Paris." + "It has…" → "…is Paris. It has…").
func rejoinWrap(cur, next string) string {
	if cur == "" {
		return next
	}
	if next == "" {
		return cur
	}
	a, _ := utf8.DecodeLastRuneInString(cur)
	b, _ := utf8.DecodeRuneInString(next)
	if isCJK(a) || isCJK(b) {
		return cur + next
	}
	return cur + " " + next
}

// collapseSpaces reduces every run of 2+ spaces to a single space (the wide-char
// / alignment padding artifact). Newlines are not present here (paragraphs are
// joined before this runs), so only spaces are touched.
func collapseSpaces(s string) string {
	if !strings.Contains(s, "  ") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isCJK reports whether r is an East Asian character or CJK/fullwidth
// punctuation — the cases where text has no inter-word spaces, so a wrap between
// such runes must rejoin with none.
func isCJK(r rune) bool {
	switch {
	case unicode.Is(unicode.Han, r),
		unicode.Is(unicode.Hiragana, r),
		unicode.Is(unicode.Katakana, r),
		unicode.Is(unicode.Hangul, r):
		return true
	case r >= 0x3000 && r <= 0x303F: // CJK symbols & punctuation (，。、！？「」…)
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // halfwidth & fullwidth forms
		return true
	}
	return false
}
