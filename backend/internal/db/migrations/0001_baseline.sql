-- +goose Up
-- Baseline schema for Cortex.
--
-- Note on goose: function bodies contain semicolons inside $$ ... $$, which goose
-- would otherwise split into separate statements. Every CREATE FUNCTION below is
-- wrapped in StatementBegin/StatementEnd to prevent that.

CREATE EXTENSION IF NOT EXISTS vector;

-- ---------------------------------------------------------------------------
-- Shared trigger: keep updated_at honest.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 1. Raw sources
-- ---------------------------------------------------------------------------
-- raw_content is nullable by design: the row is created when a job is enqueued,
-- before the worker has extracted anything. status tracks that lifecycle.
CREATE TABLE sources (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title        TEXT NOT NULL,
    source_type  TEXT NOT NULL CHECK (source_type IN ('pdf', 'web', 'youtube')),
    url_or_path  TEXT,
    raw_content  TEXT,
    content_hash BYTEA,
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'processing', 'ready', 'failed')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- sha256(raw_content), set after extraction. Partial index so the many rows
-- still awaiting extraction (hash NULL) don't collide with each other.
CREATE UNIQUE INDEX idx_sources_hash ON sources (content_hash)
    WHERE content_hash IS NOT NULL;

CREATE TRIGGER trg_sources_updated_at BEFORE UPDATE ON sources
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 2. Semantic chunks (RAG retrieval unit)
-- ---------------------------------------------------------------------------
CREATE TABLE chunks (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id    UUID NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    chunk_index  INT NOT NULL,
    content      TEXT NOT NULL,
    token_count  INT NOT NULL,
    heading_path TEXT,
    embedding    vector(768),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_chunk_per_source UNIQUE (source_id, chunk_index)
);

COMMENT ON COLUMN chunks.heading_path IS
    'Markdown heading trail from the header-aware splitter, e.g. ''Intro > Background''.';
COMMENT ON COLUMN chunks.embedding IS
    'nomic-embed-text output; NULL until the embedding step completes.';

CREATE INDEX idx_chunks_source ON chunks (source_id);

-- HNSW for cosine similarity. m/ef_construction pinned explicitly so index
-- behaviour does not silently change with a pgvector default update.
CREATE INDEX idx_chunks_embedding ON chunks
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

-- ---------------------------------------------------------------------------
-- 3. Concepts (graph nodes)
-- ---------------------------------------------------------------------------
CREATE TABLE concepts (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name             TEXT NOT NULL,
    slug             TEXT NOT NULL UNIQUE,
    summary          TEXT NOT NULL,
    connection_count INT NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Case-insensitive uniqueness: an LLM will happily emit both
-- 'Battery Degradation' and 'battery degradation' as distinct concepts.
CREATE UNIQUE INDEX idx_concepts_name_ci ON concepts (lower(name));

CREATE TRIGGER trg_concepts_updated_at BEFORE UPDATE ON concepts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 4. Concept connections (undirected graph edges)
-- ---------------------------------------------------------------------------
-- canonical_edge_order is what actually makes the edge undirected: without it,
-- UNIQUE (a, b) still admits both (A,B) and (B,A) as separate rows. It also
-- subsumes a self-loop check, since a < b implies a <> b.
--
-- Writers MUST sort the pair before inserting.
CREATE TABLE concept_connections (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    concept_a_id         UUID NOT NULL REFERENCES concepts(id) ON DELETE CASCADE,
    concept_b_id         UUID NOT NULL REFERENCES concepts(id) ON DELETE CASCADE,
    relationship_summary TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT unique_connection UNIQUE (concept_a_id, concept_b_id),
    CONSTRAINT canonical_edge_order CHECK (concept_a_id < concept_b_id)
);

-- unique_connection already indexes (a, b); this covers lookups by the far endpoint.
CREATE INDEX idx_conn_b ON concept_connections (concept_b_id);

-- Denormalised connection_count drifts if maintained from application code
-- (partial failures, concurrent writers), so the database owns it.
--
-- On concept deletion the FK cascade fires this trigger for each edge, and the
-- UPDATE against the row being deleted simply matches zero rows. That is safe
-- and intentional — do not "fix" it by removing the DELETE branch.
-- +goose StatementBegin
CREATE FUNCTION sync_connection_count() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE concepts SET connection_count = connection_count + 1
            WHERE id IN (NEW.concept_a_id, NEW.concept_b_id);
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE concepts SET connection_count = connection_count - 1
            WHERE id IN (OLD.concept_a_id, OLD.concept_b_id);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_conn_count AFTER INSERT OR DELETE ON concept_connections
    FOR EACH ROW EXECUTE FUNCTION sync_connection_count();

-- ---------------------------------------------------------------------------
-- 5. Concept provenance
-- ---------------------------------------------------------------------------
-- Ties every concept back to the passage that produced it, so a wiki page can
-- cite its sources and an LLM-extracted concept can be audited against evidence.
CREATE TABLE concept_mentions (
    concept_id UUID NOT NULL REFERENCES concepts(id) ON DELETE CASCADE,
    chunk_id   UUID NOT NULL REFERENCES chunks(id)   ON DELETE CASCADE,
    source_id  UUID NOT NULL REFERENCES sources(id)  ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (concept_id, chunk_id)
);

CREATE INDEX idx_mentions_chunk  ON concept_mentions (chunk_id);
CREATE INDEX idx_mentions_source ON concept_mentions (source_id);

-- ---------------------------------------------------------------------------
-- 6. Ingestion jobs
-- ---------------------------------------------------------------------------
-- asynq owns queueing and retries; this table owns durable, queryable status.
-- It lets a browser refreshed mid-ingest replay current progress instead of
-- hanging on a dead SSE stream, and survives a Redis restart.
CREATE TABLE jobs (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id  UUID REFERENCES sources(id) ON DELETE CASCADE,
    status     TEXT NOT NULL DEFAULT 'queued'
               CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    stage      TEXT,
    progress   INT NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    error      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON COLUMN jobs.stage IS
    'Human-readable step for the SSE stream, e.g. ''Parsing PDF''.';

CREATE INDEX idx_jobs_source ON jobs (source_id);

CREATE TRIGGER trg_jobs_updated_at BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 7. Master notes ("Jarvis" co-creation space)
-- ---------------------------------------------------------------------------
CREATE TABLE master_notes (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title        TEXT NOT NULL,
    user_thought TEXT NOT NULL,
    ai_synthesis TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_master_notes_updated_at BEFORE UPDATE ON master_notes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS master_notes;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS concept_mentions;
DROP TABLE IF EXISTS concept_connections;
DROP FUNCTION IF EXISTS sync_connection_count();
DROP TABLE IF EXISTS concepts;
DROP TABLE IF EXISTS chunks;
DROP TABLE IF EXISTS sources;
DROP FUNCTION IF EXISTS set_updated_at();
DROP EXTENSION IF EXISTS vector;
