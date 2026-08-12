// The inline image's missing-attachment placeholder (PLAN-2392 DR-12 / DR-17).
//
// Two causes land on the same element and must NOT look the same to a keyboard
// or screen-reader user:
//
//   - a transient load failure — retryable, so the placeholder is a button;
//   - a CONFIRMED deletion — `retryLoad` refuses, so a button that announces
//     itself and does nothing is a dead focus stop. That is the same failure
//     the file chip's `disabled` closes, on the surface right next to it.
//
// Driven through a REAL Tiptap editor, like the chip spec: the placeholder is
// imperative NodeView DOM and its accessibility semantics are properties of
// that DOM, which a hand-built element would not pin.
//
// The surface assertions here are PRODUCER-level — `notifyAttachmentSurfaceOpen`
// is mocked, so "opens" means "asks". A broken or absent host would be invisible
// to them; that half of the route is `attachmentImageViewerHost.svelte.test.ts`'s.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';

const deletionListeners = new Set<(uuid: string) => void>();
// Every open-the-viewer request this NodeView makes, captured RAW — before the
// channel's own addressability filter. The gate under test is the NodeView's;
// routing is `events.ts`'s and has its own spec.
const emitted: Array<Record<string, unknown>> = [];
// The restore channel FANS OUT like the deletion one above: a mock that only
// records the subscription leaves the NodeView subscribed to nothing, and every
// test of how it REACTS passes vacuously (BUG-2509).
const restoreListeners = new Set<(event: { workspaceSlug: string; itemId: string }) => void>();

vi.mock('$lib/attachments/events', () => ({
	notifyAttachmentSurfaceOpen: (event: Record<string, unknown>) => {
		emitted.push(event);
	},
	registerAttachmentDeletionListener: (fn: (uuid: string) => void) => {
		deletionListeners.add(fn);
		return () => deletionListeners.delete(fn);
	},
	registerAttachmentParentRestoredListener: (
		fn: (event: { workspaceSlug: string; itemId: string }) => void
	) => {
		restoreListeners.add(fn);
		return () => restoreListeners.delete(fn);
	},
}));

// The probe's full result union, spelled out. Without it `vi.fn` infers the
// type of the DEFAULT implementation alone, and every `mockResolvedValue` for a
// different arm is a type error — invisible under `npm run check`, which
// excludes `*.svelte.test.ts`, and a trap for whoever widens that exclude.
type ProbeResult =
	| { status: 'ok'; mime: string; size: number }
	| { status: 'missing' }
	| { status: 'transient' };

// No probe by default: these tests are about what the placeholder IS, not how
// it is discovered, and a real HEAD would make them asynchronous for no gain.
const probeMock = vi.fn<() => Promise<ProbeResult>>(async () => ({ status: 'transient' }));
// Options are threaded through rather than discarded: `cache: 'no-store'` is the
// entire point of the restore re-probe (escaping what the archived window cached),
// so it needs to be assertable (BUG-2509).
const revalidateOptions: Array<Record<string, unknown> | undefined> = [];
vi.mock('./attachment-metadata', () => ({
	fetchAttachmentMetadata: () => probeMock(),
	revalidateAttachmentMetadata: (
		_ws: string,
		_uuid: string,
		_url: unknown,
		options?: Record<string, unknown>
	) => {
		revalidateOptions.push(options);
		return probeMock();
	},
	invalidateAttachmentMetadata: () => {},
	mimeToFormat: () => null,
}));

const { AttachmentImage } = await import('./attachment-image');

const DEFAULT_ADDRESS = () => ({ workspaceSlug: 'ws', itemId: 'item-A', hostToken: 'apanel-1' });

function makeEditor(element: HTMLElement, address = DEFAULT_ADDRESS): Editor {
	return new Editor({
		element,
		extensions: [
			StarterKit,
			AttachmentImage.configure({
				workspaceSlug: '',
				getDownloadUrl: (uuid: string) => `/api/v1/workspaces/ws/attachments/${uuid}`,
				// A workspace IS supplied: the MIME probe is what the viewer gate
				// reads, and with an empty one it never runs (which is itself the
				// documented "unknown ⇒ keep today's behaviour" path).
				address,
				supportedFormats: () => [],
				transform: async () => {
					throw new Error('not used');
				},
			}),
		],
		content: '<p><img data-attachment-id="uuid-1" src="/api/v1/x" alt="A diagram"></p>',
		editable: true,
	});
}

describe('inline image missing placeholder', () => {
	let target: HTMLElement;
	let editor: Editor | undefined;

	beforeEach(() => {
		deletionListeners.clear();
		restoreListeners.clear();
		revalidateOptions.length = 0;
		emitted.length = 0;
		probeMock.mockClear();
		target = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		editor?.destroy();
		editor = undefined;
		target.remove();
	});

	function placeholder(): HTMLElement {
		editor ??= makeEditor(target);
		const el = target.querySelector<HTMLElement>('.attachment-missing');
		if (!el) throw new Error('placeholder did not render');
		return el;
	}

	/**
	 * Let the lazy HEAD probe AND the activation's own MIME resolution settle.
	 * Activation is asynchronous as of TASK-2433 — it resolves the MIME before
	 * emitting — so a synchronous assertion after a click would read `[]` no
	 * matter what the implementation does.
	 */
	async function settle() {
		await new Promise((resolve) => setTimeout(resolve, 0));
	}

	/**
	 * Repoint the existing image node at a different attachment via a transaction,
	 * which drives the NodeView's `update()` hook — the path a rotate/crop or a
	 * collaborative peer's edit takes. Replacing the content instead would destroy
	 * and rebuild the NodeView and prove nothing about `update()`.
	 */
	function repointImage(uuid: string) {
		const ed = (editor ??= makeEditor(target));
		let pos = -1;
		ed.state.doc.descendants((node, at) => {
			if (node.type.name === 'attachmentImage') {
				pos = at;
				return false;
			}
			return true;
		});
		if (pos < 0) throw new Error('no attachmentImage node in the document');
		ed.view.dispatch(ed.state.tr.setNodeMarkup(pos, undefined, { ...ed.state.doc.nodeAt(pos)?.attrs, uuid }));
	}

	function failLoad() {
		const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!img) throw new Error('image NodeView did not render');
		img.dispatchEvent(new Event('error'));
	}

	it('emits a surface event for a probed non-raster type — the surface, not the producer, picks the fallback arm (T4a)', async () => {
		// As of TASK-2489 the raster/non-raster fork is GONE from the producer.
		// The DR-16 guarantee — an `image/svg+xml` (active content) is never
		// handed to a raster viewer — did not disappear; it moved DOWNSTREAM to
		// the surface renderer, which routes svg onto its no-bytes fallback arm.
		// So the producer emits ONE surface event carrying the TRUE svg MIME, and
		// admission no longer branches on the type here. The resolve-before-emit
		// gate (missing → latch, transient → retry) is unchanged.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml' , size: 4096 });
		editor = makeEditor(target);
		const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!img) throw new Error('image NodeView did not render');

		// Select the node so the lazy MIME probe runs, then let it settle.
		editor.commands.setNodeSelection(1);
		await settle();

		img.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		await settle();
		// Exactly ONE surface emit, stamped with the real svg MIME — the surface
		// renderer, not the producer, decides it renders no bytes.
		expect(emitted).toEqual([
			{
				attachmentId: 'uuid-1',
				workspaceSlug: 'ws',
				itemId: 'item-A',
				hostToken: 'apanel-1',
				images: [
					{
						id: 'uuid-1',
						alt: 'A diagram',
						filename: null,
						mime_type: 'image/svg+xml',
						size_bytes: 4096,
						width: null,
						height: null,
					},
				],
				index: 0,
				invoker: img,
				filename: null,
				mime_type: 'image/svg+xml',
				size_bytes: 4096,
			},
		]);
	});

	it('emits a fully-stamped surface request for an allowlisted raster type', async () => {
		// The gate must not cost the common case: a PNG opens as it always did —
		// and this is the assertion the deletion of the old `<dialog>` rests on.
		// "The dialog is gone" is satisfied by an implementation that opens
		// NOTHING, so the claim has to be about what is EMITTED.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 4096 });
		editor = makeEditor(target);
		const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!img) throw new Error('image NodeView did not render');

		editor.commands.setNodeSelection(1);
		await settle();

		img.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		await settle();

		// The WHOLE payload, field by field. Each one is load-bearing and each
		// has a plausible wrong value: the address routes the event to one of
		// several mounted hosts (DR-8), `workspaceSlug` is what every image URL
		// is read from, `mime_type` is what lets `Lightbox` re-state the DR-16
		// gate over the set (it FAILS CLOSED on null as of TASK-2431, so an
		// event with an unresolved MIME mounts a viewer that renders no image),
		// and `invoker` is where the viewer aims focus on close.
		expect(emitted).toEqual([
			{
				attachmentId: 'uuid-1',
				workspaceSlug: 'ws',
				itemId: 'item-A',
				hostToken: 'apanel-1',
				images: [
					{
						id: 'uuid-1',
						alt: 'A diagram',
						filename: null,
						mime_type: 'image/png',
						size_bytes: 4096,
						width: null,
						height: null,
					},
				],
				index: 0,
				invoker: img,
				filename: null,
				mime_type: 'image/png',
				size_bytes: 4096,
			},
		]);
	});

	it('emits nothing at all when the MIME cannot be resolved', async () => {
		// The revised rule for this task (TASK-2431's adversarial round): a
		// `transient` probe is NOT "unknown, so keep today's behaviour" any more.
		// `Lightbox` fails closed on an unresolved MIME, so emitting one would be
		// a request that opens nothing while looking, on the bus, exactly like a
		// request that does. The retryable branch is TASK-2434's.
		probeMock.mockResolvedValue({ status: 'transient' });
		editor = makeEditor(target);
		const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!img) throw new Error('image NodeView did not render');

		img.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		await settle();

		expect(emitted).toEqual([]);
	});

	it('inertizes the transform toolbar when the attachment is deleted', async () => {
		// A confirmed deletion inertizes the WHOLE node. Rotate and crop against
		// a row that is gone can only 404, and leaving them live is the same
		// dead-control gap the placeholder's role/tabindex removal closes.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png' , size: 4096 });
		editor = makeEditor(target);
		editor.commands.setNodeSelection(1);
		await Promise.resolve();
		await Promise.resolve();

		const buttons = () =>
			Array.from(
				target.querySelectorAll<HTMLButtonElement>('.attachment-image-toolbar-btn')
			);
		expect(buttons().length).toBeGreaterThan(0);

		for (const fn of deletionListeners) fn('uuid-1');

		expect(buttons().every((b) => b.disabled)).toBe(true);
		expect(buttons()[0].title).toBe('This attachment has been deleted');
	});

	it('is a focusable button while the failure is merely transient', () => {
		editor = makeEditor(target);
		failLoad();

		const el = placeholder();
		expect(el.style.display).not.toBe('none');
		// Retryable, so it invites the retry and can be reached to perform it.
		expect(el.getAttribute('role')).toBe('button');
		expect(el.getAttribute('tabindex')).toBe('0');
		expect(el.title).toContain('retry');
	});

	it('drops its interactive semantics once the deletion is confirmed', async () => {
		editor = makeEditor(target);
		failLoad();
		expect(placeholder().getAttribute('role')).toBe('button');

		// Another surface deleted the row: authoritative, and `retryLoad`
		// refuses from here on.
		for (const fn of deletionListeners) fn('uuid-1');

		const el = placeholder();
		expect(el.getAttribute('role')).toBeNull();
		expect(el.getAttribute('tabindex')).toBeNull();
		// The copy stops inviting a retry that cannot happen, too.
		expect(el.title).toBe('This attachment has been deleted');
		expect(el.title).not.toContain('retry');
	});

	/**
	 * BUG-2509. Archiving an item 404s its attachments WITHOUT deleting them, so a
	 * NodeView that probes inside that window observes exactly what a deletion
	 * produces and latches it permanently — and nothing cleared that latch, because
	 * only a uuid swap did and a restore does not change the uuid. These pin the
	 * restore channel's contract: the signal PROMPTS a re-ask, the SERVER decides.
	 */
	describe('parent-item restore (BUG-2509)', () => {
		/** Latch the permanent placeholder the way an archived-window probe does. */
		async function latchViaArchivedWindowProbe() {
			probeMock.mockResolvedValue({ status: 'missing' });
			editor = makeEditor(target);
			failLoad();
			await settle();
			expect(placeholder().title).toBe('This attachment has been deleted');
			expect(target.querySelector<HTMLImageElement>('img')?.style.display).toBe('none');
		}

		function announceRestore(itemId = 'item-A', workspaceSlug = 'ws') {
			for (const fn of restoreListeners) fn({ workspaceSlug, itemId });
		}

		it('heals a node latched during the archived window once the server says ok', async () => {
			await latchViaArchivedWindowProbe();

			probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 10 });
			announceRestore();
			await settle();

			const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
			// The load is re-issued cache-busted, for the reason Retry is: the failed
			// request is in the HTTP cache and re-assigning the same URL can replay it.
			expect(img?.getAttribute('src')).toContain('restored=');
			// `load` is what actually repaints; the NodeView must have re-armed for it.
			img?.dispatchEvent(new Event('load'));
			expect(img?.style.display).not.toBe('none');
			expect(placeholder().style.display).toBe('none');
		});

		it('leaves a genuinely deleted attachment dead — restore is not an undo (DR-17)', async () => {
			await latchViaArchivedWindowProbe();

			// The row really is gone: deleted while the parent was archived.
			probeMock.mockResolvedValue({ status: 'missing' });
			announceRestore();
			await settle();

			expect(placeholder().style.display).not.toBe('none');
			expect(placeholder().title).toBe('This attachment has been deleted');
			expect(target.querySelector<HTMLImageElement>('img')?.style.display).toBe('none');
		});

		it('leaves the placeholder alone when the re-probe is transient — no evidence either way', async () => {
			await latchViaArchivedWindowProbe();

			probeMock.mockResolvedValue({ status: 'transient' });
			announceRestore();
			await settle();

			expect(placeholder().style.display).not.toBe('none');
			expect(target.querySelector<HTMLImageElement>('img')?.style.display).toBe('none');
		});

		it('ignores a signal for another item — routing is by address, not broadcast', async () => {
			await latchViaArchivedWindowProbe();
			probeMock.mockClear();

			announceRestore('item-B');
			await settle();

			// Not even a wasted HEAD: the address filter runs before the probe.
			expect(probeMock).not.toHaveBeenCalled();
			expect(placeholder().style.display).not.toBe('none');
		});

		it('ignores a signal for another workspace', async () => {
			await latchViaArchivedWindowProbe();
			probeMock.mockClear();

			announceRestore('item-A', 'other-ws');
			await settle();

			expect(probeMock).not.toHaveBeenCalled();
			expect(placeholder().style.display).not.toBe('none');
		});

		it('does not probe at all when nothing is being presented as missing', async () => {
			probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 10 });
			editor = makeEditor(target);
			await settle();
			probeMock.mockClear();

			announceRestore();
			await settle();

			expect(probeMock).not.toHaveBeenCalled();
		});

		/**
		 * The ordering the first version of this fix got wrong: the restore probe
		 * fenced on teardown and uuid only, so a deletion confirmed WHILE it was in
		 * flight lost to the stale positive answer — a DR-17 resurrection reachable
		 * in one interleaving.
		 */
		it('lets a deletion confirmed mid-probe WIN over a stale ok (DR-17)', async () => {
			await latchViaArchivedWindowProbe();

			let release: (r: ProbeResult) => void = () => {};
			probeMock.mockImplementation(
				() => new Promise<ProbeResult>((resolve) => (release = resolve))
			);
			announceRestore();
			await settle();

			// The row is deleted for real while the HEAD is still out.
			for (const fn of deletionListeners) fn('uuid-1');
			// …and only then does the probe answer, positively and out of date.
			release({ status: 'ok', mime: 'image/png', size: 10 });
			await settle();

			// The END STATE alone proves nothing here: the placeholder stays visible
			// until a `load` fires, so a broken fence looks identical at this instant.
			// The observable consequence of a (wrong) heal is the cache-busted reload
			// it arms — assert THAT.
			expect(target.querySelector<HTMLImageElement>('img')?.getAttribute('src')).not.toContain(
				'restored='
			);
			expect(placeholder().style.display).not.toBe('none');
			expect(placeholder().title).toBe('This attachment has been deleted');
			expect(target.querySelector<HTMLImageElement>('img')?.style.display).toBe('none');
		});

		/**
		 * Two restore signals before either HEAD answers. The NEWEST wins: receipt of
		 * a restore is itself an authoritative transition, so the earlier probe's
		 * answer is superseded rather than racing it. What must never happen is the
		 * authoritative `missing` being dropped for having lost a race.
		 */
		it('lets the newest probe decide when two are in flight, and honours its missing', async () => {
			await latchViaArchivedWindowProbe();

			const releases: Array<(r: ProbeResult) => void> = [];
			probeMock.mockImplementation(
				() => new Promise<ProbeResult>((resolve) => releases.push(resolve))
			);
			announceRestore();
			announceRestore();
			await settle();
			expect(releases).toHaveLength(2);

			// The superseded probe answers first, positively — and is ignored.
			releases[0]({ status: 'ok', mime: 'image/png', size: 10 });
			await settle();
			expect(target.querySelector<HTMLImageElement>('img')?.getAttribute('src')).not.toContain(
				'restored='
			);

			// The current probe's authoritative `missing` decides, and LATCHES rather
			// than being treated as "nothing to do".
			releases[1]({ status: 'missing' });
			await settle();
			expect(placeholder().style.display).not.toBe('none');
			expect(placeholder().title).toBe('This attachment has been deleted');
		});

		it('ignores an answer that lands after the workspace changed underneath it', async () => {
			probeMock.mockResolvedValue({ status: 'missing' });
			let ws = 'ws';
			editor = makeEditor(target, () => ({ workspaceSlug: ws, itemId: 'item-A', hostToken: 'apanel-1' }));
			failLoad();
			await settle();
			expect(placeholder().title).toBe('This attachment has been deleted');

			let release: (r: ProbeResult) => void = () => {};
			probeMock.mockImplementation(
				() => new Promise<ProbeResult>((resolve) => (release = resolve))
			);
			announceRestore('item-A', 'ws');
			await settle();

			// The pane moves to another workspace while the HEAD is in flight; the
			// answer describes the PREVIOUS workspace's copy.
			ws = 'ws-b';
			release({ status: 'ok', mime: 'image/png', size: 10 });
			await settle();

			// Again: the reload is what a heal would arm, and the placeholder's
			// visibility alone would pass with the fence removed.
			expect(target.querySelector<HTMLImageElement>('img')?.getAttribute('src')).not.toContain(
				'restored='
			);
			expect(placeholder().style.display).not.toBe('none');
			expect(target.querySelector<HTMLImageElement>('img')?.style.display).toBe('none');
		});

		it('re-probes with no-store — escaping the archived window\'s cached answer is the point', async () => {
			await latchViaArchivedWindowProbe();
			revalidateOptions.length = 0;
			probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 10 });

			announceRestore();
			await settle();

			expect(revalidateOptions).toContainEqual({ cache: 'no-store' });
		});

		/**
		 * The reverse of the mid-probe deletion race, and the one the deletion-only
		 * fence missed: an OLDER probe answering after a NEWER transition. The
		 * archived window's own probe is still out when the restore heals the node;
		 * its `missing` must not re-kill what the restore just fixed.
		 */
		it('ignores an archived-window probe that answers missing AFTER the restore healed', async () => {
			let releaseOld: (r: ProbeResult) => void = () => {};
			probeMock.mockImplementation(
				() => new Promise<ProbeResult>((resolve) => (releaseOld = resolve))
			);
			editor = makeEditor(target);
			failLoad(); // starts the archived-window probe, which does not answer yet
			await settle();
			const oldProbeRelease = releaseOld;

			// Restore arrives and heals via its own, newer probe.
			probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 10 });
			announceRestore();
			await settle();
			target.querySelector<HTMLImageElement>('img')?.dispatchEvent(new Event('load'));
			expect(placeholder().style.display).toBe('none');

			// Only now does the stale archived-window probe answer.
			oldProbeRelease({ status: 'missing' });
			await settle();

			expect(placeholder().style.display).toBe('none');
			expect(target.querySelector<HTMLImageElement>('img')?.style.display).not.toBe('none');
		});

		/**
		 * Codex round 2: fencing on the uuid by VALUE is not enough, because a swap
		 * AWAY AND BACK makes the comparison pass again. The attachment can have been
		 * deleted while the node pointed elsewhere (the deletion listener filters on
		 * the CURRENT uuid, so it ignored the event), and the stale positive answer
		 * would then revive it — a DR-17 resurrection.
		 */
		it('drops a restore answer that spans a uuid swap away and back', async () => {
			await latchViaArchivedWindowProbe();

			let release: (r: ProbeResult) => void = () => {};
			probeMock.mockImplementation(
				() => new Promise<ProbeResult>((resolve) => (release = resolve))
			);
			announceRestore();
			await settle();

			// The node moves to another attachment and back while the HEAD is out.
			probeMock.mockResolvedValue({ status: 'transient' });
			repointImage('uuid-2');
			repointImage('uuid-1');
			await settle();

			// The answer predates both swaps; the generation, not the uuid, is what
			// still knows that.
			release({ status: 'ok', mime: 'image/png', size: 10 });
			await settle();

			expect(target.querySelector<HTMLImageElement>('img')?.getAttribute('src')).not.toContain(
				'restored='
			);
		});

		it('unsubscribes on destroy — asserted on the registry, not on the end state', async () => {
			await latchViaArchivedWindowProbe();
			expect(restoreListeners.size).toBe(1);

			editor?.destroy();
			editor = undefined;
			probeMock.mockClear();

			// The registry is the only honest assertion here: a LEAKED listener
			// would still see `destroyed` and return, so "no probe fired" stays
			// green with the dispose call deleted (Codex, review of this commit).
			expect(restoreListeners.size).toBe(0);

			announceRestore();
			await settle();
			expect(probeMock).not.toHaveBeenCalled();
		});
	});

	it('does not leave focus stranded on a placeholder that just went inert', () => {
		editor = makeEditor(target);
		failLoad();
		const el = placeholder();
		el.focus();
		expect(document.activeElement).toBe(el);

		for (const fn of deletionListeners) fn('uuid-1');

		// Removing tabindex from the focused element would otherwise leave
		// focus on something unreachable by any further keystroke.
		expect(document.activeElement).not.toBe(el);
	});
});
