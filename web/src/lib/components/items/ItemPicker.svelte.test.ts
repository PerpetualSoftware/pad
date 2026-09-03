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
		cursorFor: vi.fn(),
		getByCollection: vi.fn(),
		findByIdOrSlug: vi.fn(),
	},
	localSearchMock: { search: vi.fn(), epoch: vi.fn() },
}));

vi.mock('$lib/api/client', () => ({ api: { search: (...a: unknown[]) => searchApi(...a) } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({ localIndex: localIndexMock }));
vi.mock('$lib/stores/localSearch.svelte', () => ({ localSearch: localSearchMock }));

import ItemPicker from './ItemPicker.svelte';
import ItemPickerProbe from './ItemPickerProbe.svelte';

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
/**
 * Same story for `localSearch.epoch`, which every write to the search index
 * bumps — including the cursorless optimistic paths (`localIndex.upsert` /
 * `remove`) that an applied-delta cursor never sees.
 */
const epochs = new SvelteMap<string, number>();
/**
 * The workspace cursor is kept in the double — and deliberately never bumped by
 * `bumpEpoch` — so that a component wired to it instead would still RUN and
 * simply fail to refresh. Deleting it from the mock would make such a build
 * throw instead, and a mutant that dies of a TypeError proves nothing about
 * which signal is correct.
 */
const cursors = new SvelteMap<string, string>();

function setBootstrapState(value: string) {
	bootstrapState.set('ws', value);
}

/** Stand-in for anything that mutates the local index. */
function bumpEpoch() {
	epochs.set('ws', (epochs.get('ws') ?? 0) + 1);
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
	epochs.clear();
	cursors.clear();
	setBootstrapState('ready');
	localIndexMock.bootstrapStateFor
		.mockReset()
		.mockImplementation((ws: string) => bootstrapState.get(ws) ?? 'cold');
	localSearchMock.epoch
		.mockReset()
		.mockImplementation((ws: string) => epochs.get(ws) ?? 0);
	localIndexMock.cursorFor
		.mockReset()
		.mockImplementation((ws: string) => cursors.get(ws) ?? '0');
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
		// The re-list recomputes `activeIndex` from the selected row's ID. Without
		// that, it resets to -1 and a user who has arrowed down to a row loses it
		// the moment the index finishes loading — a keystroke they never made, at
		// a moment they cannot predict.
		vi.useFakeTimers();
		loadIndex(makeRows(3));
		setBootstrapState('loading');
		searchApi.mockResolvedValue({
			results: makeRows(3).map((r) => ({ item: r })),
		});
		// Once warm the same query is answered by the local index.
		localSearchMock.search.mockReturnValue(
			makeRows(3).map((r) => ({ id: r.id, score: 1 }))
		);

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

	it('picks up an index mutation that lands while it is open', async () => {
		// codex round 2 P2: the picker holds a COPY of the rows, so a change
		// arriving while it is on screen left it listing what the workspace used
		// to contain until the query changed or it remounted. Driven through the
		// search epoch rather than the workspace cursor — round 3 P2: the cursor
		// misses every optimistic `localIndex.upsert` / `remove`, which mutate
		// the rows without advancing it.
		loadIndex(makeRows(2));
		render(ItemPicker, { props: { ...baseProps } });
		await tick();
		expect(options()).toHaveLength(2);

		loadIndex(makeRows(5));
		bumpEpoch();
		await tick();

		expect(options()).toHaveLength(5);
	});

	it('refreshes on a mutation that never advances the workspace cursor', async () => {
		// codex round 3 P2, and the whole reason the dependency is the search
		// epoch. `localIndex.upsert()` / `remove()` — optimistic creates and
		// edits, the 403 purge — mutate the rows and mirror to `localSearch`
		// WITHOUT touching `state.cursor`. This leg bumps only the epoch, so a
		// picker wired to the cursor sees nothing and stays stale.
		loadIndex(makeRows(2));
		render(ItemPicker, { props: { ...baseProps } });
		await tick();
		expect(options()).toHaveLength(2);

		loadIndex(makeRows(4));
		bumpEpoch();
		await tick();

		expect(options()).toHaveLength(4);
		expect(cursors.get('ws')).toBeUndefined(); // the cursor never moved
	});

	it('re-filters when the exclusion set arrives late', async () => {
		// codex round 4 P2. `ItemDetail` loads `itemLinks` asynchronously, so a
		// picker opened before that resolves was offering items already linked to
		// the source — clicking one is a duplicate-link write the user did not
		// know they were making.
		//
		// Driven through `ItemPickerProbe` rather than testing-library's
		// `rerender`, on purpose: `rerender` replaces the whole props object,
		// which re-runs the refresh effect whether or not it tracks `excludeIds`.
		// The mutant that drops that dependency SURVIVES a rerender-driven
		// version of this test while breaking the real parent, which changes one
		// prop at a time. The probe changes one prop.
		loadIndex(makeRows(4));
		const probe = render(ItemPickerProbe, {
			props: { wsSlug: 'ws', collection: 'colors' },
		});
		await tick();
		expect(options()).toHaveLength(4);

		probe.component.setExcludeIds(['id-2', 'id-3']);
		await tick();

		const labels = options().map((o) => o.textContent ?? '');
		expect(options()).toHaveLength(2);
		expect(labels.join('|')).not.toContain('Colour 2');
		expect(labels.join('|')).not.toContain('Colour 3');
	});

	it('drops everything it was showing when the workspace state is reset', async () => {
		// codex round 4 P1: `localIndex.reset()` — sign-out, a 403 membership
		// purge, a deleted workspace — bumps the search epoch on its way out, so
		// this effect runs. Returning early there left rows the viewer may no
		// longer be allowed to see listed AND selectable.
		loadIndex(makeRows(3));
		render(ItemPicker, { props: { ...baseProps } });
		await tick();
		expect(options()).toHaveLength(3);

		// What reset() does, in order: drop the state (so bootstrapStateFor falls
		// back to 'cold') and reset the search index (which bumps the epoch).
		localIndexMock.getByCollection.mockReturnValue([]);
		setBootstrapState('cold');
		bumpEpoch();
		await tick();

		expect(options()).toHaveLength(0);
		expect(document.body.textContent).not.toContain('Colour 1');
	});

	it('does not let a cold response land after the workspace state is reset', async () => {
		vi.useFakeTimers();
		setBootstrapState('cold');

		let release!: (v: unknown) => void;
		searchApi.mockReturnValueOnce(new Promise((res) => (release = res)));

		render(ItemPicker, { props: { ...baseProps } });
		await type('aa');
		await vi.advanceTimersByTimeAsync(250);
		expect(searchApi).toHaveBeenCalledTimes(1);

		// Reset arrives while the request is open.
		bumpEpoch();
		await tick();

		release({
			results: [{ item: { id: 'id-1', title: 'Revoked row', item_number: 1, collection_prefix: 'COLO' } }],
		});
		await vi.advanceTimersByTimeAsync(0);
		await tick();

		expect(document.body.textContent).not.toContain('Revoked row');
	});

	it('CONTROL: an ordinary cold mount is not torn down by the same effect', async () => {
		// The reset teardown is gated on already showing or awaiting something.
		// Without that gate it would fire on every cold mount and cancel the cold
		// path's own in-flight search — so this leg has to stay green.
		vi.useFakeTimers();
		setBootstrapState('cold');
		searchApi.mockResolvedValue({
			results: [{ item: { id: 'id-7', title: 'Cold row', item_number: 7, collection_prefix: 'COLO' } }],
		});

		render(ItemPicker, { props: { ...baseProps } });
		await type('aa');
		await vi.advanceTimersByTimeAsync(250);
		await tick();

		expect(document.body.textContent).toContain('Cold row');
	});

	it('keeps the selected row across a delta, and drops the highlight when that row goes away', async () => {
		loadIndex(makeRows(5));
		render(ItemPicker, { props: { ...baseProps } });
		await tick();

		await press('ArrowDown');
		await press('ArrowDown');
		await press('ArrowDown');
		const selectedId = 'id-3';
		expect(input().getAttribute('aria-activedescendant')).toBe(options()[2].id);

		// A delta that removes the rows ABOVE the selection: a blind re-list would
		// leave the highlight on index 2, which is now a different item.
		loadIndex(makeRows(5).filter((r) => r.id !== 'id-1'));
		bumpEpoch();
		await tick();
		const stillSelected = options().findIndex((o) => o.getAttribute('aria-selected') === 'true');
		expect(options()[stillSelected].textContent).toContain('Colour 3');
		expect(rowsById.get(selectedId)).toBeDefined();

		// Now the selected row itself disappears — the highlight clears rather
		// than pointing at whatever slid into that position.
		loadIndex(makeRows(5).filter((r) => r.id !== 'id-1' && r.id !== 'id-3'));
		bumpEpoch();
		await tick();
		expect(input().hasAttribute('aria-activedescendant')).toBe(false);
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

describe('ItemPicker — source: the two callers want different models', () => {
	// Lead ruling on PR #1241: the Relationships tab keeps the server FTS, which
	// indexes item BODY CONTENT. `localIndex` strips `content` by design, so the
	// warm path can never answer "the item that mentioned that phrase" — losing
	// it silently is a regression regardless of how consistent the other pickers
	// are. A relation field wants the index's model and takes the default.

	it('source="server" queries /search even with a hydrated index', async () => {
		vi.useFakeTimers();
		loadIndex(makeRows(3));
		setBootstrapState('ready'); // warm — the default source would stay local
		searchApi.mockResolvedValue({
			results: [{ item: { id: 'id-9', title: 'Body match', item_number: 9, collection_prefix: 'COLO' } }],
		});

		render(ItemPicker, { props: { ...baseProps, source: 'server' } });
		await type('phrase from the body');
		await vi.advanceTimersByTimeAsync(250);
		await tick();

		expect(searchApi).toHaveBeenCalledTimes(1);
		expect(searchApi).toHaveBeenCalledWith('phrase from the body', {
			workspace: 'ws',
			collection: 'colors',
		});
		expect(localSearchMock.search).not.toHaveBeenCalled();
		expect(document.body.textContent).toContain('Body match');
	});

	it('CONTROL: the default source, same warm index, never reaches the network', async () => {
		vi.useFakeTimers();
		loadIndex(makeRows(3));
		setBootstrapState('ready');
		localSearchMock.search.mockReturnValue([{ id: 'id-2', score: 1 }]);

		render(ItemPicker, { props: { ...baseProps } });
		await type('phrase from the body');
		await vi.advanceTimersByTimeAsync(250);
		await tick();

		expect(localSearchMock.search).toHaveBeenCalledTimes(1);
		expect(searchApi).not.toHaveBeenCalled();
	});

	it('source="server" still opens with the collection listed from the index', async () => {
		// An empty-query LISTING is not a search, and `/search` cannot answer one
		// (it requires a `q`). Only QUERIES go to the server.
		loadIndex(makeRows(3));
		render(ItemPicker, { props: { ...baseProps, source: 'server' } });
		await tick();

		expect(options()).toHaveLength(3);
		expect(searchApi).not.toHaveBeenCalled();
	});

	it('source="server" honours a late exclusion set without re-issuing the request', async () => {
		vi.useFakeTimers();
		loadIndex(makeRows(4));
		searchApi.mockResolvedValue({
			results: makeRows(4).map((r) => ({ item: r })),
		});
		const probe = render(ItemPickerProbe, {
			props: { wsSlug: 'ws', collection: 'colors', source: 'server' as const },
		});
		await type('col');
		await vi.advanceTimersByTimeAsync(250);
		await tick();
		expect(options()).toHaveLength(4);
		expect(searchApi).toHaveBeenCalledTimes(1);

		probe.component.setExcludeIds(['id-2']);
		await tick();

		expect(options()).toHaveLength(3);
		expect(options().map((o) => o.textContent ?? '').join('|')).not.toContain('Colour 2');
		// The whole point of deriving the filter: no second request.
		expect(searchApi).toHaveBeenCalledTimes(1);
	});

	it('source="server" does not re-query on an index delta', async () => {
		// A request per applied delta is exactly the rate-limiter pressure the
		// debounce exists to avoid, and the index is not this list's source of
		// truth anyway.
		vi.useFakeTimers();
		loadIndex(makeRows(3));
		searchApi.mockResolvedValue({ results: makeRows(3).map((r) => ({ item: r })) });

		render(ItemPicker, { props: { ...baseProps, source: 'server' } });
		await type('col');
		await vi.advanceTimersByTimeAsync(250);
		await tick();
		expect(searchApi).toHaveBeenCalledTimes(1);

		bumpEpoch();
		await tick();
		await vi.advanceTimersByTimeAsync(250);

		expect(searchApi).toHaveBeenCalledTimes(1);
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

// ── U8: inline create from the picker (TASK-2877) ────────────────────────
//
// Pins written BEFORE the affordance exists, per team CONVE-29.
//
// The whole affordance is OPT-IN at the call site: the picker offers a create
// row only when the host passes `oncreate`. That is not decoration — it is
// where the two rules in the unit's scope live. "Relation-field pickers only"
// is the Relationships tab simply not passing it, and "no create row unless the
// user can create in the target collection" is `FieldEditor` withholding it
// (pinned separately in `FieldEditor.relation.svelte.test.ts`, since the
// permission cascade is not this component's to know).
//
// The no-duplicate half of the proving test lives here rather than in the
// caller: it is EXACT-TITLE SUPPRESSION, not a create-time check. Once the
// item exists, a picker asked for the same text again offers the row and
// withholds the create affordance, so a second invocation cannot be a create
// at all. `FieldEditor` upserting the new row into `localIndex` is what makes
// that true on the very next keystroke; the mechanism is pinned there.
describe('ItemPicker — inline create (PLAN-2857 U8)', () => {
	function createRow(): HTMLElement | null {
		return document.querySelector<HTMLElement>('.picker-create');
	}

	const createProps = { ...baseProps, createLabel: 'Colors' };

	it('offers no create row when the host passes no oncreate — the Relationships-tab caller', async () => {
		loadIndex(makeRows(3));
		localSearchMock.search.mockReturnValue([]);
		render(ItemPicker, { props: { ...baseProps } });
		await tick();

		await type('Purple');

		expect(createRow()).toBeNull();
		// CONTROL: the same query DOES produce the row when a host opts in, so
		// this leg is measuring the opt-in and not a query that never renders.
		cleanup();
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn() } });
		await tick();
		await type('Purple');
		expect(createRow()).not.toBeNull();
	});

	it('offers the create row, naming the query and the TARGET collection, when nothing matches', async () => {
		loadIndex(makeRows(3));
		localSearchMock.search.mockReturnValue([]);
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn() } });
		await tick();

		await type('Purple');

		expect(createRow()).not.toBeNull();
		expect(createRow()!.textContent).toContain('Purple');
		// The collection is named so the row cannot be read as "create it here".
		// U8 creates in the field's declared target, which is frequently NOT the
		// collection the user is looking at.
		expect(createRow()!.textContent).toContain('Colors');
	});

	it('still offers create when matches exist but none is EXACT — the "or nothing exactly" half', async () => {
		loadIndex([
			{ id: 'id-1', title: 'Purple Rain', item_number: 1, collection_prefix: 'COLO' },
		]);
		localSearchMock.search.mockReturnValue([{ id: 'id-1', score: 1 }]);
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn() } });
		await tick();

		await type('Purple');

		expect(options()).toHaveLength(2);
		// Trailing, per the unit: the matches are what the user most likely wants.
		expect(options()[1]).toBe(createRow());
	});

	it('withholds create when an exact title already exists — the no-duplicate mechanism', async () => {
		loadIndex([
			{ id: 'id-1', title: 'Purple', item_number: 1, collection_prefix: 'COLO' },
		]);
		localSearchMock.search.mockReturnValue([{ id: 'id-1', score: 1 }]);
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn() } });
		await tick();

		await type('Purple');

		expect(createRow()).toBeNull();
		expect(options()).toHaveLength(1);
	});

	it('matches an existing title case- and whitespace-insensitively', async () => {
		// A user who typed "purple " must not mint a second "Purple". Exactness
		// here is about the user's INTENT to name an existing row, and neither
		// case nor a trailing space changes that intent.
		loadIndex([
			{ id: 'id-1', title: 'Purple', item_number: 1, collection_prefix: 'COLO' },
		]);
		localSearchMock.search.mockReturnValue([{ id: 'id-1', score: 1 }]);
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn() } });
		await tick();

		await type('  purple ');

		expect(createRow()).toBeNull();
	});

	it('offers nothing to create on an empty query, or without a target collection', async () => {
		loadIndex(makeRows(3));
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn() } });
		await tick();
		// Empty query: the scoped picker is LISTING, and "Create ''" is not a
		// thing the user asked for.
		expect(createRow()).toBeNull();

		cleanup();
		// Unscoped: there is no target collection to create into, so the row
		// would have to guess. A host that passes `oncreate` without a
		// `collection` gets nothing rather than a wrong destination.
		localSearchMock.search.mockReturnValue([]);
		render(ItemPicker, {
			props: { wsSlug: 'ws', onselect: () => {}, oncreate: vi.fn(), createLabel: 'Colors' },
		});
		await tick();
		await type('Purple');
		expect(createRow()).toBeNull();
	});

	it('offers nothing to create while a cold search is still in flight', async () => {
		// "Nothing matched" is not yet known while the server is answering. A
		// create row here would invite a duplicate of a row about to arrive.
		//
		// `aria-expanded` is the assertion that can actually FAIL, and the
		// reason this leg is worth having. The markup renders the loading
		// branch INSTEAD of the listbox, so a build that offered the create row
		// mid-flight would still show no `.picker-create` — the row would exist
		// in the options list and merely be off screen, which is trap #1 from
		// this plan's false-green note. What leaks is the combobox announcing
		// itself expanded while no listbox is rendered.
		vi.useFakeTimers();
		setBootstrapState('cold');
		let release!: (v: unknown) => void;
		searchApi.mockReturnValue(new Promise((r) => { release = r; }));
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn() } });
		await tick();

		await type('Purple');
		await vi.advanceTimersByTimeAsync(300);
		expect(createRow()).toBeNull();
		expect(input().getAttribute('aria-expanded')).toBe('false');

		release({ results: [] });
		await vi.waitFor(() => expect(createRow()).not.toBeNull());
		expect(input().getAttribute('aria-expanded')).toBe('true');
		vi.useRealTimers();
	});

	it('is keyboard-reachable as the last row, and Enter invokes it with the trimmed query', async () => {
		loadIndex([
			{ id: 'id-1', title: 'Purple Rain', item_number: 1, collection_prefix: 'COLO' },
		]);
		localSearchMock.search.mockReturnValue([{ id: 'id-1', score: 1 }]);
		const oncreate = vi.fn();
		const onselect = vi.fn();
		render(ItemPicker, { props: { ...createProps, oncreate, onselect } });
		await tick();

		await type('  Purple  ');
		await press('ArrowDown');
		expect(options()[0].getAttribute('aria-selected')).toBe('true');
		await press('ArrowDown');
		expect(createRow()!.getAttribute('aria-selected')).toBe('true');
		expect(input().getAttribute('aria-activedescendant')).toBe(createRow()!.id);

		await press('Enter');
		expect(oncreate).toHaveBeenCalledTimes(1);
		expect(oncreate).toHaveBeenCalledWith('Purple');
		// Enter on the create row must not ALSO select whatever row it replaced.
		expect(onselect).not.toHaveBeenCalled();
	});

	it('clicking the create row invokes it', async () => {
		loadIndex([]);
		localSearchMock.search.mockReturnValue([]);
		const oncreate = vi.fn();
		render(ItemPicker, { props: { ...createProps, oncreate } });
		await tick();

		await type('Purple');
		createRow()!.click();

		expect(oncreate).toHaveBeenCalledTimes(1);
		expect(oncreate).toHaveBeenCalledWith('Purple');
	});

	it('withholds create for an exact title the SEARCH RANKING did not return', async () => {
		// codex round 1 P2. `warmSearch` asks `localSearch` for `limit +
		// excluded.size` hits, so the exact match is only in `rawResults` if the
		// RANKER put it there. Resting the no-duplicate guarantee on a relevance
		// score is resting it on the wrong thing: the question "does an item with
		// this exact title exist in this collection" has an authoritative answer
		// in the index itself, and the index is already in RAM.
		//
		// The scenario is the ranker returning a full page of near-misses with
		// the exact row outside the window.
		loadIndex([
			{ id: 'id-1', title: 'Purple Rain', item_number: 1, collection_prefix: 'COLO' },
			{ id: 'id-2', title: 'Purple', item_number: 2, collection_prefix: 'COLO' },
		]);
		// The ranker returns ONLY the near-miss; the exact row is off the window.
		localSearchMock.search.mockReturnValue([{ id: 'id-1', score: 1 }]);
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn(), limit: 1 } });
		await tick();

		await type('Purple');

		expect(options().map((o) => o.textContent)).toHaveLength(1);
		expect(createRow()).toBeNull();
	});

	it('withholds create when the COLD path returns an exact title', async () => {
		// The `rawResults` check is not redundant with the collection scan, and
		// this is the case that proves it: while the index is cold there is no
		// collection to scan, and the server's answer is the only evidence that
		// the row exists. A build relying on the scan alone offers to create a
		// duplicate of a row `/search` just returned.
		setBootstrapState('cold');
		loadIndex([]);
		searchApi.mockResolvedValue({
			results: [{ item: { id: 'id-2', title: 'Purple', item_number: 2, collection_prefix: 'COLO' } }],
		});
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn() } });
		await tick();

		await type('Purple');
		await vi.waitFor(() => expect(options()).toHaveLength(1));
		expect(createRow()).toBeNull();
	});

	it('offers nothing to create when the cold search FAILED', async () => {
		// codex round 3 P2. A failed search and an empty one leave identical
		// state — no rows, not loading — and the result list is right to render
		// both as "No results". The create row is not: an empty answer says no
		// such item exists, a failed one says nothing at all, and offering to
		// create on no evidence is how the duplicate gets minted. Same rule the
		// permission gate follows: no answer must not read as permission.
		setBootstrapState('cold');
		loadIndex([]);
		searchApi.mockRejectedValue(new Error('network'));
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn() } });
		await tick();

		await type('Purple');
		await vi.waitFor(() => expect(document.body.textContent).toContain('No results'));
		expect(createRow()).toBeNull();

		// And it must not latch: the next query gets a fresh verdict.
		searchApi.mockResolvedValue({ results: [] });
		await type('Purple Haze');
		await vi.waitFor(() => expect(createRow()).not.toBeNull());
	});

	it('a cold failure does not outlive hydration — the warm answer supersedes it', async () => {
		// The reachable half of "must not latch". The failure flag is cleared by
		// whatever produces the NEXT verdict, and the warm branch never reaches
		// `coldSearch` to clear it on the way through — so without its own reset
		// a single network blip suppresses the create row for the rest of the
		// session, even once the authoritative in-RAM answer is available.
		setBootstrapState('cold');
		loadIndex([]);
		searchApi.mockRejectedValue(new Error('network'));
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn() } });
		await tick();

		await type('Purple');
		await vi.waitFor(() => expect(document.body.textContent).toContain('No results'));
		expect(createRow()).toBeNull();

		setBootstrapState('ready');
		bumpEpoch();
		await tick();
		await tick();

		expect(createRow()).not.toBeNull();
	});

	it('CONTROL: a cold index falls back to the returned rows and still offers create', async () => {
		// The collection scan needs a hydrated index. While cold there is no
		// authoritative answer to fall back ON, so the behaviour is the
		// rawResults check alone — which is the pre-fix behaviour, and correct
		// here because refusing to offer create while cold would strand the user.
		setBootstrapState('cold');
		loadIndex([{ id: 'id-2', title: 'Purple', item_number: 2, collection_prefix: 'COLO' }]);
		searchApi.mockResolvedValue({ results: [] });
		render(ItemPicker, { props: { ...createProps, oncreate: vi.fn() } });
		await tick();

		await type('Purple');
		await vi.waitFor(() => expect(createRow()).not.toBeNull());
	});

	it('does not fire a second create while the first is still in flight', async () => {
		// The duplicate this guards is not the same-text-twice case the exact
		// match covers — it is one impatient user and two Enters inside a single
		// round trip, when no row exists yet to suppress anything.
		loadIndex([]);
		localSearchMock.search.mockReturnValue([]);
		let release!: () => void;
		const oncreate = vi.fn(() => new Promise<void>((r) => { release = () => r(); }));
		render(ItemPicker, { props: { ...createProps, oncreate } });
		await tick();

		await type('Purple');
		await press('ArrowDown');
		await press('Enter');
		await press('Enter');
		expect(oncreate).toHaveBeenCalledTimes(1);

		release();
		await tick();
	});
});
