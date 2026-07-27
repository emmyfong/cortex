// Package parse converts external sources into clean markdown.
//
// Every parser returns the same Document shape, so the ingestion pipeline does
// not care whether a source arrived as a URL or a PDF.
package parse

// Document is a parsed source, ready for chunking.
type Document struct {
	// Title is the parser's best guess at the document title. It may be empty;
	// the caller is expected to fall back to something sensible.
	Title string

	// Markdown is the cleaned body text. Heading structure is preserved where
	// the source had any, since the chunker splits on it.
	Markdown string
}
