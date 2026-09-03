<script lang="ts">
	/**
	 * ItemPicker — the shared search-and-choose-an-item control (PLAN-2857 U3 /
	 * TASK-2862).
	 *
	 * Extracted from `ItemDetail`'s inline add-relationship search so the
	 * relation-field editor (U2) and the Relationships tab share ONE picker
	 * rather than a copy each. `ItemDetail` is its first caller, in unscoped
	 * mode; a relation field passes `collection` to scope it to the field's
	 * declared target.
	 *
	 * WHERE THE CANDIDATES COME FROM, and why there is no threshold.
	 *
	 * `localIndex` is a workspace-wide in-RAM read model of every item
	 * (`/workspaces/{ws}/items-index` takes no limit parameter — see
	 * `api.items.listIndex`), and `localSearch` is a MiniSearch index built
	 * over it. So the candidate rows for ANY collection are already in memory,
	 * already ranked, and cost no network call. PLAN-2857's Q1 was framed as
	 * "plain dropdown under N items, search above it"; that threshold was
	 * pricing a round-trip that does not happen. One control, always
	 * filter-shaped, correct at three items and at three thousand.
	 *
	 * The server `/search` endpoint is the COLD path only — used while the
	 * workspace's local index has not finished hydrating
	 * (`bootstrapStateFor(ws) !== 'ready'`), which is also the pre-extraction
	 * behaviour every caller had. It is not a mode the user can observe or
	 * select.
	 *
	 * DEBOUNCE. Only the cold path is debounced, and that is deliberate: the
	 * debounce exists to keep per-keystroke requests off the rate limiter
	 * (BUG-1367 lineage), and the warm path issues no requests to limit. Local
	 * search is synchronous and sub-millisecond, so debouncing it would only
	 * add latency to the common case.
	 *
	 * FENCES. A per-query `seq` invalidates an in-flight cold search when the
	 * query changes, is emptied, or the picker unmounts — a late response can
	 * never repopulate a box the user has moved on from. The ITEM-SWITCH fence
	 * is structural rather than a second counter: every caller mounts this
	 * inside a `{#key}` on the item identity (PLAN-2105 / TASK-2112), so a
	 * switch destroys the instance and its continuation with it.
	 */
	import { onDestroy, onMount, untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import { localIndex } from '$lib/stores/localIndex.svelte';
	import { localSearch } from '$lib/stores/localSearch.svelte';
	import { formatItemRef, type ItemIndexRow } from '$lib/types';

	interface Props {
		/** Workspace slug the picker searches within. */
		wsSlug: string;
		/**
		 * Target collection slug. Omitted = the whole workspace (the
		 * Relationships tab's mode); set = a relation field's declared target.
		 * When set, an EMPTY query lists the collection's most recently
		 * updated items, so the picker opens with something to choose.
		 */
		collection?: string;
		/** Item ids to hide — the item being edited, anything already linked. */
		excludeIds?: string[];
		/** Max rows rendered. Bounded so DOM cost is O(1) in collection size. */
		limit?: number;
		placeholder?: string;
		/** Accessible name for the input. */
		label?: string;
		/** Focus the input on mount. */
		autofocus?: boolean;
		/** Chosen row. The picker does not clear itself — the caller decides. */
		onselect: (item: ItemIndexRow) => void;
		/**
		 * Escape on an already-empty box.
		 *
		 * Pass one if Escape should close the surface hosting the picker.
		 * Omitting it does NOT hand Escape to the layer above: both pane hosts'
		 * keydown handlers bail on text-entry targets before they reach the
		 * escape stack (`isTextEntryTarget` in `[collection]/+page.svelte` and
		 * `[collection]/[slug]/+page.svelte`), on the deliberate rule that a
		 * text field owns Escape locally. So with no `oncancel`, Escape on an
		 * empty picker does nothing at all — which is what the inline search
		 * this replaces did too.
		 */
		oncancel?: () => void;
	}

	let {
		wsSlug,
		collection,
		excludeIds = [],
		limit = 10,
		placeholder = 'Search items...',
		label = 'Search items',
		autofocus = false,
		onselect,
		oncancel,
	}: Props = $props();

	const COLD_SEARCH_DEBOUNCE_MS = 250;

	let query = $state('');
	let results = $state<ItemIndexRow[]>([]);
	let loading = $state(false);
	/** Index into `results` of the keyboard-highlighted row; -1 = none. */
	let activeIndex = $state(-1);

	/**
	 * Plain `let`, not `$state`, per CONVE-1688: both are read and written
	 * only inside handlers, never rendered. A `$state` that an `$effect` also
	 * read would silently wedge the production effect scheduler.
	 */
	let seq = 0;
	let debounceTimer: ReturnType<typeof setTimeout> | undefined;
	let inputEl = $state<HTMLInputElement>();

	const uid = $props.id();

	let excluded = $derived(new Set(excludeIds));

	function isWarm(): boolean {
		return localIndex.bootstrapStateFor(wsSlug) === 'ready';
	}

	/** Hide excluded rows, then bound the list. Applied to every path. */
	function present(rows: ItemIndexRow[]): ItemIndexRow[] {
		const out: ItemIndexRow[] = [];
		for (const row of rows) {
			if (excluded.has(row.id)) continue;
			out.push(row);
			if (out.length >= limit) break;
		}
		return out;
	}

	/**
	 * Empty-query listing. Scoped pickers show the target collection
	 * most-recently-updated first (`getByCollection` already returns that
	 * order); an unscoped picker shows nothing, because "every item in the
	 * workspace, newest first" is not a useful prompt.
	 */
	function recent(): ItemIndexRow[] {
		if (!collection || !isWarm()) return [];
		return present(localIndex.getByCollection(wsSlug, collection));
	}

	function warmSearch(q: string): ItemIndexRow[] {
		const hits = localSearch.search(wsSlug, q, {
			collection,
			// Over-ask so exclusions can't empty a full page of results.
			limit: limit + excluded.size,
		});
		const rows: ItemIndexRow[] = [];
		for (const hit of hits) {
			const row = localIndex.findByIdOrSlug(wsSlug, hit.id);
			if (row) rows.push(row);
		}
		return present(rows);
	}

	async function coldSearch(q: string, mySeq: number) {
		loading = true;
		try {
			const res = await api.search(q, { workspace: wsSlug, collection });
			if (mySeq !== seq) return;
			results = present((res.results ?? []).map((r) => r.item));
			activeIndex = -1;
		} catch {
			if (mySeq !== seq) return;
			results = [];
			activeIndex = -1;
		} finally {
			if (mySeq === seq) loading = false;
		}
	}

	/**
	 * One entry point for every query change. Invalidates whatever was in
	 * flight FIRST, so no branch below can be repopulated by its predecessor.
	 */
	function runQuery() {
		clearTimeout(debounceTimer);
		const mySeq = ++seq;
		const q = query.trim();

		if (!q) {
			loading = false;
			results = recent();
			activeIndex = -1;
			return;
		}

		if (isWarm()) {
			loading = false;
			results = warmSearch(q);
			activeIndex = -1;
			return;
		}

		results = [];
		activeIndex = -1;
		loading = true;
		debounceTimer = setTimeout(() => coldSearch(q, mySeq), COLD_SEARCH_DEBOUNCE_MS);
	}

	function choose(row: ItemIndexRow) {
		onselect(row);
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'ArrowDown') {
			if (results.length === 0) return;
			e.preventDefault();
			activeIndex = activeIndex < results.length - 1 ? activeIndex + 1 : results.length - 1;
			return;
		}
		if (e.key === 'ArrowUp') {
			if (results.length === 0) return;
			e.preventDefault();
			activeIndex = activeIndex > 0 ? activeIndex - 1 : -1;
			return;
		}
		if (e.key === 'Enter') {
			if (activeIndex < 0 || activeIndex >= results.length) return;
			e.preventDefault();
			choose(results[activeIndex]);
			return;
		}
		if (e.key === 'Escape') {
			// Consume ONLY what this picker actually closes.
			//
			// The stopPropagation is belt-and-braces rather than load-bearing:
			// the page's Escape driver is a bubble-phase
			// `<svelte:window onkeydown>`, so it WOULD see the key — but it
			// returns early for text-entry targets before running the escape
			// stack, and this is a text input. Keeping it means the picker's
			// behaviour does not depend on that bail staying in place, and
			// declining still leaves the key untouched for anything that does
			// look at it.
			if (query) {
				e.preventDefault();
				e.stopPropagation();
				query = '';
				runQuery();
				return;
			}
			if (oncancel) {
				e.preventDefault();
				e.stopPropagation();
				oncancel();
			}
		}
	}

	onDestroy(() => {
		// `clearTimeout` is the part that matters and the part that is testable:
		// closing the picker inside the debounce window means the request is
		// never sent at all.
		//
		// The `seq` bump is belt-and-braces for a request already in flight, and
		// deliberately kept even though NO test can kill its removal — a
		// destroyed instance renders nothing, so a late write to its state is
		// unobservable by construction. Said plainly here rather than defended by
		// a test that would pass either way.
		seq++;
		clearTimeout(debounceTimer);
	});

	// Open a scoped picker with its recent list already populated, and focus the
	// input when the caller asked for it.
	//
	// `onMount`, deliberately, not `$effect`: `runQuery` reads `query` and
	// writes `results`, so as an effect it would re-run on every keystroke in
	// addition to `oninput` — two dispatches per character, each bumping `seq`
	// and cancelling the other's cold search. CONVE-1688's neighbourhood; the
	// lifecycle hook says "once, at open" without relying on that reasoning
	// holding as the body grows.
	onMount(() => {
		runQuery();
		if (autofocus) inputEl?.focus();
	});

	// Keep an OPEN picker current with the local index.
	//
	// Two ways it went stale, both found by codex review. A picker can open
	// BEFORE the workspace index has hydrated, and a scoped one then has nothing
	// to list — `recent()` returns [] while cold, and nothing re-ran it (round 1
	// P2). And once open it holds a COPY of the rows, so an SSE delta landing
	// while it is on screen left it showing what the workspace used to contain
	// (round 2 P2). Both are the same missing dependency.
	//
	// Tracked reads are the two signals that say the index changed: the
	// bootstrap state, and the workspace cursor (`$state` on WorkspaceState,
	// bumped by every applied delta batch). Everything else is read inside
	// `untrack` — reading `query` or `results` reactively would re-run this on
	// every keystroke, racing `oninput` and bumping `seq` twice per character,
	// which is also why `onMount` rather than an effect owns the first run
	// (CONVE-1688's neighbourhood).
	//
	// The re-list PRESERVES the highlighted row by id rather than skipping when
	// the user has typed. Recomputing the index is what makes the refresh safe:
	// re-running blind would take back a row the user had arrowed down to, at a
	// moment they cannot predict — a keystroke they never made. If the row is
	// gone from the new results, the highlight clears rather than silently
	// pointing at whatever slid into that position.
	$effect(() => {
		const state = localIndex.bootstrapStateFor(wsSlug);
		// Read for its dependency; the value itself carries no meaning here.
		localIndex.cursorFor(wsSlug);
		untrack(() => {
			if (state !== 'ready') return;
			const keep = activeIndex >= 0 ? results[activeIndex]?.id : undefined;
			runQuery();
			activeIndex = keep ? results.findIndex((r) => r.id === keep) : -1;
		});
	});
</script>

<div class="item-picker">
	<input
		type="text"
		class="picker-input"
		role="combobox"
		aria-label={label}
		aria-expanded={results.length > 0}
		aria-controls="picker-results-{uid}"
		aria-autocomplete="list"
		aria-activedescendant={activeIndex >= 0 ? `picker-option-${uid}-${activeIndex}` : undefined}
		{placeholder}
		bind:this={inputEl}
		bind:value={query}
		oninput={runQuery}
		onkeydown={onKeydown}
	/>

	{#if loading}
		<div class="picker-status">Searching...</div>
	{:else if results.length > 0}
		<div class="picker-results" role="listbox" id="picker-results-{uid}" aria-label={label}>
			{#each results as result, i (result.id)}
				<button
					type="button"
					class="picker-result"
					class:active={i === activeIndex}
					role="option"
					id="picker-option-{uid}-{i}"
					aria-selected={i === activeIndex}
					onclick={() => choose(result)}
				>
					{#if formatItemRef(result)}
						<span class="picker-ref">{formatItemRef(result)}</span>
					{/if}
					<span class="picker-title">{result.title}</span>
				</button>
			{/each}
		</div>
	{:else if query.trim().length > 0}
		<div class="picker-status">No results</div>
	{/if}
</div>

<style>
	/*
	 * Self-contained by necessity, not by preference: Svelte scopes styles per
	 * component, so a shared class NAME would silently render unstyled in every
	 * host (the note on ItemDetail's `.timeline-*` copy records that lesson).
	 *
	 * Sized in `em` throughout so a host sets the picker's scale once on its own
	 * wrapper — ItemDetail's relationships form wants 0.8rem, the properties
	 * panel (U2) wants its own — and every part scales together.
	 */
	.item-picker {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		min-width: 0;
	}

	.picker-input {
		width: 100%;
		padding: var(--space-1) var(--space-2);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-primary);
		color: var(--text-primary);
		font-size: 1em;
		font-family: inherit;
	}

	.picker-status {
		padding: var(--space-2);
		color: var(--text-muted);
		font-size: 1em;
	}

	.picker-results {
		display: flex;
		flex-direction: column;
		gap: 1px;
		max-height: 200px;
		overflow-y: auto;
	}

	.picker-result {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2);
		background: var(--bg-primary);
		border: none;
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		text-align: left;
		cursor: pointer;
		font-size: 1em;
		font-family: inherit;
		min-width: 0;
	}

	.picker-result:hover,
	.picker-result.active {
		background: var(--bg-hover);
	}

	.picker-ref {
		flex-shrink: 0;
		color: var(--text-muted);
		font-family: var(--font-mono);
		font-size: 0.94em;
	}

	.picker-title {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
</style>
