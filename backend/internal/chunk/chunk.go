// Package chunk splits markdown into retrieval-sized passages.
//
// The strategy is header-aware: cut on markdown headings first, so each chunk
// is one coherent section rather than an arbitrary slice. A section that still
// exceeds the token budget is split again on paragraph boundaries, with a
// sliding overlap so an idea spanning a cut survives intact in at least one
// chunk.
package chunk

import (
	"fmt"
	"strings"
)

// Chunk is one retrieval unit.
type Chunk struct {
	Index       int
	Content     string
	TokenCount  int
	HeadingPath string // e.g. "Guide > Batteries > Thermal Limits"
}

// Splitter turns markdown into chunks according to a token budget.
type Splitter struct {
	maxTokens int
	overlap   int
}

// NewSplitter validates the budget up front. Overlap must be strictly less than
// maxTokens, otherwise each chunk would re-emit the whole previous chunk and the
// splitter could never advance through the document.
func NewSplitter(maxTokens, overlap int) (*Splitter, error) {
	if maxTokens < 1 {
		return nil, fmt.Errorf("maxTokens must be positive, got %d", maxTokens)
	}
	if overlap < 0 {
		return nil, fmt.Errorf("overlap cannot be negative, got %d", overlap)
	}
	if overlap >= maxTokens {
		return nil, fmt.Errorf("overlap (%d) must be less than maxTokens (%d)", overlap, maxTokens)
	}
	return &Splitter{maxTokens: maxTokens, overlap: overlap}, nil
}

// Split converts markdown into chunks. Sections with no body text are dropped:
// a bare heading carries nothing retrievable.
func (s *Splitter) Split(markdown string) []Chunk {
	sections := splitIntoSections(markdown)

	var chunks []Chunk
	for _, sec := range sections {
		body := strings.TrimSpace(sec.body)
		if body == "" {
			continue
		}
		for _, piece := range s.splitSection(body) {
			chunks = append(chunks, Chunk{
				Index:       len(chunks),
				Content:     piece,
				TokenCount:  EstimateTokens(piece),
				HeadingPath: sec.headingPath,
			})
		}
	}
	return chunks
}

// section is a run of body text under one heading path.
type section struct {
	headingPath string
	body        string
}

// splitIntoSections walks the document line by line, tracking the heading stack
// so each section knows its full ancestry.
func splitIntoSections(markdown string) []section {
	lines := strings.Split(markdown, "\n")

	var (
		sections []section
		stack    []string // heading text, indexed by level-1
		body     strings.Builder
		inFence  bool
	)

	flush := func(path string) {
		if strings.TrimSpace(body.String()) != "" {
			sections = append(sections, section{headingPath: path, body: body.String()})
		}
		body.Reset()
	}

	for _, line := range lines {
		// A "#" inside a fenced code block is code, not a heading.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			body.WriteString(line + "\n")
			continue
		}

		level, text, isHeading := parseHeading(line)
		if isHeading && !inFence {
			flush(joinPath(stack))

			// Truncate deeper levels so descending from h3 to h2 replaces the
			// h3 rather than nesting under it.
			if level-1 < len(stack) {
				stack = stack[:level-1]
			}
			for len(stack) < level-1 {
				stack = append(stack, "")
			}
			stack = append(stack, text)
			continue
		}

		body.WriteString(line + "\n")
	}
	flush(joinPath(stack))

	return sections
}

// parseHeading recognises ATX headings (#, ##, ...) up to level 6.
func parseHeading(line string) (level int, text string, ok bool) {
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
	// "#text" is not a heading in CommonMark; a space is required.
	if trimmed[hashes] != ' ' && trimmed[hashes] != '\t' {
		return 0, "", false
	}

	return hashes, strings.TrimSpace(trimmed[hashes:]), true
}

func joinPath(stack []string) string {
	parts := make([]string, 0, len(stack))
	for _, s := range stack {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " > ")
}

// splitSection breaks one section's body into pieces that fit the budget,
// packing whole paragraphs and carrying an overlap tail between pieces.
func (s *Splitter) splitSection(body string) []string {
	if EstimateTokens(body) <= s.maxTokens {
		return []string{body}
	}

	// Expand any single paragraph that already blows the budget. Without this,
	// a wall of text with no blank lines — the normal shape of PDF-extracted
	// prose — would become one enormous chunk with a useless embedding.
	var paragraphs []string
	for _, para := range splitParagraphs(body) {
		if EstimateTokens(para) > s.maxTokens {
			paragraphs = append(paragraphs, s.splitOversized(para)...)
			continue
		}
		paragraphs = append(paragraphs, para)
	}

	var (
		pieces  []string
		current []string
		tokens  int
	)

	for _, para := range paragraphs {
		paraTokens := EstimateTokens(para)

		// Flush before adding a paragraph that would overflow the budget, but
		// never flush an empty buffer.
		if tokens+paraTokens > s.maxTokens && len(current) > 0 {
			pieces = append(pieces, strings.Join(current, "\n\n"))
			current, tokens = s.overlapTail(current)
		}

		current = append(current, para)
		tokens += paraTokens
	}

	if len(current) > 0 {
		pieces = append(pieces, strings.Join(current, "\n\n"))
	}
	return pieces
}

// splitOversized breaks a single over-budget paragraph into smaller units,
// preferring sentence boundaries and falling back to a hard word cut for text
// with no sentence punctuation at all.
func (s *Splitter) splitOversized(para string) []string {
	var (
		pieces  []string
		current []string
		tokens  int
	)

	for _, sentence := range splitSentences(para) {
		sentenceTokens := EstimateTokens(sentence)

		if tokens+sentenceTokens > s.maxTokens && len(current) > 0 {
			pieces = append(pieces, strings.Join(current, " "))
			current, tokens = nil, 0
		}

		// A single sentence over budget still has to be cut somewhere.
		if sentenceTokens > s.maxTokens {
			pieces = append(pieces, s.splitWords(sentence)...)
			continue
		}

		current = append(current, sentence)
		tokens += sentenceTokens
	}

	if len(current) > 0 {
		pieces = append(pieces, strings.Join(current, " "))
	}
	return pieces
}

// splitWords is the last resort: cut on whitespace at the token budget.
func (s *Splitter) splitWords(text string) []string {
	words := strings.Fields(text)
	perChunk := int(float64(s.maxTokens) / tokensPerWord)
	if perChunk < 1 {
		perChunk = 1
	}

	var pieces []string
	for start := 0; start < len(words); start += perChunk {
		end := min(start+perChunk, len(words))
		pieces = append(pieces, strings.Join(words[start:end], " "))
	}
	return pieces
}

// splitSentences divides on terminal punctuation followed by whitespace. This is
// intentionally simple: it can mis-split on abbreviations ("Fig. 3"), which
// costs a slightly odd boundary and nothing more.
func splitSentences(text string) []string {
	var (
		sentences []string
		current   strings.Builder
	)

	runes := []rune(text)
	for i, r := range runes {
		current.WriteRune(r)

		if r != '.' && r != '!' && r != '?' {
			continue
		}
		// Only break when whitespace follows, so "3.14" stays intact.
		if i+1 < len(runes) && runes[i+1] != ' ' && runes[i+1] != '\n' && runes[i+1] != '\t' {
			continue
		}
		if s := strings.TrimSpace(current.String()); s != "" {
			sentences = append(sentences, s)
		}
		current.Reset()
	}

	if s := strings.TrimSpace(current.String()); s != "" {
		sentences = append(sentences, s)
	}
	return sentences
}

// overlapTail returns the trailing paragraphs of a flushed piece that should be
// repeated at the start of the next one, plus their token count.
func (s *Splitter) overlapTail(paragraphs []string) ([]string, int) {
	if s.overlap == 0 {
		return nil, 0
	}

	var (
		tail   []string
		tokens int
	)
	for i := len(paragraphs) - 1; i >= 0; i-- {
		t := EstimateTokens(paragraphs[i])
		// Stop before exceeding the overlap budget, unless nothing is carried
		// yet — one paragraph of context beats none.
		if tokens+t > s.overlap && len(tail) > 0 {
			break
		}
		tail = append([]string{paragraphs[i]}, tail...)
		tokens += t
		if tokens >= s.overlap {
			break
		}
	}
	return tail, tokens
}

// splitParagraphs divides on blank lines, falling back to single lines when a
// block has none — some extracted PDFs arrive as one line per sentence.
func splitParagraphs(body string) []string {
	raw := strings.Split(body, "\n\n")

	var paragraphs []string
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		paragraphs = append(paragraphs, p)
	}

	if len(paragraphs) <= 1 {
		var lines []string
		for _, l := range strings.Split(body, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		if len(lines) > 1 {
			return lines
		}
	}
	return paragraphs
}

// tokensPerWord approximates the wordpiece tokenizer used by nomic-embed-text.
// English averages roughly 1.3 subword tokens per whitespace-delimited word.
const tokensPerWord = 1.3

// EstimateTokens approximates the token count of a string.
//
// This is deliberately an estimate, not an exact count. Chunk sizing only needs
// to be roughly right — the model's context window is 8192 tokens, far above
// any chunk we produce — and an exact count would mean vendoring the model's
// tokenizer for no practical gain.
func EstimateTokens(text string) int {
	words := len(strings.Fields(text))
	if words == 0 {
		return 0
	}
	return int(float64(words) * tokensPerWord)
}
