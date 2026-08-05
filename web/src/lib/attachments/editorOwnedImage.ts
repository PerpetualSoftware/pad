/**
 * Is this attachment image owned by a LIVE editor rather than by rendered
 * markup? (PLAN-2392 DR-12 / TASK-2432.)
 *
 * Two very different things produce `<img data-attachment-id="…">`:
 *
 *   - sanitized `{@html}` output from `$lib/markdown/attachments.ts` — inert
 *     markup, so whichever surface rendered it also owns its interactivity;
 *   - the AttachmentImage Tiptap NodeView inside a live editor — which owns
 *     its OWN activation, semantics and propagation (see
 *     `$lib/components/editor/attachment-image.ts`).
 *
 * A surface that delegates over a subtree containing BOTH cannot tell them
 * apart by selector, and ItemTimeline is exactly that surface: its delegated
 * thumbnail handlers and its role/tabindex pass cover the whole entry list,
 * and that list contains live CommentEditor instances
 * (`TimelineCommentCard.svelte`). Two concrete failures follow from treating
 * the NodeView's image as one of its own:
 *
 *   - its accessibility pass strips role/tabindex/aria-label from a NodeView
 *     image whose UUID isn't in `attMeta` — which is every image in a DRAFT
 *     comment, because `attMeta` is probed from SAVED bodies only. The image
 *     silently stops being keyboard-reachable.
 *   - its delegated keydown matches Enter/Space with no modifier check, so a
 *     MODIFIED activation key — which the NodeView deliberately lets through,
 *     because Cmd/Ctrl+Enter is CommentEditor's submit binding — opens a
 *     viewer on top of the submit.
 *
 * `.ProseMirror` is the class ProseMirror puts on every view root — but the
 * CLASS ALONE IS FORGEABLE. Rendered markdown may carry raw HTML, and
 * `sanitizeMarkdownHtml` allows both `class` and `data-attachment-id`
 * (`$lib/utils/markdown.ts`), so a comment body containing
 * `<div class="ProseMirror"><img data-attachment-id="…"></div>` is ordinary
 * user content that the class test alone would hand to nobody: invisible to
 * the timeline's delegation AND to its accessibility pass.
 *
 * `contenteditable` is what makes the marker sound. ProseMirror sets it on
 * every view root (`"false"` on a read-only one, so presence — not value — is
 * the test), and it is NOT in the sanitizer's attribute allowlist, so rendered
 * markdown cannot produce it.
 */
export function isEditorOwnedImage(el: Element): boolean {
	return el.closest('.ProseMirror[contenteditable]') !== null;
}
