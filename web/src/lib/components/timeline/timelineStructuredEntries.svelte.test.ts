import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { TimelineEntry, TimelineResponse } from '$lib/types';

/**
 * BUG-2301 — implementation notes and decision-log entries render in the item
 * timeline.
 *
 * The regression these guard against is a rendering hole with two independent
 * halves, either of which alone makes the feature invisible:
 *
 *   1. `ItemTimeline` has no branch for the `note` / `decision` kinds, so the
 *      entry falls through the {#if} chain and renders an empty rail.
 *   2. `visibleKinds` — a WHITELIST — omits them, so they never reach the
 *      render at all. This is how the original renderer's replacement would
 *      have shipped dead: the server can merge them perfectly and the tab
 *      still shows nothing.
 *
 * Half 2 is covered here rather than only in ItemDetail because the filter
 * lives on this component; the ItemDetail call site is asserted separately.
 */

const timelineListMock = vi.fn<(ws: string, slug: string) => Promise<TimelineResponse>>();

vi.mock('$lib/api/client', () => ({
	api: {
		timeline: {
			list: (ws: string, slug: string) => timelineListMock(ws, slug),
		},
		comments: {
			create: vi.fn(),
			update: vi.fn(),
			delete: vi.fn(),
			addReaction: vi.fn(),
			removeReaction: vi.fn(),
		},
		attachments: {
			downloadUrl: (ws: string, id: string) => `/api/v1/workspaces/${ws}/attachments/${id}`,
		},
	},
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: () => () => {},
	},
}));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { userId: 'user-1', user: { id: 'user-1', role: 'member' } },
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => false },
}));

vi.mock('$lib/components/editor/attachment-metadata', () => ({
	fetchAttachmentMetadata: () => Promise.resolve({ status: 'transient' as const }),
	revalidateAttachmentMetadata: () => Promise.resolve({ status: 'transient' as const }),
	invalidateAttachmentMetadata: vi.fn(),
}));

vi.mock('$lib/components/CommentEditor.svelte', async () => ({
	default: (await import('./fixtures/InertCommentEditor.svelte')).default,
}));

const { default: ItemTimeline } = await import('./ItemTimeline.svelte');

function noteEntry(overrides: Partial<TimelineEntry> = {}): TimelineEntry {
	return {
		id: 'note-1',
		kind: 'note',
		created_at: '2026-04-02T10:00:00Z',
		actor: 'agent',
		source: 'structured',
		note: { id: 'note-1', summary: 'ensureWorkspace gained a slug-aware attach path', details: 'Refactored the init command.' },
		...overrides,
	};
}

function decisionEntry(overrides: Partial<TimelineEntry> = {}): TimelineEntry {
	return {
		id: 'decision-1',
		kind: 'decision',
		created_at: '2026-04-02T11:00:00Z',
		actor: 'user',
		source: 'structured',
		decision: {
			id: 'decision-1',
			decision: 'Store notes in reserved field keys',
			rationale: 'Avoids a new table for the first cut.',
		},
		...overrides,
	};
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

const props = $state<{
	wsSlug: string;
	username: string;
	itemSlug: string;
	currentContent: string;
	itemId: string;
	collectionId: string;
	hostToken: string;
	visibleKinds: Array<'comment' | 'activity' | 'version' | 'note' | 'decision'> | undefined;
}>({
	wsSlug: 'ws',
	username: 'alice',
	itemSlug: 'TASK-1',
	currentContent: '',
	itemId: 'item-a',
	collectionId: 'coll-1',
	hostToken: 'host-1',
	visibleKinds: undefined,
});

async function settle() {
	for (let i = 0; i < 8; i++) {
		await tick();
		flushSync();
	}
}

function cards(): HTMLElement[] {
	return Array.from(host.querySelectorAll<HTMLElement>('.entry-content > div.card'));
}

function renderedText(): string {
	return host.textContent ?? '';
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	props.visibleKinds = undefined;
	timelineListMock.mockReset();
});

afterEach(() => {
	if (app) {
		unmount(app);
		app = null;
	}
	host.remove();
});

describe('structured timeline entries', () => {
	it('renders a note entry with its summary and details', async () => {
		timelineListMock.mockResolvedValue({ entries: [noteEntry()], has_more: false });
		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		const text = renderedText();
		expect(text).toContain('ensureWorkspace gained a slug-aware attach path');
		expect(text).toContain('Refactored the init command.');
		// The kind has to be legible as a note, not indistinguishable from a comment.
		expect(text).toContain('Note');
	});

	it('renders a decision entry with its decision and rationale', async () => {
		timelineListMock.mockResolvedValue({ entries: [decisionEntry()], has_more: false });
		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		const text = renderedText();
		expect(text).toContain('Store notes in reserved field keys');
		expect(text).toContain('Avoids a new table for the first cut.');
		expect(text).toContain('Decision');
	});

	it('gives a decision more visual weight than a note', async () => {
		timelineListMock.mockResolvedValue({
			entries: [decisionEntry(), noteEntry()],
			has_more: false,
		});
		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		const rendered = cards();
		expect(rendered).toHaveLength(2);
		const decisionCard = rendered.find((c) => c.textContent?.includes('Store notes'));
		const noteCard = rendered.find((c) => c.textContent?.includes('ensureWorkspace'));
		expect(decisionCard?.classList.contains('decision')).toBe(true);
		expect(noteCard?.classList.contains('decision')).toBe(false);
	});

	it('labels the writer, so an agent note is not read as a human one', async () => {
		timelineListMock.mockResolvedValue({
			entries: [noteEntry(), decisionEntry()],
			has_more: false,
		});
		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		const rendered = cards();
		const noteCard = rendered.find((c) => c.textContent?.includes('ensureWorkspace'));
		const decisionCard = rendered.find((c) => c.textContent?.includes('Store notes'));
		expect(noteCard?.textContent).toContain('Agent');
		expect(decisionCard?.textContent).toContain('User');
	});

	it('renders an entry whose optional body is absent', async () => {
		timelineListMock.mockResolvedValue({
			entries: [
				noteEntry({ note: { id: 'note-1', summary: 'summary only' } }),
				decisionEntry({ decision: { id: 'decision-1', decision: 'decision only' } }),
			],
			has_more: false,
		});
		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		expect(renderedText()).toContain('summary only');
		expect(renderedText()).toContain('decision only');
		expect(cards()).toHaveLength(2);
	});

	// A partial entry — kind set, payload missing — must still produce a card.
	// Guarding the branch on the payload leaves the rail dot and connector
	// drawn with nothing beside them, which reads as a broken render rather
	// than as a thin entry.
	it('renders a card for an entry whose payload is missing entirely', async () => {
		timelineListMock.mockResolvedValue({
			entries: [
				noteEntry({ note: undefined }),
				decisionEntry({ decision: undefined }),
			],
			has_more: false,
		});
		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		expect(cards()).toHaveLength(2);
		// The kind label still identifies what the entry is.
		expect(renderedText()).toContain('Note');
		expect(renderedText()).toContain('Decision');
	});

	// The whitelist half of the hole. A kind the filter omits renders nowhere,
	// no matter how correct the server merge is.
	it('honours visibleKinds — the Activity set shows them, the Versions set does not', async () => {
		timelineListMock.mockResolvedValue({
			entries: [noteEntry(), decisionEntry()],
			has_more: false,
		});
		props.visibleKinds = ['comment', 'activity', 'note', 'decision'];
		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();
		expect(cards()).toHaveLength(2);

		props.visibleKinds = ['version'];
		await settle();
		expect(cards()).toHaveLength(0);
	});

	// The exact configuration that shipped the feature invisible the first
	// time: server merges the kinds, whitelist predates them.
	it('a whitelist that predates the kinds hides them entirely', async () => {
		timelineListMock.mockResolvedValue({
			entries: [noteEntry(), decisionEntry()],
			has_more: false,
		});
		props.visibleKinds = ['comment', 'activity'];
		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();
		expect(cards()).toHaveLength(0);
	});

	// Body text is written as plain text by the CLI, never through a markdown
	// pass, so it must not be interpreted as markup.
	it('does not interpret entry text as HTML', async () => {
		timelineListMock.mockResolvedValue({
			entries: [
				noteEntry({
					note: {
						id: 'note-1',
						summary: 'a <script>alert(1)</script> summary',
						details: '<img src=x onerror="alert(2)">',
					},
				}),
			],
			has_more: false,
		});
		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		expect(host.querySelector('script')).toBeNull();
		expect(host.querySelector('img')).toBeNull();
		expect(renderedText()).toContain('<script>alert(1)</script>');
	});
});
