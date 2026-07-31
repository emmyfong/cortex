package extract

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func candidates(n int) []Candidate {
	out := make([]Candidate, n)
	for i := range out {
		out[i] = Candidate{
			AID: "a" + string(rune('A'+i)), AName: "Concept A" + string(rune('A'+i)), ASummary: "Summary A.",
			BID: "b" + string(rune('A'+i)), BName: "Concept B" + string(rune('A'+i)), BSummary: "Summary B.",
			Origin: "cooccurrence",
		}
	}
	return out
}

// judgeServer replies with the given judgements for every batch.
func judgeServer(t *testing.T, reply func(batchSize int) []judgement) (*httptest.Server, *int) {
	t.Helper()
	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++

		var req judgeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Count the numbered lines to learn how many pairs were sent.
		batchSize := strings.Count(req.Messages[1].Content, "<->")

		payload, _ := json.Marshal(judgeEnvelope{Judgements: reply(batchSize)})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaResponse{
			Message: ollamaMessage{Role: "assistant", Content: string(payload)},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// The whole point of judging: rejected pairs must not become edges.
func TestJudgeDropsUnrelatedPairs(t *testing.T) {
	srv, _ := judgeServer(t, func(n int) []judgement {
		out := make([]judgement, n)
		for i := range out {
			// Accept only the first pair of each batch.
			out[i] = judgement{
				Pair:    i + 1,
				Related: i == 0,
				Reason:  "Ion transport determines the charge rate.",
			}
		}
		return out
	})

	got, err := New(srv.URL, "llama3.1:8b").Judge(t.Context(), candidates(4))

	if err != nil {
		t.Fatalf("Judge() = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("accepted %d relationships, want 1", len(got))
	}
	if !strings.Contains(got[0].Reason, "Ion transport") {
		t.Errorf("reason = %q, want the model's explanation", got[0].Reason)
	}
	if got[0].Origin != "cooccurrence" {
		t.Errorf("origin = %q, want it carried through from the candidate", got[0].Origin)
	}
}

// Silence is not consent: a pair the model never answered about must not become
// an edge by default.
func TestJudgeDropsUnansweredPairs(t *testing.T) {
	srv, _ := judgeServer(t, func(int) []judgement {
		// Answer only pair 1, whatever the batch size.
		return []judgement{{Pair: 1, Related: true, Reason: "Lithium ions migrate through the electrolyte."}}
	})

	got, err := New(srv.URL, "llama3.1:8b").Judge(t.Context(), candidates(5))

	if err != nil {
		t.Fatalf("Judge() = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Errorf("accepted %d relationships, want only the answered one", len(got))
	}
}

// An accepted edge with no stated reason reintroduces the boilerplate problem
// the judge exists to solve.
func TestJudgeDropsRelatedPairsWithNoReason(t *testing.T) {
	srv, _ := judgeServer(t, func(int) []judgement {
		return []judgement{
			{Pair: 1, Related: true, Reason: "   "},
			{Pair: 2, Related: true, Reason: "Heat accelerates electrolyte breakdown."},
		}
	})

	got, err := New(srv.URL, "llama3.1:8b").Judge(t.Context(), candidates(2))

	if err != nil {
		t.Fatalf("Judge() = %v, want nil", err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Reason, "Heat accelerates") {
		t.Errorf("got %+v, want only the pair with a real reason", got)
	}
}

func TestJudgeIgnoresOutOfRangeAndDuplicateIndices(t *testing.T) {
	srv, _ := judgeServer(t, func(int) []judgement {
		return []judgement{
			{Pair: 0, Related: true, Reason: "Index below range."},
			{Pair: 99, Related: true, Reason: "Index above range."},
			{Pair: 1, Related: true, Reason: "Dendrite growth degrades the separator."},
			{Pair: 1, Related: true, Reason: "Duplicate answer for the same pair index."},
		}
	})

	got, err := New(srv.URL, "llama3.1:8b").Judge(t.Context(), candidates(2))

	if err != nil {
		t.Fatalf("Judge() = %v, want nil", err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Reason, "Dendrite growth") {
		t.Errorf("got %+v, want only the single valid judgement", got)
	}
}

// Batching keeps prompts small enough that a local model stays discriminating.
func TestJudgeBatchesLargeCandidateSets(t *testing.T) {
	srv, calls := judgeServer(t, func(n int) []judgement {
		out := make([]judgement, n)
		for i := range out {
			out[i] = judgement{Pair: i + 1, Related: false, Reason: "no"}
		}
		return out
	})

	total := judgeBatchSize*2 + 3
	if _, err := New(srv.URL, "llama3.1:8b").Judge(t.Context(), candidates(total)); err != nil {
		t.Fatalf("Judge() = %v, want nil", err)
	}

	if *calls != 3 {
		t.Errorf("made %d requests for %d candidates, want 3 batches", *calls, total)
	}
}

// One bad batch should cost its own edges, not the whole document's.
func TestJudgeToleratesOneFailedBatch(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(ollamaResponse{Error: "transient failure"})
			return
		}
		payload, _ := json.Marshal(judgeEnvelope{
			Judgements: []judgement{{Pair: 1, Related: true, Reason: "Cathode material determines the cell voltage."}},
		})
		_ = json.NewEncoder(w).Encode(ollamaResponse{
			Message: ollamaMessage{Content: string(payload)},
		})
	}))
	defer srv.Close()

	got, err := New(srv.URL, "llama3.1:8b").Judge(t.Context(), candidates(judgeBatchSize+2))

	if err != nil {
		t.Fatalf("Judge() = %v, want the surviving batch to succeed", err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Reason, "Cathode material") {
		t.Errorf("got %+v, want the second batch's edge", got)
	}
}

// Every batch failing means the model is unreachable, which must surface rather
// than looking like "no concepts are related".
func TestJudgeFailsWhenEveryBatchFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(ollamaResponse{Error: "model unavailable"})
	}))
	defer srv.Close()

	_, err := New(srv.URL, "llama3.1:8b").Judge(t.Context(), candidates(3))

	if err == nil {
		t.Fatal("Judge() = nil error when every batch failed, want an error")
	}
}

func TestJudgeEmptyInput(t *testing.T) {
	srv, calls := judgeServer(t, func(int) []judgement { return nil })

	got, err := New(srv.URL, "m").Judge(t.Context(), nil)

	if err != nil || len(got) != 0 {
		t.Errorf("Judge(nil) = (%v, %v), want (nil, nil)", got, err)
	}
	if *calls != 0 {
		t.Errorf("made %d requests for no candidates, want 0", *calls)
	}
}

// Every rejected string here came from real output: the model accepting a pair
// it could not justify, either by echoing the rubric or asserting a shared
// subject area. Those become boilerplate edges, which is what judging replaces.
func TestStatesARelationship(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   bool
	}{
		// Real connections, named concretely.
		{"type-of", "Chloride Solid Electrolyte is a type of Solid Electrolyte.", true},
		{"consequence", "Capacity Fade is a consequence of Solid State Battery design.", true},
		{"material used in", "Carbon nanotubes are a material used in battery electrodes.", true},
		{"causal", "Dendrite growth pierces the separator and causes short circuits.", true},

		// Observed failures — the rubric echoed back instead of answered.
		{"echoes rubric (method)", "One is a method, material, or measure used for the other.", false},
		{"echoes rubric (cause)", "One is a cause, mechanism, component, or consequence of the other.", false},

		// Observed failures — shared subject area asserted as a relationship.
		{"shared subject", "Both are related to electric vehicles and their benefits.", false},
		{"shared industry", "Both are used in the automotive industry.", false},
		{"proximity", "They are discussed together in the same section.", false},
		{"old boilerplate", "Discussed together in Solid-state battery — Overview", false},

		// Too short to be a justification.
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"terse", "Related.", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statesARelationship(tt.reason); got != tt.want {
				t.Errorf("statesARelationship(%q) = %v, want %v", tt.reason, got, tt.want)
			}
		})
	}
}

// The filter must apply through Judge, not only in isolation.
func TestJudgeDropsPairsWithHollowReasons(t *testing.T) {
	srv, _ := judgeServer(t, func(int) []judgement {
		return []judgement{
			{Pair: 1, Related: true, Reason: "Both are related to electric vehicles."},
			{Pair: 2, Related: true, Reason: "Lithium ions move through the solid electrolyte."},
		}
	})

	got, err := New(srv.URL, "llama3.1:8b").Judge(t.Context(), candidates(2))

	if err != nil {
		t.Fatalf("Judge() = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("accepted %d relationships, want only the one with a real reason", len(got))
	}
	if !strings.Contains(got[0].Reason, "Lithium ions") {
		t.Errorf("kept the wrong edge: %q", got[0].Reason)
	}
}
