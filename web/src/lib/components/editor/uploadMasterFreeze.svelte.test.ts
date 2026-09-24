// BUG-2177: an attachment upload in flight when the master editor freezes (a
// detail pane opened over it, TASK-2172 / TASK-2180) used to DROP its result:
// the bytes were stored server-side and nothing referenced them, silently.
// Lead ruling, day 78: the upload is DEFERRED, inserted at its mapped
// placeholder once the editor is editable again; an editor that is gone by
// then tells the user, naming the file.
//
// Driven through the REAL Editor.svelte mount and its real upload wiring
// (CONVE-19). The freeze is `setEditable(false)`, which is what Editor.svelte's
// `editable` prop does for a peeking master.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import type { Editor as TiptapEditor } from '@tiptap/core';

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'user-1',
		user: { id: 'user-1', role: 'member' },
		identityEpoch: 0,
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));

const pendingUploads: { resolve: (v: unknown) => void; reject: (e: unknown) => void }[] = [];
vi.mock('$lib/api/client', () => ({
	api: {
		server: { capabilities: () => new Promise(() => {}) },
		attachments: {
			downloadUrl: (ws: string, id: string) => `/api/v1/workspaces/${ws}/attachments/${id}`,
			transform: vi.fn(),
			upload: vi.fn(
				() => new Promise((resolve, reject) => pendingUploads.push({ resolve, reject }))
			),
		},
		items: { list: vi.fn(async () => []) },
	},
}));

const { default: BodyEditor } = await import('./Editor.svelte');
const { page } = await import('$app/state');
const { toastStore } = await import('$lib/stores/toast.svelte');

const RESULT = {
	id: '44444444-4444-4444-8444-444444444444',
	url: '/x',
	mime: 'application/pdf',
	size: 10,
	filename: 'report.pdf',
	category: 'document' as const,
};

async function settle(): Promise<void> {
	for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
}

function chipUuids(editor: TiptapEditor): string[] {
	const out: string[] = [];
	editor.state.doc.descendants((n) => {
		if (n.type.name === 'attachmentChip') out.push(n.attrs.uuid as string);
	});
	return out;
}

describe('upload in flight across a master freeze (BUG-2177)', () => {
	let target: HTMLElement;
	let instance: Record<string, unknown> | null = null;
	let tiptap: TiptapEditor | null = null;

	beforeEach(() => {
		pendingUploads.length = 0;
		toastStore.clearAll();
		page.params.workspace = 'ws';
		target = document.body.appendChild(document.createElement('div'));
		tiptap = null;
		instance = mount(BodyEditor, {
			target,
			props: {
				content: 'hello world',
				itemId: 'item-2177',
				hostToken: 'upload-freeze',
				onEditor: (e: TiptapEditor) => {
					tiptap = e;
				},
			},
		}) as Record<string, unknown>;
		flushSync();
	});

	afterEach(() => {
		if (instance) unmount(instance);
		instance = null;
		target.remove();
	});

	function editor(): TiptapEditor {
		if (!tiptap) throw new Error('Editor.svelte did not construct a Tiptap editor');
		return tiptap;
	}

	function startUploadAtEnd(): void {
		const e = editor();
		e.commands.setTextSelection(e.state.doc.content.size - 1);
		expect(e.commands.uploadAttachments([new File(['x'], RESULT.filename, { type: RESULT.mime })])).toBe(true);
		expect(pendingUploads).toHaveLength(1);
	}

	it('CONTROL: an upload finishing on an editable editor is inserted at once', async () => {
		startUploadAtEnd();
		pendingUploads[0].resolve(RESULT);
		await settle();
		expect(chipUuids(editor())).toEqual([RESULT.id]);
	});

	it('finishing while FROZEN inserts nothing yet, then inserts at the MAPPED placeholder on thaw', async () => {
		startUploadAtEnd();
		const e = editor();
		e.setEditable(false);
		// A peer's edit while frozen (programmatic transactions still apply to a
		// read-only view): text inserted BEFORE the placeholder shifts it.
		e.view.dispatch(e.state.tr.insertText('PEER ', 1));
		pendingUploads[0].resolve(RESULT);
		await settle();
		expect(chipUuids(e)).toEqual([]);

		e.setEditable(true);
		await settle();
		expect(chipUuids(e)).toEqual([RESULT.id]);
		// Landed where the user dropped it (after "hello world"), carried past
		// the peer's insertion, not at the stale pre-shift position.
		const text = e.state.doc.textContent;
		expect(text.startsWith('PEER hello world')).toBe(true);
		let chipPos = -1;
		e.state.doc.descendants((n, pos) => {
			if (n.type.name === 'attachmentChip') chipPos = pos;
		});
		expect(e.state.doc.textBetween(0, chipPos)).toContain('PEER hello world');
	});

	it('a peer DELETING the placeholder while frozen: nothing inserted at the boundary on thaw, and the user is told', async () => {
		startUploadAtEnd();
		const e = editor();
		e.setEditable(false);
		// The whole paragraph holding the placeholder goes (a peer's delete, or a
		// document replaced by a restore): its mapped number survives, the place
		// the user dropped the file does not.
		e.view.dispatch(e.state.tr.replaceWith(0, e.state.doc.content.size, e.schema.nodes.paragraph.create(null, e.schema.text('replaced by a peer'))));
		pendingUploads[0].resolve(RESULT);
		await settle();
		e.setEditable(true);
		await settle();
		expect(chipUuids(e)).toEqual([]);
		expect(e.state.doc.textContent).toBe('replaced by a peer');
		const messages = toastStore.toasts.map((t) => t.message);
		expect(messages.some((m) => m.includes(RESULT.filename) && m.includes('not inserted'))).toBe(true);
	});

	it('an editor destroyed while a finished upload is PARKED tells the user the file is stored but not inserted', async () => {
		startUploadAtEnd();
		editor().setEditable(false);
		pendingUploads[0].resolve(RESULT);
		await settle();
		expect(chipUuids(editor())).toEqual([]);
		unmount(instance!);
		instance = null;
		await settle();
		const messages = toastStore.toasts.map((t) => t.message);
		expect(messages.filter((m) => m.includes(RESULT.filename) && m.includes('not inserted'))).toHaveLength(1);
	});

	it('an editor destroyed before the upload finishes tells the user the file is stored but not inserted', async () => {
		startUploadAtEnd();
		unmount(instance!);
		instance = null;
		pendingUploads[0].resolve(RESULT);
		await settle();
		const messages = toastStore.toasts.map((t) => t.message);
		expect(messages.some((m) => m.includes(RESULT.filename) && m.includes('not inserted'))).toBe(true);
	});

	it('a FAILURE while frozen is still reported, since the user started that upload', async () => {
		const alertSpy = vi.spyOn(window, 'alert').mockImplementation(() => {});
		const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
		try {
			startUploadAtEnd();
			editor().setEditable(false);
			pendingUploads[0].reject(new Error('network down'));
			await settle();
			expect(alertSpy).toHaveBeenCalledTimes(1);
			expect(String(alertSpy.mock.calls[0][0])).toContain(RESULT.filename);
		} finally {
			alertSpy.mockRestore();
			errSpy.mockRestore();
		}
	});
});
