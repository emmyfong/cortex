package parse

import (
	"regexp"
	"strings"
)

// Extracted articles carry material that is worse than useless in a retrieval
// index: bibliographies embed as passages that can surface as search results,
// and inline citation markers add noise to every chunk they appear in. A single
// Wikipedia article was measured at 518 citation markers, with 34% of its
// chunks consisting solely of reference entries.
//
// Cleaning happens after markdown conversion, where citations have a
// recognisable link shape, and before chunking, so token budgets are spent on
// actual content.

var (
	// Markdown links pointing at citation anchors: "[\[126\]](#cite_note-x)"
	// and the "[↑](#cite_ref-237)" back-links that begin reference entries.
	//
	// The link text itself contains escaped brackets, so the optional \[ and \]
	// around the inner run are load-bearing. Forbidding brackets inside that run
	// also stops the match from spanning across an adjacent ordinary link.
	citationLinkPattern = regexp.MustCompile(`\[\\?\[?[^\[\]]*\\?\]?\]\(#cite_(?:note|ref)[^)]*\)`)

	// An escaped bare "[\[12\]]" left behind by a different converter.
	escapedCitationPattern = regexp.MustCompile(`\[\\\[\d+\\\]\]`)

	// A bare "[12]" in citation position. The leading whitespace is required so
	// that indexing expressions such as arr[12] are left alone; it is consumed
	// with the marker and the surrounding cleanup restores correct spacing.
	bareCitationPattern = regexp.MustCompile(`\s\[\d{1,3}\]`)

	// Wiki "[edit]" section links, which render beside every heading and are
	// navigation rather than content.
	editLinkPattern = regexp.MustCompile(`\\?\[\[edit\]\([^)]*\)\]|\[edit\]\([^)]*action=edit[^)]*\)`)

	// Collapse the gaps left once markers are removed.
	spaceBeforePunctuation = regexp.MustCompile(`\s+([,.;:!?])`)
	repeatedSpaces         = regexp.MustCompile(`[ \t]{2,}`)
	excessBlankLines       = regexp.MustCompile(`\n{3,}`)
)

// boilerplateHeadings end the useful part of an article. Everything from such a
// heading to the next heading of the same or higher level is dropped.
var boilerplateHeadings = map[string]bool{
	"references":      true,
	"external links":  true,
	"see also":        true,
	"further reading": true,
	"bibliography":    true,
	"notes":           true,
	"citations":       true,
	"sources":         true,
	"footnotes":       true,
}

// CleanMarkdown removes citation apparatus and boilerplate sections.
func CleanMarkdown(markdown string) string {
	lines := strings.Split(markdown, "\n")
	kept := make([]string, 0, len(lines))

	// skipUntilLevel > 0 means we are inside a boilerplate section and are
	// waiting for a heading at that level or shallower.
	skipUntilLevel := 0

	for _, line := range lines {
		if level, text, isHeading := parseMarkdownHeading(line); isHeading {
			if skipUntilLevel > 0 && level <= skipUntilLevel {
				skipUntilLevel = 0
			}
			if boilerplateHeadings[strings.ToLower(strings.TrimSpace(text))] {
				skipUntilLevel = level
				continue
			}
		}

		if skipUntilLevel > 0 {
			continue
		}
		// A reference entry, recognised by its leading back-link. These often
		// appear with no heading at all, so section skipping alone misses them.
		if isReferenceEntry(line) {
			continue
		}
		kept = append(kept, line)
	}

	cleaned := strings.Join(kept, "\n")
	// Link form first: stripping the bare brackets earlier would orphan the
	// "(#cite_note-...)" half of each link.
	cleaned = citationLinkPattern.ReplaceAllString(cleaned, "")
	cleaned = escapedCitationPattern.ReplaceAllString(cleaned, "")
	cleaned = editLinkPattern.ReplaceAllString(cleaned, "")
	cleaned = bareCitationPattern.ReplaceAllString(cleaned, "")
	cleaned = spaceBeforePunctuation.ReplaceAllString(cleaned, "$1")
	cleaned = repeatedSpaces.ReplaceAllString(cleaned, " ")
	cleaned = excessBlankLines.ReplaceAllString(cleaned, "\n\n")

	return strings.TrimSpace(cleaned)
}

// isReferenceEntry reports whether a line is a bibliography entry rather than
// prose. Wikipedia renders each one with a leading up-arrow back-link.
func isReferenceEntry(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	return strings.HasPrefix(trimmed, "[↑](#cite_ref") ||
		strings.HasPrefix(trimmed, "^ ") ||
		strings.HasPrefix(trimmed, "[^")
}

// parseMarkdownHeading recognises ATX headings. It mirrors the chunker's parser
// deliberately: cleaning must agree with chunking about what a heading is.
func parseMarkdownHeading(line string) (level int, text string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "#") {
		return 0, "", false
	}

	hashes := 0
	for hashes < len(trimmed) && trimmed[hashes] == '#' {
		hashes++
	}
	if hashes > 6 || hashes == len(trimmed) {
		return 0, "", false
	}
	if trimmed[hashes] != ' ' && trimmed[hashes] != '\t' {
		return 0, "", false
	}
	return hashes, strings.TrimSpace(trimmed[hashes:]), true
}
