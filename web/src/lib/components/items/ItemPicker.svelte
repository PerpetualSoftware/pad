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
		/**
		 * Where matches come from. This is a MODEL choice, not a performance
		 * one, and the two callers want different models:
		 *
		 *   `'index'` (default) — the in-RAM workspace index, ranked by
		 *   `localSearch` over title / ref / tags / parent / field values. No
		 *   network call. Falls back to the server only while the index has not
		 *   hydrated. Right for a RELATION field, where you are choosing a row
		 *   from a known collection and know what it is called.
		 *
		 *   `'server'` — always `/search`, whose FTS also indexes item BODY
		 *   CONTENT. `localIndex` strips `content` by design, so the index can
		 *   never answer "the item that mentioned that phrase". Right for the
		 *   Relationships tab, where you are finding an item you remember
		 *   rather than one you can name — and what it did before this
		 *   component existed.
		 */
		source?: 'index' | 'server';
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
		 * Inline create (PLAN-2857 U8). Passing this adds a trailing
		 * "Create \"<query>\" in <collection>" row; omitting it is how a caller
		 * declines the affordance, and the ONLY way to decline it.
		 *
		 * That opt-in shape is deliberate. Both rules in U8's scope are the
		 * caller's to know and neither is this component's:
		 *
		 *   * *Relation fields only.* The Relationships tab links items that
		 *     already exist, so it passes nothing and renders exactly as before.
		 *   * *Permission.* "Can this user create in the TARGET collection" is
		 *     the collection-level `canEditCollection` cascade — the same
		 *     predicate behind "+ New" — which lives in the workspace store. A
		 *     picker that consulted it here would be a second copy of an answer
		 *     the server also enforces.
		 *
		 * Awaited, and re-entrant calls are dropped while one is in flight, so
		 * two Enters inside one round trip cannot mint two items. Rejections are
		 * swallowed HERE and belong to the caller: it owns the API call, so it
		 * owns the error surface (a toast) and the decision to leave the query in
		 * place for a retry.
		 */
		oncreate?: (title: string) => void | Promise<void>;
		/**
		 * Display name of the target collection for the create row. Falls back to
		 * the slug, which is the wrong register ("in colors" vs "in Colors") but
		 * never wrong about WHERE the item would land — the fact the row exists
		 * to convey.
		 */
		createLabel?: string;
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
		source = 'index',
		limit = 10,
		placeholder = 'Search items...',
		label = 'Search items',
		autofocus = false,
		onselect,
		oncreate,
		createLabel,
		oncancel,
	}: Props = $props();

	const COLD_SEARCH_DEBOUNCE_MS = 250;

	let query = $state('');
	/**
	 * What the source last returned, BEFORE exclusions. Kept separate so
	 * `results` can re-filter reactively when `excludeIds` changes — which it
	 * does late, since `ItemDetail` loads `itemLinks` asynchronously (codex
	 * round 4 P2). Deriving that filter is what lets the server-backed caller
	 * honour a late exclusion without re-issuing a request per change.
	 */
	let rawResults = $state<ItemIndexRow[]>([]);
	let loading = $state(false);
	/**
	 * A cold search ANSWERED, and `rawResults` is that answer.
	 *
	 * Stated positively on purpose (codex rounds 3 and 5). The negative form
	 * — "the last search failed" — was false in three different states that
	 * are not answers at all: before the first request, after a failure, and
	 * after `localIndex.reset()` drops everything on a sign-out or 403 purge.
	 * Each left the flag reading "fine" and put a create row on screen backed
	 * by no evidence. A flag that must be cleared everywhere is a flag that
	 * will be missed somewhere; this one is set in exactly one place, by the
	 * event that earns it.
	 *
	 * Only the COLD path needs it. A settled warm index is authoritative by
	 * itself, and `indexCanProveAbsence` is what asks that question.
	 */
	let coldAnswered = $state(false);
	/**
	 * The highlighted row's ID, not its index. Identity survives the list
	 * changing under it — a delta landing, a late exclusion arriving — where an
	 * index silently moves the highlight onto whatever slid into that position.
	 * `activeIndex` is derived from it, so nothing has to remember to re-resolve
	 * it at each site that can change the list.
	 */
	let activeId = $state<string | null>(null);

	/**
	 * Plain `let`, not `$state`, per CONVE-1688: both are read and written
	 * only inside handlers, never rendered. A `$state` that an `$effect` also
	 * read would silently wedge the production effect scheduler.
	 */
	let seq = 0;
	let debounceTimer: ReturnType<typeof setTimeout> | undefined;
	let inputEl = $state<HTMLInputElement>();

	const uid = $props.id();

	/**
	 * Identifier for the create row in the same namespace as the result rows'
	 * ids, so ONE `activeId` addresses either. A NUL-prefixed sentinel because
	 * item ids are UUIDs and can never collide with it.
	 */
	const CREATE_OPTION_ID = '\u0000create';

	type PickerOption =
		| { kind: 'item'; id: string; row: ItemIndexRow }
		| { kind: 'create'; id: string };

	let excluded = $derived(new Set(excludeIds));
	let results = $derived(present(rawResults));

	/**
	 * Whether to offer the create row, for the query as it stands.
	 *
	 * "Matches nothing, or nothing EXACTLY" — and the exact-title question is
	 * asked of the INDEX, not of the ranking.
	 *
	 * `rawResults` is checked first because it is free and is the only answer
	 * available while the index is cold. But it is a RANKED, WINDOWED answer:
	 * `warmSearch` asks for `limit + excluded.size` hits, so an exact row that
	 * the ranker placed outside that window is simply absent, and resting the
	 * no-duplicate guarantee on a relevance score rests it on the wrong thing
	 * (codex round 1 P2). "Does an item with this exact title exist in this
	 * collection" has an authoritative answer in `localIndex`, already in RAM,
	 * so the warm path asks it directly. `getByCollection` excludes
	 * soft-deleted rows, which is right: a deleted "Purple" should not stop the
	 * user minting a live one.
	 *
	 * This suppression IS the no-duplicate half of U8's proving test. There is
	 * no create-time uniqueness check anywhere below, and deliberately so: the
	 * second invocation with the same text never reaches a create, because by
	 * then the row exists and this returns false. What makes that true on the
	 * next keystroke is the caller upserting the new item into `localIndex`.
	 *
	 * Not offered while `loading`: mid-flight, "nothing matched" is not yet
	 * known, and a create row there invites a duplicate of a row about to land.
	 */
	let createTitle = $derived(query.trim());
	function titleIs(row: ItemIndexRow, wanted: string): boolean {
		return (row.title ?? '').trim().toLowerCase() === wanted;
	}
	let showCreate = $derived.by((): boolean => {
		// No `loading` term. It and the per-query `coldAnswered` reset below are
		// a redundant PAIR — either alone suppresses the row for the whole
		// in-flight window, and a mutant removing either one survived while the
		// other stood. That is not defence in depth, it is one guard and one
		// line that looks like a guard. `coldAnswered` is the one kept, because
		// it states the actual rule (something authoritative has answered FOR
		// THIS QUERY) where `loading` is a UI state that merely correlates with
		// it, and only the warm path can be settled while nothing is loading.
		if (!oncreate || !collection || !createTitle) return false;
		const wanted = createTitle.toLowerCase();
		if (rawResults.some((r) => titleIs(r, wanted))) return false;
		// Offer only where SOMETHING authoritative has answered "no such item",
		// which is the same rule the permission gate and `coldFailed` follow: no
		// evidence must not read as permission.
		//
		//   * COLD — offer only once `/search` has actually ANSWERED. The server
		//     is authoritative and its empty answer is real evidence; not having
		//     asked yet, a failed request, and a workspace whose state was just
		//     dropped are all silence, and silence is not evidence. (Refusing
		//     outright would strand every user whose index has not hydrated,
		//     which is why this waits for the answer rather than the index.)
		//   * READY, settled — the in-RAM collection is authoritative; scan it.
		//   * READY, resyncing — the rows are a cache snapshot that delta-sync
		//     has not reconciled, and `rawResults` came from THAT, so nothing in
		//     reach can support the inference. Withhold until it settles; the
		//     window is seconds and a duplicate outlives it.
		if (!isWarm()) return coldAnswered;
		if (!indexCanProveAbsence()) return false;
		return !localIndex.getByCollection(wsSlug, collection).some((r) => titleIs(r, wanted));
	});

	/**
	 * Result rows plus the create row, in render AND keyboard order — one list,
	 * so arrowing onto the create row needs no special case and cannot fall out
	 * of step with what is on screen.
	 */
	let options = $derived.by((): PickerOption[] => {
		const out: PickerOption[] = results.map((row) => ({ kind: 'item', id: row.id, row }));
		if (showCreate) out.push({ kind: 'create', id: CREATE_OPTION_ID });
		return out;
	});
	let activeIndex = $derived(activeId ? options.findIndex((o) => o.id === activeId) : -1);

	function isWarm(): boolean {
		return localIndex.bootstrapStateFor(wsSlug) === 'ready';
	}

	/**
	 * Is the local index a source we may reason about ABSENCE from?
	 *
	 * `ready` is not enough (codex round 4). It coexists with `pendingResync`:
	 * `localIndex` hydrates from the IDB cache and serves those rows while
	 * delta-sync catches up, so during that window a row that EXISTS can be
	 * missing from the snapshot. Presence in the index is still evidence — the
	 * row was real when it was cached — but absence is not, and absence is
	 * exactly what the create row is derived from.
	 *
	 * Only `showCreate` asks this. Search and listing deliberately keep using
	 * `isWarm`: showing cached rows during a resync is right, and it is only
	 * the inference "therefore no such item exists" that the cache cannot bear.
	 */
	function indexCanProveAbsence(): boolean {
		return isWarm() && !localIndex.pendingResyncFor(wsSlug);
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
		// Deliberately independent of `source`: an empty-query LISTING is not a
		// search, `/search` cannot answer one (it requires a `q`), and the index
		// holds the rows either way. A server-backed picker therefore still opens
		// with its collection listed, and only its QUERIES go to the server.
		if (!collection || !isWarm()) return [];
		return localIndex.getByCollection(wsSlug, collection);
	}

	function warmSearch(q: string): ItemIndexRow[] {
		const hits = localSearch.search(wsSlug, q, {
			collection,
			// Over-ask so exclusions can't empty a full page of results. Sized
			// against the exclusion set the caller has RIGHT NOW; a later one is
			// handled by `results` re-filtering, which can only shrink the list.
			limit: limit + excluded.size,
		});
		const rows: ItemIndexRow[] = [];
		for (const hit of hits) {
			const row = localIndex.findByIdOrSlug(wsSlug, hit.id);
			if (row) rows.push(row);
		}
		return rows;
	}

	async function coldSearch(q: string, mySeq: number) {
		loading = true;
		try {
			const res = await api.search(q, { workspace: wsSlug, collection });
			if (mySeq !== seq) return;
			const rows = (res.results ?? []).map((r) => r.item);
			rawResults = rows;
			// A TRUNCATED page is not an answer to "does this exact title
			// exist" — the row could be on a page we never fetched (codex round
			// 7). Same defect as trusting the local ranker's window, arriving
			// from the server side.
			//
			// Completeness is read from the PAGE LENGTH, not from `total`
			// (codex round 8). `total` cannot answer it: when the count query
			// fails, `store.search` sets total = -1, floors it to 0, and then
			// floors it again to `len(results)` — "Ensure total is never less
			// than actual results", search.go:604-608 — so a failed count is
			// indistinguishable on the wire from an exact-fit page. A page
			// SHORTER than the limit the server echoes back is proof there is no
			// next page, and it holds whatever the count did. A full page is not
			// proof either way, so it does not count as an answer.
			const pageLimit = res.limit ?? 0;
			coldAnswered = pageLimit > 0 && rows.length < pageLimit;
			activeId = null;
		} catch {
			if (mySeq !== seq) return;
			rawResults = [];
			coldAnswered = false;
			activeId = null;
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
			rawResults = recent();
			activeId = null;
			return;
		}

		// `source: 'server'` skips the warm branch entirely — not as a fallback
		// but as the caller's declared model, since only the server's FTS can
		// match item body content.
		if (source === 'index' && isWarm()) {
			loading = false;
			rawResults = warmSearch(q);
			activeId = null;
			return;
		}

		rawResults = [];
		// The previous answer described the PREVIOUS query. This is the line
		// that makes `coldAnswered` mean "answered for what is in the box now",
		// and with the `loading` term gone it is load-bearing on its own: drop
		// it and query B offers a create row on the strength of query A's
		// answer, while B is still in flight.
		coldAnswered = false;
		activeId = null;
		loading = true;
		debounceTimer = setTimeout(() => coldSearch(q, mySeq), COLD_SEARCH_DEBOUNCE_MS);
	}

	function choose(row: ItemIndexRow) {
		onselect(row);
	}

	/**
	 * Plain `let` for the same reason `seq` is (CONVE-1688): written in a
	 * handler, read in a handler, never rendered. Making it `$state` would put a
	 * write inside the effect graph for a value nothing displays.
	 */
	let creating = false;

	async function invokeCreate() {
		if (creating || !oncreate) return;
		const title = createTitle;
		if (!title) return;
		creating = true;
		try {
			await oncreate(title);
		} catch {
			// The caller owns the error surface — see the `oncreate` prop doc.
			// Swallowed rather than rethrown so an unhandled rejection cannot
			// escape a keydown handler.
		} finally {
			// A write to a destroyed instance is a no-op, so this needs no fence.
			creating = false;
		}
	}

	function activate(option: PickerOption) {
		if (option.kind === 'create') {
			void invokeCreate();
			return;
		}
		choose(option.row);
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'ArrowDown') {
			if (options.length === 0) return;
			e.preventDefault();
			const next = activeIndex < options.length - 1 ? activeIndex + 1 : options.length - 1;
			activeId = options[next]?.id ?? null;
			return;
		}
		if (e.key === 'ArrowUp') {
			if (options.length === 0) return;
			e.preventDefault();
			const next = activeIndex - 1;
			activeId = next >= 0 ? (options[next]?.id ?? null) : null;
			return;
		}
		if (e.key === 'Enter') {
			if (activeIndex < 0 || activeIndex >= options.length) return;
			e.preventDefault();
			activate(options[activeIndex]);
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
	// Four ways it went stale, all found by codex review, all the same missing
	// dependency. A picker can open BEFORE the workspace index has hydrated, and
	// a scoped one then has nothing to list — `recent()` returns [] while cold,
	// and nothing re-ran it (round 1). Once open it holds a COPY of the rows, so
	// a delta landing while it is on screen left it showing what the workspace
	// used to contain (round 2). The first fix for that tracked the wrong
	// signal, missing every mutation that does not advance the cursor (round 3).
	// And `excludeIds` arriving late — `ItemDetail` loads `itemLinks`
	// asynchronously, so a picker opened before that resolves was offering items
	// that are already linked (round 4).
	//
	// Tracked reads are the two signals that say the INDEX changed: the bootstrap
	// state and `localSearch.epoch(ws)`. The exclusion set is not one of them —
	// `results` derives through `present()`, so a late `excludeIds` re-filters on
	// its own, without a re-query and therefore without a request on the
	// server-backed caller.
	//
	// The epoch, NOT the workspace cursor. Round 2 used the cursor and round 3
	// caught that it misses every cursorless mutation — `localIndex.upsert()` /
	// `remove()` (optimistic creates, edits, the 403 purge) update the rows and
	// mirror to `localSearch` without advancing it. The epoch is bumped by every
	// `localSearch` write, and EVERY `localIndex` path that touches `state.items`
	// mirrors there — `applyDelta`, `upsert`, `remove`, `removeByCollection`,
	// `applyRetag`, `reset` — so it strictly dominates the cursor as a
	// "something changed" signal. It exists for exactly this consumer shape:
	// its own doc comment describes an `$effect` re-deriving search results,
	// added for the identical staleness bug in TASK-1364.
	//
	// Everything else is read inside `untrack` — reading `query` or `results`
	// reactively would re-run this on every keystroke, racing `oninput` and
	// bumping `seq` twice per character, which is also why `onMount` rather than
	// an effect owns the first run (CONVE-1688's neighbourhood).
	//
	// The re-list PRESERVES the highlighted row by id rather than skipping when
	// the user has typed. Recomputing the index is what makes the refresh safe:
	// re-running blind would take back a row the user had arrowed down to, at a
	// moment they cannot predict — a keystroke they never made. If the row is
	// gone from the new results, the highlight clears rather than silently
	// pointing at whatever slid into that position.
	$effect(() => {
		const state = localIndex.bootstrapStateFor(wsSlug);
		// Read for its dependency; the value carries no meaning here.
		localSearch.epoch(wsSlug);
		// TRACKED (codex round 10): the scope itself. Every other read below is
		// untracked to keep this off the keystroke path, and `collection` was
		// swept up in that — but it is not a per-keystroke value, it is the
		// question the results answer. `ItemDetail` can change a relation
		// field's declared target (a schema edit, or an SSE-driven refresh)
		// without remounting this picker, and the rows then on screen belong to
		// the collection it USED to point at while remaining selectable under
		// the new one. Predates U8 (it arrived with the extraction in
		// TASK-2862); fixed here because U8 makes `collection` load-bearing —
		// it is now the destination an inline create writes to.
		void collection;
		untrack(() => {
			if (state !== 'ready') {
				// The workspace's state was DROPPED — `localIndex.reset()` on
				// sign-out, a 403 membership purge, or a workspace deletion. It
				// bumps the search epoch on its way out, so this effect runs, and
				// leaving early would strand rows the viewer may no longer be
				// allowed to see: still listed, still selectable, and a late cold
				// response still able to add more (codex round 4 P1).
				//
				// Unconditional, after measuring that a guard on "actually showing
				// or awaiting something" was dead: on the ordinary cold mount this
				// runs once with nothing to clear, and every other non-ready run
				// reaches it with state worth dropping either way. A mutant that
				// removed the guard could not be killed, so the guard went instead
				// of acquiring a comment claiming it protects something.
				//
				// `seq++` is what stops an in-flight response landing after the
				// reset; `clearTimeout` stops one that has not been sent yet.
				seq++;
				clearTimeout(debounceTimer);
				rawResults = [];
				// The rows this answer described were just dropped, so it
				// describes nothing (codex round 5). Without this the picker
				// offers to create over a purged workspace.
				coldAnswered = false;
				activeId = null;
				loading = false;
				return;
			}
			// Server-sourced QUERIES are not re-issued on an index change: the
			// index is not their source of truth, and a request per delta is
			// exactly the rate-limiter pressure the debounce exists to avoid. The
			// empty-query LISTING still comes from the index, so it does refresh.
			if (source === 'server' && query.trim()) return;
			const keep = activeId;
			runQuery();
			activeId = keep;
		});
	});
</script>

<div class="item-picker">
	<input
		type="text"
		class="picker-input"
		role="combobox"
		aria-label={label}
		aria-expanded={options.length > 0}
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
	{:else if options.length > 0}
		<div class="picker-results" role="listbox" id="picker-results-{uid}" aria-label={label}>
			{#each options as option, i (option.id)}
				{#if option.kind === 'create'}
					<button
						type="button"
						class="picker-result picker-create"
						class:active={i === activeIndex}
						role="option"
						id="picker-option-{uid}-{i}"
						aria-selected={i === activeIndex}
						onclick={() => void invokeCreate()}
					>
						<span class="picker-create-mark" aria-hidden="true">+</span>
						<span class="picker-title">
							Create "{createTitle}" in {createLabel ?? collection}
						</span>
					</button>
				{:else}
					<button
						type="button"
						class="picker-result"
						class:active={i === activeIndex}
						role="option"
						id="picker-option-{uid}-{i}"
						aria-selected={i === activeIndex}
						onclick={() => choose(option.row)}
					>
						{#if formatItemRef(option.row)}
							<span class="picker-ref">{formatItemRef(option.row)}</span>
						{/if}
						<span class="picker-title">{option.row.title}</span>
					</button>
				{/if}
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

	/*
	 * The create row reads as an action, not as another item — it is the one
	 * row that mints something. Muted until it is the active/hovered row, which
	 * `.picker-result`'s own rule already handles.
	 */
	.picker-create {
		color: var(--text-secondary, var(--text-muted));
	}

	.picker-create-mark {
		flex-shrink: 0;
		color: var(--text-muted);
		font-family: var(--font-mono);
		font-size: 0.94em;
	}
</style>
