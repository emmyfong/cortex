// Package extract pulls named concepts out of passages using a local LLM.
//
// This is the fuzziest stage of the pipeline: an LLM decides what counts as a
// concept. Everything here is therefore defensive — the model's output is
// treated as untrusted input, validated, normalised, and capped before any of
// it reaches the database.
package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Limits on what the model is allowed to return. A local 8B model will
// occasionally emit a whole sentence as a "concept name", or repeat one concept
// twenty times; these bounds keep that out of the graph.
const (
	maxConceptsPerChunk = 5
	maxNameLength       = 80
	maxNameWords        = 6
	maxSummaryLength    = 400
	minNameLength       = 2
)

// requestTimeout is generous: an 8B model on a laptop GPU takes seconds per
// passage, and a cold model load can take much longer than that.
const requestTimeout = 3 * time.Minute

// contextWindow must fit the system prompt plus one passage. Ollama defaults to
// a small window and silently truncates the prompt beyond it, which produces
// confidently wrong extractions rather than an error.
const contextWindow = 8192

// Concept is one idea the model found in a passage.
type Concept struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

// Client calls a local Ollama model to extract concepts.
type Client struct {
	baseURL string
	model   string
	http    *http.Client
}

func New(baseURL, model string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http:    &http.Client{Timeout: requestTimeout},
	}
}

// Model reports which model this client extracts with.
func (c *Client) Model() string { return c.model }

const systemPrompt = `You extract key concepts from text to build a knowledge graph.

A concept is a specific idea, mechanism, technology, method, or phenomenon that the passage explains or discusses substantively.

Rules:
- Extract at most 5 concepts. Fewer is better. If the passage is boilerplate, a reference list, or discusses nothing substantive, return an empty list.
- Only extract a concept if the passage says something about it. Ignore passing mentions.
- "name" must be a canonical noun phrase of 1 to 4 words, in Title Case. Not a sentence.
- "summary" must be one sentence describing the concept as this passage uses it.
- Do not extract generic terms like "Data", "System", "Research", "Introduction", or "Method".
- Do not extract people, organisations, dates, or citations.`

type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Format   json.RawMessage `json:"format,omitempty"`
	Options  ollamaOptions   `json:"options"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaOptions struct {
	// Temperature 0 keeps extraction as repeatable as a local model manages;
	// the same document should not produce a different graph on every run.
	Temperature float64 `json:"temperature"`
	NumCtx      int     `json:"num_ctx"`
}

type ollamaResponse struct {
	Message ollamaMessage `json:"message"`
	Error   string        `json:"error,omitempty"`
}

// conceptSchema constrains the model's output shape. Ollama enforces it during
// decoding, which removes a whole class of "the model wrote prose instead of
// JSON" failures rather than leaving them to be parsed around.
var conceptSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "concepts": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name":    {"type": "string"},
          "summary": {"type": "string"}
        },
        "required": ["name", "summary"]
      }
    }
  },
  "required": ["concepts"]
}`)

type conceptEnvelope struct {
	Concepts []Concept `json:"concepts"`
}

// FromPassage extracts concepts from one passage.
//
// An empty result is a normal outcome, not an error: plenty of passages are
// boilerplate, tables, or reference lists with nothing worth graphing.
func (c *Client) FromPassage(ctx context.Context, passage string) ([]Concept, error) {
	passage = strings.TrimSpace(passage)
	if passage == "" {
		return nil, nil
	}

	payload, err := json.Marshal(ollamaRequest{
		Model:  c.model,
		Stream: false,
		Format: conceptSchema,
		Messages: []ollamaMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: "Extract concepts from this passage:\n\n" + passage},
		},
		Options: ollamaOptions{Temperature: 0, NumCtx: contextWindow},
	})
	if err != nil {
		return nil, fmt.Errorf("encode extraction request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build extraction request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call ollama at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	var decoded ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode ollama response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if decoded.Error != "" {
			// Surface Ollama's wording — "model not found" tells the user
			// exactly what to pull.
			return nil, fmt.Errorf("ollama returned %d: %s", resp.StatusCode, decoded.Error)
		}
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var envelope conceptEnvelope
	if err := json.Unmarshal([]byte(decoded.Message.Content), &envelope); err != nil {
		// Schema-constrained decoding makes this rare, but a model can still
		// stop early and produce truncated JSON.
		return nil, fmt.Errorf("model %q did not return valid JSON: %w", c.model, err)
	}

	return Clean(envelope.Concepts), nil
}

var (
	whitespacePattern = regexp.MustCompile(`\s+`)
	nonSlugPattern    = regexp.MustCompile(`[^a-z0-9]+`)
)

// Clean normalises and filters raw model output.
//
// Exported so the rules can be tested directly against the kinds of output a
// small local model actually produces, without needing a live model.
func Clean(raw []Concept) []Concept {
	seen := make(map[string]bool, len(raw))
	cleaned := make([]Concept, 0, len(raw))

	for _, candidate := range raw {
		name := normalizeName(candidate.Name)
		if !validName(name) {
			continue
		}

		// The schema's own case-insensitive uniqueness would reject duplicates
		// at insert time; filtering here keeps that from becoming an error path.
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true

		summary := whitespacePattern.ReplaceAllString(strings.TrimSpace(candidate.Summary), " ")
		if summary == "" {
			// A concept with no explanation is not usable on a wiki page.
			continue
		}
		if len(summary) > maxSummaryLength {
			summary = summary[:maxSummaryLength]
		}

		cleaned = append(cleaned, Concept{Name: name, Summary: summary})
		if len(cleaned) == maxConceptsPerChunk {
			break
		}
	}

	return cleaned
}

func normalizeName(name string) string {
	name = whitespacePattern.ReplaceAllString(strings.TrimSpace(name), " ")
	// Models sometimes wrap names in quotes or end them with a full stop.
	name = strings.Trim(name, `"'.,;:`)
	return Canonicalize(strings.TrimSpace(name))
}

// Canonicalize folds trivial surface variants of the same concept together.
//
// Without this the graph fragments badly: a single article produced
// "Solid-State Batteries", "Solid-State Battery", and "Energy Densities"
// alongside "Energy Density" as separate nodes, splitting the connections of
// its most important concept across several weakly-linked ones. Case is already
// handled by the database's unique index on lower(name); this covers the two
// other variants a model produces constantly — plurals and hyphenation.
//
// It deliberately does not attempt semantic merging ("SSBs" with "Solid State
// Battery"): that needs embeddings and can merge things that should stay apart.
func Canonicalize(name string) string {
	// Hyphens are a stylistic choice the model makes inconsistently for the
	// same term, so they are folded to spaces.
	name = strings.ReplaceAll(name, "-", " ")
	name = whitespacePattern.ReplaceAllString(name, " ")
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	words := strings.Fields(name)
	words[len(words)-1] = singularize(words[len(words)-1])
	return strings.Join(words, " ")
}

// pluralExceptions are words whose singular form these rules would mangle.
var pluralExceptions = map[string]bool{
	"series": true, "species": true, "analysis": true, "basis": true,
	"physics": true, "mathematics": true, "electronics": true, "kinetics": true,
	"dynamics": true, "thermodynamics": true, "statistics": true, "ethics": true,
	"lens": true, "bias": true, "gas": true, "plus": true, "focus": true,
	"status": true, "apparatus": true, "corpus": true, "consensus": true,
}

// singularize applies regular English plural rules to one word.
func singularize(word string) string {
	lower := strings.ToLower(word)
	if len(lower) < 4 || pluralExceptions[lower] {
		return word
	}

	switch {
	case strings.HasSuffix(lower, "ies"):
		// batteries -> battery, densities -> density
		return word[:len(word)-3] + "y"
	case strings.HasSuffix(lower, "sses"),
		strings.HasSuffix(lower, "shes"),
		strings.HasSuffix(lower, "ches"),
		strings.HasSuffix(lower, "xes"),
		strings.HasSuffix(lower, "zes"):
		// processes -> process, matches -> match
		return word[:len(word)-2]
	case strings.HasSuffix(lower, "ss"),
		strings.HasSuffix(lower, "us"),
		strings.HasSuffix(lower, "is"),
		strings.HasSuffix(lower, "as"),
		strings.HasSuffix(lower, "os"):
		// mass, radius, analysis — already singular
		return word
	case strings.HasSuffix(lower, "s"):
		// electrolytes -> electrolyte
		return word[:len(word)-1]
	}
	return word
}

// validName rejects the shapes a small model reliably gets wrong: sentences,
// bare numbers, single letters, and generic filler.
func validName(name string) bool {
	if len(name) < minNameLength || len(name) > maxNameLength {
		return false
	}
	if len(strings.Fields(name)) > maxNameWords {
		return false
	}
	// A trailing sentence terminator survived trimming, so this is prose.
	if strings.ContainsAny(name, "?!\n") {
		return false
	}

	hasLetter := false
	for _, r := range name {
		if unicode.IsLetter(r) {
			hasLetter = true
			break
		}
	}
	if !hasLetter {
		return false
	}

	return !genericNames[strings.ToLower(name)]
}

// genericNames are terms that appear in almost any document and would connect
// everything to everything, which makes the graph useless rather than dense.
var genericNames = map[string]bool{
	"data": true, "system": true, "systems": true, "research": true,
	"introduction": true, "conclusion": true, "method": true, "methods": true,
	"methodology": true, "results": true, "discussion": true, "abstract": true,
	"overview": true, "background": true, "summary": true, "analysis": true,
	"approach": true, "technology": true, "information": true, "process": true,
	"references": true, "table": true, "figure": true, "example": true,
	"study": true, "paper": true, "article": true, "section": true,
}

// Slug renders a concept name as a URL-safe identifier.
func Slug(name string) string {
	slug := nonSlugPattern.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "concept"
	}
	if len(slug) > 100 {
		slug = strings.Trim(slug[:100], "-")
	}
	return slug
}
