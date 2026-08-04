// Attachment action descriptors (PLAN-2392 DR-5 / DR-5a / DR-19, TASK-2422).
//
// The point of the descriptor list is that the panel and the viewer cannot
// drift, so the assertions here are about the CONTRACT each descriptor
// publishes — which element it renders as, whether it exists at all for a
// given MIME, and what its href / download / run actually do — rather than
// about either renderer.
import { describe, it, expect, vi, beforeEach } from 'vitest';

const deleteMock = vi.fn();
const downloadUrlMock = vi.fn(
	(ws: string, id: string) => `/api/v1/workspaces/${ws}/attachments/${id}`
);

vi.mock('$lib/api/client', () => ({
	api: {
		attachments: {
			delete: (...args: unknown[]) => deleteMock(...args),
			downloadUrl: (ws: string, id: string) => downloadUrlMock(ws, id),
		},
	},
}));

const announceMock = vi.fn();
vi.mock('$lib/attachments/events', () => ({
	announceAttachmentDeleted: (...args: unknown[]) => announceMock(...args),
}));

const copyToClipboardMock = vi.fn(async (_text: string) => true);
vi.mock('$lib/utils/clipboard', () => ({
	copyToClipboard: (text: string) => copyToClipboardMock(text),
}));

import type { AttachmentAction, AttachmentActionContext } from './actions';

const { ATTACHMENT_ACTIONS, attachmentActionsFor, attachmentLinkUrl } = await import('./actions');

type Ctx = AttachmentActionContext;

// The descriptors import the real `canBrowserPreview` (DR-16 keeps every
// "what can this MIME do" question in one module), so these cases assert
// against the shipping predicate rather than a stand-in.

function ctx(overrides: Partial<Ctx> = {}): Ctx {
	return {
		workspaceSlug: 'ws',
		attachment: { id: 'att-1', filename: 'report.pdf', mime_type: 'application/pdf' },
		mutationsEnabled: true,
		origin: 'https://pad.example',
		...overrides,
	};
}

function action(id: string): AttachmentAction {
	const found = ATTACHMENT_ACTIONS.find((a) => a.id === id);
	if (!found) throw new Error(`no descriptor for ${id}`);
	return found;
}

beforeEach(() => {
	deleteMock.mockReset();
	deleteMock.mockResolvedValue(undefined);
	announceMock.mockReset();
	copyToClipboardMock.mockReset();
	copyToClipboardMock.mockResolvedValue(true);
	downloadUrlMock.mockClear();
});

describe('attachment action descriptors', () => {
	it('exposes exactly the four in-scope actions, in render order', () => {
		expect(ATTACHMENT_ACTIONS.map((a) => a.id)).toEqual([
			'open',
			'download',
			'copy-link',
			'delete',
		]);
	});

	it('omits Open entirely for a type the browser cannot preview', () => {
		const zip = ctx({
			attachment: { id: 'att-2', filename: 'bundle.zip', mime_type: 'application/zip' },
		});
		expect(attachmentActionsFor(zip).map((a) => a.id)).toEqual([
			'download',
			'copy-link',
			'delete',
		]);
		// Not "present but disabled" — a greyed Open would imply a preview
		// Pad could give and won't.
		expect(action('open').applies(zip)).toBe(false);
	});

	it('does not offer Open for an SVG, even though it is labelled image/*', () => {
		// DR-16: the predicate is an exact allowlist, not an `image/` prefix,
		// and the descriptors reach it directly — a caller cannot hand them a
		// looser one. An SVG can carry active content, so it gets Download only.
		const svg = ctx({
			attachment: { id: 'att-3', filename: 'diagram.svg', mime_type: 'image/svg+xml' },
		});
		expect(attachmentActionsFor(svg).map((a) => a.id)).toEqual([
			'download',
			'copy-link',
			'delete',
		]);
	});

	it('offers Open for a PDF, as a new-tab anchor', () => {
		const pdf = ctx();
		expect(attachmentActionsFor(pdf).map((a) => a.id)).toEqual([
			'open',
			'download',
			'copy-link',
			'delete',
		]);
		const open = action('open');
		expect(open.element).toBe('anchor');
		if (open.element !== 'anchor') throw new Error('unreachable');
		expect(open.href(pdf)).toBe('/api/v1/workspaces/ws/attachments/att-1');
		expect(open.target).toBe('_blank');
		expect(open.rel).toBe('noopener noreferrer');
		// An anchor performs its own navigation; a `run` here would double-fire.
		expect('run' in open).toBe(false);
	});

	it('renders Download as an anchor carrying a real download filename (DR-16)', () => {
		const c = ctx();
		const download = action('download');
		expect(download.element).toBe('anchor');
		if (download.element !== 'anchor') throw new Error('unreachable');
		expect(download.applies(c)).toBe(true);
		expect(download.href(c)).toBe('/api/v1/workspaces/ws/attachments/att-1');
		expect(download.download?.(c)).toBe('report.pdf');
	});

	it('copies an absolute same-origin URL and labels itself as workspace-scoped (DR-5a)', async () => {
		const c = ctx();
		const copy = action('copy-link');
		expect(copy.element).toBe('button');
		if (copy.element !== 'button') throw new Error('unreachable');

		const url = attachmentLinkUrl(c);
		expect(url).toBe('https://pad.example/api/v1/workspaces/ws/attachments/att-1');
		// Absolute, so it survives being pasted somewhere else.
		expect(new URL(url).origin).toBe('https://pad.example');

		await copy.run(c);
		expect(copyToClipboardMock).toHaveBeenCalledWith(url);

		// The user-visible text says what the link actually is — not a share link.
		expect(`${copy.label} ${copy.description ?? ''}`.toLowerCase()).toContain('workspace');
	});

	it('falls back to location.origin when the context does not supply one', () => {
		const original = (globalThis as { location?: unknown }).location;
		Object.defineProperty(globalThis, 'location', {
			value: { origin: 'https://runtime.example' },
			configurable: true,
			writable: true,
		});
		try {
			const c = ctx();
			delete (c as { origin?: string }).origin;
			expect(attachmentLinkUrl(c)).toBe(
				'https://runtime.example/api/v1/workspaces/ws/attachments/att-1'
			);
		} finally {
			if (original === undefined) delete (globalThis as { location?: unknown }).location;
			else
				Object.defineProperty(globalThis, 'location', {
					value: original,
					configurable: true,
					writable: true,
				});
		}
	});

	it('surfaces a clipboard failure rather than reporting a copy that did not happen', async () => {
		copyToClipboardMock.mockResolvedValue(false);
		const onCopied = vi.fn();
		const copy = action('copy-link');
		if (copy.element !== 'button') throw new Error('unreachable');
		await expect(copy.run(ctx({ onCopied }))).rejects.toThrow(/clipboard/i);
		expect(onCopied).not.toHaveBeenCalled();
	});

	it('disables Delete when mutations are off, and refuses to run anyway', async () => {
		const readOnly = ctx({ mutationsEnabled: false });
		const del = action('delete');
		if (del.element !== 'button') throw new Error('unreachable');

		expect(del.applies(readOnly)).toBe(true);
		expect(del.enabled(readOnly)).toBe(false);
		expect(del.enabled(ctx())).toBe(true);
		expect(del.danger).toBe(true);

		await del.run(readOnly);
		expect(deleteMock).not.toHaveBeenCalled();
		expect(announceMock).not.toHaveBeenCalled();
	});

	it('deletes exactly like the tile does: api.delete then announceAttachmentDeleted', async () => {
		const onDeleted = vi.fn();
		const del = action('delete');
		if (del.element !== 'button') throw new Error('unreachable');

		await del.run(ctx({ onDeleted }));

		expect(deleteMock).toHaveBeenCalledWith('ws', 'att-1');
		expect(announceMock).toHaveBeenCalledWith('ws', 'att-1');
		expect(onDeleted).toHaveBeenCalledWith('att-1');
		// No state_generation / undo token in this plan (DR-19).
		expect(announceMock.mock.calls[0]).toHaveLength(2);
	});

	it('aborts the delete before any request when the host confirmation says no', async () => {
		const del = action('delete');
		if (del.element !== 'button') throw new Error('unreachable');
		await del.run(ctx({ confirmDelete: () => false }));
		expect(deleteMock).not.toHaveBeenCalled();
		expect(announceMock).not.toHaveBeenCalled();
	});

	it('treats a 404 as authoritative: broadcasts and does not throw', async () => {
		deleteMock.mockRejectedValue(Object.assign(new Error('gone'), { code: 'not_found' }));
		const onDeleted = vi.fn();
		const del = action('delete');
		if (del.element !== 'button') throw new Error('unreachable');

		await expect(del.run(ctx({ onDeleted }))).resolves.toBeUndefined();
		expect(announceMock).toHaveBeenCalledWith('ws', 'att-1');
		expect(onDeleted).toHaveBeenCalledWith('att-1');
	});

	it('propagates a real delete failure without announcing a deletion', async () => {
		deleteMock.mockRejectedValue(Object.assign(new Error('boom'), { code: 'internal' }));
		const del = action('delete');
		if (del.element !== 'button') throw new Error('unreachable');

		await expect(del.run(ctx())).rejects.toThrow('boom');
		expect(announceMock).not.toHaveBeenCalled();
	});

	it('disables the addressable actions when the workspace or id is missing', () => {
		const unaddressable = ctx({ workspaceSlug: '' });
		for (const id of ['open', 'download', 'copy-link', 'delete']) {
			expect(action(id).enabled(unaddressable)).toBe(false);
		}
	});
});
