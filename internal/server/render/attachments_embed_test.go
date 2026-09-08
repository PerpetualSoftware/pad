package render

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// BUG-2964 — <img> vs file chip is decided by what can actually be SERVED and
// PAINTED, not by a `image/` MIME prefix.
//
// The defect: a pure-Go build derives no HEIC thumbnail, and the byte endpoint
// SILENTLY serves the original when the variant row is missing, so a prefix
// test produced an <img> full of bytes Chrome and Firefox cannot decode.

func TestShouldEmbedAsImage(t *testing.T) {
	cases := []struct {
		name    string
		mime    string
		derived []string
		known   bool
		want    bool
	}{
		// The defect itself.
		{"heic with no derived variant is a chip", "image/heic", nil, true, false},
		{"heif with no derived variant is a chip", "image/heif", nil, true, false},
		{"heic WITH a derived variant stays an image", "image/heic", []string{"thumb-md"}, true, true},

		// CONTROL. Fails if the rule is collapsed to variant availability
		// alone — the tempting simplification, and wrong, because the same
		// pure-Go build derives no AVIF thumbnail and every current browser
		// decodes AVIF.
		{"CONTROL avif with no derived variant stays an image", "image/avif", nil, true, true},
		// CONTROL. SVG is kept out of the in-app viewer for ACTIVE-CONTENT
		// reasons; an SVG inside an <img> runs no script. Reusing the viewer's
		// predicate here would turn every existing SVG embed into a chip.
		{"CONTROL svg with no derived variant stays an image", "image/svg+xml", nil, true, true},

		{"png", "image/png", nil, true, true},
		{"jpeg", "image/jpeg", nil, true, true},
		{"gif", "image/gif", nil, true, true},
		{"webp", "image/webp", nil, true, true},

		{"pdf is never an image", "application/pdf", []string{"thumb-md"}, true, false},
		{"mime parameters do not defeat the match", "image/svg+xml; charset=utf-8", nil, true, true},
		{"mime parameters do not defeat the refusal", "image/heic; foo=bar", nil, true, false},

		// UNKNOWN is not FALSE. nil means the caller did not look; falling back
		// to the prefix rule is what keeps an unprobed render identical to its
		// pre-BUG-2964 output.
		{"unknown heic falls back to the prefix rule", "image/heic", nil, false, true},
		{"unknown png falls back to the prefix rule", "image/png", nil, false, true},
		{"unknown pdf is still not an image", "application/pdf", nil, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldEmbedAsImage(&AttachmentMeta{
				MimeType:        tc.mime,
				DerivedVariants: tc.derived,
				DerivedKnown:    tc.known,
			}, "thumb-md")
			if got != tc.want {
				t.Errorf("ShouldEmbedAsImage(%q, derived=%v known=%v) = %v, want %v",
					tc.mime, tc.derived, tc.known, got, tc.want)
			}
		})
	}
}

func TestShouldEmbedAsImage_NilMeta(t *testing.T) {
	if ShouldEmbedAsImage(nil, "thumb-md") {
		t.Error("ShouldEmbedAsImage(nil, …) = true, want false")
	}
}

// TestImgPaintableMimes_MatchesTypeScript enforces the lock-step this file's
// header requires, rather than asserting it in a comment (which protects
// nobody). The Go renderer and the TS renderer must produce byte-identical
// output for the same input, so the two paintable lists have to be the same
// list — and a divergence would show up as server-rendered and client-rendered
// markdown disagreeing about whether a given attachment is an image, which is
// exactly the class of bug nobody notices until a reader reports it.
//
// Reads the TS source rather than a generated artifact so it fails on the edit
// that causes the drift, in the tree where that edit was made.
func TestImgPaintableMimes_MatchesTypeScript(t *testing.T) {
	// internal/server/render -> repo root
	tsPath := filepath.Join("..", "..", "..", "web", "src", "lib", "markdown", "attachments.ts")
	src, err := os.ReadFile(tsPath)
	if err != nil {
		t.Fatalf("read TS renderer at %s: %v", tsPath, err)
	}

	block := regexp.MustCompile(`(?s)const IMG_PAINTABLE_MIMES:[^=]*=\s*new Set\(\[(.*?)\]\)`).
		FindSubmatch(src)
	if block == nil {
		t.Fatalf("could not find IMG_PAINTABLE_MIMES in %s — if it was renamed, "+
			"update this test rather than deleting it; the lock-step is the point", tsPath)
	}

	var fromTS []string
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(block[1], -1) {
		fromTS = append(fromTS, string(m[1]))
	}
	if len(fromTS) == 0 {
		t.Fatal("parsed IMG_PAINTABLE_MIMES but found no entries — the regex is wrong, " +
			"and an empty list would pass vacuously if compared to an empty Go map")
	}

	var fromGo []string
	for mime := range imgPaintableMimes {
		fromGo = append(fromGo, mime)
	}
	sort.Strings(fromTS)
	sort.Strings(fromGo)

	if strings.Join(fromTS, ",") != strings.Join(fromGo, ",") {
		t.Errorf("paintable MIME lists have drifted:\n  TS: %v\n  Go: %v", fromTS, fromGo)
	}
}

// TestShouldEmbedAsImage_RequestedVariant — codex round 1.
//
// The question is about THE VARIANT THIS RENDER WILL REQUEST, not about whether
// any thumbnail exists. Derivation writes thumb-sm and thumb-md independently,
// so a partial derivation leaves one present and the other absent. A boolean
// "has a thumbnail" flag answers the wrong question: it says image, the render
// asks for the missing variant, the endpoint falls back to the undecodable
// original, and the broken image is back — behind machinery that looks like it
// should have prevented exactly this.
func TestShouldEmbedAsImage_RequestedVariant(t *testing.T) {
	heic := func(have ...string) *AttachmentMeta {
		return &AttachmentMeta{MimeType: "image/heic", DerivedVariants: have, DerivedKnown: true}
	}

	if ShouldEmbedAsImage(heic("thumb-sm"), "thumb-md") {
		t.Error("only thumb-sm exists but a thumb-md render was called an image")
	}
	if ShouldEmbedAsImage(heic("thumb-md"), "thumb-sm") {
		t.Error("only thumb-md exists but a thumb-sm render was called an image")
	}
	if !ShouldEmbedAsImage(heic("thumb-sm"), "thumb-sm") {
		t.Error("a render satisfied by its own variant was called a chip")
	}
	if !ShouldEmbedAsImage(heic("thumb-sm", "thumb-md"), "thumb-md") {
		t.Error("both variants present but the thumb-md render was called a chip")
	}
	// An ORIGINAL-pointed render cannot be carried by a variant existing: the
	// browser gets the uploader's bytes, so only the paintable half can carry it.
	if ShouldEmbedAsImage(heic("thumb-sm", "thumb-md"), "") {
		t.Error("an original-pointed HEIC render was called an image")
	}
	png := &AttachmentMeta{MimeType: "image/png", DerivedVariants: nil, DerivedKnown: true}
	if !ShouldEmbedAsImage(png, "") {
		t.Error("an original-pointed PNG render was called a chip")
	}
}

// TestRenderAttachmentImage_UsesTheDecidedVariant pins the coupling that made
// the round-1 finding possible in the first place: the variant the decision asks
// about and the variant the URL requests are the same constant.
func TestRenderAttachmentImage_UsesTheDecidedVariant(t *testing.T) {
	html := RenderAttachmentImage(&AttachmentMeta{ID: "a1", MimeType: "image/png"}, "alt", "ws")
	if !strings.Contains(html, "variant="+imageRenderVariant) {
		t.Errorf("rendered img does not request %q: %s", imageRenderVariant, html)
	}
}
