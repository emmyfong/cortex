package parse

import (
	"strings"
	"testing"
)

func TestCleanMarkdownStripsCitationMarkers(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "removes wikipedia-style inline citation link",
			in:   `Degradation is temperature-dependent.[\[117\]](#cite_note-Voelker-2014-117) It accelerates above 40C.`,
			want: "Degradation is temperature-dependent. It accelerates above 40C.",
		},
		{
			name: "removes several markers in one sentence",
			in:   `Studies agree.[\[1\]](#cite_note-1)[\[2\]](#cite_note-2)[\[3\]](#cite_note-3)`,
			want: "Studies agree.",
		},
		{
			name: "removes bare bracketed numerals",
			in:   "The cell degrades [12] over time.",
			want: "The cell degrades over time.",
		},
		{
			// Stripping a marker before punctuation must not leave a gap.
			name: "does not leave a space before punctuation",
			in:   `Capacity falls[\[9\]](#cite_note-9), then stabilises.`,
			want: "Capacity falls, then stabilises.",
		},
		{
			name: "leaves ordinary links alone",
			in:   "See [the manual](https://example.com/manual) for details.",
			want: "See [the manual](https://example.com/manual) for details.",
		},
		{
			// Wiki "[edit]" links sit beside every heading and are navigation.
			name: "removes wiki edit-section links",
			in:   `\[[edit](https://en.wikipedia.org/w/index.php?title=X&action=edit&section=34 "Edit section")] Extraction of raw materials`,
			want: "Extraction of raw materials",
		},
		{
			name: "leaves prose without citations unchanged",
			in:   "A plain sentence with no citations at all.",
			want: "A plain sentence with no citations at all.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CleanMarkdown(tt.in); got != tt.want {
				t.Errorf("CleanMarkdown()\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

// Reference entries frequently appear with no heading above them, so they must
// be recognised by shape rather than by section.
func TestCleanMarkdownDropsUnheadedReferenceEntries(t *testing.T) {
	input := `## Environmental impact

Mining has significant local effects.

[↑](#cite_ref-237) ["Thacker Pass Lithium mine approval"](https://example.com/a)
[↑](#cite_ref-224) Monnighoff, Xaver; Friesen, Alex (June 2020)`

	got := CleanMarkdown(input)

	if !strings.Contains(got, "Mining has significant local effects.") {
		t.Errorf("dropped real content: %q", got)
	}
	for _, unwanted := range []string{"Thacker Pass", "Monnighoff", "cite_ref"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("reference entry survived (%q): %q", unwanted, got)
		}
	}
}

func TestCleanMarkdownDropsBoilerplateSections(t *testing.T) {
	input := `# Article

Real content here.

## References

Reference one.
Reference two.

## External links

A link.

## Conclusion

Content that must survive.`

	got := CleanMarkdown(input)

	if !strings.Contains(got, "Real content here.") {
		t.Errorf("lost body content: %q", got)
	}
	// A section at the same level ends the skip, so later real content returns.
	if !strings.Contains(got, "Content that must survive.") {
		t.Errorf("skipping did not stop at the next heading: %q", got)
	}
	for _, unwanted := range []string{"Reference one", "Reference two", "A link"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("boilerplate survived (%q): %q", unwanted, got)
		}
	}
}

// A nested subsection under References must also go, rather than ending the
// skip early because it is a heading.
func TestCleanMarkdownSkipsNestedBoilerplateSubsections(t *testing.T) {
	input := `## References

### Primary sources

Source A.

### Secondary sources

Source B.

## Real Section

Keep this.`

	got := CleanMarkdown(input)

	for _, unwanted := range []string{"Source A", "Source B", "Primary sources"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("nested boilerplate survived (%q): %q", unwanted, got)
		}
	}
	if !strings.Contains(got, "Keep this.") {
		t.Errorf("lost content after boilerplate: %q", got)
	}
}

func TestCleanMarkdownIsCaseInsensitiveOnHeadings(t *testing.T) {
	input := "## REFERENCES\n\nRef text.\n\n## Body\n\nKeep."

	got := CleanMarkdown(input)

	if strings.Contains(got, "Ref text") {
		t.Errorf("uppercase heading not matched: %q", got)
	}
	if !strings.Contains(got, "Keep.") {
		t.Errorf("lost content: %q", got)
	}
}

func TestCleanMarkdownCollapsesBlankLines(t *testing.T) {
	got := CleanMarkdown("One.\n\n\n\n\nTwo.")

	if strings.Contains(got, "\n\n\n") {
		t.Errorf("blank lines not collapsed: %q", got)
	}
}
