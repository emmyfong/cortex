// Package blob stores original uploaded files on the filesystem, addressed by
// the sha256 of their content.
//
// Content addressing gives three properties worth having here: identical
// uploads share one file on disk, stored files are immutable, and — because a
// path is derived only from hex digits — no filename supplied by a caller can
// influence where bytes land. Path traversal is impossible by construction
// rather than by sanitisation.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var (
	// ErrNotFound means no blob exists for the requested hash.
	ErrNotFound = errors.New("blob not found")

	// ErrTooLarge means the content exceeded the store's size limit.
	ErrTooLarge = errors.New("content exceeds maximum size")
)

// hashLength is the sha256 digest size. Anything else is not a valid key.
const hashLength = sha256.Size

// dirPermissions and filePermissions keep blobs readable only by the owner:
// ingested documents are the user's private material.
const (
	dirPermissions  = 0o700
	filePermissions = 0o600
)

// Store is a content-addressed file store rooted at a directory.
type Store struct {
	root     string
	maxBytes int64
}

// Ref identifies a stored blob.
type Ref struct {
	Hash []byte
	Size int64
}

// New creates a store rooted at dir, creating the directory if needed.
func New(root string, maxBytes int64) (*Store, error) {
	if root == "" {
		return nil, errors.New("blob store root directory is required")
	}
	if maxBytes < 1 {
		return nil, fmt.Errorf("maxBytes must be positive, got %d", maxBytes)
	}
	if err := os.MkdirAll(root, dirPermissions); err != nil {
		return nil, fmt.Errorf("create blob root %q: %w", root, err)
	}
	return &Store{root: root, maxBytes: maxBytes}, nil
}

// Put stores content and returns its hash and size.
//
// The content is streamed to a temp file while being hashed, then renamed into
// its final content-addressed location. Hashing and writing in one pass avoids
// buffering a whole upload in memory; the rename makes the blob appear
// atomically, so a reader never observes a partial file.
func (s *Store) Put(r io.Reader) (Ref, error) {
	temp, err := os.CreateTemp(s.root, ".incoming-*")
	if err != nil {
		return Ref{}, fmt.Errorf("create temp blob: %w", err)
	}
	tempName := temp.Name()

	// Remove the temp file on every failure path. Harmless after a successful
	// rename, when the name no longer exists.
	defer func() {
		temp.Close()
		_ = os.Remove(tempName)
	}()

	digest := sha256.New()

	// Read one byte beyond the limit so an oversized input is detected rather
	// than silently truncated to exactly maxBytes.
	written, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(r, s.maxBytes+1))
	if err != nil {
		return Ref{}, fmt.Errorf("write blob: %w", err)
	}
	if written > s.maxBytes {
		return Ref{}, fmt.Errorf("%w: %d bytes exceeds limit of %d", ErrTooLarge, written, s.maxBytes)
	}
	if err := temp.Close(); err != nil {
		return Ref{}, fmt.Errorf("close temp blob: %w", err)
	}

	sum := digest.Sum(nil)
	final, err := s.path(sum)
	if err != nil {
		return Ref{}, err
	}

	if err := os.MkdirAll(filepath.Dir(final), dirPermissions); err != nil {
		return Ref{}, fmt.Errorf("create blob directory: %w", err)
	}

	// An existing blob has identical content by definition, so keep it and
	// discard the newly written copy.
	if _, err := os.Stat(final); err == nil {
		return Ref{Hash: sum, Size: written}, nil
	}

	if err := os.Rename(tempName, final); err != nil {
		return Ref{}, fmt.Errorf("commit blob: %w", err)
	}
	if err := os.Chmod(final, filePermissions); err != nil {
		return Ref{}, fmt.Errorf("set blob permissions: %w", err)
	}

	return Ref{Hash: sum, Size: written}, nil
}

// Open returns a reader for a stored blob. The caller must close it.
func (s *Store) Open(hash []byte) (io.ReadCloser, error) {
	path, err := s.path(hash)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, HashToHex(hash))
	}
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	return file, nil
}

// Path returns the on-disk location of a stored blob.
//
// Exposed for tools that require a filesystem path rather than a reader —
// pdftotext is invoked with a path, and copying the blob to a temp file just to
// hand it one would double the I/O for no benefit. Callers must treat the file
// as read-only; blobs are immutable.
func (s *Store) Path(hash []byte) (string, error) {
	path, err := s.path(hash)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotFound, HashToHex(hash))
	}
	return path, nil
}

// Exists reports whether a blob is stored.
func (s *Store) Exists(hash []byte) bool {
	path, err := s.path(hash)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Delete removes a blob. Deleting an absent blob is not an error: the caller's
// desired state already holds.
func (s *Store) Delete(hash []byte) error {
	path, err := s.path(hash)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete blob: %w", err)
	}
	return nil
}

// path maps a hash to its location: <root>/ab/cdef...  The two-character shard
// keeps any single directory from accumulating every blob, which some
// filesystems handle poorly.
func (s *Store) path(hash []byte) (string, error) {
	if len(hash) != hashLength {
		return "", fmt.Errorf("invalid blob hash: expected %d bytes, got %d", hashLength, len(hash))
	}
	encoded := hex.EncodeToString(hash)
	return filepath.Join(s.root, encoded[:2], encoded), nil
}

// HashToHex renders a hash for display, URLs, or logs.
func HashToHex(hash []byte) string {
	return hex.EncodeToString(hash)
}

// HexToHash parses a hex-encoded hash, rejecting anything that is not a valid
// digest — so a caller-supplied string can never become an arbitrary path.
func HexToHash(s string) ([]byte, error) {
	decoded, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid blob hash encoding: %w", err)
	}
	if len(decoded) != hashLength {
		return nil, fmt.Errorf("invalid blob hash: expected %d bytes, got %d", hashLength, len(decoded))
	}
	return decoded, nil
}
