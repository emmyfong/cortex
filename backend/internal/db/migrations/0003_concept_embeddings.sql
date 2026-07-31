-- +goose Up
-- Concept embeddings exist to find *cross-document* relationship candidates.
--
-- Co-occurrence can only ever link concepts that share a passage, so it cannot
-- produce an edge between ideas read in different documents — which is the most
-- valuable kind of link this system is meant to surface. Embedding each
-- concept's name and summary lets nearest-neighbour search propose those pairs;
-- an LLM then decides whether the pair is genuinely related.

ALTER TABLE concepts ADD COLUMN embedding vector(768);

COMMENT ON COLUMN concepts.embedding IS
    'Embedding of "name: summary", used to propose cross-document relationship candidates.';

CREATE INDEX idx_concepts_embedding ON concepts
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

-- Records why a pair was proposed, so a weak graph can be diagnosed later
-- without re-running extraction.
ALTER TABLE concept_connections
    ADD COLUMN origin TEXT NOT NULL DEFAULT 'cooccurrence'
        CHECK (origin IN ('cooccurrence', 'similarity', 'manual'));

COMMENT ON COLUMN concept_connections.origin IS
    'Which candidate generator proposed this pair before the judge accepted it.';

-- Existing edges were created by raw co-occurrence with a boilerplate summary
-- ("Discussed together in X"), never judged for whether the concepts actually
-- relate. Measured on real data, 90% rested on a single shared passage. They
-- are not salvageable as relationships, and keeping them would silently mix
-- unjudged noise into a judged graph.
DELETE FROM concept_connections;

-- +goose Down
ALTER TABLE concept_connections DROP COLUMN IF EXISTS origin;
DROP INDEX IF EXISTS idx_concepts_embedding;
ALTER TABLE concepts DROP COLUMN IF EXISTS embedding;
