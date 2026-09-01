import { describe, it, expect } from 'vitest';
import { renderMarkdownDocument, renderMarkedWithAttachments } from './markdown';

/**
 * `renderMarkdownDocument`'s ISOLATION contract (IDEA-2712, codex R3 #3).
 *
 * jsdom, because the function ends in `sanitizeMarkdownHtml`, which needs a
 * window and returns '' without one.
 *
 * The attachment context this module threads through `marked` is MODULE-LEVEL
 * state. `renderMarkdownDocument` renders a FOREIGN document — a file attached
 * to an item, authored elsewhere — so it must resolve none of that document's
 * `pad-attachment:` references against whatever workspace happens to be
 * rendering it. Leaving the context alone is not enough: a nested call inherits
 * the outer one.
 *
 * The nesting is reachable, not theoretical — a resolver or a `missing` hook is
 * caller-supplied and may itself render markdown, which is exactly why
 * `renderMarkedWithAttachments` already saves and restores rather than clearing
 * to null.
 */

const UUID = '01234567-89ab-cdef-0123-456789abcdef';
const ATTACHMENT_MD = `![diagram](pad-attachment:${UUID})`;

describe('renderMarkdownDocument — context isolation', () => {
	it('does NOT inherit an outer attachment context', () => {
		let inner = '';
		// The outer render installs a context whose resolver would happily
		// resolve the UUID against `outer-ws`; from inside it, the document
		// renderer runs.
		renderMarkedWithAttachments(ATTACHMENT_MD, {
			resolver: (id) => {
				inner = renderMarkdownDocument(ATTACHMENT_MD);
				return {
					id,
					mime_type: 'image/png',
					filename: 'outer.png',
					size_bytes: 10,
					width: 1,
					height: 1
				};
			},
			workspaceSlug: 'outer-ws'
		});

		// The discriminating artifact: the OUTER workspace's URL. If the inner
		// call inherited the context it would have resolved the reference and
		// emitted `/api/v1/workspaces/outer-ws/attachments/<uuid>` — a live link
		// into a workspace the attached document knows nothing about.
		expect(inner).not.toContain('outer-ws');
		expect(inner).not.toContain('/api/v1/workspaces/');
		// It rendered SOMETHING (the paragraph wrapper), so the assertion above
		// is isolation rather than the call having failed outright.
		expect(inner).toContain('<p>');
	});

	it('RESTORES the outer context afterwards', () => {
		// Clearing without restoring would be the opposite bug: the outer
		// document's remaining references would stop resolving mid-parse.
		let sawInner = false;
		const html = renderMarkedWithAttachments(
			`${ATTACHMENT_MD}\n\n![second](pad-attachment:${UUID})`,
			{
				resolver: (id) => {
					if (!sawInner) {
						sawInner = true;
						renderMarkdownDocument('# nested');
					}
					return {
						id,
						mime_type: 'image/png',
						filename: 'outer.png',
						size_bytes: 10,
						width: 1,
						height: 1
					};
				},
				workspaceSlug: 'outer-ws'
			}
		);

		expect(sawInner).toBe(true);
		// BOTH outer references still resolved — the second one is the one a
		// non-restoring implementation would strand.
		const matches = html.match(/\/api\/v1\/workspaces\/outer-ws\//g) ?? [];
		expect(matches.length).toBe(2);
	});

	it('drops an unresolvable pad-attachment reference rather than linking it', () => {
		// The behaviour the doc comment now states, asserted instead of guessed:
		// with no context, marked emits `<img src="pad-attachment:…">` and
		// DOMPurify does not know that scheme, so the src is removed.
		const html = renderMarkdownDocument(ATTACHMENT_MD);
		expect(html).not.toContain('pad-attachment:');
	});
});
