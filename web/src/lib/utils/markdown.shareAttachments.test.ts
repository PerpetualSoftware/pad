import { describe, it, expect } from 'vitest';
import { marked } from 'marked';
import { renderMarkdown, renderMarkedWithAttachments } from './markdown';
import {
	renderAttachmentUnavailable,
	type AttachmentMeta
} from '$lib/markdown/attachments';

// BUG-2389 — the share route's markdown pass is now attachment-aware.
// These pin the three behaviors that matter:
//   1. share-shaped rendering (null resolver + unavailable placeholder)
//      turns refs into HONEST placeholders, never broken <img>/dead links;
//   2. the wrapper is OPT-IN — a bare marked() call keeps its pre-fix
//      fallthrough, so no other surface changes behavior;
//   3. a real resolver through the same wrapper yields full resolution,
//      which is what the token-scoped read path (2b) will plug into.

const UUID = '01234567-89ab-cdef-0123-456789abcdef';
const IMG_MD = `before ![diagram](pad-attachment:${UUID}) after`;
const LINK_MD = `see [the file](pad-attachment:${UUID})`;

function shareRender(md: string): string {
	return renderMarkedWithAttachments(md, {
		resolver: () => null,
		workspaceSlug: '',
		missing: renderAttachmentUnavailable
	});
}

describe('share-page attachment rendering (BUG-2389)', () => {
	it('renders an image ref as the honest unavailable placeholder', () => {
		const html = shareRender(IMG_MD);
		expect(html).toContain('attachment-unavailable');
		expect(html).toContain("aren't available on shared pages");
		expect(html).toContain('diagram');
		// Never a broken image pointing at the unfetchable pseudo-URL.
		expect(html).not.toContain('<img src="pad-attachment:');
	});

	it('renders a link ref as a placeholder, not a dead link', () => {
		const html = shareRender(LINK_MD);
		expect(html).toContain('attachment-unavailable');
		expect(html).toContain('the file');
		expect(html).not.toContain('href="pad-attachment:');
	});

	it('does NOT say "missing or deleted" — the attachment exists', () => {
		const html = shareRender(IMG_MD);
		expect(html).not.toContain('missing or has been deleted');
	});

	it('control: a bare marked() call keeps the pre-fix fallthrough (wrapper is opt-in)', () => {
		const html = marked(IMG_MD) as string;
		expect(html).toContain('<img src="pad-attachment:');
		expect(html).not.toContain('attachment-unavailable');
	});

	it('a real resolver through the same wrapper resolves fully (the 2b plug-in point)', () => {
		const meta: AttachmentMeta = {
			id: UUID,
			filename: 'diagram.png',
			mime_type: 'image/png',
			size_bytes: 1234
		} as AttachmentMeta;
		const html = renderMarkedWithAttachments(IMG_MD, {
			resolver: () => meta,
			workspaceSlug: 'ws-1'
		});
		expect(html).toContain('<img');
		expect(html).not.toContain('attachment-unavailable');
		expect(html).not.toContain('pad-attachment:');
	});

	it('escapes adversarial alt/label text flowing into the placeholder', () => {
		const hostile = `![<img src=x onerror=alert(1)>"'&](pad-attachment:${UUID})`;
		const html = shareRender(hostile);
		// The payload must arrive entity-escaped, never as live markup.
		expect(html).not.toContain('<img');
		expect(html).toContain('&lt;img src=x onerror=alert(1)&gt;');
		expect(html).toContain('attachment-unavailable');
	});

	// The DOMPurify-preservation leg lives in
	// markdown.shareAttachments.svelte.test.ts — it needs the jsdom
	// project (DOMPurify's default export requires a window), and vitest
	// routes environments by filename.

	it('is re-entrancy safe — a nested render restores the outer context', () => {
		// A missing-hook that itself renders markdown through the wrapper
		// (with a DIFFERENT hook) must not strand the outer parse: the ref
		// AFTER the nested call still gets the share placeholder.
		const nestedHook = (uuid: string, alt: string) => {
			renderMarkedWithAttachments('plain text, no refs', {
				resolver: () => null,
				workspaceSlug: '',
				missing: () => '<span class="inner-hook"></span>'
			});
			return renderAttachmentUnavailable(uuid, alt);
		};
		const html = renderMarkedWithAttachments(
			`![a](pad-attachment:${UUID}) and ![b](pad-attachment:${UUID})`,
			{ resolver: () => null, workspaceSlug: '', missing: nestedHook }
		);
		expect((html.match(/attachment-unavailable"/g) ?? []).length).toBe(2);
		expect(html).not.toContain('<img src="pad-attachment:');
	});

	it('a nested renderMarkdown() also restores the outer context', () => {
		// renderMarkdown (the wiki-link pipeline) shares the same module
		// context and used to clear it unconditionally — a hook that
		// renders a sub-document through it must not strand the outer
		// share render (codex round 2).
		const nestedHook = (uuid: string, alt: string) => {
			renderMarkdown('plain text, no refs', [], 'ws-1');
			return renderAttachmentUnavailable(uuid, alt);
		};
		const html = renderMarkedWithAttachments(
			`![a](pad-attachment:${UUID}) and ![b](pad-attachment:${UUID})`,
			{ resolver: () => null, workspaceSlug: '', missing: nestedHook }
		);
		expect((html.match(/attachment-unavailable"/g) ?? []).length).toBe(2);
		expect(html).not.toContain('<img src="pad-attachment:');
	});

	it('clears the module context — a following bare marked() is unaffected', () => {
		shareRender(IMG_MD);
		const html = marked(IMG_MD) as string;
		expect(html).toContain('<img src="pad-attachment:');
	});
});

// TASK-2637 (BUG-2389 2b) — the share page now supplies a urlBuilder that
// targets the token-scoped byte endpoint (with the signed suffix for
// protected links) instead of the workspace path. These pin the frontend
// seam: the override reaches the <img src>, carries the requested variant,
// and merges a signed fragment robustly.
describe('share-page attachment urlBuilder (TASK-2637)', () => {
	const meta: AttachmentMeta = {
		id: UUID,
		filename: 'diagram.png',
		mime_type: 'image/png',
		size_bytes: 1234
	} as AttachmentMeta;

	it('routes the <img src> through urlBuilder, not the workspace path', () => {
		const html = renderMarkedWithAttachments(IMG_MD, {
			resolver: () => meta,
			workspaceSlug: 'ws-1',
			urlBuilder: (uuid) => `/api/v1/s/tok/attachments/${uuid}`
		});
		expect(html).toContain(`<img src="/api/v1/s/tok/attachments/${UUID}"`);
		// The default workspace URL must NOT appear — the override won.
		expect(html).not.toContain('/api/v1/workspaces/ws-1/attachments/');
	});

	it('passes the image variant to urlBuilder', () => {
		let seen: string | undefined = 'UNCALLED';
		renderMarkedWithAttachments(IMG_MD, {
			resolver: () => meta,
			workspaceSlug: '',
			imageVariant: 'thumb-md',
			urlBuilder: (uuid, variant) => {
				seen = variant;
				return `/x/${uuid}`;
			}
		});
		expect(seen).toBe('thumb-md');
	});

	it('merges a signed fragment with the variant param (protected-link shape)', () => {
		// Mirrors the share page's URLSearchParams merge: sig fragment + variant.
		const sig = 'exp=123&sig=abc';
		const build = (uuid: string, variant?: string) => {
			const params = new URLSearchParams();
			if (variant) params.set('variant', variant);
			if (sig) for (const [k, v] of new URLSearchParams(sig)) params.set(k, v);
			const qs = params.toString();
			return `/api/v1/s/tok/attachments/${uuid}${qs ? `?${qs}` : ''}`;
		};
		const html = renderMarkedWithAttachments(IMG_MD, {
			resolver: () => meta,
			workspaceSlug: '',
			imageVariant: 'thumb-md',
			urlBuilder: build
		});
		expect(html).toContain(`/api/v1/s/tok/attachments/${UUID}?`);
		expect(html).toContain('variant=thumb-md');
		expect(html).toContain('exp=123');
		expect(html).toContain('sig=abc');
	});

	it('a uuid absent from the refs map still renders the honest placeholder', () => {
		// The share page resolver returns null for an unanchored/unrenderable
		// id; that must fall to the unavailable placeholder, never a broken img.
		const html = renderMarkedWithAttachments(IMG_MD, {
			resolver: () => null, // not in refs
			workspaceSlug: '',
			missing: renderAttachmentUnavailable,
			urlBuilder: (uuid) => `/api/v1/s/tok/attachments/${uuid}`
		});
		expect(html).toContain('attachment-unavailable');
		expect(html).not.toContain('/api/v1/s/tok/attachments/');
	});
});
