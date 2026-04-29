package render

import (
	"strings"
	"testing"
)

func intp(i int) *int { return &i }

func TestIsAttachmentHref(t *testing.T) {
	cases := []struct {
		href string
		want bool
	}{
		{"pad-attachment:abc-123", true},
		{"pad-attachment:", true}, // technically a prefix, ParseAttachmentHref returns ""
		{"pad-attachments:abc", false},
		{"http://example.com", false},
		{"pad-attachment", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsAttachmentHref(c.href); got != c.want {
			t.Errorf("IsAttachmentHref(%q) = %v, want %v", c.href, got, c.want)
		}
	}
}

func TestParseAttachmentHref(t *testing.T) {
	cases := []struct {
		href string
		want string
	}{
		{"pad-attachment:abc-123", "abc-123"},
		{"pad-attachment: abc-123 ", "abc-123"}, // whitespace trimmed
		{"pad-attachment:", ""},                 // empty UUID
		{"pad-attachment:   ", ""},              // whitespace-only UUID
		{"http://example.com", ""},              // not a pad-attachment href
		{"", ""},
	}
	for _, c := range cases {
		if got := ParseAttachmentHref(c.href); got != c.want {
			t.Errorf("ParseAttachmentHref(%q) = %q, want %q", c.href, got, c.want)
		}
	}
}

func TestAttachmentDownloadURL(t *testing.T) {
	cases := []struct {
		ws, id, variant, want string
	}{
		{"acme", "abc-123", "", "/api/v1/workspaces/acme/attachments/abc-123"},
		{"acme", "abc-123", "thumb-md", "/api/v1/workspaces/acme/attachments/abc-123?variant=thumb-md"},
		{"weird ws", "id with space", "", "/api/v1/workspaces/weird%20ws/attachments/id%20with%20space"},
	}
	for _, c := range cases {
		if got := AttachmentDownloadURL(c.ws, c.id, c.variant); got != c.want {
			t.Errorf("AttachmentDownloadURL(%q,%q,%q) = %q, want %q", c.ws, c.id, c.variant, got, c.want)
		}
	}
}

func TestIsImageMime(t *testing.T) {
	cases := map[string]bool{
		"image/png":       true,
		"image/jpeg":      true,
		"image/webp":      true,
		"IMAGE/PNG":       true, // case-insensitive
		" image/png":      true, // whitespace tolerated
		"application/pdf": false,
		"text/plain":      false,
		"video/mp4":       false,
		"":                false,
		"image":           false,
		"images/png":      false,
	}
	for mime, want := range cases {
		if got := IsImageMime(mime); got != want {
			t.Errorf("IsImageMime(%q) = %v, want %v", mime, got, want)
		}
	}
}

func TestFormatAttachmentSize(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{-1, ""},
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{10 * 1024, "10 KB"},
		{1024 * 1024, "1.0 MB"},
		{int64(5.5 * 1024 * 1024), "5.5 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
	}
	for _, c := range cases {
		if got := FormatAttachmentSize(c.bytes); got != c.want {
			t.Errorf("FormatAttachmentSize(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}

func TestRenderAttachmentImage(t *testing.T) {
	meta := &AttachmentMeta{
		ID:        "abc-123",
		MimeType:  "image/png",
		Filename:  "screenshot.png",
		SizeBytes: 12345,
		Width:     intp(800),
		Height:    intp(600),
	}
	got := RenderAttachmentImage(meta, "Screenshot of dashboard", "acme")
	wantSubstrs := []string{
		`src="/api/v1/workspaces/acme/attachments/abc-123?variant=thumb-md"`,
		`data-attachment-id="abc-123"`,
		`alt="Screenshot of dashboard"`,
		`width="800"`,
		`height="600"`,
	}
	for _, s := range wantSubstrs {
		if !strings.Contains(got, s) {
			t.Errorf("RenderAttachmentImage missing %q in: %s", s, got)
		}
	}
}

func TestRenderAttachmentImage_NoDimensions(t *testing.T) {
	meta := &AttachmentMeta{ID: "abc", MimeType: "image/webp", Filename: "x.webp", SizeBytes: 1}
	got := RenderAttachmentImage(meta, "", "ws")
	if strings.Contains(got, "width=") || strings.Contains(got, "height=") {
		t.Errorf("expected no width/height when dims unknown, got: %s", got)
	}
	// Falls back to filename when alt is empty.
	if !strings.Contains(got, `alt="x.webp"`) {
		t.Errorf("expected alt fallback to filename, got: %s", got)
	}
}

func TestRenderAttachmentImage_EscapesUserInput(t *testing.T) {
	meta := &AttachmentMeta{ID: `<id>`, MimeType: "image/png", Filename: `f.png`, SizeBytes: 1}
	got := RenderAttachmentImage(meta, `"><script>alert(1)</script>`, "ws")
	if strings.Contains(got, "<script>") {
		t.Errorf("alt text not escaped: %s", got)
	}
	if strings.Contains(got, "<id>") {
		t.Errorf("id not escaped: %s", got)
	}
}

func TestRenderAttachmentImage_NilMeta(t *testing.T) {
	if got := RenderAttachmentImage(nil, "x", "ws"); got != "" {
		t.Errorf("nil meta should return empty string, got %q", got)
	}
}

func TestRenderAttachmentChip(t *testing.T) {
	meta := &AttachmentMeta{
		ID:        "abc-123",
		MimeType:  "application/pdf",
		Filename:  "report.pdf",
		SizeBytes: 5 * 1024 * 1024,
	}
	got := RenderAttachmentChip(meta, "", "acme")
	wants := []string{
		`class="file-chip"`,
		`href="/api/v1/workspaces/acme/attachments/abc-123"`,
		`data-attachment-id="abc-123"`,
		`download="report.pdf"`,
		`target="_blank"`,
		`rel="noopener noreferrer"`,
		`report.pdf`,
		`5.0 MB`,
	}
	for _, s := range wants {
		if !strings.Contains(got, s) {
			t.Errorf("chip missing %q in: %s", s, got)
		}
	}
}

func TestRenderAttachmentChip_DisplayTextOverridesFilename(t *testing.T) {
	meta := &AttachmentMeta{ID: "x", MimeType: "application/zip", Filename: "release.zip", SizeBytes: 100}
	got := RenderAttachmentChip(meta, "Latest release", "ws")
	if !strings.Contains(got, ">Latest release</span>") {
		t.Errorf("expected display text override, got: %s", got)
	}
	// download attribute always uses canonical filename, not the display text.
	if !strings.Contains(got, `download="release.zip"`) {
		t.Errorf("download attribute should use filename, got: %s", got)
	}
}

func TestRenderAttachmentChip_EscapesUserInput(t *testing.T) {
	meta := &AttachmentMeta{ID: "x", MimeType: "application/pdf", Filename: `<bad>".pdf`, SizeBytes: 1}
	got := RenderAttachmentChip(meta, `<script>alert(1)</script>`, "ws")
	if strings.Contains(got, "<script>") {
		t.Errorf("display text not escaped: %s", got)
	}
	if strings.Contains(got, `download="<bad>`) {
		t.Errorf("filename not escaped in download attr: %s", got)
	}
}

func TestRenderAttachmentMissing(t *testing.T) {
	got := RenderAttachmentMissing("abc-123", "Lost photo")
	wants := []string{
		`class="attachment-missing"`,
		`data-attachment-id="abc-123"`,
		`title="This attachment is missing or has been deleted"`,
		`Lost photo`,
	}
	for _, s := range wants {
		if !strings.Contains(got, s) {
			t.Errorf("missing placeholder lacks %q in: %s", s, got)
		}
	}
}

func TestRenderAttachmentMissing_DefaultText(t *testing.T) {
	got := RenderAttachmentMissing("abc-123", "")
	if !strings.Contains(got, "Missing attachment") {
		t.Errorf("expected default text, got: %s", got)
	}
}

func TestRenderAttachmentMissing_EscapesUuid(t *testing.T) {
	got := RenderAttachmentMissing(`<uuid>`, "")
	if strings.Contains(got, "<uuid>") {
		t.Errorf("uuid not escaped: %s", got)
	}
}

func TestResolveAttachmentImage(t *testing.T) {
	resolver := func(uuid string) *AttachmentMeta {
		switch uuid {
		case "img-1":
			return &AttachmentMeta{ID: "img-1", MimeType: "image/png", Filename: "shot.png", SizeBytes: 100}
		case "pdf-1":
			return &AttachmentMeta{ID: "pdf-1", MimeType: "application/pdf", Filename: "doc.pdf", SizeBytes: 100}
		}
		return nil
	}

	// Image MIME → <img>
	got := ResolveAttachmentImage("pad-attachment:img-1", "Screenshot", "ws", resolver)
	if !strings.HasPrefix(got, "<img ") {
		t.Errorf("expected <img>, got: %s", got)
	}

	// Non-image MIME → file chip (image syntax over a PDF)
	got = ResolveAttachmentImage("pad-attachment:pdf-1", "doc", "ws", resolver)
	if !strings.HasPrefix(got, `<a class="file-chip"`) {
		t.Errorf("expected file chip for non-image MIME, got: %s", got)
	}

	// Missing → placeholder
	got = ResolveAttachmentImage("pad-attachment:missing-1", "alt", "ws", resolver)
	if !strings.Contains(got, "attachment-missing") {
		t.Errorf("expected missing placeholder, got: %s", got)
	}

	// Non-attachment href → empty (caller should have filtered)
	if got := ResolveAttachmentImage("http://example.com/foo.png", "alt", "ws", resolver); got != "" {
		t.Errorf("non-attachment href should return empty, got: %s", got)
	}
}

func TestResolveAttachmentLink_AlwaysChip(t *testing.T) {
	resolver := func(uuid string) *AttachmentMeta {
		return &AttachmentMeta{ID: uuid, MimeType: "image/png", Filename: "x.png", SizeBytes: 100}
	}
	// Link syntax over an image → still chip
	got := ResolveAttachmentLink("pad-attachment:img-1", "Click me", "ws", resolver)
	if !strings.HasPrefix(got, `<a class="file-chip"`) {
		t.Errorf("link form should always render chip, got: %s", got)
	}
}

func TestResolveAttachmentReferences_BasicReplacement(t *testing.T) {
	resolver := func(uuid string) *AttachmentMeta {
		switch uuid {
		case "img-1":
			return &AttachmentMeta{ID: "img-1", MimeType: "image/png", Filename: "shot.png", SizeBytes: 1024}
		case "pdf-1":
			return &AttachmentMeta{ID: "pdf-1", MimeType: "application/pdf", Filename: "doc.pdf", SizeBytes: 4096}
		}
		return nil
	}
	in := `Here is an image: ![alt text](pad-attachment:img-1)

And a PDF: [Download report](pad-attachment:pdf-1)

Missing: ![](pad-attachment:gone-1)`
	out := ResolveAttachmentReferences(in, "acme", resolver)

	// Image rendered
	if !strings.Contains(out, `<img `) || !strings.Contains(out, `data-attachment-id="img-1"`) {
		t.Errorf("image reference not resolved: %s", out)
	}
	// PDF chip rendered
	if !strings.Contains(out, `class="file-chip"`) || !strings.Contains(out, `data-attachment-id="pdf-1"`) {
		t.Errorf("pdf reference not resolved: %s", out)
	}
	// Missing placeholder rendered
	if !strings.Contains(out, "attachment-missing") || !strings.Contains(out, `data-attachment-id="gone-1"`) {
		t.Errorf("missing reference not resolved: %s", out)
	}
}

func TestResolveAttachmentReferences_SkipsFencedCodeBlocks(t *testing.T) {
	resolver := func(uuid string) *AttachmentMeta {
		return &AttachmentMeta{ID: uuid, MimeType: "image/png", Filename: "x.png", SizeBytes: 1}
	}
	in := "Outside: ![real](pad-attachment:abc)\n\n```markdown\n![docs example](pad-attachment:should-stay)\n[link example](pad-attachment:also-stay)\n```\n\nAfter: ![real2](pad-attachment:def)\n"
	out := ResolveAttachmentReferences(in, "ws", resolver)

	// Outside the fence, references are resolved
	if !strings.Contains(out, `data-attachment-id="abc"`) {
		t.Errorf("expected outer reference resolved, got: %s", out)
	}
	if !strings.Contains(out, `data-attachment-id="def"`) {
		t.Errorf("expected post-fence reference resolved, got: %s", out)
	}

	// Inside the fence, the literal markdown text must be preserved
	if !strings.Contains(out, "![docs example](pad-attachment:should-stay)") {
		t.Errorf("fenced image example was substituted, got: %s", out)
	}
	if !strings.Contains(out, "[link example](pad-attachment:also-stay)") {
		t.Errorf("fenced link example was substituted, got: %s", out)
	}
}

func TestResolveAttachmentReferences_TildeFence(t *testing.T) {
	resolver := func(uuid string) *AttachmentMeta { return &AttachmentMeta{ID: uuid} }
	in := "~~~\n![x](pad-attachment:x)\n~~~\n"
	out := ResolveAttachmentReferences(in, "ws", resolver)
	if !strings.Contains(out, "![x](pad-attachment:x)") {
		t.Errorf("tilde fence not respected: %s", out)
	}
}

func TestResolveAttachmentReferences_RoundTripStable(t *testing.T) {
	// Same input + same metadata must produce identical output every call —
	// the share/export pipeline relies on this for cache validation.
	resolver := func(uuid string) *AttachmentMeta {
		return &AttachmentMeta{ID: uuid, MimeType: "image/png", Filename: "x.png", SizeBytes: 100, Width: intp(10), Height: intp(20)}
	}
	in := `# Title

![ok](pad-attachment:abc)`
	a := ResolveAttachmentReferences(in, "ws", resolver)
	b := ResolveAttachmentReferences(in, "ws", resolver)
	if a != b {
		t.Errorf("non-deterministic output:\n  a=%s\n  b=%s", a, b)
	}
}

func TestResolveAttachmentReferences_NilResolver(t *testing.T) {
	in := `![x](pad-attachment:abc)`
	if got := ResolveAttachmentReferences(in, "ws", nil); got != in {
		t.Errorf("nil resolver should pass-through, got: %s", got)
	}
}

func TestResolveAttachmentReferences_NoFalsePositives(t *testing.T) {
	resolver := func(uuid string) *AttachmentMeta {
		return &AttachmentMeta{ID: uuid, MimeType: "image/png", Filename: "x.png", SizeBytes: 1}
	}
	// References to other URL schemes must not be substituted.
	in := `![alt](http://example.com) and [link](https://example.com) and [ref](pad-attachment-not:abc)`
	out := ResolveAttachmentReferences(in, "ws", resolver)
	if out != in {
		t.Errorf("non-attachment refs were modified:\n  in =%s\n  out=%s", in, out)
	}
}

func TestResolveAttachmentReferences_HandlesTitleSuffix(t *testing.T) {
	// Markdown links allow an optional " title" after the URL.
	resolver := func(uuid string) *AttachmentMeta {
		return &AttachmentMeta{ID: uuid, MimeType: "image/png", Filename: "x.png", SizeBytes: 1}
	}
	in := `![alt](pad-attachment:abc "Some title")`
	out := ResolveAttachmentReferences(in, "ws", resolver)
	if !strings.Contains(out, `data-attachment-id="abc"`) {
		t.Errorf("title-suffixed reference not resolved: %s", out)
	}
}

func TestResolveAttachmentReferences_EscapedBrackets(t *testing.T) {
	// Marked's tokenizer accepts `\]` inside link/image labels and emits the
	// literal `]`. The Go regex must do the same to keep server/client
	// rendering byte-aligned for edge-case labels.
	resolver := func(uuid string) *AttachmentMeta {
		switch uuid {
		case "img-1":
			return &AttachmentMeta{ID: "img-1", MimeType: "image/png", Filename: "a.png", SizeBytes: 1}
		case "doc-1":
			return &AttachmentMeta{ID: "doc-1", MimeType: "application/pdf", Filename: "doc.pdf", SizeBytes: 1}
		}
		return nil
	}
	cases := []struct {
		name     string
		in       string
		wantSubs []string
	}{
		{
			name: "escaped close bracket in image alt",
			in:   `![Q1 \] report](pad-attachment:img-1)`,
			wantSubs: []string{
				`data-attachment-id="img-1"`,
				`alt="Q1 ] report"`,
			},
		},
		{
			name: "escaped close bracket in link text",
			in:   `[A \] B](pad-attachment:doc-1)`,
			wantSubs: []string{
				`data-attachment-id="doc-1"`,
				`>A ] B</span>`,
			},
		},
		{
			name: "escaped backslash and bracket combo",
			in:   `[a \\ b \[ c \] d](pad-attachment:doc-1)`,
			wantSubs: []string{
				`>a \ b [ c ] d</span>`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveAttachmentReferences(c.in, "ws", resolver)
			for _, s := range c.wantSubs {
				if !strings.Contains(got, s) {
					t.Errorf("missing %q in: %s", s, got)
				}
			}
		})
	}
}

func TestUnescapeMarkdownText(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"plain":             "plain",
		`\\`:                `\`,
		`\]`:                `]`,
		`\[`:                `[`,
		`\(`:                `(`,
		`\)`:                `)`,
		`\!`:                `!`,
		`a \] b`:            `a ] b`,
		`a \\ b \[ c \] d`:  `a \ b [ c ] d`,
		`unaffected: \n \t`: `unaffected: \n \t`, // non-punctuation escapes pass through
		`trailing \`:        `trailing \`,        // dangling backslash preserved
	}
	for in, want := range cases {
		if got := unescapeMarkdownText(in); got != want {
			t.Errorf("unescapeMarkdownText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitCodeFences_NestedAndUnclosed(t *testing.T) {
	// An unclosed fence at end-of-input should not crash. Everything after
	// the opener is treated as inside the fence.
	in := "before\n```\nstill inside\n"
	chunks := splitCodeFences(in)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %#v", len(chunks), chunks)
	}
	if chunks[0].fenced || !strings.Contains(chunks[0].text, "before") {
		t.Errorf("first chunk wrong: %#v", chunks[0])
	}
	if !chunks[1].fenced || !strings.Contains(chunks[1].text, "still inside") {
		t.Errorf("second chunk wrong: %#v", chunks[1])
	}
}
