import { describe, it, expect } from 'vitest';
import DOMPurify from 'dompurify';
import { renderMarkedWithAttachments } from './markdown';
import { renderAttachmentUnavailable } from '$lib/markdown/attachments';

// BUG-2389, jsdom half (DOMPurify needs a window; vitest routes
// environments by filename — see markdown.shareAttachments.test.ts for
// the node-side legs). Pins that the share page's default-config
// DOMPurify pass keeps the placeholder intact: the span, its
// data-attachment-id, and the title wording all survive, and hostile
// alt text stays neutralized after sanitization.

const UUID = '01234567-89ab-cdef-0123-456789abcdef';

function shareRender(md: string): string {
	return renderMarkedWithAttachments(md, {
		resolver: () => null,
		workspaceSlug: '',
		missing: renderAttachmentUnavailable
	});
}

describe('share placeholder × DOMPurify (BUG-2389)', () => {
	it("survives the share page's default-config DOMPurify pass intact", () => {
		// Same call shape as /s/[token]: DOMPurify.sanitize(raw), no config.
		const clean = DOMPurify.sanitize(
			shareRender(`![diagram](pad-attachment:${UUID})`)
		);
		expect(clean).toContain('attachment-unavailable');
		expect(clean).toContain(`data-attachment-id="${UUID}"`);
		expect(clean).toContain("aren't available on shared pages");
	});

	it('a hostile alt stays neutralized after sanitization', () => {
		const clean = DOMPurify.sanitize(
			shareRender(`![<script>alert(1)</script>](pad-attachment:${UUID})`)
		);
		expect(clean).not.toContain('<script');
		expect(clean).toContain('attachment-unavailable');
	});
});
