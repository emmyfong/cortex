package extract

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These cases are the shapes an 8B model actually produces — sentences instead
// of names, duplicates, empty summaries, quoted strings.
func TestCleanFiltersBadModelOutput(t *testing.T) {
	tests := []struct {
		name string
		raw  []Concept
		want []string // expected names, in order
	}{
		{
			name: "keeps well-formed concepts",
			raw: []Concept{
				{Name: "Battery Degradation", Summary: "Capacity loss over charge cycles."},
				{Name: "Solid Electrolyte", Summary: "A non-liquid ion conductor."},
			},
			want: []string{"Battery Degradation", "Solid Electrolyte"},
		},
		{
			name: "drops a sentence masquerading as a name",
			raw: []Concept{
				{Name: "The passage explains how batteries lose capacity over many cycles", Summary: "x"},
				{Name: "Thermal Runaway", Summary: "Self-sustaining overheating."},
			},
			want: []string{"Thermal Runaway"},
		},
		{
			name: "drops generic filler terms",
			raw: []Concept{
				{Name: "Data", Summary: "Some data."},
				{Name: "Introduction", Summary: "The intro."},
				{Name: "Methodology", Summary: "How it was done."},
				{Name: "Ion Transport", Summary: "Movement of ions through the cell."},
			},
			want: []string{"Ion Transport"},
		},
		{
			name: "deduplicates case-insensitively",
			raw: []Concept{
				{Name: "Battery Degradation", Summary: "First."},
				{Name: "battery degradation", Summary: "Second."},
				{Name: "BATTERY DEGRADATION", Summary: "Third."},
			},
			want: []string{"Battery Degradation"},
		},
		{
			name: "drops concepts with no summary",
			raw: []Concept{
				{Name: "Nameless Thing", Summary: "   "},
				{Name: "Real Concept", Summary: "It means something."},
			},
			want: []string{"Real Concept"},
		},
		{
			name: "strips surrounding quotes and trailing punctuation",
			raw: []Concept{
				{Name: `"Thermal Runaway."`, Summary: "Overheating."},
			},
			want: []string{"Thermal Runaway"},
		},
		{
			name: "collapses internal whitespace",
			raw: []Concept{
				{Name: "Solid   State\tBattery", Summary: "A  battery   type."},
			},
			want: []string{"Solid State Battery"},
		},
		{
			name: "drops names with no letters",
			raw: []Concept{
				{Name: "2024", Summary: "A year."},
				{Name: "42", Summary: "A number."},
				{Name: "Real One", Summary: "Fine."},
			},
			want: []string{"Real One"},
		},
		{
			name: "drops single characters",
			raw:  []Concept{{Name: "A", Summary: "A letter."}},
			want: nil,
		},
		{
			name: "empty input yields nothing",
			raw:  nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Clean(tt.raw)

			if len(got) != len(tt.want) {
				t.Fatalf("got %d concepts %v, want %d %v", len(got), names(got), len(tt.want), tt.want)
			}
			for i, want := range tt.want {
				if got[i].Name != want {
					t.Errorf("concept %d = %q, want %q", i, got[i].Name, want)
				}
			}
		})
	}
}

// An unbounded concept list would let one runaway passage dominate the graph.
func TestCleanCapsConceptCount(t *testing.T) {
	var raw []Concept
	for i := range 20 {
		raw = append(raw, Concept{
			Name:    "Concept Number " + string(rune('A'+i)),
			Summary: "A summary.",
		})
	}

	got := Clean(raw)

	if len(got) != maxConceptsPerChunk {
		t.Errorf("got %d concepts, want the cap of %d", len(got), maxConceptsPerChunk)
	}
}

func TestCleanTruncatesLongSummaries(t *testing.T) {
	raw := []Concept{{Name: "Verbose Concept", Summary: strings.Repeat("word ", 400)}}

	got := Clean(raw)

	if len(got) != 1 {
		t.Fatalf("got %d concepts, want 1", len(got))
	}
	if len(got[0].Summary) > maxSummaryLength {
		t.Errorf("summary length = %d, want at most %d", len(got[0].Summary), maxSummaryLength)
	}
}

func TestSlug(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"Battery Degradation", "battery-degradation"},
		{"Solid-State Battery", "solid-state-battery"},
		{"C++ Templates", "c-templates"},
		{"  Leading and trailing  ", "leading-and-trailing"},
		{"Multiple   Spaces", "multiple-spaces"},
		{"UPPERCASE", "uppercase"},
		{"Ion Transport (Fast)", "ion-transport-fast"},
		{"???", "concept"},
		{"", "concept"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Slug(tt.name); got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestSlugIsBounded(t *testing.T) {
	slug := Slug(strings.Repeat("verylongword ", 40))

	if len(slug) > 100 {
		t.Errorf("slug length = %d, want at most 100", len(slug))
	}
	if strings.HasSuffix(slug, "-") {
		t.Errorf("slug has a trailing hyphen: %q", slug)
	}
}

// fakeOllama serves a canned chat response.
func fakeOllama(t *testing.T, content string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(ollamaResponse{
			Message: ollamaMessage{Role: "assistant", Content: content},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFromPassageParsesAndCleans(t *testing.T) {
	srv := fakeOllama(t, `{"concepts":[
		{"name":"Battery Degradation","summary":"Capacity loss over cycles."},
		{"name":"Data","summary":"Generic filler."}
	]}`, http.StatusOK)

	got, err := New(srv.URL, "llama3.1:8b").FromPassage(t.Context(), "some passage text")

	if err != nil {
		t.Fatalf("FromPassage() = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Name != "Battery Degradation" {
		t.Errorf("got %v, want only Battery Degradation (Data is filtered)", names(got))
	}
}

// A passage that yields nothing is normal, not an error — reference lists and
// boilerplate should produce no concepts.
func TestFromPassageEmptyResultIsNotAnError(t *testing.T) {
	srv := fakeOllama(t, `{"concepts":[]}`, http.StatusOK)

	got, err := New(srv.URL, "llama3.1:8b").FromPassage(t.Context(), "[1] Smith, J. (2020). Some Paper.")

	if err != nil {
		t.Errorf("FromPassage() = %v, want nil for an empty extraction", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d concepts, want 0", len(got))
	}
}

func TestFromPassageBlankInputSkipsTheModel(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "m").FromPassage(t.Context(), "   \n  ")

	if err != nil || len(got) != 0 {
		t.Errorf("FromPassage(blank) = (%v, %v), want (nil, nil)", got, err)
	}
	if called {
		t.Error("called the model for a blank passage")
	}
}

func TestFromPassageSurfacesOllamaError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(ollamaResponse{Error: `model "ghost" not found, try pulling it first`})
	}))
	defer srv.Close()

	_, err := New(srv.URL, "ghost").FromPassage(t.Context(), "text")

	if err == nil {
		t.Fatal("FromPassage() = nil, want error")
	}
	// The user needs Ollama's own wording to know what to pull.
	if !strings.Contains(err.Error(), "not found, try pulling it first") {
		t.Errorf("ollama's message was swallowed: %v", err)
	}
}

func TestFromPassageRejectsMalformedJSON(t *testing.T) {
	srv := fakeOllama(t, `{"concepts": [{"name": "Truncated`, http.StatusOK)

	_, err := New(srv.URL, "llama3.1:8b").FromPassage(t.Context(), "text")

	if err == nil {
		t.Fatal("FromPassage() accepted truncated JSON, want error")
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("error should name the problem: %v", err)
	}
}

// The request must pin an explicit context window: Ollama's default is small
// and silently truncates the prompt, producing confident nonsense.
func TestRequestPinsContextWindowAndZeroTemperature(t *testing.T) {
	var received ollamaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaResponse{
			Message: ollamaMessage{Content: `{"concepts":[]}`},
		})
	}))
	defer srv.Close()

	_, _ = New(srv.URL, "llama3.1:8b").FromPassage(t.Context(), "passage")

	if received.Options.NumCtx != contextWindow {
		t.Errorf("num_ctx = %d, want %d", received.Options.NumCtx, contextWindow)
	}
	if received.Options.Temperature != 0 {
		t.Errorf("temperature = %v, want 0 for repeatable extraction", received.Options.Temperature)
	}
	if received.Stream {
		t.Error("stream = true, want false for a single structured response")
	}
	if len(received.Format) == 0 {
		t.Error("no JSON schema sent; output shape would be unconstrained")
	}
}

func names(concepts []Concept) []string {
	out := make([]string, len(concepts))
	for i, c := range concepts {
		out[i] = c.Name
	}
	return out
}

// Surface variants of one concept must fold together, or the graph fragments:
// a real article split its central concept across five nodes this way.
func TestCanonicalize(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		// Plurals — the dominant duplicate source.
		{"Solid-State Batteries", "Solid State Battery"},
		{"Solid-State Battery", "Solid State Battery"},
		{"Energy Densities", "Energy Density"},
		{"Energy Density", "Energy Density"},
		{"Lithium-Ion Batteries", "Lithium Ion Battery"},
		{"Solid Electrolytes", "Solid Electrolyte"},
		{"Processes", "Process"},

		// Words that merely end in s and must survive intact.
		{"Ionic Conductivity", "Ionic Conductivity"},
		{"Thermal Analysis", "Thermal Analysis"},
		{"Time Series", "Time Series"},
		{"Ideal Gas", "Ideal Gas"},
		{"Charge Bias", "Charge Bias"},
		{"Solid Mass", "Solid Mass"},
		{"Plasma Physics", "Plasma Physics"},

		// Short words are left alone rather than mangled.
		{"Ion", "Ion"},
		{"Gas", "Gas"},

		// Whitespace is collapsed; the trailing word is still singularized,
		// which is the point of the function.
		{"  Extra   Spaces  ", "Extra Space"},
		{"  Ionic   Conductivity  ", "Ionic Conductivity"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Canonicalize(tt.name); got != tt.want {
				t.Errorf("Canonicalize(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// The end-to-end effect: variants arriving from different passages collapse to
// one concept rather than several.
func TestCleanMergesSurfaceVariants(t *testing.T) {
	got := Clean([]Concept{
		{Name: "Solid-State Batteries", Summary: "First mention."},
		{Name: "Solid State Battery", Summary: "Second mention."},
		{Name: "solid-state batteries", Summary: "Third mention."},
	})

	if len(got) != 1 {
		t.Fatalf("got %d concepts %v, want them merged into 1", len(got), names(got))
	}
	if got[0].Name != "Solid State Battery" {
		t.Errorf("merged name = %q, want the canonical singular form", got[0].Name)
	}
}
