// The inline body image's keyboard contract (PLAN-2392 DR-12 / TASK-2432).
//
// An inline `![alt](pad-attachment:UUID)` in an item body or a comment renders
// through the AttachmentImage NodeView, and until now that `<img>` opened the
// viewer on click and was unreachable by any other means: no role, no tabindex,
// no name. This spec pins the button contract that closes that, and — more
// importantly — pins the two ways the contract is easy to get WRONG:
//
//   - activation firing TWICE. A keyed remount or a fast dialog can hide a
//     duplicate visually, so every activation assertion here is a COUNT
//     (`dialog.attachment-image-lightbox` elements in the document), never a
//     truthiness check. `toBe(1)` fails on 2; `not.toBeNull()` does not.
//
//   - the KEYBOARD path bypassing the MIME gate. The gate used to live inside
//     the click handler, which made it a property of the mouse. Both routes now
//     call one `activate()`, and the refusal is asserted through the KEYBOARD,
//     which is the assertion that would have caught a second emitter.
//
// Driven through a REAL Tiptap editor, like the placeholder spec next door: the
// semantics live on imperative NodeView DOM and a hand-built element would pin
// nothing about the code under test.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import { isEditorOwnedImage } from '$lib/attachments/editorOwnedImage';

const deletionListeners = new Set<(uuid: string) => void>();
vi.mock('$lib/attachments/events', () => ({
	notifyAttachmentPanelOpen: () => {},
	registerAttachmentDeletionListener: (fn: (uuid: string) => void) => {
		deletionListeners.add(fn);
		return () => deletionListeners.delete(fn);
	},
}));

const probeMock = vi.fn(async () => ({ status: 'transient' as const }));
vi.mock('./attachment-metadata', () => ({
	fetchAttachmentMetadata: () => probeMock(),
	revalidateAttachmentMetadata: () => probeMock(),
	invalidateAttachmentMetadata: () => {},
	mimeToFormat: () => null,
}));

const { AttachmentImage } = await import('./attachment-image');

const BODY_CONTENT = '<p><img data-attachment-id="uuid-1" src="/api/v1/x" alt="A diagram"></p>';

/** The item-body configuration (Editor.svelte): transforms enabled. */
function makeEditor(element: HTMLElement, content: string = BODY_CONTENT): Editor {
	return new Editor({
		element,
		extensions: [
			StarterKit,
			AttachmentImage.configure({
				workspaceSlug: 'ws',
				getDownloadUrl: (uuid: string, variant?: string) =>
					`/api/v1/workspaces/ws/attachments/${uuid}?variant=${variant ?? 'thumb-md'}`,
				address: () => ({ workspaceSlug: 'ws', itemId: 'item-A', hostToken: 'apanel-1' }),
				supportedFormats: ['png'],
				transform: async () => {
					throw new Error('not used');
				},
			}),
		],
		content,
		editable: true,
	});
}

/**
 * The COMMENT configuration, copied from CommentEditor.svelte: it configures
 * AttachmentImage independently, with transforms deliberately switched off.
 * Asserted separately because "the other surface configures it too" is exactly
 * the kind of thing that gets the behaviour only on the surface you tested.
 */
function makeCommentEditor(element: HTMLElement): Editor {
	return new Editor({
		element,
		extensions: [
			StarterKit,
			AttachmentImage.configure({
				getDownloadUrl: (uuid: string) => `/api/v1/workspaces/ws/attachments/${uuid}`,
				workspaceSlug: 'ws',
				address: () => ({ workspaceSlug: 'ws', itemId: 'item-A', hostToken: 'apanel-1' }),
				supportedFormats: [] as string[],
				transform: async () => {
					throw new Error('Image transforms are not available in comments.');
				},
			}),
		],
		content: BODY_CONTENT,
		editable: true,
	});
}

/** How many times activation actually fired. One dialog per open. */
function openCount(): number {
	return document.querySelectorAll('dialog.attachment-image-lightbox').length;
}

/**
 * A stand-in for ItemTimeline's delegated keydown handler, built from the same
 * pieces as the real one: the same `img[data-attachment-id]` selector, the same
 * activation keys, the same modifier guard, and — critically — the same
 * `isEditorOwnedImage` ownership check that keeps it off a live editor's DOM.
 *
 * It OPENS rather than just counting. A counter alone proves the event escaped
 * but leaves openCount() at 1, so the failure that actually matters — two
 * viewers on screen from one keypress — would not show up in the assertion.
 */
function timelineDelegation(
	onFire: () => void,
	opts: { ownership: boolean } = { ownership: true }
): (e: Event) => void {
	return (e: Event) => {
		const ev = e as KeyboardEvent;
		if (ev.key !== 'Enter' && ev.key !== ' ') return;
		if (ev.ctrlKey || ev.metaKey || ev.altKey || ev.shiftKey) return;
		const img = (e.target as HTMLElement | null)?.closest<HTMLElement>(
			'img[data-attachment-id]'
		);
		if (!img) return;
		// `ownership: false` is the PRE-TASK-2432 shape, kept deliberately: the
		// node's own stopPropagation has to hold up against a delegated opener
		// that does NOT know to skip editor-owned DOM, because ItemTimeline is
		// not the only surface that could ever delegate over one.
		if (opts.ownership && isEditorOwnedImage(img)) return;
		onFire();
		const dialog = document.createElement('dialog');
		dialog.className = 'attachment-image-lightbox';
		document.body.appendChild(dialog);
	};
}

describe('inline body image — keyboard activation (DR-12)', () => {
	let host: HTMLElement;
	let target: HTMLElement;
	let editor: Editor | undefined;

	beforeEach(() => {
		deletionListeners.clear();
		probeMock.mockClear();
		probeMock.mockResolvedValue({ status: 'transient' as const });
		host = document.body.appendChild(document.createElement('div'));
		target = host.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		editor?.destroy();
		editor = undefined;
		host.remove();
		document.querySelectorAll('dialog.attachment-image-lightbox').forEach((d) => d.remove());
	});

	function image(): HTMLImageElement {
		const el = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!el) throw new Error('image NodeView did not render');
		return el;
	}

	function press(el: HTMLElement, key: string, mods: KeyboardEventInit = {}): KeyboardEvent {
		const ev = new KeyboardEvent('keydown', {
			key,
			bubbles: true,
			cancelable: true,
			...mods,
		});
		el.dispatchEvent(ev);
		return ev;
	}

	function click(el: HTMLElement): MouseEvent {
		const ev = new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 });
		el.dispatchEvent(ev);
		return ev;
	}

	/** Let the lazy HEAD probe (armed by selecting the node) settle. */
	async function settleProbe() {
		editor?.commands.setNodeSelection(1);
		await Promise.resolve();
		await Promise.resolve();
		await Promise.resolve();
	}

	it('is a named, focusable button', () => {
		editor = makeEditor(target);
		const img = image();

		expect(img.getAttribute('role')).toBe('button');
		expect(img.getAttribute('tabindex')).toBe('0');
		// Name from alt. There is no filename on the node's attrs and the HEAD
		// metadata carries none either, so the filename form DR-12 sketches has
		// no source here — alt, then a generic fallback.
		expect(img.getAttribute('aria-label')).toBe('View image: A diagram');
	});

	it('falls back to a generic name when the image has no alt', () => {
		editor = makeEditor(target, '<p><img data-attachment-id="uuid-1" src="/api/v1/x"></p>');
		expect(image().getAttribute('aria-label')).toBe('View attachment image');
	});

	it('opens on Enter, exactly once', () => {
		editor = makeEditor(target);
		expect(openCount()).toBe(0);

		press(image(), 'Enter');

		// A COUNT, not a truthiness check: a second emitter (a keyboard path that
		// activated on its own AND a synthesized click) shows up here as 2 and is
		// invisible to `not.toBeNull()`.
		expect(openCount()).toBe(1);
	});

	it('opens on Space exactly once and suppresses the page scroll', () => {
		editor = makeEditor(target);
		expect(openCount()).toBe(0);

		const ev = press(image(), ' ');

		expect(openCount()).toBe(1);
		// Unhandled Space scrolls the document — and inside a contenteditable it
		// would also be taken as text input against the selected atom.
		expect(ev.defaultPrevented).toBe(true);
	});

	it('accepts the legacy "Spacebar" key name too', () => {
		// Same alias the file chip next door accepts — older engines report Space
		// under this name, and an image that ignored it would be inert there.
		editor = makeEditor(target);
		expect(openCount()).toBe(0);

		const ev = press(image(), 'Spacebar');

		expect(openCount()).toBe(1);
		expect(ev.defaultPrevented).toBe(true);
	});

	it('opens on a mouse click, exactly once', () => {
		editor = makeEditor(target);
		expect(openCount()).toBe(0);
		click(image());
		expect(openCount()).toBe(1);
	});

	// Both activation keys, because the propagation stop is per-key: a handler
	// that stopped for Enter and not Space would pass an Enter-only test and
	// still double-open on the key most people press.
	for (const key of ['Enter', ' ']) {
		it(`does not double-open when ${key === ' ' ? 'Space' : key} lands inside a delegated container`, () => {
			// ItemTimeline delegates thumbnail click/keydown across its whole entry
			// list, and that list CONTAINS live CommentEditor instances whose bodies
			// render this NodeView — an `img[data-attachment-id]`, which is exactly
			// the selector the delegation matches. Without stopPropagation, one
			// keypress activates the node AND the timeline's own opener: two viewers.
			//
			// The stand-in ancestor OPENS, rather than just counting: a counter
			// alone proves the event escaped but leaves the count at 1, so the
			// failure it is actually about — two viewers on screen — would not be
			// visible in the assertion that matters.
			let delegated = 0;
			host.addEventListener(
				'keydown',
				timelineDelegation(() => (delegated += 1), { ownership: false })
			);
			editor = makeCommentEditor(target);
			expect(openCount()).toBe(0);

			press(image(), key);

			expect(delegated).toBe(0);
			expect(openCount()).toBe(1);
		});
	}

	// A modified key is a shortcut, not an activation. Cmd/Ctrl+Enter is
	// CommentEditor's SUBMIT binding and an image is a perfectly ordinary thing
	// to have focused when you post — an activation handler that matched on
	// `key` alone would swallow it and open a viewer instead of posting.
	for (const [label, mods] of [
		['Ctrl+Enter', { ctrlKey: true }],
		['Meta+Enter', { metaKey: true }],
		['Shift+Enter', { shiftKey: true }],
		['Alt+Space', { altKey: true }],
	] as const) {
		it(`treats ${label} as a shortcut, not an activation`, () => {
			let seen = 0;
			let timelineOpened = 0;
			host.addEventListener('keydown', () => (seen += 1));
			// The REAL ItemTimeline delegation, ownership check included. A
			// modified key is deliberately let through so CommentEditor's submit
			// binding can see it — which means it also reaches this handler, and
			// that handler must not turn a submit into a viewer.
			host.addEventListener(
				'keydown',
				timelineDelegation(() => (timelineOpened += 1))
			);
			editor = makeCommentEditor(target);
			const key = label.endsWith('Space') ? ' ' : 'Enter';

			// Isolated first — NOT bubbling, so nothing but this node's own
			// listener can touch the event. ProseMirror's keymap legitimately
			// consumes some of these combinations once they reach the editor
			// root, and asserting on the bubbled event would be measuring that
			// instead of the handler under test.
			const solo = new KeyboardEvent('keydown', { key, cancelable: true, ...mods });
			image().dispatchEvent(solo);
			expect(openCount()).toBe(0);
			expect(solo.defaultPrevented).toBe(false);

			// Then bubbling, for the other half: the event must still REACH the
			// surface that owns the shortcut. A handler that declined to
			// activate but kept swallowing the event would break the binding
			// just as thoroughly as one that opened a viewer.
			press(image(), key, mods);
			expect(seen).toBe(1);
			expect(openCount()).toBe(0);
			expect(timelineOpened).toBe(0);
		});
	}

	it('opens once for a HELD key, not once per repeat', () => {
		// Every repeat is another keydown. Without a guard, leaning on Enter
		// stacks a viewer per repeat — "exactly once" has to mean once per
		// gesture. A truthiness check would not see this at all.
		let delegated = 0;
		host.addEventListener(
			'keydown',
			timelineDelegation(() => (delegated += 1), { ownership: false })
		);
		editor = makeEditor(target);

		const img = image();
		press(img, 'Enter');
		for (let i = 0; i < 4; i++) press(img, 'Enter', { repeat: true });

		expect(openCount()).toBe(1);
		// And the repeats stay suppressed: they belong to the activation the
		// user made once, so letting them escape would hand the surrounding
		// surface four keypresses that never happened.
		expect(delegated).toBe(0);
	});

	it('still reaches the delegated container for keys it does not handle', () => {
		// The propagation stop is scoped to the activation keys; swallowing
		// everything would break Escape, Tab-adjacent handlers and the editor's
		// own key handling on the surrounding surface.
		let seen = 0;
		host.addEventListener('keydown', () => (seen += 1));
		editor = makeEditor(target);

		const ev = press(image(), 'Escape');

		expect(seen).toBe(1);
		// Nor is it swallowed: an Escape the node consumed would never reach the
		// surface that closes a pane or a dialog.
		expect(ev.defaultPrevented).toBe(false);
		expect(openCount()).toBe(0);
	});

	it('refuses a probed non-raster type through the KEYBOARD, not just the mouse', async () => {
		// The gate used to live inside the click handler. A keyboard path that
		// emitted on its own would have sailed straight past it, so the refusal
		// is asserted on the route that would have bypassed it.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml' });
		editor = makeEditor(target);

		// BEFORE the probe answers, this is an ordinary activatable button (the
		// documented "unknown ⇒ keep today's behaviour" path). Pinning that here
		// is what makes the refusal below attributable to the RESOLVED MIME
		// rather than to any other inertness route — an empty uuid, a latched
		// deletion, a hidden image — all of which would already be true now.
		expect(image().getAttribute('role')).toBe('button');

		await settleProbe();
		expect(probeMock).toHaveBeenCalled();

		press(image(), 'Enter');
		expect(openCount()).toBe(0);

		// And it stops being a focus stop at all, rather than announcing itself
		// as a button that does nothing.
		expect(image().getAttribute('role')).toBeNull();
		expect(image().getAttribute('tabindex')).toBeNull();
		expect(image().getAttribute('aria-label')).toBeNull();
	});

	it('still opens an allowlisted raster type after the probe resolves', async () => {
		// The control for the refusal above: same code path, same probe, same
		// number of awaits — only the MIME differs.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png' });
		editor = makeEditor(target);
		await settleProbe();
		expect(probeMock).toHaveBeenCalled();

		expect(image().getAttribute('role')).toBe('button');
		expect(openCount()).toBe(0);
		press(image(), 'Enter');
		expect(openCount()).toBe(1);
	});

	it('makes a deleted image inert rather than a dead focus stop', () => {
		editor = makeEditor(target);
		const img = image();
		expect(img.getAttribute('role')).toBe('button');

		for (const fn of deletionListeners) fn('uuid-1');

		expect(img.getAttribute('role')).toBeNull();
		expect(img.getAttribute('tabindex')).toBeNull();
		expect(img.getAttribute('aria-label')).toBeNull();
		// Inert, not merely unreachable: activation itself refuses, so a
		// synthetic event or a stale focus can't open a viewer on a row that
		// no longer exists.
		press(img, 'Enter');
		click(img);
		expect(openCount()).toBe(0);
	});

	it('does not strand focus on an image that just went inert', () => {
		editor = makeEditor(target);
		const img = image();
		img.focus();
		expect(document.activeElement).toBe(img);

		for (const fn of deletionListeners) fn('uuid-1');

		// Removing tabindex from the focused element would otherwise leave focus
		// somewhere no further keystroke can leave.
		expect(document.activeElement).not.toBe(img);
	});

	it('gives a comment editor the same contract as an item body', () => {
		// CommentEditor.svelte configures AttachmentImage independently, so
		// "works in the body" is not evidence it works in a comment.
		editor = makeCommentEditor(target);
		const img = image();

		expect(img.getAttribute('role')).toBe('button');
		expect(img.getAttribute('tabindex')).toBe('0');
		expect(img.getAttribute('aria-label')).toBe('View image: A diagram');

		expect(openCount()).toBe(0);
		press(img, 'Enter');
		expect(openCount()).toBe(1);
	});

	it('will not ACTIVATE while the image is showing a load-failure placeholder', () => {
		// Attributes are not the contract — activation is. Stripping role and
		// tabindex hides the image from Tab, but a stale event still in flight, a
		// synthetic one, or focus that predates the failure all reach the
		// listeners directly, and there is nothing behind a transient failure to
		// open. Asserting only the attributes leaves this hole wide open, which
		// is precisely the shape of bug this phase keeps shipping.
		editor = makeEditor(target);
		const img = image();
		img.dispatchEvent(new Event('error'));

		press(img, 'Enter');
		press(img, ' ');
		click(img);

		expect(openCount()).toBe(0);
	});

	it('is not a focus stop while the image is showing a load-failure placeholder', () => {
		// The placeholder next to it carries the retry affordance; two controls
		// for one thing, one of which is invisible, is not a contract.
		editor = makeEditor(target);
		const img = image();
		img.dispatchEvent(new Event('error'));

		// The premise first: the image really did hand over to the placeholder.
		// Stripping the semantics off a still-visible image would be a different
		// (and wrong) implementation that these two assertions alone would accept.
		const placeholder = target.querySelector<HTMLElement>('.attachment-missing');
		expect(placeholder?.style.display).not.toBe('none');
		expect(img.style.display).toBe('none');

		expect(img.getAttribute('role')).toBeNull();
		expect(img.getAttribute('tabindex')).toBeNull();
	});
});
