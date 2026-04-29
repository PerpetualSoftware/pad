// Package render implements server-side rendering helpers shared between
// API endpoints that emit pre-rendered HTML (a future shared-item view,
// markdown export pipelines, etc.) and the editor's preview path.
//
// Today the only consumer is the attachment reference resolver — image
// and file-chip rendering for `pad-attachment:UUID` markdown references.
// The TS-side companion lives at `web/src/lib/markdown/attachments.ts`;
// keep the two implementations in lock-step so server-rendered and
// client-rendered output match for the same input. See DOC-865 for the
// architecture.
package render

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
)

// AttachmentMeta is the minimal metadata needed to render a `pad-attachment:UUID`
// reference. Trimmed from the full models.Attachment row so callers can pass
// a thin DTO without dragging the whole package graph in.
type AttachmentMeta struct {
	ID        string
	MimeType  string
	Filename  string
	SizeBytes int64
	Width     *int
	Height    *int
}

// AttachmentResolver looks up an attachment by UUID. Returns nil for
// missing/deleted attachments — the resolver renders a "missing"
// placeholder in that case so the document doesn't show a broken-image
// icon.
type AttachmentResolver func(uuid string) *AttachmentMeta

// attachmentRefPrefix is the URL scheme item content uses to reference
// attachments (mirrored on the TS side as ATTACHMENT_URL_PREFIX). Stored
// verbatim — never resolved to a backend URL — so a backend migration can
// rewrite storage_keys without touching item content. See DOC-865.
const attachmentRefPrefix = "pad-attachment:"

// IsAttachmentHref reports whether href is a `pad-attachment:UUID` reference.
func IsAttachmentHref(href string) bool {
	return strings.HasPrefix(href, attachmentRefPrefix)
}

// ParseAttachmentHref returns the UUID portion of a `pad-attachment:UUID`
// reference, or "" if href is not a pad-attachment reference / has an
// empty UUID after trimming whitespace.
func ParseAttachmentHref(href string) string {
	if !IsAttachmentHref(href) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(href, attachmentRefPrefix))
}

// AttachmentDownloadURL builds the canonical attachment download URL.
// Pass an empty variant for the original; otherwise "thumb-sm" / "thumb-md"
// (the server falls back to the original if no derived row exists).
func AttachmentDownloadURL(workspaceSlug, attachmentID, variant string) string {
	base := "/api/v1/workspaces/" + url.PathEscape(workspaceSlug) + "/attachments/" + url.PathEscape(attachmentID)
	if variant == "" {
		return base
	}
	return base + "?variant=" + url.QueryEscape(variant)
}

// IsImageMime reports whether the MIME type renders inline as an image.
// Mirrors `image/*` from the server-side allowlist (image/png, image/jpeg,
// image/gif, image/webp, image/avif, image/heic, image/heif).
func IsImageMime(mime string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "image/")
}

// FormatAttachmentSize returns a human-readable byte count ("832 B",
// "1.2 MB", "5 GB"). Negative sizes return "" — that's a corrupt-row
// signal, not something to print to the user.
func FormatAttachmentSize(bytes int64) string {
	if bytes < 0 {
		return ""
	}
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	n := float64(bytes) / 1024
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if n < 10 {
		return fmt.Sprintf("%.1f %s", n, units[i])
	}
	return fmt.Sprintf("%.0f %s", n, units[i])
}

// RenderAttachmentImage builds an inline `<img>` element for an image
// attachment. Width/height attributes are emitted when both are known and
// positive so the browser can reserve layout space before the bytes
// arrive — avoiding the reflow jank that would otherwise hit on every
// render cycle.
//
// The `data-attachment-id` attribute is the editor's hook for click-to-
// zoom, rotate, and crop interactions.
//
// All user-controlled strings are HTML-escaped before interpolation;
// callers can safely pipe the result into a larger HTML document.
func RenderAttachmentImage(meta *AttachmentMeta, alt, workspaceSlug string) string {
	if meta == nil {
		return ""
	}
	src := AttachmentDownloadURL(workspaceSlug, meta.ID, "thumb-md")
	altText := alt
	if strings.TrimSpace(altText) == "" {
		altText = meta.Filename
	}
	sizeAttrs := ""
	if meta.Width != nil && meta.Height != nil && *meta.Width > 0 && *meta.Height > 0 {
		sizeAttrs = fmt.Sprintf(` width="%d" height="%d"`, *meta.Width, *meta.Height)
	}
	return fmt.Sprintf(`<img src="%s" data-attachment-id="%s" alt="%s"%s>`,
		html.EscapeString(src),
		html.EscapeString(meta.ID),
		html.EscapeString(altText),
		sizeAttrs,
	)
}

// RenderAttachmentChip builds a Notion-style file-chip anchor for a
// non-image attachment. `target=_blank` + `rel=noopener noreferrer`
// matches the existing external-link defaults, and the `download`
// attribute lets browsers save the file with its canonical filename.
//
// displayText overrides the chip label when non-empty; otherwise the
// attachment's filename is used.
func RenderAttachmentChip(meta *AttachmentMeta, displayText, workspaceSlug string) string {
	if meta == nil {
		return ""
	}
	href := AttachmentDownloadURL(workspaceSlug, meta.ID, "")
	label := displayText
	if strings.TrimSpace(label) == "" {
		label = meta.Filename
	}
	size := FormatAttachmentSize(meta.SizeBytes)
	sizeSpan := ""
	if size != "" {
		sizeSpan = ` <span class="file-chip-size">· ` + html.EscapeString(size) + `</span>`
	}
	return fmt.Sprintf(
		`<a class="file-chip" href="%s" data-attachment-id="%s" download="%s" target="_blank" rel="noopener noreferrer"><span class="file-chip-icon" aria-hidden="true">📄</span><span class="file-chip-name">%s</span>%s</a>`,
		html.EscapeString(href),
		html.EscapeString(meta.ID),
		html.EscapeString(meta.Filename),
		html.EscapeString(label),
		sizeSpan,
	)
}

// RenderAttachmentMissing builds a placeholder span for a missing/deleted
// attachment. Keeps the document layout intact and tells the user *why*
// there's no image — a broken-image icon would suggest a transient network
// failure, but this is a permanent metadata state (the row is soft-deleted
// or the UUID was never valid).
func RenderAttachmentMissing(uuid, alt string) string {
	safeAlt := alt
	if strings.TrimSpace(safeAlt) == "" {
		safeAlt = "Missing attachment"
	}
	return fmt.Sprintf(
		`<span class="attachment-missing" data-attachment-id="%s" title="This attachment is missing or has been deleted">📎 %s</span>`,
		html.EscapeString(uuid),
		html.EscapeString(safeAlt),
	)
}

// ResolveAttachmentImage resolves a `![alt](pad-attachment:UUID)` image
// reference into rendered HTML. Falls back to a file chip when the
// attachment exists but isn't an image MIME — the markdown author asked
// for an embed, but a PDF or zip can't sensibly inline.
func ResolveAttachmentImage(href, alt, workspaceSlug string, resolve AttachmentResolver) string {
	uuid := ParseAttachmentHref(href)
	if uuid == "" {
		return ""
	}
	meta := resolve(uuid)
	if meta == nil {
		return RenderAttachmentMissing(uuid, alt)
	}
	if IsImageMime(meta.MimeType) {
		return RenderAttachmentImage(meta, alt, workspaceSlug)
	}
	chipText := alt
	if strings.TrimSpace(chipText) == "" {
		chipText = meta.Filename
	}
	return RenderAttachmentChip(meta, chipText, workspaceSlug)
}

// ResolveAttachmentLink resolves a `[text](pad-attachment:UUID)` link
// reference into rendered HTML. Always renders as a file chip — link
// syntax indicates the user wants a downloadable reference, not an
// inline embed, even when the underlying MIME is an image.
func ResolveAttachmentLink(href, text, workspaceSlug string, resolve AttachmentResolver) string {
	uuid := ParseAttachmentHref(href)
	if uuid == "" {
		return ""
	}
	meta := resolve(uuid)
	if meta == nil {
		return RenderAttachmentMissing(uuid, text)
	}
	return RenderAttachmentChip(meta, text, workspaceSlug)
}

// markdownAttachmentRefRE matches both image and link forms of a
// pad-attachment reference in source markdown:
//
//	![alt](pad-attachment:UUID)
//	[text](pad-attachment:UUID)
//
// The leading `!?` captures whether the form is image or link. The
// alt/text capture accepts CommonMark escape sequences (`\]`, `\\`, etc.)
// so labels containing literal brackets — e.g. `[Q1 \] report](pad-…)` —
// match the same way marked does on the TS side; without this, the two
// renderers disagree on edge-case labels and the lock-step contract
// breaks. Captured escapes are unwound in unescapeMarkdownText before the
// label reaches the render helpers. The UUID capture rejects whitespace
// and `)` so it stops at the closing paren. Markdown's optional " title"
// suffix on a link destination is handled — anything between the UUID
// and `)` after a space matches the title group we currently ignore
// (our HTML output doesn't surface the title attribute today).
//
// References inside fenced code blocks must be skipped — that's done
// by ResolveAttachmentReferences's caller via splitCodeFences.
var markdownAttachmentRefRE = regexp.MustCompile(
	`(!?)\[((?:\\.|[^\]\\])*)\]\(pad-attachment:([^\s)]+)(?:\s+"[^"]*")?\)`,
)

// unescapeMarkdownText reverses CommonMark escape sequences in a captured
// label. Marked's tokenizer drops the backslash and emits the literal
// character, so the regex-based Go path needs to mirror that behavior
// before passing the label to the render helpers — otherwise a label
// like `Q1 \] report` would render with the backslash visible.
//
// We only unescape backslash + ASCII punctuation that can appear inside
// a label match: `\\`, `\]`, `\[`, `\(`, `\)`, `\!`. Other escapes
// pass through unchanged — matches CommonMark §6.1 closely enough for
// the labels Pad's editor produces.
func unescapeMarkdownText(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\', ']', '[', '(', ')', '!':
				b.WriteByte(s[i+1])
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ResolveAttachmentReferences scans `markdown` for `pad-attachment:UUID`
// references in standard markdown image/link syntax and replaces each
// with rendered HTML using the supplied resolver:
//
//   - `![alt](pad-attachment:UUID)` → <img> for image MIMEs, file chip otherwise
//   - `[text](pad-attachment:UUID)` → file chip
//   - resolver returns nil → missing-attachment placeholder span
//
// References inside fenced code blocks (```…```) are left untouched so
// documentation about the reference syntax renders verbatim. References
// inside inline code (single backticks) are NOT skipped — that's a known
// limitation; the editor doesn't generate references inside code spans,
// and stripping them robustly would require a full markdown tokenizer.
//
// The output is mixed markdown + HTML. Downstream callers can pass it
// through a CommonMark-compatible markdown→HTML stage, which renders
// HTML blocks and inline HTML transparently.
func ResolveAttachmentReferences(markdown, workspaceSlug string, resolve AttachmentResolver) string {
	if resolve == nil {
		return markdown
	}
	chunks := splitCodeFences(markdown)
	var b strings.Builder
	b.Grow(len(markdown))
	for _, c := range chunks {
		if c.fenced {
			b.WriteString(c.text)
			continue
		}
		b.WriteString(markdownAttachmentRefRE.ReplaceAllStringFunc(c.text, func(match string) string {
			sub := markdownAttachmentRefRE.FindStringSubmatch(match)
			if sub == nil {
				return match
			}
			isImage := sub[1] == "!"
			altOrText := unescapeMarkdownText(sub[2])
			uuid := strings.TrimSpace(sub[3])
			href := attachmentRefPrefix + uuid
			if isImage {
				return ResolveAttachmentImage(href, altOrText, workspaceSlug, resolve)
			}
			return ResolveAttachmentLink(href, altOrText, workspaceSlug, resolve)
		}))
	}
	return b.String()
}

// codeChunk is a single span of input — either inside a fenced code
// block (fenced=true) or outside one. ResolveAttachmentReferences
// skips substitution inside fenced spans so docs that demonstrate the
// reference syntax render literally.
type codeChunk struct {
	text   string
	fenced bool
}

// splitCodeFences partitions markdown source into alternating fenced /
// non-fenced spans. The fence detector recognizes the CommonMark form:
// at least three backticks or tildes at the start of a line, optional
// info string, terminated by a fence of the same character at column 0
// (or end of input). Indented code blocks are not skipped — they're
// rare in our content and the spec for indented code is already fragile.
func splitCodeFences(s string) []codeChunk {
	var chunks []codeChunk
	lines := strings.SplitAfter(s, "\n")
	var cur strings.Builder
	inFence := false
	var fenceChar byte
	var fenceLen int
	flush := func(fenced bool) {
		if cur.Len() == 0 {
			return
		}
		chunks = append(chunks, codeChunk{text: cur.String(), fenced: fenced})
		cur.Reset()
	}
	for _, line := range lines {
		if !inFence {
			if ch, n, ok := openingFence(line); ok {
				// flush the non-fenced span we were accumulating
				flush(false)
				inFence = true
				fenceChar = ch
				fenceLen = n
				cur.WriteString(line)
				continue
			}
			cur.WriteString(line)
			continue
		}
		// inside a fence
		cur.WriteString(line)
		if isClosingFence(line, fenceChar, fenceLen) {
			flush(true)
			inFence = false
		}
	}
	if inFence {
		flush(true)
	} else {
		flush(false)
	}
	return chunks
}

// openingFence checks if line starts a CommonMark fenced code block.
// Returns the fence character (' ` ' or '~'), the length, and ok.
func openingFence(line string) (byte, int, bool) {
	trimmed := strings.TrimLeft(line, " ")
	// CommonMark allows up to 3 spaces of indent before the fence.
	if len(line)-len(trimmed) > 3 {
		return 0, 0, false
	}
	if len(trimmed) < 3 {
		return 0, 0, false
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	// For backtick fences, the info string MUST NOT contain a backtick
	// (CommonMark §4.5). We don't bother validating beyond fence length
	// here; isClosingFence enforces the matching rules on the close.
	return ch, n, true
}

// isClosingFence checks if line closes a fenced code block opened with
// `fenceLen` of `fenceChar`. The closing fence must be at least as long
// as the opening fence, and must consist only of the fence character
// (after optional indentation).
func isClosingFence(line string, fenceChar byte, fenceLen int) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return false
	}
	// Strip the trailing newline if present.
	trimmed = strings.TrimRight(trimmed, "\n")
	trimmed = strings.TrimRight(trimmed, "\r")
	if len(trimmed) < fenceLen {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != fenceChar {
			// closing fence may be followed only by whitespace
			return strings.TrimSpace(trimmed[i:]) == "" && i >= fenceLen
		}
	}
	return len(trimmed) >= fenceLen
}
