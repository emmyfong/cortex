package httpx

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These handlers reject bad input before touching the database or the queue,
// so the rejection paths can be exercised with nil dependencies. A test that
// reached either would panic, which is itself a useful signal.
func testSourceHandler() *SourceHandler {
	return &SourceHandler{
		Logger:         discardLogger(),
		MaxUploadBytes: 1 << 20,
	}
}

func TestCreateFromURLRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty url", `{"url":""}`},
		{"whitespace url", `{"url":"   "}`},
		{"missing url field", `{}`},
		// Non-HTTP schemes are an SSRF and local-file-read vector.
		{"file scheme", `{"url":"file:///etc/passwd"}`},
		{"gopher scheme", `{"url":"gopher://example.com"}`},
		{"scheme-less", `{"url":"example.com/article"}`},
		{"malformed json", `{"url":`},
		// DisallowUnknownFields means a typo'd field is reported, not ignored.
		{"unknown field", `{"urls":"https://example.com"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/sources/url",
				strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			testSourceHandler().CreateFromURL().ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (body: %s)",
					rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

// Content-Type and file extension are both attacker-controlled, so the upload
// handler must decide from the file's magic bytes.
func TestCreateFromUploadRejectsNonPDF(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		content  string
		wantCode int
	}{
		{"html disguised as pdf", "evil.pdf", "<html><body>not a pdf</body></html>", http.StatusBadRequest},
		{"empty file", "empty.pdf", "", http.StatusBadRequest},
		{"script with pdf extension", "x.pdf", "#!/bin/sh\nrm -rf /", http.StatusBadRequest},
		{"truncated magic", "short.pdf", "%PD", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType := multipartBody(t, "file", tt.filename, tt.content)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/sources/upload", body)
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()

			testSourceHandler().CreateFromUpload().ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

func TestCreateFromUploadRequiresFileField(t *testing.T) {
	body, contentType := multipartBody(t, "document", "x.pdf", "%PDF-1.4 content")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sources/upload", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()

	testSourceHandler().CreateFromUpload().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestDecodeJSONRejectsOversizedBody(t *testing.T) {
	huge := `{"url":"` + strings.Repeat("a", maxJSONBody+100) + `"}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sources/url", strings.NewReader(huge))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	testSourceHandler().CreateFromURL().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

// Error responses are read by the browser and must not carry internal detail.
func TestErrorResponsesAreJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sources/url",
		strings.NewReader(`{"url":"file:///etc/passwd"}`))
	rec := httptest.NewRecorder()

	testSourceHandler().CreateFromURL().ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", got)
	}

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body.Error == "" {
		t.Error("error response has an empty message")
	}
}

func multipartBody(t *testing.T, field, filename, content string) (*bytes.Buffer, string) {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	return &buf, writer.FormDataContentType()
}
