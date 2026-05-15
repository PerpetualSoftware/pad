package urlimport

import (
	"bytes"
	"fmt"
	nurl "net/url"
	"regexp"
	"strings"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/go-shiori/go-readability"
)

// ConvertResult is the markdown output of a converter plus light
// metadata recovered from the source document.
type ConvertResult struct {
	// Markdown is the converted body.
	Markdown string
	// Title is the document's <title> or extracted heading. May be
	// empty if no usable title was found.
	Title string
	// Byline is the article author, if any. Best-effort.
	Byline string
}

// ConvertGeneric converts an HTML document into markdown by first
// running go-readability to strip chrome, navigation, and ads, then
// piping the cleaned HTML through html-to-markdown. The pageURL is used
// by Readability to resolve relative links; pass an empty string when
// the source URL is unavailable.
//
// The function is robust to malformed input — it returns generic input
// even when Readability cannot identify a primary article (e.g. a
// directory listing or a page with no clear "content" container) by
// falling back to a whole-body conversion.
func ConvertGeneric(html []byte, pageURL string) (*ConvertResult, error) {
	if len(bytes.TrimSpace(html)) == 0 {
		return nil, fmt.Errorf("urlimport: empty HTML body")
	}

	var u *nurl.URL
	if pageURL != "" {
		parsed, err := nurl.Parse(pageURL)
		if err == nil {
			u = parsed
		}
	}

	article, raErr := readability.FromReader(bytes.NewReader(html), u)
	// Pick the source for the markdown converter: Readability's cleaned
	// content when it succeeded, otherwise the raw HTML.
	source := article.Content
	if raErr != nil || strings.TrimSpace(source) == "" {
		source = string(html)
	}

	// WithDomain resolves relative links/images against pageURL. Without
	// it, fallback-path output keeps href="/foo" which would resolve
	// against the Pad host when rendered — broken navigation.
	var convertOpts []converter.ConvertOptionFunc
	if pageURL != "" {
		convertOpts = append(convertOpts, converter.WithDomain(pageURL))
	}

	md, err := htmltomd.ConvertString(source, convertOpts...)
	if err != nil {
		return nil, fmt.Errorf("urlimport: html→markdown: %w", err)
	}

	cleaned := cleanupMarkdown(md)
	if strings.TrimSpace(cleaned) == "" {
		return nil, fmt.Errorf("urlimport: empty markdown after conversion")
	}

	return &ConvertResult{
		Markdown: cleaned,
		Title:    strings.TrimSpace(article.Title),
		Byline:   strings.TrimSpace(article.Byline),
	}, nil
}

// cleanupMarkdown applies idempotent post-processing to the converter's
// output: collapses runs of blank lines, trims trailing whitespace on
// each line, normalizes line endings, and strips a single trailing
// newline so the result composes cleanly when concatenated.
//
// The line-by-line trim preserves the markdown "two trailing spaces"
// hard-line-break idiom — html-to-markdown emits `<br>` as `"  \n"` so
// blindly stripping trailing whitespace would silently demote hard
// breaks to soft wraps. Trailing runs of 1, 3+, or any tab-mix are
// still removed.
//
// Cleanup is intentionally conservative — we don't try to "fix" the
// converter's structural choices (heading levels, code-fence languages,
// list markers). Those are the converter's responsibility.
var reBlankRuns = regexp.MustCompile(`\n{3,}`)

func cleanupMarkdown(md string) string {
	// Normalize CRLF / CR to LF.
	md = strings.ReplaceAll(md, "\r\n", "\n")
	md = strings.ReplaceAll(md, "\r", "\n")

	// Per-line trailing whitespace cleanup that preserves
	// hard-line-break markers (exactly two trailing spaces, no tabs).
	lines := strings.Split(md, "\n")
	for i, line := range lines {
		stripped := strings.TrimRight(line, " \t")
		// Hard-line-break: line ends with exactly two spaces (no tab
		// mixed in) and the stripped form is non-empty. A "trailing
		// space" line that's otherwise blank is just whitespace junk.
		spaces := len(line) - len(stripped)
		hadTab := strings.ContainsAny(line[len(stripped):], "\t")
		if !hadTab && spaces == 2 && stripped != "" {
			lines[i] = stripped + "  "
		} else {
			lines[i] = stripped
		}
	}
	md = strings.Join(lines, "\n")

	// Collapse runs of >=3 newlines down to exactly 2 (one blank line).
	md = reBlankRuns.ReplaceAllString(md, "\n\n")
	return strings.TrimSpace(md) + "\n"
}
