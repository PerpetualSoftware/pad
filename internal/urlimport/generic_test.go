package urlimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestConvertGeneric_AvailityShape(t *testing.T) {
	html := loadFixture(t, "availity-shape.html")
	res, err := ConvertGeneric(html, "https://example.com/docs/eligibility")
	if err != nil {
		t.Fatalf("ConvertGeneric: %v", err)
	}
	if res.Markdown == "" {
		t.Fatal("Markdown is empty")
	}

	// Title should be extracted.
	if !strings.Contains(res.Title, "Eligibility") {
		t.Errorf("Title = %q, want to contain 'Eligibility'", res.Title)
	}

	// Article body must survive.
	wantSubstrings := []string{
		"Authentication",
		"Endpoints",
		"POST /v1/eligibility",
		"memberId",
		"RFC 7807",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("Markdown missing %q\n--- markdown ---\n%s\n---", want, res.Markdown)
		}
	}

	// Navigation/footer chrome should be stripped — we don't want
	// "Twitter", "GitHub" link list or analytics scripts in the output.
	unwanted := []string{
		"analytics",
		"console.log",
	}
	for _, bad := range unwanted {
		if strings.Contains(res.Markdown, bad) {
			t.Errorf("Markdown unexpectedly contains %q\n--- markdown ---\n%s\n---", bad, res.Markdown)
		}
	}
}

func TestConvertGeneric_MDNShape(t *testing.T) {
	html := loadFixture(t, "mdn-shape.html")
	res, err := ConvertGeneric(html, "https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/Array/flat")
	if err != nil {
		t.Fatalf("ConvertGeneric: %v", err)
	}

	if !strings.Contains(res.Title, "flat") {
		t.Errorf("Title = %q, want to contain 'flat'", res.Title)
	}

	wantSubstrings := []string{
		"Array.prototype.flat",
		"Syntax",
		"depth",
		"Examples",
		"arr1.flat",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("Markdown missing %q\n--- markdown ---\n%s\n---", want, res.Markdown)
		}
	}
}

func TestConvertGeneric_EmptyBody(t *testing.T) {
	_, err := ConvertGeneric([]byte("   \n  "), "")
	if err == nil {
		t.Fatal("expected error for empty body, got nil")
	}
}

func TestConvertGeneric_PlainTextFallback(t *testing.T) {
	// When the input isn't a recognizable HTML document, the converter
	// should still produce something rather than fail. Readability may
	// not identify an article — the fallback path runs html-to-markdown
	// on the raw input.
	body := []byte("<p>Just one paragraph.</p>")
	res, err := ConvertGeneric(body, "")
	if err != nil {
		t.Fatalf("ConvertGeneric: %v", err)
	}
	if !strings.Contains(res.Markdown, "Just one paragraph") {
		t.Errorf("Markdown = %q, want to contain the paragraph text", res.Markdown)
	}
}

func TestCleanupMarkdown(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			name: "trailing whitespace stripped",
			in:   "foo   \nbar\t\n",
			want: "foo\nbar\n",
		},
		{
			name: "triple newline collapsed",
			in:   "foo\n\n\n\nbar\n",
			want: "foo\n\nbar\n",
		},
		{
			name: "CRLF normalized",
			in:   "foo\r\nbar\r\n",
			want: "foo\nbar\n",
		},
		{
			name: "single trailing newline preserved",
			in:   "foo\n",
			want: "foo\n",
		},
		{
			name: "leading whitespace trimmed",
			in:   "\n\n\nfoo\n",
			want: "foo\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanupMarkdown(tc.in); got != tc.want {
				t.Errorf("cleanupMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
