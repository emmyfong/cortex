package chunk

import (
	"strings"
	"testing"
)

func testSplitter(t *testing.T, maxTokens, overlap int) *Splitter {
	t.Helper()
	s, err := NewSplitter(maxTokens, overlap)
	if err != nil {
		t.Fatalf("NewSplitter(%d, %d): %v", maxTokens, overlap, err)
	}
	return s
}

func TestNewSplitterRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name      string
		maxTokens int
		overlap   int
	}{
		{"zero max", 0, 10},
		{"negative max", -1, 10},
		{"negative overlap", 600, -1},
		// Overlap >= max means every chunk re-emits the whole previous chunk,
		// so the splitter can never move forward.
		{"overlap equal to max", 600, 600},
		{"overlap larger than max", 600, 900},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewSplitter(tt.maxTokens, tt.overlap); err == nil {
				t.Errorf("NewSplitter(%d, %d) = nil error, want error",
					tt.maxTokens, tt.overlap)
			}
		})
	}
}

// Splitting on headers is the whole point: a chunk should be one coherent
// section, not an arbitrary slice of the document.
func TestSplitsOnMarkdownHeaders(t *testing.T) {
	markdown := `# Title

Intro paragraph.

## Battery Degradation

Heat accelerates capacity loss.

## Consumer Incentives

Rebates shift demand.`

	chunks := testSplitter(t, 600, 50).Split(markdown)

	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3:\n%s", len(chunks), formatChunks(chunks))
	}

	wantHeadings := []string{"Title", "Title > Battery Degradation", "Title > Consumer Incentives"}
	for i, want := range wantHeadings {
		if chunks[i].HeadingPath != want {
			t.Errorf("chunk %d heading = %q, want %q", i, chunks[i].HeadingPath, want)
		}
	}

	if !strings.Contains(chunks[1].Content, "Heat accelerates") {
		t.Errorf("chunk 1 lost its body text: %q", chunks[1].Content)
	}
	// Content from a different section must not bleed in.
	if strings.Contains(chunks[1].Content, "Rebates shift") {
		t.Errorf("chunk 1 leaked the next section: %q", chunks[1].Content)
	}
}

func TestHeadingPathTracksNesting(t *testing.T) {
	markdown := `# Guide

## Batteries

### Thermal Limits

Above forty degrees the cell degrades faster.

## Policy

Subsidies matter.`

	chunks := testSplitter(t, 600, 50).Split(markdown)

	paths := make([]string, len(chunks))
	for i, c := range chunks {
		paths[i] = c.HeadingPath
	}

	// Descending to Policy (h2) must pop the h3 off the path, not append to it.
	wantLast := "Guide > Policy"
	if paths[len(paths)-1] != wantLast {
		t.Errorf("last heading path = %q, want %q (full: %v)",
			paths[len(paths)-1], wantLast, paths)
	}

	var found bool
	for _, p := range paths {
		if p == "Guide > Batteries > Thermal Limits" {
			found = true
		}
	}
	if !found {
		t.Errorf("nested h3 path missing, got paths: %v", paths)
	}
}

// A section longer than the budget must be broken up rather than emitted whole,
// otherwise one huge chunk produces a blurry embedding that matches nothing.
func TestOversizedSectionIsSplitOnParagraphs(t *testing.T) {
	paragraph := strings.Repeat("word ", 100) // ~100 words, ~130 tokens
	body := strings.Join([]string{paragraph, paragraph, paragraph, paragraph, paragraph}, "\n\n")
	markdown := "## Long Section\n\n" + body

	splitter := testSplitter(t, 200, 20)
	chunks := splitter.Split(markdown)

	if len(chunks) < 2 {
		t.Fatalf("oversized section produced %d chunk(s), want it split", len(chunks))
	}

	for i, c := range chunks {
		// Allow one paragraph of headroom: an indivisible paragraph larger than
		// the budget is emitted whole rather than cut mid-sentence.
		if c.TokenCount > 400 {
			t.Errorf("chunk %d has %d tokens, far above the 200 budget", i, c.TokenCount)
		}
		if c.HeadingPath != "Long Section" {
			t.Errorf("chunk %d lost its heading path: %q", i, c.HeadingPath)
		}
	}
}

// Overlap is what keeps an idea that straddles a cut point retrievable.
func TestOverlapRepeatsTailOfPreviousChunk(t *testing.T) {
	var paragraphs []string
	for i := range 6 {
		paragraphs = append(paragraphs,
			strings.Repeat("alpha ", 40)+"marker"+string(rune('A'+i)))
	}
	markdown := "## Section\n\n" + strings.Join(paragraphs, "\n\n")

	withOverlap := testSplitter(t, 120, 40).Split(markdown)
	noOverlap := testSplitter(t, 120, 0).Split(markdown)

	if len(withOverlap) < 2 || len(noOverlap) < 2 {
		t.Fatalf("need multiple chunks to compare overlap; got %d and %d",
			len(withOverlap), len(noOverlap))
	}

	totalWith := 0
	for _, c := range withOverlap {
		totalWith += c.TokenCount
	}
	totalWithout := 0
	for _, c := range noOverlap {
		totalWithout += c.TokenCount
	}

	// Overlap duplicates content, so total emitted tokens must exceed the
	// non-overlapping total.
	if totalWith <= totalWithout {
		t.Errorf("overlap did not duplicate any content: with=%d without=%d",
			totalWith, totalWithout)
	}
}

func TestChunkIndicesAreSequentialFromZero(t *testing.T) {
	markdown := "# A\n\ntext\n\n## B\n\ntext\n\n## C\n\ntext"

	chunks := testSplitter(t, 600, 50).Split(markdown)

	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("chunk at position %d has Index %d", i, c.Index)
		}
	}
}

func TestEmptyAndWhitespaceInputProduceNoChunks(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"spaces", "   "},
		{"newlines", "\n\n\n"},
		// A heading with no body carries no retrievable content.
		{"heading only", "## Heading With No Body\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := testSplitter(t, 600, 50).Split(tt.input); len(got) != 0 {
				t.Errorf("Split(%q) produced %d chunks, want 0:\n%s",
					tt.input, len(got), formatChunks(got))
			}
		})
	}
}

func TestUnheadedDocumentIsChunked(t *testing.T) {
	markdown := strings.Repeat("Plain prose with no headings at all. ", 200)

	chunks := testSplitter(t, 150, 20).Split(markdown)

	if len(chunks) < 2 {
		t.Fatalf("unheaded document produced %d chunk(s), want several", len(chunks))
	}
	for i, c := range chunks {
		if strings.TrimSpace(c.Content) == "" {
			t.Errorf("chunk %d is empty", i)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantMin int
		wantMax int
	}{
		{"empty", "", 0, 0},
		{"single word", "hello", 1, 3},
		// ~10 words should land near 13 tokens; assert a band, not a value,
		// since this is deliberately an estimate.
		{"ten words", strings.Repeat("word ", 10), 8, 20},
		{"hundred words", strings.Repeat("word ", 100), 90, 180},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateTokens(tt.text)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("EstimateTokens(%d chars) = %d, want between %d and %d",
					len(tt.text), got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func formatChunks(chunks []Chunk) string {
	var sb strings.Builder
	for _, c := range chunks {
		content := c.Content
		if len(content) > 60 {
			content = content[:60] + "..."
		}
		sb.WriteString("  [" + c.HeadingPath + "] " + content + "\n")
	}
	return sb.String()
}
