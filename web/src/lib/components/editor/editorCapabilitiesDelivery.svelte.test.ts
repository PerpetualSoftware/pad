// Do the server's image-processor capabilities actually REACH the inline
// image NodeView? (BUG-2426 / PLAN-2392 phase 3a / TASK-2435.)
//
// They used to not. `Editor.svelte` fetched `/server/capabilities` and wrote
// the format list onto the extension — `ext.options.supportedFormats = …` —
// and Tiptap's `options` is a GETTER that returns a fresh spread per access,
// so the write landed on a temporary and was gone by the next statement. The
// NodeView compounds it by snapshotting `this.options` once when it is built.
// Every visible symptom (rotate + crop permanently disabled with the "not
// available in this build" tooltip) came from a line that reads like it works.
//
// That is why NONE of the assertions below are about the assignment, the
// option value, or the notification: they are about a REAL `Editor.svelte`
// mount, its REAL toolbar DOM, and whether the buttons a user would click are
// enabled. **Reverting the reader to `ext.options.supportedFormats = …` must
// turn this file red** — which it does at three independent points: the
// pre-capabilities toolbar never leaves its degraded state, a toolbar built
// AFTER capabilities resolve is degraded too (the case the notification cannot
// paper over), and the per-format assertions below cannot be satisfied by any
// implementation that merely enables everything once the fetch lands.
//
// The COMMENT composer is the other half, and it is NOT the same case:
// `CommentEditor` configures transforms off ON PURPOSE. A "fix" that hands it
// the server's formats would be a regression, so its config is pinned here
// too — after capabilities resolve and after the change notification has been
// fanned out to every live toolbar.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import type { Editor as TiptapEditor } from '@tiptap/core';
import type { ServerCapabilities } from '$lib/types';

const PNG = '11111111-1111-4111-8111-111111111111';
const TIFF = '22222222-2222-4222-8222-222222222222';

/** What the server says its libvips build can process. TIFF is absent. */
const SERVER_FORMATS = ['png', 'jpeg'];

/**
 * The list THIS test's server reports. Mutable, and one test below sets it to
 * a build that cannot do PNG: a "fix" that ignored the response and hard-coded
 * the common list would satisfy every other assertion here (Codex round 1), so
 * the only thing that separates delivery from a constant is asking a second
 * server a different question.
 */
let serverFormats: string[] = SERVER_FORMATS;

const MIMES: Record<string, string> = {
	[PNG]: 'image/png',
	[TIFF]: 'image/tiff',
};

// The capabilities fetch, held OPEN until a test resolves it. Every editor
// below therefore mounts in the pre-capabilities world — which is the world
// the bug lived in, and the only one where "did the value arrive later" is a
// question with two possible answers.
let releaseCapabilities: (() => void) | null = null;
const capabilitiesMock = vi.fn<() => Promise<ServerCapabilities>>(
	() =>
		new Promise<ServerCapabilities>((resolve) => {
			releaseCapabilities = () =>
				resolve({ image: { image_formats: serverFormats, can_transcode: true, max_pixels: 1e8 } });
		})
);

vi.mock('$lib/api/client', () => ({
	api: {
		server: { capabilities: () => capabilitiesMock() },
		attachments: {
			downloadUrl: (ws: string, id: string, variant?: string) =>
				`/api/v1/workspaces/${ws}/attachments/${id}?variant=${variant ?? 'thumb-md'}`,
			transform: vi.fn(),
			upload: vi.fn(),
		},
		items: { list: vi.fn(async () => []) },
	},
}));

// Only the network HEAD probe is stubbed; `mimeToFormat` — the mapping the
// per-format gate is actually made of — stays REAL, so a test asserting that
// TIFF is refused is asserting against production's own format table.
vi.mock('./attachment-metadata', async (importOriginal) => {
	const actual = await importOriginal<typeof import('./attachment-metadata')>();
	return {
		...actual,
		fetchAttachmentMetadata: async (_ws: string, uuid: string) =>
			MIMES[uuid] ? { status: 'ok' as const, mime: MIMES[uuid], size: 4096 } : { status: 'transient' as const },
		revalidateAttachmentMetadata: async (_ws: string, uuid: string) =>
			MIMES[uuid] ? { status: 'ok' as const, mime: MIMES[uuid], size: 4096 } : { status: 'transient' as const },
		invalidateAttachmentMetadata: () => {},
	};
});

// The two REAL mount sites. Nothing here re-declares their extension config —
// a local `AttachmentImage.configure({ … })` would pin a copy of the thing
// under test and stay green through the bug.
const { default: BodyEditor } = await import('./Editor.svelte');
const { default: CommentEditor } = await import('$lib/components/CommentEditor.svelte');
const { AttachmentImage } = await import('./attachment-image');

const { page } = await import('$app/state');

type Mounted = Record<string, unknown>;

describe('server image capabilities reach the inline image NodeView (BUG-2426)', () => {
	let target: HTMLElement;
	const mounted: Mounted[] = [];

	beforeEach(() => {
		serverFormats = SERVER_FORMATS;
		releaseCapabilities = null;
		capabilitiesMock.mockClear();
		page.params.workspace = 'ws';
		target = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		while (mounted.length) unmount(mounted.pop() as Mounted);
		target.remove();
		document.querySelectorAll('.attachment-image-toolbar').forEach((el) => el.remove());
	});

	/** Mount the REAL body editor and hand back its Tiptap instance. */
	function mountBody(markdown: string): TiptapEditor {
		let tiptap: TiptapEditor | null = null;
		mounted.push(
			mount(BodyEditor, {
				target,
				props: {
					content: markdown,
					itemId: 'item-A',
					hostToken: 'apanel-1',
					onEditor: (e: TiptapEditor) => {
						tiptap = e;
					},
				},
			}) as Mounted
		);
		flushSync();
		if (!tiptap) throw new Error('Editor.svelte did not construct a Tiptap editor');
		return tiptap;
	}

	/**
	 * Select the image node so its NodeView builds the toolbar (`selectNode`),
	 * then return the toolbar's buttons. Selection is driven through the
	 * editor's own command rather than a synthetic mouse event: jsdom has no
	 * layout, so ProseMirror's coordinate-based click-to-select cannot run.
	 */
	function toolbarButtons(tiptap: TiptapEditor, uuid: string): HTMLButtonElement[] {
		let pos: number | null = null;
		tiptap.state.doc.descendants((node, at) => {
			if (node.type.name === 'attachmentImage' && node.attrs.uuid === uuid) pos = at;
			return true;
		});
		if (pos === null) throw new Error(`no attachmentImage node for ${uuid} in the parsed document`);
		tiptap.commands.setNodeSelection(pos);
		const wrapper = tiptap.view.dom.querySelector<HTMLElement>(
			`.attachment-image-wrapper:has(img[data-attachment-id="${uuid}"]), [data-attachment-id="${uuid}"]`
		);
		const toolbar =
			wrapper?.closest('.attachment-image-wrapper')?.querySelector('.attachment-image-toolbar') ??
			tiptap.view.dom.querySelector('.attachment-image-toolbar');
		if (!toolbar) throw new Error('selecting the image did not build a toolbar');
		return Array.from(toolbar.querySelectorAll<HTMLButtonElement>('.attachment-image-toolbar-btn'));
	}

	/** Let the capabilities promise + the MIME probe settle. */
	async function settle() {
		for (let i = 0; i < 4; i++) await new Promise((r) => setTimeout(r, 0));
		flushSync();
	}

	async function resolveCapabilities() {
		if (!releaseCapabilities) throw new Error('Editor.svelte never called api.server.capabilities()');
		releaseCapabilities();
		await settle();
	}

	it('enables a toolbar that was built BEFORE capabilities resolved', async () => {
		const tiptap = mountBody(`![A diagram](pad-attachment:${PNG})`);
		await settle();

		// Pre-capabilities: the honest degraded state. Asserted so the
		// post-resolution assertion cannot pass vacuously against buttons that
		// were enabled the whole time.
		const before = toolbarButtons(tiptap, PNG);
		expect(before.length).toBeGreaterThan(0);
		expect(before.every((b) => b.disabled)).toBe(true);
		expect(before[0].title).toMatch(/not available in this build/i);

		await resolveCapabilities();

		// Same buttons — the toolbar is not rebuilt — now live.
		const after = toolbarButtons(tiptap, PNG);
		expect(after).toEqual(before);
		expect(after.some((b) => b.disabled)).toBe(false);
		expect(after[0].title).not.toMatch(/not available in this build/i);
	});

	it('enables a toolbar built AFTER capabilities resolved', async () => {
		// The case the capability-change NOTIFICATION cannot rescue: this
		// toolbar does not exist when the fan-out runs, so its state comes
		// entirely from reading the option at build time. Under the old
		// post-configure assignment this stayed disabled forever.
		const tiptap = mountBody(`![A diagram](pad-attachment:${PNG})`);
		await settle();
		await resolveCapabilities();

		const btns = toolbarButtons(tiptap, PNG);
		await settle();
		expect(btns.length).toBeGreaterThan(0);
		expect(btns.some((b) => b.disabled)).toBe(false);
	});

	it('still refuses a format the server does not support', async () => {
		// The guard against a "fix" that just enables everything once the
		// fetch lands: TIFF is a real image with a real MIME, absent from
		// SERVER_FORMATS, and must stay disabled with the format-specific
		// message — which is only reachable if the ACTUAL list arrived.
		const tiptap = mountBody(`![A scan](pad-attachment:${TIFF})`);
		await settle();
		await resolveCapabilities();

		const btns = toolbarButtons(tiptap, TIFF);
		await settle();
		expect(btns.every((b) => b.disabled)).toBe(true);
		expect(btns[0].title).toContain('image/tiff');
		expect(btns[0].title).toContain(SERVER_FORMATS.join(', '));
	});

	it('reports the formats THIS server sent, not a hard-coded list', async () => {
		// Same PNG image, a build whose processor cannot do PNG. Every other
		// assertion in this file is satisfiable by an implementation that
		// ignores the response and hard-codes the usual list; this one is not.
		serverFormats = ['jpeg'];
		const tiptap = mountBody(`![A diagram](pad-attachment:${PNG})`);
		await settle();
		await resolveCapabilities();

		const btns = toolbarButtons(tiptap, PNG);
		await settle();
		expect(btns.every((b) => b.disabled)).toBe(true);
		expect(btns[0].title).toContain('image/png');
		expect(btns[0].title).toContain('this build supports jpeg');
	});

	it('leaves the COMMENT composer with transforms off, capabilities or not', async () => {
		// `CommentEditor` switches transforms off deliberately, and shares the
		// module-level capability fan-out with the body editor — so "the body
		// editor now receives capabilities" is exactly the change that could
		// leak into comments.
		//
		// Its Tiptap instance is not exposed to hosts, so this asserts the
		// configuration the REAL component hands the REAL extension, captured
		// through `configure` and INVOKED at the same moment the NodeView
		// would invoke it (after capabilities resolved, after the fan-out).
		// It is the same value `refreshToolbarState` gates on; the body tests
		// above are what tie that value to the buttons.
		// Capture without disturbing behaviour: wrap the real method, and hand
		// back what it really returns, so both editors mount for real.
		const realConfigure = AttachmentImage.configure.bind(AttachmentImage);
		const captured: Array<{ supportedFormats: () => string[] }> = [];
		vi.spyOn(AttachmentImage, 'configure').mockImplementation((opts) => {
			captured.push(opts as unknown as { supportedFormats: () => string[] });
			return realConfigure(opts);
		});

		try {
			const tiptap = mountBody(`![A diagram](pad-attachment:${PNG})`);
			mounted.push(
				mount(CommentEditor, {
					target: document.body.appendChild(document.createElement('div')),
					props: { wsSlug: 'ws', itemId: 'item-A', hostToken: 'apanel-1', onSubmit: () => {} },
				}) as Mounted
			);
			flushSync();
			await settle();
			await resolveCapabilities();

			// Two mount sites, two readers, one shared capability fetch.
			expect(captured.length).toBe(2);
			const [bodyOpts, commentOpts] = captured;
			expect(bodyOpts.supportedFormats()).toEqual(SERVER_FORMATS);
			expect(commentOpts.supportedFormats()).toEqual([]);

			// And the body editor's own toolbar agrees, so the reader that
			// stayed empty is the comment one, not a mis-captured pair.
			expect(toolbarButtons(tiptap, PNG).some((b) => b.disabled)).toBe(false);
		} finally {
			vi.restoreAllMocks();
		}
	});
});
