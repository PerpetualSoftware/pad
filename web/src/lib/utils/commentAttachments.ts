/**
 * Paste / drag-drop attachment upload for the plain-textarea comment
 * composers (ItemTimeline + TimelineCommentCard reply box). The rich
 * item editor has its own ProseMirror-based plugin (attachment-upload.ts);
 * comments are plain markdown textareas, so they get this lighter helper:
 * upload the file, splice a `pad-attachment:UUID` markdown reference into
 * the bound value at the caret, and let the existing render path resolve
 * it to an inline `<img>` / file chip (IDEA-1650).
 *
 * Uploads leave attachments.item_id NULL — exactly like the editor —
 * because the canonical association is the markdown reference in the
 * posted comment body, not a column. The orphan GC's reference scan
 * (Store.AttachmentReferenced) covers comment bodies so a posted
 * screenshot is protected; an abandoned draft (placeholder never
 * resolved into a posted comment) falls to GC after the grace period.
 */

import { api } from '$lib/api/client';
import type { AttachmentUploadResult } from '$lib/types';

/** Extract file payloads from a paste event (screenshots, copied files). */
export function filesFromPaste(event: ClipboardEvent): File[] {
	const dt = event.clipboardData;
	if (!dt) return [];
	const out: File[] = [];
	for (const item of Array.from(dt.items)) {
		if (item.kind !== 'file') continue;
		const file = item.getAsFile();
		if (file) out.push(file);
	}
	return out;
}

/** Extract dropped files from a drop event. */
export function filesFromDrop(event: DragEvent): File[] {
	const dt = event.dataTransfer;
	if (!dt || dt.files.length === 0) return [];
	return Array.from(dt.files);
}

// UUIDs are the only thing after the `pad-attachment:` prefix in a
// well-formed reference; the 36-char canonical form is what the upload
// handler returns. Anchored to that shape so trailing `)`/whitespace in
// the markdown doesn't bleed into the captured id.
const ATTACHMENT_REF_RE = /pad-attachment:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})/g;

/** Every `pad-attachment:UUID` id referenced in a markdown string. */
export function attachmentRefsIn(text: string): string[] {
	if (!text) return [];
	const out: string[] = [];
	for (const m of text.matchAll(ATTACHMENT_REF_RE)) out.push(m[1]);
	return out;
}

/**
 * Markdown snippet for an uploaded attachment. Image MIMEs use image
 * syntax (inline embed); everything else uses link syntax (file chip) —
 * mirrors the editor's nodeForResult image/chip split so a comment and
 * the item body render the same upload identically.
 */
export function markdownRefFor(result: AttachmentUploadResult): string {
	const name = result.filename || 'attachment';
	const ref = `pad-attachment:${result.id}`;
	return result.category === 'image' ? `![${name}](${ref})` : `[${name}](${ref})`;
}

/** Bound-value accessors the host component supplies for a textarea. */
export interface TextareaUploadHandle {
	/** Read the current textarea value. */
	getValue(): string;
	/** Write a new value back to the bound state. */
	setValue(next: string): void;
	/** +1 when an upload starts, -1 when it settles — drives the busy gate. */
	onPendingDelta(delta: number): void;
	/** Surface an upload failure to the user. */
	onError(message: string): void;
}

// Monotonic across the page so concurrent uploads never share a token.
let uploadCounter = 0;

/**
 * Upload pasted/dropped files and weave `pad-attachment:` markdown into
 * the bound textarea value. Each file gets a visible placeholder spliced
 * in at the caret immediately; the placeholder is swapped for the real
 * reference on success or removed on failure. Uploads run concurrently
 * and each replaces its own (unique) placeholder regardless of completion
 * order — so editing or pasting more while one is in flight is safe.
 */
export function uploadIntoTextarea(
	files: File[],
	textarea: HTMLTextAreaElement,
	wsSlug: string,
	handle: TextareaUploadHandle
): void {
	if (files.length === 0) return;

	const value = handle.getValue();
	const start = textarea.selectionStart ?? value.length;
	const end = textarea.selectionEnd ?? start;

	const jobs = files.map((file) => {
		uploadCounter += 1;
		const token = `pad-upload-${Date.now().toString(36)}-${uploadCounter}`;
		const name = file.name || 'attachment';
		// The token sits in the href slot so the placeholder string is
		// unique even when two files share a filename — string replace
		// below then targets exactly this upload.
		return { file, placeholder: `![Uploading ${name}…](${token})` };
	});

	// Splice all placeholders in at the selection, space-separated so
	// adjacent embeds stay distinct markdown tokens. Pad with leading /
	// trailing spaces only when the surrounding text would otherwise abut.
	const inserted = jobs.map((j) => j.placeholder).join(' ');
	const lead = start > 0 && !/\s$/.test(value.slice(0, start)) ? ' ' : '';
	const trail = end < value.length && !/^\s/.test(value.slice(end)) ? ' ' : '';
	handle.setValue(value.slice(0, start) + lead + inserted + trail + value.slice(end));

	for (const job of jobs) {
		handle.onPendingDelta(1);
		api.attachments
			.upload(wsSlug, job.file)
			.then((res) => {
				handle.setValue(handle.getValue().replace(job.placeholder, markdownRefFor(res)));
			})
			.catch((err) => {
				// Drop the placeholder (and a trailing space it introduced) so a
				// failed upload leaves the draft as if nothing was pasted.
				handle.setValue(
					handle.getValue().replace(`${job.placeholder} `, '').replace(job.placeholder, '')
				);
				handle.onError(err instanceof Error ? err.message : 'Upload failed');
			})
			.finally(() => handle.onPendingDelta(-1));
	}
}
