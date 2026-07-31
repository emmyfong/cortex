package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Relationship judging turns candidate pairs into graph edges.
//
// Candidate generation is cheap and permissive — co-occurrence within a passage,
// embedding similarity across documents — and deliberately over-produces. This
// step is what makes an edge mean something: the model decides whether two
// concepts are actually related and says why, in the same spirit as a
// hand-written wikilink. Pairs it rejects are dropped, not stored weakly.

// judgeBatchSize bounds how many pairs go in one request. Larger batches are
// cheaper per pair but degrade quality — a small model asked about thirty pairs
// at once starts rubber-stamping them.
const judgeBatchSize = 8

// Candidate is a proposed relationship awaiting judgement.
type Candidate struct {
	AID      string
	AName    string
	ASummary string

	BID      string
	BName    string
	BSummary string

	// Origin records which generator proposed the pair.
	Origin string
}

// Relationship is an accepted edge with the reason it was accepted.
type Relationship struct {
	AID    string
	BID    string
	Reason string
	Origin string
}

const judgeSystemPrompt = `You decide whether pairs of concepts are genuinely related, for a knowledge graph.

Answer "related": true ONLY if you can name the specific link between them:
- One causes, produces, prevents, or degrades the other.
- One is a component, material, or measurable property of the other.
- One is a method or technique used to build, measure, or analyse the other.
- They are direct alternatives that trade off against each other.
- One is a specific type of the other.

Answer "related": false for everything else. In particular, these are NOT relationships:
- "Both are related to electric vehicles" — shared subject area is not a relationship.
- "Both are used in the same industry" — shared context is not a relationship.
- "One is a method, material, or measure used for the other" — this restates the
  question instead of answering it. Name the actual method or material.
- "They are discussed together" — proximity in a document is not a relationship.
- Anything where you cannot say what the link IS in concrete terms.

Be strict. Most pairs you are shown are NOT meaningfully related, including
pairs drawn from the same document. Rejecting a pair costs nothing; a graph
where everything connects to everything is useless.

The "reason" must be one short sentence naming the specific connection, using
the concepts' own words. If you cannot write such a sentence, the answer is
false.`

type judgeRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Format   json.RawMessage `json:"format,omitempty"`
	Options  ollamaOptions   `json:"options"`
}

var judgeSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "judgements": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "pair":    {"type": "integer"},
          "related": {"type": "boolean"},
          "reason":  {"type": "string"}
        },
        "required": ["pair", "related", "reason"]
      }
    }
  },
  "required": ["judgements"]
}`)

type judgement struct {
	Pair    int    `json:"pair"`
	Related bool   `json:"related"`
	Reason  string `json:"reason"`
}

type judgeEnvelope struct {
	Judgements []judgement `json:"judgements"`
}

// Judge filters candidates down to the pairs the model considers related.
//
// A batch that fails is skipped rather than failing the whole document: losing
// some edges is much better than losing every edge plus the concepts.
func (c *Client) Judge(ctx context.Context, candidates []Candidate) ([]Relationship, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	var accepted []Relationship
	var lastErr error
	batches := 0
	failures := 0

	for start := 0; start < len(candidates); start += judgeBatchSize {
		end := min(start+judgeBatchSize, len(candidates))
		batch := candidates[start:end]
		batches++

		judged, err := c.judgeBatch(ctx, batch)
		if err != nil {
			failures++
			lastErr = err
			continue
		}
		accepted = append(accepted, judged...)
	}

	// Every batch failing means the model is unreachable or misconfigured,
	// which is worth reporting rather than silently producing no edges.
	if failures == batches {
		return nil, fmt.Errorf("all %d judgement batches failed: %w", batches, lastErr)
	}
	return accepted, nil
}

func (c *Client) judgeBatch(ctx context.Context, batch []Candidate) ([]Relationship, error) {
	var prompt strings.Builder
	prompt.WriteString("Judge each pair:\n\n")
	for i, candidate := range batch {
		fmt.Fprintf(&prompt, "%d. %q (%s)  <->  %q (%s)\n",
			i+1,
			candidate.AName, candidate.ASummary,
			candidate.BName, candidate.BSummary)
	}

	payload, err := json.Marshal(judgeRequest{
		Model:  c.model,
		Stream: false,
		Format: judgeSchema,
		Messages: []ollamaMessage{
			{Role: "system", Content: judgeSystemPrompt},
			{Role: "user", Content: prompt.String()},
		},
		Options: ollamaOptions{Temperature: 0, NumCtx: contextWindow},
	})
	if err != nil {
		return nil, fmt.Errorf("encode judge request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build judge request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call ollama at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	var decoded ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode judge response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if decoded.Error != "" {
			return nil, fmt.Errorf("ollama returned %d: %s", resp.StatusCode, decoded.Error)
		}
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var envelope judgeEnvelope
	if err := json.Unmarshal([]byte(decoded.Message.Content), &envelope); err != nil {
		return nil, fmt.Errorf("model %q did not return valid judgement JSON: %w", c.model, err)
	}

	return acceptJudgements(batch, envelope.Judgements), nil
}

// acceptJudgements maps the model's answers back onto the candidates.
//
// A pair the model omitted or indexed out of range is dropped rather than
// assumed related: silence is not consent for an edge.
func acceptJudgements(batch []Candidate, judgements []judgement) []Relationship {
	seen := make(map[int]bool, len(judgements))
	accepted := make([]Relationship, 0, len(judgements))

	for _, j := range judgements {
		index := j.Pair - 1
		if index < 0 || index >= len(batch) || seen[index] {
			continue
		}
		seen[index] = true

		if !j.Related {
			continue
		}

		reason := strings.TrimSpace(whitespacePattern.ReplaceAllString(j.Reason, " "))
		if !statesARelationship(reason) {
			continue
		}
		if len(reason) > maxSummaryLength {
			reason = reason[:maxSummaryLength]
		}

		candidate := batch[index]
		accepted = append(accepted, Relationship{
			AID:    candidate.AID,
			BID:    candidate.BID,
			Reason: reason,
			Origin: candidate.Origin,
		})
	}

	return accepted
}

// emptyReasons are the phrasings the model falls back on when it has accepted a
// pair it cannot actually justify. Each was observed on real output: two of
// them are the model echoing the rubric back verbatim, the rest assert a shared
// subject area rather than a relationship.
//
// This is a backstop for prompt instructions the model sometimes ignores. An
// edge whose only justification is one of these is exactly the boilerplate the
// judge exists to eliminate, so it is dropped rather than stored.
var emptyReasons = []string{
	"one is a method, material, or measure used for the other",
	"one is a cause, mechanism, component, or consequence",
	"both are related to",
	"both are used in",
	"both are associated with",
	"they are discussed together",
	"discussed together in",
	"appear in the same",
	"same document",
	"same field",
	"same industry",
	"related to each other",
}

// statesARelationship rejects reasons that assert a connection without naming
// one. A very short reason is also rejected: "They are related." is not a
// justification.
func statesARelationship(reason string) bool {
	const minReasonLength = 15

	if len(reason) < minReasonLength {
		return false
	}

	lower := strings.ToLower(reason)
	for _, empty := range emptyReasons {
		if strings.Contains(lower, empty) {
			return false
		}
	}
	return true
}
