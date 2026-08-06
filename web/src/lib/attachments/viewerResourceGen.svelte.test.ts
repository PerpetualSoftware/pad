import { describe, it, expect, afterEach } from 'vitest';
import { flushSync } from 'svelte';
import { createViewerResourceGen } from './viewerResource.svelte';

/**
 * The generation EFFECT (TASK-2428) — the production one `ItemDetail` calls,
 * not a copy of it.
 *
 * Two things are under test, and the second is why this file exists at all:
 *
 *  1. Which transitions advance the counter. Driven through the same inputs
 *     `ItemDetail` supplies (`loadedItemWsSlug`, `item?.id`, `itemMatchesRef`),
 *     replaying the sequences a real session produces — a route alias, a
 *     schema-edit reload, an A→B switch through the empty mid-switch boundary,
 *     a cross-workspace nav that keeps the ref.
 *
 *  2. That the effect keeps the FLUSH alive. An `$effect` that reads a `$state`
 *     it also writes in its tracked scope aborts the flush and strands
 *     unrelated reactivity in the same batch, reporting nothing in a production
 *     build (CONVE-1688). A test that only checks the counter's VALUE passes
 *     through that, because the counter is what the aborted effect already
 *     wrote. So every case below runs beside an independent probe effect that
 *     must keep observing — the neighbour is the assertion.
 */

// One reactive input object, the shape ItemDetail's reader closes over.
const input = $state({ workspaceSlug: '', itemId: null as string | null, loaded: false });

interface Harness {
	gen: () => number;
	/** Values the neighbouring effect observed, in order. */
	probeSeen: number[];
	/** How many times the neighbour re-ran. */
	probeRuns: () => number;
	stop: () => void;
}

let running: Harness | null = null;

function start(): Harness {
	const probeSeen: number[] = [];
	let probeRuns = 0;
	let read!: () => number;
	const stop = $effect.root(() => {
		const resource = createViewerResourceGen(() => ({
			workspaceSlug: input.workspaceSlug,
			itemId: input.itemId,
			loaded: input.loaded,
		}));
		read = () => resource.current;
		// The neighbour. It depends on the counter, so a stranded flush shows up
		// as a value it never sees — and it is a SEPARATE effect, so an aborted
		// batch takes it down with the one that aborted.
		$effect(() => {
			probeSeen.push(resource.current);
			probeRuns += 1;
		});
	});
	flushSync();
	running = { gen: () => read(), probeSeen, probeRuns: () => probeRuns, stop };
	return running;
}

/** Applies one observable state of the loaded item, then settles. */
function set(next: Partial<typeof input>) {
	Object.assign(input, next);
	flushSync();
}

/** The mid-switch boundary: `itemMatchesRef` false, item not yet adopted. */
const MID_SWITCH = { loaded: false };

afterEach(() => {
	running?.stop();
	running = null;
	Object.assign(input, { workspaceSlug: '', itemId: null, loaded: false });
});

describe('createViewerResourceGen', () => {
	it('advances once when the first item loads, and the neighbour sees it', () => {
		const h = start();
		expect(h.gen()).toBe(0);

		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: true });
		expect(h.gen()).toBe(1);
		// Not just the value: the downstream effect actually re-ran with it.
		expect(h.probeSeen).toEqual([0, 1]);
	});

	it('does NOT advance on a collection-only or username-only route alias', () => {
		// `/dave/ws/tasks/TASK-1` → `/dave/ws/bugs/TASK-1` (or a different
		// username for the same workspace). The route change triggers a
		// `loadData()`, which re-adopts the SAME item under the SAME workspace,
		// so every input this effect reads is re-asserted unchanged. That is
		// the whole reason the route is not one of its inputs.
		const h = start();
		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: true });
		const runsBefore = h.probeRuns();

		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: true });
		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: true });

		expect(h.gen()).toBe(1);
		// The neighbour did not re-run either: nothing downstream was disturbed
		// by a refresh that changed nothing.
		expect(h.probeRuns()).toBe(runsBefore);
	});

	it('does NOT advance on the reload that follows a collection schema edit', () => {
		// `loadData()` flips `loading` and re-adopts the same item. On this
		// path `itemMatchesRef` never goes false (the item is not nulled), so
		// the inputs never leave the loaded state.
		const h = start();
		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: true });
		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: true });

		expect(h.gen()).toBe(1);
		expect(h.probeSeen).toEqual([0, 1]);
	});

	it('counts an A→B switch ONCE, through the empty mid-switch boundary', () => {
		const h = start();
		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: true });

		set(MID_SWITCH);
		expect(h.gen()).toBe(1); // the gap is not a resource
		set({ workspaceSlug: 'ws', itemId: 'item-b', loaded: true });

		expect(h.gen()).toBe(2);
		expect(h.probeSeen).toEqual([0, 1, 2]);
	});

	it('counts a boundary flap that lands back on the same item as NOTHING', () => {
		const h = start();
		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: true });
		const runsBefore = h.probeRuns();

		set(MID_SWITCH);
		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: true });

		expect(h.gen()).toBe(1);
		expect(h.probeRuns()).toBe(runsBefore);
	});

	it('advances on a same-ref DIFFERENT-WORKSPACE change', () => {
		// A reused pane navigating ws1→ws2 while carrying `?item=<ref>`, where
		// both workspaces own that ref (IDEA-2135). The plain item id would
		// miss it if the two happened to resolve to the same id; the workspace
		// is part of the identity precisely so it cannot.
		const h = start();
		set({ workspaceSlug: 'ws1', itemId: 'item-a', loaded: true });
		set({ workspaceSlug: 'ws2', itemId: 'item-a', loaded: true });

		expect(h.gen()).toBe(2);
		expect(h.probeSeen).toEqual([0, 1, 2]);
	});

	it('counts A→B→A as three resources, keeping the neighbour live throughout', () => {
		// Rapid j/k paging. Coming back to A is a change too: whatever was open
		// belonged to B. This is also the sequence a self-invalidating effect
		// survives longest, because its first write is a no-op.
		const h = start();
		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: true });
		set(MID_SWITCH);
		set({ workspaceSlug: 'ws', itemId: 'item-b', loaded: true });
		set(MID_SWITCH);
		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: true });

		expect(h.gen()).toBe(3);
		expect(h.probeSeen).toEqual([0, 1, 2, 3]);
		// Still reacting after five transitions — the flush was never aborted.
		set(MID_SWITCH);
		set({ workspaceSlug: 'ws', itemId: 'item-c', loaded: true });
		expect(h.probeSeen).toEqual([0, 1, 2, 3, 4]);
	});

	it('never advances while nothing is loaded', () => {
		const h = start();
		set({ workspaceSlug: 'ws', itemId: 'item-a', loaded: false });
		set({ workspaceSlug: 'ws', itemId: null, loaded: false });

		expect(h.gen()).toBe(0);
		expect(h.probeSeen).toEqual([0]);
	});
});
