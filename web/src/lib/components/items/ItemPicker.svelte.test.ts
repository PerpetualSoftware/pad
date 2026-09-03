// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// ItemPicker is PLAN-2857 U3 (TASK-2862): the add-relationship search lifted
// out of ItemDetail so a relation field (U2) and the Relationships tab share
// one control. The design pass ruled OUT the dropdown-vs-search threshold the
// plan asked for, on the grounds that `localIndex` already holds every item in
// the workspace — so the assertions that matter are (a) collection size does
// not change which control renders or how much DOM it costs, (b) the warm path
// makes NO network call, and (c) the server search still runs when the local
// index has not hydrated.
//
// (c) is the negative control for (b): without it, a picker that queried
// nothing at all would pass every warm assertion here.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';

// `vi.mock` factories are hoisted above every other statement in the file, so
// the doubles they close over have to be hoisted with them.
const { searchApi, localIndexMock, localSearchMock } = vi.hoisted(() => ({
	searchApi: vi.fn(),
	localIndexMock: {
		bootstrapStateFor: vi.fn(),
		getByCollection: vi.fn(),
		findByIdOrSlug: vi.fn(),
	},
	localSearchMock: { search: vi.fn() },
}));

vi.mock('$lib/api/client', () => ({ api: { search: (...a: unknown[]) => searchApi(...a) } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({ localIndex: localIndexMock }));
vi.mock('$lib/stores/localSearch.svelte', () => ({ localSearch: localSearchMock }));

import ItemPicker from './ItemPicker.svelte';

interface Row {
	id: string;
	title: string;
	item_number: number;
	collection_prefix: string;
}

let rowsById = new Map<string, Row>();

/**
 * Reactive backing for `bootstrapStateFor`. The component tracks that read so
 * it can re-list a scoped collection when hydration lands, and a bare `vi.fn`
 * return value is not reactive — a test driving it that way cannot tell a
 * working refresh from a missing one. `SvelteMap` is a plain runtime class
 * (no compiler step), so it gives this `.ts` file the tracked get / triggering
 * set that the real store gets from `$state`.
 */
const bootstrapState = new SvelteMap<string, string>();

function setBootstrapState(value: string) {
	bootstrapState.set('ws', value);
}

function makeRows(n: number, prefix = 'COLO'): Row[] {
	const rows: Row[] = [];
	for (let i = 1; i <= n; i++) {
		rows.push({ id: `id-${i}`, title: `Colour ${i}`, item_number: i, collection_prefix: prefix });
	}
	return rows;
}

function loadIndex(rows: Row[]) {
	rowsById = new Map(rows.map((r) => [r.id, r]));
	localIndexMock.getByCollection.mockReturnValue(rows);
}

function options(): HTMLElement[] {
	return Array.from(document.querySelectorAll<HTMLElement>('[role="option"]'));
}

function input(): HTMLInputElement {
	const el = document.querySelector<HTMLInputElement>('.picker-input');
	if (!el) throw new Error('.picker-input not found');
	return el;
}

async function type(value: string) {
	const el = input();
	el.value = value;
	el.dispatchEvent(new Event('input', { bubbles: true }));
	await tick();
}

async function press(key: string, opts: KeyboardEventInit = {}) {
	input().dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...opts }));
	await tick();
}

const baseProps = { wsSlug: 'ws', collection: 'colors', onselect: () => {} };

beforeEach(() => {
	searchApi.mockReset();
	searchApi.mockResolvedValue({ results: [] });
	bootstrapState.clear();
	setBootstrapState('ready');
	localIndexMock.bootstrapStateFor
		.mockReset()
		.mockImplementation((ws: string) => bootstrapState.get(ws) ?? 'cold');
	localIndexMock.getByCollection.mockReset().mockReturnValue([]);
	localIndexMock.findByIdOrSlug.mockReset().mockImplementation((_ws: string, id: string) => rowsById.get(id) ?? null);
	localSearchMock.search.mockReset().mockReturnValue([]);
	rowsById = new Map();
});

afterEach(() => {
	cleanup();
	vi.useRealTimers();
	document.body.innerHTML = '';
});

describe('ItemPicker — collection size does not change the control (PLAN-2857 Q1)', () => {
	it('lists a small target collection in full, with no network call', async () => {
		loadIndex(makeRows(3));
		render(ItemPicker, { props: { ...baseProps } });
		await tick();

		expect(options()).toHaveLength(3);
		expect(input().getAttribute('role')).toBe('combobox');
		expect(searchApi).not.toHaveBeenCalled();
	});

	it('renders the SAME bounded row count at 20 items and at 2000, still with no network call', async () => {
		loadIndex(makeRows(20));
		const { unmount } = render(ItemPicker, { props: { ...baseProps } });
		await tick();
		const small = options().length;
		unmount();

		loadIndex(makeRows(2000));
		render(ItemPicker, { props: { ...baseProps } });
		await tick();
		const large = options().length;

		// Bounded, and bounded to the SAME number — the row count is a function
		// of `limit`, not of collection size. A picker that switched to a
		// different control (or rendered every row) above a threshold fails here.
		expect(small).toBe(10);
		expect(large).toBe(10);
		expect(large).toBe(small);
		expect(document.querySelectorAll('select')).toHaveLength(0);
		expect(searchApi).not.toHaveBeenCalled();
	});

	it('honours an explicit limit at both sizes', async () => {
		loadIndex(makeRows(2000));
		render(ItemPicker, { props: { ...baseProps, limit: 4 } });
		await tick();
		expect(options()).toHaveLength(4);
	});

	it('hides excluded rows', async () => {
		loadIndex(makeRows(5));
		render(ItemPicker, { props: { ...baseProps, excludeIds: ['id-2', 'id-4'] } });
		await tick();

		const labels = options().map((o) => o.textContent?.replace(/\s+/g, ' ').trim());
		expect(labels).toHaveLength(3);
		expect(labels.join('|')).not.toContain('Colour 2');
		expect(labels.join('|')).not.toContain('Colour 4');
	});

	it('lists the collection once the local index finishes hydrating, without the user typing', async () => {
		// codex round 1 P2: a scoped picker opened while the index was cold had
		// nothing to list and nothing re-ran the listing, so it stayed empty
		// until the user typed.
		loadIndex(makeRows(3));
		setBootstrapState('loading');
		render(ItemPicker, { props: { ...baseProps } });
		await tick();
		expect(options()).toHaveLength(0);

		setBootstrapState('ready');
		await tick();

		expect(options().length).toBeGreaterThan(0);
		expect(searchApi).not.toHaveBeenCalled();
	});

	it('hydrating mid-session does not reset a selection the user has already made', async () => {
		// The `!query.trim()` guard on the hydration effect. Without it the
		// re-list runs unconditionally and resets `activeIndex` to -1, so a user
		// who has arrowed down to a row loses it the moment the index finishes
		// loading — a keystroke they never made, at a moment they cannot predict.
		vi.useFakeTimers();
		loadIndex(makeRows(3));
		setBootstrapState('loading');
		searchApi.mockResolvedValue({
			results: makeRows(3).map((r) => ({ item: r })),
		});

		render(ItemPicker, { props: { ...baseProps } });
		await type('col');
		await vi.advanceTimersByTimeAsync(250);
		await tick();
		expect(options()).toHaveLength(3);

		await press('ArrowDown');
		await press('ArrowDown');
		const selected = input().getAttribute('aria-activedescendant');
		expect(selected).toBe(options()[1].id);

		setBootstrapState('ready');
		await vi.advanceTimersByTimeAsync(0);
		await tick();

		expect(input().getAttribute('aria-activedescendant')).toBe(selected);
	});

	it('shows nothing on an empty query when unscoped — the Relationships-tab mode', async () => {
		loadIndex(makeRows(5));
		render(ItemPicker, { props: { wsSlug: 'ws', onselect: () => {} } });
		await tick();

		expect(options()).toHaveLength(0);
		expect(localIndexMock.getByCollection).not.toHaveBeenCalled();
	});

	it('searches the local index, scoped to the collection, when the user types', async () => {
		loadIndex(makeRows(3));
		localSearchMock.search.mockReturnValue([{ id: 'id-2', score: 1 }]);
		render(ItemPicker, { props: { ...baseProps } });
		await tick();

		await type('col');

		expect(localSearchMock.search).toHaveBeenCalledWith(
			'ws',
			'col',
			expect.objectContaining({ collection: 'colors' })
		);
		expect(options()).toHaveLength(1);
		expect(searchApi).not.toHaveBeenCalled();
	});
});

describe('ItemPicker — cold path (the control leg for "no network call")', () => {
	it('calls the server search, collection-scoped, while the local index is cold', async () => {
		vi.useFakeTimers();
		setBootstrapState('cold');
		searchApi.mockResolvedValue({
			results: [{ item: { id: 'id-9', title: 'Server row', item_number: 9, collection_prefix: 'COLO' } }],
		});
		render(ItemPicker, { props: { ...baseProps } });

		await type('col');
		expect(searchApi).not.toHaveBeenCalled(); // debounced, not fired per keystroke

		await vi.advanceTimersByTimeAsync(250);
		expect(searchApi).toHaveBeenCalledTimes(1);
		expect(searchApi).toHaveBeenCalledWith('col', { workspace: 'ws', collection: 'colors' });

		await tick();
		expect(options()).toHaveLength(1);
		expect(localSearchMock.search).not.toHaveBeenCalled();
	});

	const STALE = {
		results: [{ item: { id: 'id-1', title: 'Stale row', item_number: 1, collection_prefix: 'COLO' } }],
	};

	it('does not let a superseded in-flight response repopulate the box', async () => {
		// The SECOND request is allowed to land first, deliberately. An earlier
		// draft of this test released the stale response while the picker was
		// still `loading`, and it passed against a build with the staleness check
		// DELETED — because the loading branch renders instead of the result
		// list, so the stale row was in `results` and merely not on screen. The
		// mutant survived; the fix is to end the scenario with loading FALSE, so
		// the only thing standing between the stale row and the DOM is the fence.
		vi.useFakeTimers();
		setBootstrapState('cold');

		let releaseFirst!: (v: unknown) => void;
		searchApi
			.mockReturnValueOnce(new Promise((res) => (releaseFirst = res)))
			.mockResolvedValue({ results: [] });

		render(ItemPicker, { props: { ...baseProps } });
		await type('aa');
		await vi.advanceTimersByTimeAsync(250);
		expect(searchApi).toHaveBeenCalledTimes(1);

		// The user keeps typing while the first request is still open; the second
		// one resolves empty and settles the picker.
		await type('bb');
		await vi.advanceTimersByTimeAsync(250);
		expect(searchApi).toHaveBeenCalledTimes(2);
		expect(document.body.textContent).not.toContain('Searching...');

		releaseFirst(STALE);
		await vi.advanceTimersByTimeAsync(0);
		await tick();

		expect(document.body.textContent).not.toContain('Stale row');
		expect(options()).toHaveLength(0);
	});

	it('CONTROL: that exact response DOES reach the DOM when nothing superseded it', async () => {
		// Without this leg the fence test above would pass against a picker that
		// never renders a server result at all.
		vi.useFakeTimers();
		setBootstrapState('cold');

		let release!: (v: unknown) => void;
		searchApi.mockReturnValueOnce(new Promise((res) => (release = res)));

		render(ItemPicker, { props: { ...baseProps } });
		await type('aa');
		await vi.advanceTimersByTimeAsync(250);

		release(STALE);
		await vi.advanceTimersByTimeAsync(0);
		await tick();

		expect(document.body.textContent).toContain('Stale row');
		expect(options()).toHaveLength(1);
	});

	it('never fires a request the user closed the picker before earning', async () => {
		// This asserts the onDestroy `clearTimeout`, and it is deliberately about
		// the REQUEST rather than the rendered result. The tempting version —
		// unmount, then resolve an in-flight response, then assert the stale row
		// is not in the DOM — cannot fail: an unmounted component renders nothing
		// either way, so it passes with every teardown guard deleted. A test that
		// no mutant can kill is not evidence, so it is not here.
		vi.useFakeTimers();
		setBootstrapState('cold');

		const { unmount } = render(ItemPicker, { props: { ...baseProps } });
		await type('aa');
		unmount();
		await vi.advanceTimersByTimeAsync(250);

		expect(searchApi).not.toHaveBeenCalled();
	});
});

describe('ItemPicker — keyboard', () => {
	it('moves the active row with the arrows and reports it via aria-activedescendant', async () => {
		loadIndex(makeRows(3));
		render(ItemPicker, { props: { ...baseProps } });
		await tick();

		expect(input().hasAttribute('aria-activedescendant')).toBe(false);

		await press('ArrowDown');
		expect(options()[0].getAttribute('aria-selected')).toBe('true');
		expect(input().getAttribute('aria-activedescendant')).toBe(options()[0].id);

		await press('ArrowDown');
		expect(options()[1].getAttribute('aria-selected')).toBe('true');
		expect(input().getAttribute('aria-activedescendant')).toBe(options()[1].id);

		await press('ArrowUp');
		expect(options()[0].getAttribute('aria-selected')).toBe('true');
	});

	it('selects the active row on Enter, and does nothing on Enter with no active row', async () => {
		loadIndex(makeRows(3));
		const onselect = vi.fn();
		render(ItemPicker, { props: { ...baseProps, onselect } });
		await tick();

		await press('Enter');
		expect(onselect).not.toHaveBeenCalled();

		await press('ArrowDown');
		await press('ArrowDown');
		await press('Enter');
		expect(onselect).toHaveBeenCalledTimes(1);
		expect(onselect.mock.calls[0][0].id).toBe('id-2');
	});

	it('clicking a row selects it', async () => {
		loadIndex(makeRows(2));
		const onselect = vi.fn();
		render(ItemPicker, { props: { ...baseProps, onselect } });
		await tick();

		options()[1].click();
		expect(onselect).toHaveBeenCalledTimes(1);
		expect(onselect.mock.calls[0][0].id).toBe('id-2');
	});
});

describe('ItemPicker — Escape only consumes what it closes', () => {
	// The page's Escape driver is a bubble-phase window listener; a
	// late-registered window listener stands in for it here.
	//
	// What that driver DOES with the key is deliberately not modelled: both pane
	// hosts return early for text-entry targets before they reach the escape
	// stack, so a picker with no `oncancel` leaves Escape with no owner rather
	// than handing it upward (codex round 1 P2 — the first version of this file
	// claimed otherwise in its own comment, without having read the handler).
	// These legs assert only what this component does: consume the key when it
	// has something to close, and leave it untouched when it does not.
	function windowEscapeSpy() {
		const seen = vi.fn();
		const handler = (e: Event) => seen(e);
		window.addEventListener('keydown', handler);
		return { seen, off: () => window.removeEventListener('keydown', handler) };
	}

	it('clears a non-empty query and does not let Escape reach the layer above', async () => {
		loadIndex(makeRows(3));
		localSearchMock.search.mockReturnValue([{ id: 'id-1', score: 1 }]);
		render(ItemPicker, { props: { ...baseProps } });
		await tick();
		await type('col');
		expect(input().value).toBe('col');

		const { seen, off } = windowEscapeSpy();
		await press('Escape');
		off();

		expect(input().value).toBe('');
		expect(seen).not.toHaveBeenCalled();
	});

	it('calls oncancel on an empty box, and still consumes the key', async () => {
		loadIndex(makeRows(3));
		const oncancel = vi.fn();
		render(ItemPicker, { props: { ...baseProps, oncancel } });
		await tick();

		const { seen, off } = windowEscapeSpy();
		await press('Escape');
		off();

		expect(oncancel).toHaveBeenCalledTimes(1);
		expect(seen).not.toHaveBeenCalled();
	});

	it('CONTROL: with no oncancel and an empty box, the picker leaves the key alone', async () => {
		// Without this leg the two above would pass against a picker that
		// swallowed EVERY Escape unconditionally.
		loadIndex(makeRows(3));
		render(ItemPicker, { props: { ...baseProps } });
		await tick();

		const { seen, off } = windowEscapeSpy();
		await press('Escape');
		off();

		expect(seen).toHaveBeenCalledTimes(1);
		expect(seen.mock.calls[0][0].defaultPrevented).toBe(false);
	});
});
