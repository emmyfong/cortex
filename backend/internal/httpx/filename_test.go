package httpx

import "testing"

// The filename goes into a quoted Content-Disposition value, so anything that
// could close the quote or inject a header must be stripped.
func TestSafeFilename(t *testing.T) {
	tests := []struct {
		name  string
		given string
		want  string
	}{
		{"ordinary name", "report.pdf", "report.pdf"},
		{"spaces are fine", "Q4 Report Final.pdf", "Q4 Report Final.pdf"},

		// Header injection: a raw CRLF would end the header and start another.
		{"strips CRLF", "evil\r\nX-Injected: yes.pdf", "evilX-Injected: yes.pdf"},
		{"strips newline", "two\nlines.pdf", "twolines.pdf"},
		{"strips control characters", "null\x00byte.pdf", "nullbyte.pdf"},

		// A double quote would close the quoted header value.
		{"strips double quotes", `say "hi".pdf`, "say hi.pdf"},

		// Path components can't influence where bytes land (the blob is
		// addressed by hash), but a bare basename is still what should be sent.
		// Both separators are handled on every platform, not just the host's.
		{"strips unix directory components", "../../etc/passwd", "passwd"},
		{"strips windows path", `C:\Users\emmyf\secret.pdf`, "secret.pdf"},
		{"strips mixed separators", `a/b\c/d.pdf`, "d.pdf"},

		{"empty falls back", "", "document.pdf"},
		{"dot falls back", ".", "document.pdf"},
		{"dotdot falls back", "..", "document.pdf"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeFilename(tt.given); got != tt.want {
				t.Errorf("safeFilename(%q) = %q, want %q", tt.given, got, tt.want)
			}
		})
	}
}
