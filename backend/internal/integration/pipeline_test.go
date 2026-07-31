package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emmyf/cortex/backend/internal/chunk"
	"github.com/emmyf/cortex/backend/internal/embed"
	"github.com/emmyf/cortex/backend/internal/events"
	"github.com/emmyf/cortex/backend/internal/extract"
	"github.com/emmyf/cortex/backend/internal/ingest"
	"github.com/emmyf/cortex/backend/internal/parse"
	"github.com/emmyf/cortex/backend/internal/store"
	"github.com/hibiken/asynq"
)

// newIngestHandler builds the real pipeline against the stub model. The queue
// client is nil so ingestion does not enqueue follow-up extraction — each test
// drives the stages it cares about explicitly.
func newIngestHandler(t *testing.T, stub *stubOllama, publisher *events.Publisher, doc parse.Document) *ingest.Handler {
	t.Helper()

	splitter, err := chunk.NewSplitter(600, 50)
	if err != nil {
		t.Fatalf("NewSplitter: %v", err)
	}

	parser := stubParser{doc: doc}

	return ingest.NewHandler(
		testStore,
		nil, // blob store unused: the stub parser needs no file
		nil, // no queue client
		parser,
		parser,
		embed.New(stub.URL, "stub-embed"),
		splitter,
		publisher,
		discardLogger(),
	)
}

func newConceptHandler(t *testing.T, stub *stubOllama, publisher *events.Publisher) *ingest.ConceptHandler {
	t.Helper()
	return ingest.NewConceptHandler(
		testStore,
		extract.New(stub.URL, "stub-chat"),
		embed.New(stub.URL, "stub-embed"),
		publisher,
		discardLogger(),
	)
}

// seedIngestedSource writes a source with chunks directly, standing in for a
// completed ingestion so extraction can be tested on its own.
func seedIngestedSource(t *testing.T, st *store.Store, title string, passages []string) string {
	t.Helper()
	ctx := context.Background()

	sourceID, err := st.CreateSource(ctx, title, store.TypeWeb, "https://example.com/doc")
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if err := st.SetSourceContent(ctx, sourceID, title, title+"\n\n"+passages[0]); err != nil {
		t.Fatalf("SetSourceContent: %v", err)
	}

	chunks := make([]chunk.Chunk, len(passages))
	vectors := make([][]float32, len(passages))
	for i, text := range passages {
		chunks[i] = chunk.Chunk{
			Index:       i,
			Content:     text,
			TokenCount:  len(text) / 4,
			HeadingPath: "Section",
		}
		v := make([]float32, embeddingDimensions)
		v[0] = float32(i+1) / 100
		vectors[i] = v
	}
	if err := st.ReplaceChunks(ctx, sourceID, chunks, vectors); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}
	if err := st.MarkSourceReady(ctx, sourceID); err != nil {
		t.Fatalf("MarkSourceReady: %v", err)
	}
	return sourceID
}

// Progress must survive the trip through Redis: the worker publishes and the
// API subscribes from a different process, so an in-process channel would pass
// a unit test and fail in production.
func TestProgressEventsCrossRedis(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const jobID = "job-crossing-redis"

	subscriber := events.NewSubscriber(redisAddr)
	defer subscriber.Close()

	updates, err := subscriber.Subscribe(ctx, jobID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	publisher := events.NewPublisher(redisAddr, discardLogger())
	defer publisher.Close()

	publisher.Publish(jobID, events.Event{
		Type: events.EventStatus, Stage: "Working", Progress: 42,
	})
	publisher.Publish(jobID, events.Event{
		Type: events.EventComplete, Stage: "Complete", Progress: 100, Chunks: 7,
	})

	var received []events.Event
	for len(received) < 2 {
		select {
		case event, open := <-updates:
			if !open {
				t.Fatal("subscription closed before both events arrived")
			}
			received = append(received, event)
		case <-ctx.Done():
			t.Fatalf("timed out after %d event(s)", len(received))
		}
	}

	if received[0].Progress != 42 || received[0].Stage != "Working" {
		t.Errorf("first event = %+v, want the 42%% status", received[0])
	}
	if !received[1].IsTerminal() || received[1].Chunks != 7 {
		t.Errorf("second event = %+v, want a terminal complete carrying chunk count", received[1])
	}
}

// A subscriber for one job must not see another job's events, or two concurrent
// uploads would cross-contaminate each other's progress bars.
func TestEventsAreScopedPerJob(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	subscriber := events.NewSubscriber(redisAddr)
	defer subscriber.Close()

	mine, err := subscriber.Subscribe(ctx, "job-mine")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	publisher := events.NewPublisher(redisAddr, discardLogger())
	defer publisher.Close()

	publisher.Publish("job-theirs", events.Event{Type: events.EventStatus, Stage: "Not yours", Progress: 50})
	publisher.Publish("job-mine", events.Event{Type: events.EventStatus, Stage: "Yours", Progress: 10})

	select {
	case event := <-mine:
		if event.Stage != "Yours" {
			t.Errorf("received another job's event: %+v", event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for this job's event")
	}
}

// The full pipeline against a real database: parse, chunk, embed, store.
func TestIngestPipelineEndToEnd(t *testing.T) {
	st := requireStack(t)
	ctx := context.Background()

	stub := newStubOllama(t)
	publisher := events.NewPublisher(redisAddr, discardLogger())
	defer publisher.Close()

	const sourceURL = "https://example.com/battery-physics"
	doc := parse.Document{
		Title: "Battery Physics",
		Markdown: "# Battery Physics\n\n" +
			longParagraph("Lithium ions move between electrodes.") +
			"\n\n## Degradation\n\n" +
			longParagraph("Capacity fades as the interphase thickens."),
	}

	sourceID, err := st.CreateSource(ctx, sourceURL, store.TypeWeb, sourceURL)
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	jobID, err := st.CreateJob(ctx, sourceID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	task, err := ingest.NewTask(ingest.Payload{
		JobID: jobID, SourceID: sourceID,
		SourceType: store.TypeWeb, Ref: sourceURL,
	})
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	if err := newIngestHandler(t, stub, publisher, doc).ProcessTask(ctx, task); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	source, _ := st.GetSource(ctx, sourceID)
	if source.Status != store.SourceReady {
		t.Errorf("source status = %q, want %q", source.Status, store.SourceReady)
	}

	count, _ := st.CountChunks(ctx, sourceID)
	if count == 0 {
		t.Fatal("pipeline stored no chunks")
	}

	job, _ := st.GetJob(ctx, jobID)
	if job.Status != store.JobSucceeded || job.Progress != 100 {
		t.Errorf("job = %+v, want succeeded at 100%%", job)
	}
}

// An embedding failure must leave nothing half-written: no chunks, a failed
// job, and a source marked failed rather than silently empty-but-ready.
func TestIngestFailureLeavesNoPartialState(t *testing.T) {
	st := requireStack(t)
	ctx := context.Background()

	stub := newStubOllama(t)
	stub.EmbedFail = true

	publisher := events.NewPublisher(redisAddr, discardLogger())
	defer publisher.Close()

	const sourceURL = "https://example.com/doc"
	doc := parse.Document{
		Title:    "Doc",
		Markdown: "# Doc\n\n" + longParagraph("Some body text about batteries."),
	}

	sourceID, _ := st.CreateSource(ctx, sourceURL, store.TypeWeb, sourceURL)
	jobID, _ := st.CreateJob(ctx, sourceID)

	task, _ := ingest.NewTask(ingest.Payload{
		JobID: jobID, SourceID: sourceID,
		SourceType: store.TypeWeb, Ref: sourceURL,
	})

	if err := newIngestHandler(t, stub, publisher, doc).ProcessTask(ctx, task); err == nil {
		t.Fatal("ProcessTask succeeded despite an embedding failure")
	}

	if count, _ := st.CountChunks(ctx, sourceID); count != 0 {
		t.Errorf("%d chunks written despite failure, want 0", count)
	}

	job, _ := st.GetJob(ctx, jobID)
	if job.Status != store.JobFailed {
		t.Errorf("job status = %q, want %q", job.Status, store.JobFailed)
	}
	if job.Error == "" {
		t.Error("failed job recorded no reason")
	}
	// The message reaches a user, so queue plumbing must not appear in it.
	if strings.Contains(job.Error, "skip retry") {
		t.Errorf("queue internals leaked into the job error: %q", job.Error)
	}

	source, _ := st.GetSource(ctx, sourceID)
	if source.Status != store.SourceFailed {
		t.Errorf("source status = %q, want %q", source.Status, store.SourceFailed)
	}
}

// Concept extraction over a real database: nodes, provenance, and the
// co-occurrence edges that make the graph.
func TestConceptExtractionBuildsGraph(t *testing.T) {
	st := requireStack(t)
	ctx := context.Background()

	stub := newStubOllama(t)
	stub.Concepts = []map[string]string{
		{"name": "Battery Degradation", "summary": "Capacity loss over cycles."},
		{"name": "Solid Electrolyte Interphase", "summary": "A passivating layer on the anode."},
	}
	stub.JudgeRelated = true

	publisher := events.NewPublisher(redisAddr, discardLogger())
	defer publisher.Close()

	sourceID := seedIngestedSource(t, st, "Battery Paper", []string{
		"Passage one about degradation mechanisms.",
		"Passage two about the interphase layer.",
	})
	jobID, _ := st.CreateJob(ctx, sourceID)

	task, err := ingest.NewExtractTask(ingest.ExtractPayload{JobID: jobID, SourceID: sourceID})
	if err != nil {
		t.Fatalf("NewExtractTask: %v", err)
	}

	if err := newConceptHandler(t, stub, publisher).ProcessTask(ctx, task); err != nil {
		t.Fatalf("extraction ProcessTask: %v", err)
	}

	concepts, err := st.ListConcepts(ctx, 10)
	if err != nil {
		t.Fatalf("ListConcepts: %v", err)
	}
	if len(concepts) != 2 {
		t.Fatalf("got %d concepts, want 2: %+v", len(concepts), concepts)
	}

	// Both concepts appear in every passage, so they must be connected — and
	// the trigger must have counted that edge on both endpoints.
	for _, c := range concepts {
		if c.ConnectionCount != 1 {
			t.Errorf("concept %q has connection_count %d, want 1", c.Name, c.ConnectionCount)
		}
		if c.MentionCount != 2 {
			t.Errorf("concept %q has %d mentions, want 2 (one per passage)", c.Name, c.MentionCount)
		}
	}

	graph, err := st.LoadGraph(ctx, 50)
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if len(graph.Nodes) != 2 || len(graph.Edges) != 1 {
		t.Errorf("graph has %d nodes and %d edges, want 2 and 1", len(graph.Nodes), len(graph.Edges))
	}

	// The edge must carry the judge's stated reason. Boilerplate here was the
	// original defect: a relationship_summary that explained nothing.
	for _, e := range graph.Edges {
		if strings.Contains(e.Summary, "Discussed together in") {
			t.Errorf("edge kept the old co-occurrence boilerplate: %q", e.Summary)
		}
		if e.Summary == "" {
			t.Error("edge has no stated reason")
		}
	}

	// Every edge endpoint must be a node in the same payload, or a renderer
	// will either crash or invent a phantom node.
	present := map[string]bool{}
	for _, n := range graph.Nodes {
		present[n.ID] = true
	}
	for _, e := range graph.Edges {
		if !present[e.Source] || !present[e.Target] {
			t.Errorf("edge %s→%s references a node missing from the payload", e.Source, e.Target)
		}
	}
}

// Re-extracting must not duplicate nodes or double-count edges: the same
// document processed twice should converge on the same graph.
func TestReExtractionIsIdempotent(t *testing.T) {
	st := requireStack(t)
	ctx := context.Background()

	stub := newStubOllama(t)
	stub.Concepts = []map[string]string{
		{"name": "Ion Transport", "summary": "Movement of ions."},
		{"name": "Thermal Runaway", "summary": "Uncontrolled heating."},
	}
	stub.JudgeRelated = true

	publisher := events.NewPublisher(redisAddr, discardLogger())
	defer publisher.Close()

	sourceID := seedIngestedSource(t, st, "Doc", []string{"Passage about both topics."})
	handler := newConceptHandler(t, stub, publisher)

	for run := range 2 {
		jobID, _ := st.CreateJob(ctx, sourceID)
		task, _ := ingest.NewExtractTask(ingest.ExtractPayload{JobID: jobID, SourceID: sourceID})
		if err := handler.ProcessTask(ctx, task); err != nil {
			t.Fatalf("extraction run %d: %v", run+1, err)
		}
	}

	concepts, _ := st.ListConcepts(ctx, 20)
	if len(concepts) != 2 {
		t.Errorf("after two runs there are %d concepts, want 2", len(concepts))
	}
	for _, c := range concepts {
		if c.ConnectionCount != 1 {
			t.Errorf("concept %q connection_count = %d after re-extraction, want 1",
				c.Name, c.ConnectionCount)
		}
		if c.MentionCount != 1 {
			t.Errorf("concept %q mention count = %d after re-extraction, want 1",
				c.Name, c.MentionCount)
		}
	}
}

// A model that returns nothing usable must fail the extraction job while
// leaving the document searchable — chunks and embeddings are still valid.
func TestExtractionFailureKeepsDocumentSearchable(t *testing.T) {
	st := requireStack(t)
	ctx := context.Background()

	stub := newStubOllama(t)
	stub.ChatFail = true

	publisher := events.NewPublisher(redisAddr, discardLogger())
	defer publisher.Close()

	sourceID := seedIngestedSource(t, st, "Doc", []string{"A passage."})
	jobID, _ := st.CreateJob(ctx, sourceID)
	task, _ := ingest.NewExtractTask(ingest.ExtractPayload{JobID: jobID, SourceID: sourceID})

	if err := newConceptHandler(t, stub, publisher).ProcessTask(ctx, task); err == nil {
		t.Fatal("extraction succeeded despite a failing model")
	}

	job, _ := st.GetJob(ctx, jobID)
	if job.Status != store.JobFailed {
		t.Errorf("extraction job status = %q, want %q", job.Status, store.JobFailed)
	}

	// The point of splitting extraction into its own task: ingestion's output
	// survives an extraction failure.
	source, _ := st.GetSource(ctx, sourceID)
	if source.Status != store.SourceReady {
		t.Errorf("source status = %q, want it to stay %q", source.Status, store.SourceReady)
	}
	if count, _ := st.CountChunks(ctx, sourceID); count == 0 {
		t.Error("chunks were discarded by an extraction failure")
	}
}

// Task payloads cross a process boundary as JSON; a field lost in transit would
// mean the worker silently processing the wrong thing.
func TestTaskPayloadsSurviveTheQueueEncoding(t *testing.T) {
	ingestTask, err := ingest.NewTask(ingest.Payload{
		JobID: "j1", SourceID: "s1", SourceType: store.TypePDF,
		BlobHash: "deadbeef", TitleHint: "paper.pdf",
	})
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	if ingestTask.Type() != ingest.TypeIngestSource {
		t.Errorf("ingest task type = %q", ingestTask.Type())
	}

	extractTask, err := ingest.NewExtractTask(ingest.ExtractPayload{JobID: "j2", SourceID: "s2"})
	if err != nil {
		t.Fatalf("NewExtractTask: %v", err)
	}
	if extractTask.Type() != ingest.TypeExtractConcepts {
		t.Errorf("extract task type = %q", extractTask.Type())
	}

	// The two must be distinguishable, or the mux would route to the wrong
	// handler.
	if ingestTask.Type() == extractTask.Type() {
		t.Error("ingest and extract tasks share a type")
	}
	var _ *asynq.Task = ingestTask
}

// stubParser stands in for the real parsers so the pipeline can be exercised
// without a network fetch. The web parser's SSRF guard blocks loopback by
// design, so a local test server is not reachable — and relaxing that guard to
// suit the tests would trade a real protection for convenience.
type stubParser struct {
	doc parse.Document
	err error
}

func (p stubParser) Parse(context.Context, string) (parse.Document, error) {
	return p.doc, p.err
}

// longParagraph pads a sentence so the chunker treats it as real content.
func longParagraph(sentence string) string {
	return strings.TrimSpace(strings.Repeat(sentence+" ", 12))
}

// The core of the change: co-occurrence proposes, it does not decide. Concepts
// that share a passage but are judged unrelated must produce no edge at all.
func TestUnrelatedConceptsProduceNoEdges(t *testing.T) {
	st := requireStack(t)
	ctx := context.Background()

	stub := newStubOllama(t)
	stub.Concepts = []map[string]string{
		{"name": "Pulsed Laser Deposition", "summary": "A thin-film fabrication technique."},
		{"name": "Consumer Rebate", "summary": "A purchase incentive."},
	}
	stub.JudgeRelated = false // the judge rejects the pair

	publisher := events.NewPublisher(redisAddr, discardLogger())
	defer publisher.Close()

	sourceID := seedIngestedSource(t, st, "Doc", []string{"A passage mentioning both in passing."})
	jobID, _ := st.CreateJob(ctx, sourceID)
	task, _ := ingest.NewExtractTask(ingest.ExtractPayload{JobID: jobID, SourceID: sourceID})

	if err := newConceptHandler(t, stub, publisher).ProcessTask(ctx, task); err != nil {
		t.Fatalf("extraction ProcessTask: %v", err)
	}

	// The concepts themselves are still real and still recorded.
	concepts, _ := st.ListConcepts(ctx, 10)
	if len(concepts) != 2 {
		t.Fatalf("got %d concepts, want 2 — rejecting an edge must not drop concepts", len(concepts))
	}

	graph, err := st.LoadGraph(ctx, 50)
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if len(graph.Edges) != 0 {
		t.Errorf("got %d edges for a rejected pair, want 0: %+v", len(graph.Edges), graph.Edges)
	}
	for _, c := range concepts {
		if c.ConnectionCount != 0 {
			t.Errorf("concept %q has connection_count %d after rejection, want 0", c.Name, c.ConnectionCount)
		}
	}
}
