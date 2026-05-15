package urlimport

import (
	"bytes"
	"fmt"
	nurl "net/url"
	"regexp"
	"strings"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown/v2"
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

	md, err := htmltomd.ConvertString(source)
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
// Cleanup is intentionally conservative — we don't try to "fix" the
// converter's structural choices (heading levels, code-fence languages,
// list markers). Those are the converter's responsibility.
var (
	reTrailingSpace = regexp.MustCompile(`[ \t]+\n`)
	reBlankRuns     = regexp.MustCompile(`\n{3,}`)
)

func cleanupMarkdown(md string) string {
	// Normalize CRLF / CR to LF.
	md = strings.ReplaceAll(md, "\r\n", "\n")
	md = strings.ReplaceAll(md, "\r", "\n")
	// Strip trailing whitespace on each line.
	md = reTrailingSpace.ReplaceAllString(md, "\n")
	// Collapse runs of >=3 newlines down to exactly 2 (one blank line).
	md = reBlankRuns.ReplaceAllString(md, "\n\n")
	return strings.TrimSpace(md) + "\n"
}
