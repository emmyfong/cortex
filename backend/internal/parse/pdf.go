package parse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// PDFParser extracts text by shelling out to pdftotext (poppler/xpdf).
//
// Pure-Go PDF extraction was evaluated and rejected: the available libraries
// return text with all inter-word spacing lost ("Providedproperattribution..."),
// because PDF stores glyphs at coordinates rather than as words, and
// reconstructing spaces from positions requires font metrics they do not
// expose. pdftotext does this correctly and is the standard tool for the job.
//
// pdftotext ships with Git for Windows (mingw64/bin) and is packaged everywhere
// else; ErrPDFToolMissing explains how to install it when absent.
type PDFParser struct {
	binary string
}

// ErrPDFToolMissing signals that the external extraction tool is unavailable.
var ErrPDFToolMissing = errors.New("pdftotext not found")

func NewPDFParser(binary string) *PDFParser {
	if binary == "" {
		binary = "pdftotext"
	}
	return &PDFParser{binary: binary}
}

// Available reports whether the extraction tool can be found, so callers can
// fail fast at startup instead of at the first PDF upload.
func (p *PDFParser) Available() error {
	if _, err := exec.LookPath(p.binary); err != nil {
		return fmt.Errorf(
			"%w at %q: install it with `winget install oschwartz10612.Poppler`, "+
				"or set PDFTOTEXT_PATH to its location "+
				"(Git for Windows bundles one at C:\\Program Files\\Git\\mingw64\\bin\\pdftotext.exe)",
			ErrPDFToolMissing, p.binary)
	}
	return nil
}

// Parse extracts text from the PDF at path.
//
// path is produced by the ingestion layer (a temp file it wrote), never taken
// directly from a request. exec.CommandContext passes arguments as a vector
// with no shell involved, so the path cannot be interpreted as a command.
func (p *PDFParser) Parse(ctx context.Context, path string) (Document, error) {
	if err := p.Available(); err != nil {
		return Document{}, err
	}

	// -q       suppress warnings on stderr for malformed but readable PDFs
	// -enc     force UTF-8 rather than the locale default
	// -nopgbrk omit form-feed page separators, which are not semantically
	//          meaningful once the text is chunked
	// -        write to stdout
	cmd := exec.CommandContext(ctx, p.binary, "-q", "-enc", "UTF-8", "-nopgbrk", path, "-")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return Document{}, fmt.Errorf("pdftotext failed: %s", detail)
	}

	text := normalizePDFText(stdout.String())
	if text == "" {
		return Document{}, fmt.Errorf(
			"no text extracted; the PDF may be a scanned image, which needs OCR rather than text extraction")
	}

	return Document{
		// PDFs carry no reliable title; the ingestion layer falls back to the
		// uploaded filename.
		Title:    "",
		Markdown: text,
	}, nil
}

// normalizePDFText tidies extraction output into something the chunker can work
// with. PDF text arrives hard-wrapped at the column width, so consecutive
// non-empty lines are joined back into paragraphs while blank lines are kept as
// paragraph breaks.
func normalizePDFText(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	// Form feeds survive some encodings even with -nopgbrk.
	raw = strings.ReplaceAll(raw, "\f", "\n\n")

	var (
		paragraphs []string
		current    strings.Builder
		// pendingHyphen means the previous line ended mid-word, so the next
		// line must be appended with no separator at all.
		pendingHyphen bool
	)

	flush := func() {
		if text := strings.TrimSpace(current.String()); text != "" {
			paragraphs = append(paragraphs, text)
		}
		current.Reset()
		pendingHyphen = false
	}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}

		if current.Len() > 0 && !pendingHyphen {
			current.WriteString(" ")
		}

		// A trailing hyphen marks a word split across lines: write the stem
		// without the hyphen and join the next line directly onto it, so
		// "degrada-" + "tion" becomes "degradation" rather than "degrada tion".
		if strings.HasSuffix(line, "-") {
			current.WriteString(strings.TrimSuffix(line, "-"))
			pendingHyphen = true
			continue
		}

		current.WriteString(line)
		pendingHyphen = false
	}
	flush()

	return strings.TrimSpace(strings.Join(paragraphs, "\n\n"))
}
