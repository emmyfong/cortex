package parse

import (
	"strings"
	"testing"
)

func TestNormalizePDFText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "rejoins hard-wrapped lines into a paragraph",
			raw:  "The dominant sequence transduction\nmodels are based on complex\nrecurrent networks.",
			want: "The dominant sequence transduction models are based on complex recurrent networks.",
		},
		{
			// The whole reason this function exists: PDF wraps at the column
			// width, and a naive join would corrupt words split by a hyphen.
			name: "rejoins a word split by a trailing hyphen without a space",
			raw:  "Battery degrada-\ntion accelerates above forty degrees.",
			want: "Battery degradation accelerates above forty degrees.",
		},
		{
			name: "preserves blank lines as paragraph breaks",
			raw:  "First paragraph text.\n\nSecond paragraph text.",
			want: "First paragraph text.\n\nSecond paragraph text.",
		},
		{
			name: "treats form feeds as paragraph breaks",
			raw:  "End of page one.\f\nStart of page two.",
			want: "End of page one.\n\nStart of page two.",
		},
		{
			name: "normalizes CRLF",
			raw:  "Line one\r\nline two",
			want: "Line one line two",
		},
		{
			name: "collapses surplus blank lines",
			raw:  "One.\n\n\n\nTwo.",
			want: "One.\n\nTwo.",
		},
		{
			name: "empty input yields empty output",
			raw:  "   \n\n  \n",
			want: "",
		},
		{
			name: "a real hyphenated compound at end of line still joins",
			raw:  "state-of-the-\nart results",
			want: "state-of-theart results",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizePDFText(tt.raw); got != tt.want {
				t.Errorf("normalizePDFText()\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestNewPDFParserDefaultsBinary(t *testing.T) {
	if got := NewPDFParser("").binary; got != "pdftotext" {
		t.Errorf("default binary = %q, want %q", got, "pdftotext")
	}
	if got := NewPDFParser("/custom/pdftotext").binary; got != "/custom/pdftotext" {
		t.Errorf("binary = %q, want the supplied path", got)
	}
}

func TestAvailableGivesActionableErrorWhenMissing(t *testing.T) {
	err := NewPDFParser("definitely-not-a-real-binary-xyz").Available()

	if err == nil {
		t.Fatal("Available() = nil for a missing binary, want error")
	}
	// A bare "not found" would leave the user guessing; the message must say
	// how to fix it.
	if !strings.Contains(err.Error(), "winget install") {
		t.Errorf("error lacks install guidance: %v", err)
	}
}
