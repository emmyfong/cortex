package parse

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	readability "codeberg.org/readeck/go-readability/v2"
	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
)

// userAgent identifies the fetcher honestly. Some sites block unknown clients,
// and pretending to be a browser invites being treated as one.
const userAgent = "Cortex/0.1 (local knowledge graph; +https://github.com/emmyfong/cortex)"

// WebParser fetches an article URL and reduces it to readable markdown.
//
// The HTTP client is SSRF-hardened; see safeurl.go.
type WebParser struct {
	client   *http.Client
	maxBytes int64
}

func NewWebParser(maxBytes int64) *WebParser {
	return &WebParser{
		client:   newSafeHTTPClient(),
		maxBytes: maxBytes,
	}
}

// Parse fetches rawURL and extracts the main article content, discarding
// navigation, advertising, and other page furniture.
func (p *WebParser) Parse(ctx context.Context, rawURL string) (Document, error) {
	parsed, err := validateURL(rawURL)
	if err != nil {
		return Document{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return Document{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := p.client.Do(req)
	if err != nil {
		return Document{}, fmt.Errorf("fetch %s: %w", parsed.Redacted(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Document{}, fmt.Errorf("fetch %s: server returned %d", parsed.Redacted(), resp.StatusCode)
	}

	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		if !strings.Contains(strings.ToLower(contentType), "html") {
			return Document{}, fmt.Errorf("expected HTML, got content-type %q", contentType)
		}
	}

	// Cap the read: an unbounded body from an untrusted host is a memory
	// exhaustion vector.
	body := io.LimitReader(resp.Body, p.maxBytes)

	article, err := readability.FromReader(body, parsed)
	if err != nil {
		return Document{}, fmt.Errorf("extract article: %w", err)
	}
	if article.Node == nil {
		return Document{}, fmt.Errorf("no readable article content found at %s", parsed.Redacted())
	}

	markdown, err := htmltomarkdown.ConvertNode(article.Node)
	if err != nil {
		return Document{}, fmt.Errorf("convert to markdown: %w", err)
	}

	// Strip citation apparatus and boilerplate before the text reaches the
	// chunker, so token budgets are spent on content rather than bibliography.
	text := CleanMarkdown(string(markdown))
	if text == "" {
		return Document{}, fmt.Errorf("article at %s converted to empty markdown", parsed.Redacted())
	}

	return Document{
		Title:    strings.TrimSpace(article.Title()),
		Markdown: text,
	}, nil
}
