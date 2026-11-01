// TASK-4195: a file name is data a stranger controls; wherever it is shown it
// must be isolated so a right-to-left override inside it cannot reorder the
// text around it. Per-component sites have their own tests (strip, storage,
// lightbox, chip); this file covers the helpers and the string/markup sites
// that need no component mount.
import { describe, it, expect } from 'vitest';
import {
	displayFilename,
	displayFilenameIsolated,
	isolateText
} from './display';
import {
	EVIL_NAME,
	FSI,
	PDI,
	expectFsiPdi,
	expectIsolated,
	expectNoIsolates
} from './bidiIsolation.testutil';
import {
	attachmentDeletePrompt,
	workspaceAttachmentDeletePrompt
} from '$lib/components/attachments/AttachmentDeleteConfirm.svelte';
import { buildPlaceholder, storedNotInsertedMessage } from '$lib/components/editor/attachment-upload';
import { renderAttachmentChip, renderAttachmentImage } from '$lib/markdown/attachments';
import {
	renderMarkedWithAttachments,
	renderMarkdown,
	sanitizeMarkdownHtml
} from '$lib/utils/markdown';
import { restrictShareMarkup } from '$lib/components/share/shareRender';

const ID = '11111111-2222-3333-4444-555555555555';
const meta = (filename: string) => ({
	id: ID,
	mime_type: 'application/pdf',
	filename,
	size_bytes: 10,
	width: null,
	height: null
});

describe('isolateText / displayFilenameIsolated', () => {
	it('wraps in FSI .. PDI', () => {
		expect(isolateText('x')).toBe(FSI + 'x' + PDI);
		expect(FSI).toBe(String.fromCharCode(0x2068));
		expect(PDI).toBe(String.fromCharCode(0x2069));
	});
	it('isolates the normalized name, including the blank fallback', () => {
		expectFsiPdi(displayFilenameIsolated(EVIL_NAME), EVIL_NAME);
		expect(displayFilenameIsolated('  ')).toBe(FSI + 'Untitled file' + PDI);
	});
	it('GUARD: displayFilename itself stays isolate-free (it feeds data seeds)', () => {
		expectNoIsolates(displayFilename(EVIL_NAME));
		expect(displayFilename(EVIL_NAME)).toBe(EVIL_NAME);
	});
});

describe('delete prompts (site 15)', () => {
	it('isolate the name in both prompts', () => {
		expectFsiPdi(attachmentDeletePrompt(EVIL_NAME, true), EVIL_NAME);
		expectFsiPdi(attachmentDeletePrompt(EVIL_NAME, false), EVIL_NAME);
		expectFsiPdi(workspaceAttachmentDeletePrompt(EVIL_NAME), EVIL_NAME);
	});
});

describe('attachment-upload (sites 7, 16)', () => {
	it('the Uploading placeholder keeps the name inside a bdi', () => {
		const el = buildPlaceholder(EVIL_NAME);
		expectIsolated(el, EVIL_NAME);
		expect(el.textContent).toContain('Uploading ');
	});
	it('the stored-not-inserted notice isolates the name only', () => {
		const msg = storedNotInsertedMessage(EVIL_NAME);
		expectFsiPdi(msg, EVIL_NAME);
		expect(msg.startsWith(FSI + EVIL_NAME + PDI + ' was uploaded')).toBe(true);
	});
});

describe('markdown file chip (site 6, 12)', () => {
	function mount(html: string): HTMLElement {
		const d = document.createElement('div');
		d.innerHTML = html;
		return d;
	}
	it('keeps the bdi through the sanitizer, with download= untouched', () => {
		const raw = renderAttachmentChip(meta(EVIL_NAME), EVIL_NAME, 'ws');
		const clean = sanitizeMarkdownHtml(raw);
		expectIsolated(mount(clean), EVIL_NAME);
		const a = mount(clean).querySelector('a.file-chip')!;
		expect(a.getAttribute('download')).toBe(EVIL_NAME);
		expectNoIsolates(a.getAttribute('download'));
		expectNoIsolates(clean);
	});
	it('keeps the bdi through the share restrict pass', () => {
		const raw = renderAttachmentChip(meta(EVIL_NAME), EVIL_NAME, 'ws');
		const shared = restrictShareMarkup(sanitizeMarkdownHtml(raw));
		expectIsolated(mount(shared), EVIL_NAME);
	});
	it('renders through the full body pipeline', () => {
		const html = renderMarkedWithAttachments(`[${EVIL_NAME}](pad-attachment:${ID})`, {
			resolver: () => meta(EVIL_NAME),
			workspaceSlug: 'ws',
			urlBuilder: (u: string) => `/a/${u}`
		});
		expectIsolated(mount(sanitizeMarkdownHtml(html)), EVIL_NAME);
	});
	it('isolates only the filename FALLBACK of an image alt, never authored alt', () => {
		const img = { ...meta(EVIL_NAME), mime_type: 'image/png', width: 1, height: 1 };
		expectFsiPdi(mount(renderAttachmentImage(img, '', 'ws')).querySelector('img')!.getAttribute('alt'), EVIL_NAME);
		const authored = mount(renderAttachmentImage(img, 'my alt', 'ws')).querySelector('img')!;
		expect(authored.getAttribute('alt')).toBe('my alt');
	});
});

describe('bdi in the markdown sanitizer allowlist (Q2)', () => {
	it('survives, with every attribute it does not allowlist stripped, and is inert', () => {
		const dirty = '<bdi onclick="alert(1)" style="position:fixed" dir="rtl" onmouseover="x()">hi</bdi>';
		for (const out of [sanitizeMarkdownHtml(dirty), renderMarkdown(dirty)]) {
			const bdi = new DOMParser().parseFromString(out, 'text/html').querySelector('bdi')!;
			expect(bdi, out).not.toBeNull();
			expect(bdi.textContent).toBe('hi');
			for (const attr of ['onclick', 'style', 'dir', 'onmouseover']) {
				expect(bdi.hasAttribute(attr), attr).toBe(false);
			}
		}
	});
	it('does not let a script ride along inside a user-authored bdi', () => {
		const out = sanitizeMarkdownHtml('<bdi><script>alert(1)</script><img src=x onerror=alert(1)></bdi>');
		expect(out).not.toContain('<script');
		expect(out).not.toContain('onerror');
	});
});
