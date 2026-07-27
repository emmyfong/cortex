-- +goose Up
-- Retain the original uploaded file so a source can be opened as it was
-- received, not only as extracted text.
--
-- Files are stored content-addressed on the filesystem (see internal/blob),
-- keyed by the sha256 of the original bytes. Identical uploads therefore share
-- one file on disk, and the database holds only the key.
--
-- Note this is a different hash from sources.content_hash: content_hash is the
-- sha256 of the *extracted markdown* and detects duplicate content across
-- source types, while file_hash is the sha256 of the *original bytes* and
-- addresses the stored blob. Two PDFs with different bytes can extract to
-- identical text, so the two must stay separate.

ALTER TABLE sources
    ADD COLUMN file_hash BYTEA,
    ADD COLUMN file_size BIGINT,
    ADD COLUMN original_filename TEXT;

COMMENT ON COLUMN sources.file_hash IS
    'sha256 of the original uploaded bytes; the blob store key. NULL for web sources.';
COMMENT ON COLUMN sources.original_filename IS
    'Filename as supplied by the client, used only for Content-Disposition.';

-- Finds every source sharing a blob, so deletion can tell whether the file on
-- disk is still referenced.
CREATE INDEX idx_sources_file_hash ON sources (file_hash) WHERE file_hash IS NOT NULL;

-- Uploaded sources previously stored a temp-file path in url_or_path. The
-- worker deleted that file after extraction, so every such value is a dangling
-- pointer to something that no longer exists. Clear them rather than leaving
-- data that looks meaningful and is not.
UPDATE sources SET url_or_path = NULL WHERE source_type = 'pdf';

-- +goose Down
DROP INDEX IF EXISTS idx_sources_file_hash;

ALTER TABLE sources
    DROP COLUMN IF EXISTS file_hash,
    DROP COLUMN IF EXISTS file_size,
    DROP COLUMN IF EXISTS original_filename;
