package blob

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestPutAndOpenRoundTrip(t *testing.T) {
	store := testStore(t)
	content := []byte("%PDF-1.4 some pdf bytes")

	ref, err := store.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if ref.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", ref.Size, len(content))
	}
	if len(ref.Hash) != 32 {
		t.Errorf("Hash length = %d, want 32 (sha256)", len(ref.Hash))
	}

	reader, err := store.Open(ref.Hash)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()

	var got bytes.Buffer
	if _, err := got.ReadFrom(reader); err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(got.Bytes(), content) {
		t.Errorf("round-tripped content = %q, want %q", got.Bytes(), content)
	}
}

// Content addressing means an identical upload costs no extra disk.
func TestPutIsContentAddressedAndDeduplicates(t *testing.T) {
	store := testStore(t)
	content := []byte("identical bytes")

	first, err := store.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	second, err := store.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}

	if !bytes.Equal(first.Hash, second.Hash) {
		t.Error("identical content produced different hashes")
	}

	count := 0
	_ = filepath.Walk(store.root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			count++
		}
		return nil
	})
	if count != 1 {
		t.Errorf("stored %d files for identical content, want 1", count)
	}
}

func TestPutDifferentContentDiffersInHash(t *testing.T) {
	store := testStore(t)

	a, _ := store.Put(bytes.NewReader([]byte("alpha")))
	b, _ := store.Put(bytes.NewReader([]byte("beta")))

	if bytes.Equal(a.Hash, b.Hash) {
		t.Error("different content produced the same hash")
	}
}

// An unbounded write from an untrusted upload is a disk-exhaustion vector.
func TestPutRejectsOversizedContent(t *testing.T) {
	store, err := New(t.TempDir(), 100)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = store.Put(bytes.NewReader(bytes.Repeat([]byte("x"), 500)))

	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("Put oversized = %v, want ErrTooLarge", err)
	}

	// A rejected write must not leave a partial file behind.
	count := 0
	_ = filepath.Walk(store.root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			count++
		}
		return nil
	})
	if count != 0 {
		t.Errorf("%d files left after a rejected write, want 0", count)
	}
}

func TestOpenMissingBlob(t *testing.T) {
	store := testStore(t)

	_, err := store.Open(bytes.Repeat([]byte{0xAB}, 32))

	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Open missing = %v, want ErrNotFound", err)
	}
}

// The hash is the only thing that determines a path, so a malformed hash must
// be rejected rather than turned into some arbitrary filesystem location.
func TestRejectsMalformedHash(t *testing.T) {
	store := testStore(t)

	tests := []struct {
		name string
		hash []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"too short", bytes.Repeat([]byte{1}, 16)},
		{"too long", bytes.Repeat([]byte{1}, 64)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := store.Open(tt.hash); err == nil {
				t.Error("Open accepted a malformed hash")
			}
			if err := store.Delete(tt.hash); err == nil {
				t.Error("Delete accepted a malformed hash")
			}
		})
	}
}

// Paths are built from hex digits only, so no input can escape the root.
func TestPathStaysInsideRoot(t *testing.T) {
	store := testStore(t)

	ref, err := store.Put(bytes.NewReader([]byte("content")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	path, err := store.path(ref.Hash)
	if err != nil {
		t.Fatalf("path: %v", err)
	}

	root, _ := filepath.Abs(store.root)
	abs, _ := filepath.Abs(path)
	if !strings.HasPrefix(abs, root) {
		t.Errorf("path %q escaped root %q", abs, root)
	}
	if strings.Contains(path, "..") {
		t.Errorf("path contains traversal: %q", path)
	}
}

func TestDelete(t *testing.T) {
	store := testStore(t)

	ref, _ := store.Put(bytes.NewReader([]byte("to be deleted")))

	if err := store.Delete(ref.Hash); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Open(ref.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("blob still readable after delete: %v", err)
	}

	// Deleting an absent blob is not an error — the desired state holds.
	if err := store.Delete(ref.Hash); err != nil {
		t.Errorf("second Delete = %v, want nil", err)
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New("", 1024); err == nil {
		t.Error("New accepted an empty root directory")
	}
}

func TestHexRoundTrip(t *testing.T) {
	store := testStore(t)
	ref, _ := store.Put(bytes.NewReader([]byte("hex test")))

	hex := HashToHex(ref.Hash)
	if len(hex) != 64 {
		t.Errorf("hex length = %d, want 64", len(hex))
	}

	back, err := HexToHash(hex)
	if err != nil {
		t.Fatalf("HexToHash: %v", err)
	}
	if !bytes.Equal(back, ref.Hash) {
		t.Error("hex round trip changed the hash")
	}

	for _, bad := range []string{"", "zz", "not-hex", strings.Repeat("a", 63)} {
		if _, err := HexToHash(bad); err == nil {
			t.Errorf("HexToHash(%q) accepted invalid input", bad)
		}
	}
}
