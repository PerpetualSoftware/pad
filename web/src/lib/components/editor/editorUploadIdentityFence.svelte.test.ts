// BUG-3105 PR B, codex round 1: the body editor's upload callback resolves by
// INSERTING the uploaded node into the live document, so an upload that lands
// after the signed-in user changed would write the previous user's attachment
// into a document the next user is editing. The callback now captures an
// identity fence before its await and REJECTS on a mismatch — which the upload
// plugin turns into "remove the placeholder, insert nothing".
//
// Driven through the REAL `Editor.svelte` mount and the `upload` option its own
// `AttachmentUpload.configure` installed — not a copy of that config — so the
// binding is what is under test (team CONVE-19).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import type { Editor as TiptapEditor } from '@tiptap/core';

// A controllable identity epoch with the real store's semantics: a fence
// captures the epoch and answers whether it still holds.
const identity = vi.hoisted(() => ({ epoch: 0 }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'user-1',
		user: { id: 'user-1', role: 'member' },
		get identityEpoch() {
			return identity.epoch;
		},
		identityFence: () => {
			const captured = identity.epoch;
			return () => identity.epoch === captured;
		},
		onIdentityChange: () => () => {},
	},
}));

let releaseUpload: ((v: unknown) => void) | null = null;
const uploadMock = vi.hoisted(() => vi.fn());
vi.mock('$lib/api/client', () => ({
	api: {
		server: { capabilities: () => new Promise(() => {}) },
		attachments: {
			downloadUrl: (ws: string, id: string) => `/api/v1/workspaces/${ws}/attachments/${id}`,
			transform: vi.fn(),
			upload: uploadMock,
		},
		items: { list: vi.fn(async () => []) },
	},
}));

const { default: BodyEditor } = await import('./Editor.svelte');
const { page } = await import('$app/state');

const RESULT = {
	id: '33333333-3333-4333-8333-333333333333',
	filename: 'a.png',
	mime_type: 'image/png',
	size: 10,
	url: '/x',
};

describe('Editor upload callback — identity fence (BUG-3105)', () => {
	let target: HTMLElement;
	let instance: Record<string, unknown> | null = null;
	let tiptap: TiptapEditor | null = null;

	beforeEach(() => {
		identity.epoch = 0;
		releaseUpload = null;
		uploadMock.mockReset();
		uploadMock.mockImplementation(
			() => new Promise((resolve) => (releaseUpload = resolve))
		);
		page.params.workspace = 'ws';
		target = document.body.appendChild(document.createElement('div'));
		tiptap = null;
		instance = mount(BodyEditor, {
			target,
			props: {
				content: '',
				itemId: 'item-A',
				hostToken: 'upload-fence-1',
				onEditor: (e: TiptapEditor) => {
					tiptap = e;
				},
			},
		}) as Record<string, unknown>;
		flushSync();
	});

	afterEach(() => {
		if (instance) unmount(instance);
		target.remove();
	});

	function configuredUpload(): (file: File) => Promise<unknown> {
		if (!tiptap) throw new Error('Editor.svelte did not construct a Tiptap editor');
		const ext = tiptap.extensionManager.extensions.find((e) => e.name === 'attachmentUpload');
		if (!ext) throw new Error('no attachmentUpload extension on the real mount');
		return (ext.options as { upload: (file: File) => Promise<unknown> }).upload;
	}

	it('CONTROL: with the identity unchanged, the upload resolves to the server result', async () => {
		const pending = configuredUpload()(new File(['x'], 'a.png', { type: 'image/png' }));
		expect(uploadMock).toHaveBeenCalledTimes(1);
		releaseUpload!(RESULT);
		await expect(pending).resolves.toEqual(RESULT);
	});

	it('an upload resolving after the signed-in user changed is REFUSED, not inserted', async () => {
		const pending = configuredUpload()(new File(['x'], 'a.png', { type: 'image/png' }));
		// The request really went out under the first identity — this is the
		// window the fence exists for, not a refusal before sending.
		expect(uploadMock).toHaveBeenCalledTimes(1);
		identity.epoch++;
		releaseUpload!(RESULT);
		await expect(pending).rejects.toThrow('upload refused: the signed-in user changed');
	});
});
