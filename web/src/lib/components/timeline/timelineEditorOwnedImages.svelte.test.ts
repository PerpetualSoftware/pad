// The timeline vs. a LIVE editor mounted inside it (PLAN-2392 DR-12 / TASK-2432).
//
// ItemTimeline delegates thumbnail click/keydown across its whole entry list
// and runs a role/tabindex pass over every `img[data-attachment-id]` in it.
// That list CONTAINS live CommentEditor instances, whose inline images are
// AttachmentImage NodeViews that own their own activation and semantics. Two
// components reaching for the same DOM produced two failures:
//
//   - the semantics pass stripped role/tabindex/aria-label from a NodeView
//     image, because `attMeta` is probed from SAVED bodies and an image being
//     EDITED is not in it — leaving it mouse-openable and keyboard-dead;
//   - the delegated keydown had no modifier check, so Cmd/Ctrl+Enter — which
//     the NodeView deliberately lets through, because it is CommentEditor's
//     submit binding — opened a viewer on top of the submit.
//
// EVERYTHING REAL: the real ItemTimeline, the real TimelineCommentCard, the
// real CommentEditor with its real Tiptap/AttachmentImage configuration, the
// real markdown pipeline, the real Lightbox. Nothing about the delegation or
// the ownership check is restated here — a copy of that logic would stay green
// after the guards were deleted from the component, which is the whole reason
// this file exists rather than another stand-in.
//
// The two viewers are distinguishable, which is what makes "exactly once"
// observable: the NodeView opens its own `dialog.attachment-image-lightbox`,
// the timeline opens the `Lightbox` component (`.lightbox-backdrop`).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Comment, TimelineEntry, TimelineResponse } from '$lib/types';
import { __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';
import { _resetEscapeStackForTests } from '$lib/stores/escapeStack';

const PNG = '11111111-1111-4111-8111-111111111111';
// An attachment whose HEAD probe does not resolve to a MIME, so it never
// enters `attMeta`. This is what EVERY image in a draft comment looks like to
// the timeline — `attMeta` is probed from SAVED bodies only — and it is the
// case in which the timeline's accessibility pass would strip a NodeView's
// semantics. An image the editor and a saved body SHARE is already probed, so
// a fixture built only from that one cannot reach the bug.
const UNPROBED = '99999999-9999-4999-8999-999999999999';

const timelineListMock = vi.fn<(ws: string, slug: string) => Promise<TimelineResponse>>();
// The comment-edit write. Observed rather than ignored: "Cmd+Enter does not
// open a viewer" is only half the claim — the other half is that the SUBMIT it
// was for still happened.
const commentUpdateMock =
	vi.fn<(ws: string, id: string, payload: { body: string }) => Promise<unknown>>();

vi.mock('$lib/api/client', () => ({
	api: {
		timeline: { list: (ws: string, slug: string) => timelineListMock(ws, slug) },
		comments: {
			create: vi.fn(),
			update: (ws: string, id: string, payload: { body: string }) =>
				commentUpdateMock(ws, id, payload),
			delete: vi.fn(),
			addReaction: vi.fn(),
			removeReaction: vi.fn(),
		},
		attachments: {
			downloadUrl: (ws: string, id: string, variant?: string) =>
				`/api/v1/workspaces/${ws}/attachments/${id}${variant ? `?variant=${variant}` : ''}`,
			upload: vi.fn(),
		},
	},
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onItemEvent: () => () => {} },
}));

// The comment's author, so its Edit affordance is offered and a real
// CommentEditor can be mounted over its body.
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { userId: 'user-1', user: { id: 'user-1', role: 'member' } },
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => true },
}));

vi.mock('$lib/components/editor/attachment-metadata', () => ({
	fetchAttachmentMetadata: (_ws: string, uuid: string) =>
		Promise.resolve(
			uuid === PNG
				? { status: 'ok' as const, mime: 'image/png', size: 4096 }
				: { status: 'transient' as const }
		),
	revalidateAttachmentMetadata: () => Promise.resolve({ status: 'transient' as const }),
	invalidateAttachmentMetadata: vi.fn(),
	mimeToFormat: () => null,
}));

// NOTE: CommentEditor is deliberately NOT stubbed here (the sibling
// ItemTimeline spec stubs it, which is exactly why that spec cannot see any of
// this). Tiptap runs for real.
const { default: ItemTimeline } = await import('./ItemTimeline.svelte');

function respond(): TimelineResponse {
	const c: Comment = {
		id: 'c1',
		item_id: 'item-a',
		workspace_id: 'ws-1',
		author: 'alice',
		user_id: 'user-1',
		body: `![a diagram](pad-attachment:${PNG})\n\n![a sketch](pad-attachment:${UNPROBED})`,
		created_by: 'alice',
		source: 'web',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as Comment;
	const e: TimelineEntry = {
		id: 'e-c1',
		kind: 'comment',
		created_at: c.created_at,
		actor: 'alice',
		source: 'web',
		comment: c,
	};
	return { entries: [e], has_more: false };
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

const props = $state({
	wsSlug: 'ws',
	username: 'alice',
	itemSlug: 'TASK-1',
	currentContent: '',
	itemId: 'item-a',
	collectionId: 'coll-1',
	visibleKinds: undefined as Array<'comment' | 'activity' | 'version'> | undefined,
});

/** Fetch → probe → render → the deferred semantics pass, plus Tiptap's mount. */
async function settle() {
	for (let i = 0; i < 10; i++) {
		await tick();
		flushSync();
	}
}

/** The image the LIVE EDITOR owns, i.e. the NodeView's. */
function editorImage(id: string = PNG): HTMLElement {
	const el = host.querySelector<HTMLElement>(
		`.ProseMirror[contenteditable] img[data-attachment-id="${id}"]`
	);
	if (!el) throw new Error(`no editor-owned image rendered for ${id}`);
	return el;
}

/** The image the TIMELINE owns, i.e. rendered `{@html}` markup. */
function bodyImage(): HTMLElement {
	const el = host.querySelector<HTMLElement>('.comment-body img[data-attachment-id]');
	if (!el) throw new Error('no rendered body image');
	return el;
}

/** Viewers currently open, counted per owner so a double-open is visible. */
function viewers() {
	return {
		node: document.querySelectorAll('dialog.attachment-image-lightbox').length,
		timeline: document.querySelectorAll('.lightbox-backdrop').length,
	};
}

function press(el: HTMLElement, key: string, mods: KeyboardEventInit = {}) {
	el.dispatchEvent(
		new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...mods })
	);
	flushSync();
}

function click(el: HTMLElement) {
	el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
	flushSync();
}

/** Open the comment's inline edit form — a real CommentEditor over its body. */
async function enterEditMode() {
	const btn = host.querySelector<HTMLButtonElement>('.edit-btn');
	if (!btn) throw new Error('no Edit affordance — check the author/admin gate');
	btn.click();
	await settle();
}

describe('ItemTimeline vs. a live editor inside it', () => {
	beforeEach(async () => {
		_resetEscapeStackForTests();
		__resetViewerBackdropForTests();
		timelineListMock.mockReset();
		timelineListMock.mockResolvedValue(respond());
		commentUpdateMock.mockReset();
		commentUpdateMock.mockResolvedValue(undefined);
		props.visibleKinds = undefined;
		host = document.body.appendChild(document.createElement('div'));
		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();
	});

	afterEach(() => {
		if (app) unmount(app);
		app = null;
		host.remove();
		document
			.querySelectorAll('dialog.attachment-image-lightbox, .lightbox-backdrop')
			.forEach((d) => d.remove());
	});

	it('mounts a live editor whose image already carries the NodeView contract', async () => {
		// The PREMISE the rest of this file rests on: entering edit mode really
		// does produce a live ProseMirror root with a real AttachmentImage
		// NodeView inside the timeline. This test does NOT cover the ownership
		// check — the timeline's pass has not re-run at this point, so it stays
		// green without it. "…when its semantics pass RE-RUNS" below is the one
		// that carries that guard.
		await enterEditMode();
		const img = editorImage();

		expect(img.getAttribute('role')).toBe('button');
		expect(img.getAttribute('tabindex')).toBe('0');
		expect(img.getAttribute('aria-label')).toBe('View image: a diagram');
	});

	// Exactly-once for the three ordinary gestures, end to end through both
	// components. What actually keeps the timeline out of these is the
	// NodeView's own stopPropagation, NOT the ownership check — they stay green
	// with the ownership check deleted, and the double-click test below is the
	// one that covers it. Kept because "one keypress, one viewer" is the
	// headline claim and it should be asserted against the real pair.
	it('opens exactly one viewer on Enter inside the editor', async () => {
		await enterEditMode();
		expect(viewers()).toEqual({ node: 0, timeline: 0 });

		press(editorImage(), 'Enter');

		expect(viewers()).toEqual({ node: 1, timeline: 0 });
	});

	it('opens exactly one viewer on Space inside the editor', async () => {
		await enterEditMode();
		press(editorImage(), ' ');
		expect(viewers()).toEqual({ node: 1, timeline: 0 });
	});

	it('opens exactly one viewer on a click inside the editor', async () => {
		await enterEditMode();
		click(editorImage());
		expect(viewers()).toEqual({ node: 1, timeline: 0 });
	});

	it('SUBMITS on Cmd/Ctrl+Enter inside the editor, and opens no viewer', async () => {
		// The end-to-end statement of the bug this fixed: with an image focused,
		// Cmd+Enter opened a viewer INSTEAD of posting. Both halves are asserted
		// — "no viewer appeared" alone would also be satisfied by a keystroke
		// that did nothing at all, which is the other way to break this.
		//
		// Not uniquely load-bearing for either guard on its own: the NodeView's
		// modifier guard and the timeline's ownership check each independently
		// keep the viewer shut. It is here for the user-visible behaviour.
		await enterEditMode();
		const img = editorImage();

		press(img, 'Enter', { metaKey: true });
		await settle();

		expect(viewers()).toEqual({ node: 0, timeline: 0 });
		expect(commentUpdateMock).toHaveBeenCalledTimes(1);
		expect(commentUpdateMock.mock.calls[0][2].body).toContain(PNG);
	});

	it('opens NO timeline viewer on a DOUBLE-click inside the editor', async () => {
		// The path the NodeView's own stopPropagation does NOT cover, and
		// therefore the one the ownership check actually carries: multi-click is
		// deliberately let through (`event.detail > 1`) so a user can drag-select
		// around an image. Without the check, the second click of an ordinary
		// double-click reaches the timeline's delegation and opens ITS viewer on
		// top of the one the first click already opened.
		await enterEditMode();
		const img = editorImage();

		img.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		img.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 2 }));
		flushSync();

		expect(viewers()).toEqual({ node: 1, timeline: 0 });
	});

	it('leaves the editor-owned image alone when its semantics pass RE-RUNS', async () => {
		// Two things both have to be true to reach the bug, and each of them
		// alone makes this test vacuous:
		//
		//  - the pass is deferred and dependency-driven, so merely OPENING an
		//    editor does not trigger it. Something must make it run again while
		//    the editor is up. Flipping the pane's kind filter is the ordinary
		//    way a user does that; entries are keyed, so the card and its live
		//    editor survive the re-render.
		//  - the image must be one `attMeta` does not know, since the pass only
		//    strips what it cannot resolve. An image shared with the saved body
		//    is already probed and would be skipped for the wrong reason.
		await enterEditMode();
		const before = editorImage(UNPROBED);
		expect(before.getAttribute('role')).toBe('button');

		props.visibleKinds = ['comment', 'activity', 'version'];
		await settle();
		props.visibleKinds = undefined;
		await settle();

		const img = editorImage(UNPROBED);
		// The SAME element, not a replacement. If the flip had torn the card
		// down and rebuilt it, a fresh NodeView would have re-applied its
		// semantics and every assertion below would pass without the pass ever
		// having had the chance to strip them.
		expect(img).toBe(before);
		expect(img.getAttribute('role')).toBe('button');
		expect(img.getAttribute('tabindex')).toBe('0');
		expect(img.getAttribute('aria-label')).toBe('View image: a sketch');
		// And it still activates — semantics without activation is the dead stop
		// in its other direction.
		press(img, 'Enter');
		expect(viewers()).toEqual({ node: 1, timeline: 0 });
	});

	// The control. Everything above is about the timeline declining to act; this
	// is what it must still do, so a predicate that simply answered "true"
	// everywhere is caught here. Split per gesture rather than opening twice in
	// one test: tearing a mounted Lightbox down mid-test by hand would leave the
	// component and the escape/backdrop registries disagreeing, and the second
	// assertion would then be measuring that instead.
	it('still opens its OWN rendered thumbnail by mouse', async () => {
		const img = bodyImage();
		expect(img.getAttribute('role')).toBe('button');
		expect(img.getAttribute('tabindex')).toBe('0');

		click(img);
		expect(viewers()).toEqual({ node: 0, timeline: 1 });
	});

	it('still opens its OWN rendered thumbnail by keyboard', async () => {
		press(bodyImage(), 'Enter');
		expect(viewers()).toEqual({ node: 0, timeline: 1 });
	});

	it('still declines a MODIFIED key on its own rendered thumbnail', async () => {
		// The modifier guard is not scoped to editor-owned DOM: a shortcut is a
		// shortcut wherever it is pressed.
		press(bodyImage(), 'Enter', { metaKey: true });
		expect(viewers()).toEqual({ node: 0, timeline: 0 });

		press(bodyImage(), 'Enter');
		expect(viewers()).toEqual({ node: 0, timeline: 1 });
	});
});
