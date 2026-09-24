// BUG-2177: a rotate (or crop) whose server-side transform finishes after the
// master editor froze (a pane opened over it) used to drop its result with no
// word to the user. Lead ruling, day 78: NOT replayed on thaw (applying an
// image edit minutes later changes the document while the user is not
// looking); the user is told it was interrupted.
//
// Driven through the REAL Editor.svelte mount and the real image toolbar, on
// the setup editorCapabilitiesDelivery uses (capabilities resolved, MIME probe
// stubbed), so the button clicked is the one a user clicks.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import type { Editor as TiptapEditor } from '@tiptap/core';

const PNG = '11111111-1111-4111-8111-111111111111';
const ROTATED = '55555555-5555-4555-8555-555555555555';

let releaseTransform: ((v: unknown) => void) | null = null;
vi.mock('$lib/api/client', () => ({
	api: {
		server: {
			capabilities: async () => ({ image: { image_formats: ['png'], can_transcode: true, max_pixels: 1e8 } }),
		},
		attachments: {
			downloadUrl: (ws: string, id: string, variant?: string) =>
				`/api/v1/workspaces/${ws}/attachments/${id}?variant=${variant ?? 'thumb-md'}`,
			transform: vi.fn(() => new Promise((resolve) => (releaseTransform = resolve))),
			upload: vi.fn(),
		},
		items: { list: vi.fn(async () => []) },
	},
}));

vi.mock('./attachment-metadata', async (importOriginal) => {
	const actual = await importOriginal<typeof import('./attachment-metadata')>();
	const ok = { status: 'ok' as const, mime: 'image/png', size: 4096 };
	return {
		...actual,
		fetchAttachmentMetadata: async () => ok,
		revalidateAttachmentMetadata: async () => ok,
		invalidateAttachmentMetadata: () => {},
	};
});

const { default: BodyEditor } = await import('./Editor.svelte');
const { IMAGE_EDIT_INTERRUPTED } = await import('./attachment-image');
const { page } = await import('$app/state');

async function settle(): Promise<void> {
	for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
	flushSync();
}

function imageUuid(tiptap: TiptapEditor): string | null {
	let uuid: string | null = null;
	tiptap.state.doc.descendants((n) => {
		if (n.type.name === 'attachmentImage') uuid = n.attrs.uuid as string;
	});
	return uuid;
}

describe('an image edit finishing after the master froze (BUG-2177)', () => {
	let target: HTMLElement;
	let instance: Record<string, unknown> | null = null;
	let tiptap: TiptapEditor | null = null;
	let alertSpy: ReturnType<typeof vi.spyOn>;
	let errSpy: ReturnType<typeof vi.spyOn>;

	beforeEach(async () => {
		releaseTransform = null;
		page.params.workspace = 'ws';
		alertSpy = vi.spyOn(window, 'alert').mockImplementation(() => {});
		errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
		target = document.body.appendChild(document.createElement('div'));
		instance = mount(BodyEditor, {
			target,
			props: {
				content: `![A diagram](pad-attachment:${PNG})`,
				itemId: 'item-A',
				hostToken: 'freeze-1',
				onEditor: (e: TiptapEditor) => {
					tiptap = e;
				},
			},
		}) as Record<string, unknown>;
		flushSync();
		await settle();
	});

	afterEach(() => {
		if (instance) unmount(instance);
		target.remove();
		document.querySelectorAll('.attachment-image-toolbar').forEach((el) => el.remove());
		alertSpy.mockRestore();
		errSpy.mockRestore();
	});

	async function clickRotate(): Promise<void> {
		const e = tiptap!;
		let pos: number | null = null;
		e.state.doc.descendants((n, at) => {
			if (n.type.name === 'attachmentImage') pos = at;
		});
		e.commands.setNodeSelection(pos!);
		await settle();
		const btn = e.view.dom.querySelector<HTMLButtonElement>('.attachment-image-toolbar-btn[data-degrees]');
		if (!btn) throw new Error('no rotate button on the real toolbar');
		expect(btn.disabled).toBe(false);
		btn.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));
		btn.click();
		await settle();
		if (!releaseTransform) throw new Error('clicking rotate did not start a transform');
	}

	it('CONTROL: on an editable editor the rotate lands and nothing is reported', async () => {
		await clickRotate();
		releaseTransform!({ id: ROTATED });
		await settle();
		expect(imageUuid(tiptap!)).toBe(ROTATED);
		expect(alertSpy).not.toHaveBeenCalled();
	});

	it('finishing while FROZEN leaves the image as it was and tells the user', async () => {
		await clickRotate();
		tiptap!.setEditable(false);
		releaseTransform!({ id: ROTATED });
		await settle();
		expect(imageUuid(tiptap!)).toBe(PNG);
		expect(alertSpy).toHaveBeenCalledTimes(1);
		expect(String(alertSpy.mock.calls[0][0])).toContain(IMAGE_EDIT_INTERRUPTED);
		// Not replayed on thaw (the ruling): the document stays as it was.
		tiptap!.setEditable(true);
		await settle();
		expect(imageUuid(tiptap!)).toBe(PNG);
	});
});
