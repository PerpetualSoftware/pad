<script lang="ts">
	import { page } from '$app/state';
	import { goto, afterNavigate } from '$app/navigation';
	import { browser } from '$app/environment';
	import { onDestroy } from 'svelte';
	import ItemDetail from '$lib/components/items/ItemDetail.svelte';
	import PaneHost from '$lib/components/collections/PaneHost.svelte';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import { createPaneController } from '$lib/collections/paneHostController';
	import { createPaneMintSettle, PANE_MINT_SETTLE_MS } from '$lib/collections/paneMintSettle';
	import { resolvePaneTarget } from '$lib/collections/paneTarget';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import { runTopEscape, topEscapePriority, ESCAPE_PRIORITY } from '$lib/stores/escapeStack';
	import type { PaneTarget, ResolvedItemIdentity } from '$lib/types';

	// Full-page pane HOST (PLAN-2154 Phase 2 / Architecture E, bullet 5 /
	// TASK-2174 — the Q1 payoff). This route was a 47-line wrapper that rendered
	// the item body full-page via the shared, embeddable <ItemDetail>; it now
	// mounts the SAME right-docked detail pane the collection page carries beside
	// the master, so clicking a child / related / wiki-linked item inside the
	// full-page item opens a navigable mini-browser pane WITHOUT leaving the
	// master.
	//
	// The MASTER ref is the `[slug]` PATH param; the pane ref is the `?item=`
	// QUERY param — no collision (the two live in different parts of the URL).
	// The forbidden case (`?item=` resolving to the MASTER itself — two collab
	// providers on one room sharing the itemID-only sessionStorage cursor) is
	// guarded on BOTH the open/drill paths AND stripped on cold load once the
	// master's identity resolves (`isMaster` + the strip effect below).
	//
	// This host MIRRORS the collection page's pane wiring (same `PaneHost`, same
	// `createPaneController`, same `paneMint` settle, same depth-aware ESC via the
	// shared escape stack) but DROPS everything collection-specific: there is no
	// list, so no j/k pane-follow, no `focusedItemId` highlight, and no list-row
	// focus-return — the controller's `cancelFollow` / `captureReturnFocus` /
	// `setBypassNavGuard` deps are correctly no-ops here.

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	let collSlug = $derived(page.params.collection ?? '');
	let ref = $derived(page.params.slug ?? '');

	// The pane's single URL truth — the `?item=` query param (the master is the
	// `[slug]` path param, so the two never collide). Mounts the pane below via
	// `{#if openItemRef}`; the actual <ItemDetail> `ref` keys off the coalesced
	// `paneMintForRoute` (TASK-2166), never this raw value.
	let openItemRef = $derived(page.url.searchParams.get('item'));

	// Threaded up from the MASTER ItemDetail: true once its loaded item matches
	// the URL ref. Parks the snapshot scroll restore until the master's content
	// is the URL's content (matching the pre-extraction `ready()` predicate).
	let scrollReady = $state(false);

	// The MASTER's resolved {id, ref, slug} identity (TASK-2173), captured via
	// `onIdentity`. Drives the `?item == master` guard so a slug-path master and
	// a ref-query `?item=` (or vice versa) that alias the SAME item are caught.
	// `null` until the master's loaded item matches its URL ref.
	let masterIdentity = $state<ResolvedItemIdentity | null>(null);

	// Scroll restoration parks on the master's OWN `.item-page` overflow column
	// (not the layout's `.main-content`), which is where the master content
	// scrolls once the flex-row host is in place — pane open or not (PLAN-2154
	// Architecture E / TASK-2171's `scrollTarget` getter). `.item-page` is the
	// FIRST (ancestor) `.item-page` in document order, so `querySelector` resolves
	// to this host's scroll column, not <ItemDetail>'s inner content wrapper of
	// the same (Svelte-scoped, separately-styled) class name.
	const scrollRestoration = createScrollRestoration({
		ready: () => scrollReady,
		persistKey: () =>
			wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null,
		scrollTarget: () =>
			browser ? document.querySelector<HTMLElement>('.item-page') : null,
	});
	export const snapshot = scrollRestoration.snapshot;

	// ── Provider-mint settle (PLAN-2154 Architecture D / TASK-2166) ────────
	// Replicated verbatim from the collection host. `openItemRef` is the immediate
	// URL truth (mount boundary, focus, guards); the pane's <ItemDetail> `ref`
	// PROP re-drives loadData/collabKey (destroy + fresh-mint the Y.Doc/WS
	// provider), so it settles a same-pathname popstate burst (held Back/Forward)
	// down to ONE mint via `paneMintForRoute`. `paneMintForRoute` falls back to
	// the live `openItemRef` the instant the pathname diverges (a genuine route
	// change), so a stale settling ref can't pair against a new route.
	let paneMintRef = $state<string | null>(page.url.searchParams.get('item'));
	let paneMintPathname: string = page.url.pathname;
	const paneMintSettle = createPaneMintSettle({
		settleMs: PANE_MINT_SETTLE_MS,
		onSettle: (r) => {
			paneMintRef = r;
		},
	});
	afterNavigate((nav) => {
		const samePathname =
			!!nav.from && nav.to?.url.pathname === nav.from.url.pathname;
		const effectiveType = samePathname ? nav.type : 'goto';
		paneMintPathname = nav.to?.url.pathname ?? paneMintPathname;
		paneMintSettle.onNavigate(effectiveType, nav.to?.url.searchParams.get('item') ?? null);
	});
	let paneMintForRoute = $derived(
		page.url.pathname === paneMintPathname ? paneMintRef : openItemRef,
	);

	// The PaneHost shell instance — bound once it mounts (i.e. the pane is open).
	// The controller's `focusPaneRegion` dep resolves it lazily at hop time.
	let paneHostEl = $state<PaneHost | undefined>();

	// ── Pane-navigation controller (PLAN-2154 Architecture A/E) ────────────
	// The SAME controller the collection page mounts — one depth/ownership state
	// machine, one three-way close. The full-page host has NO list, so the
	// collection-specific deps are no-ops: no j/k follow to cancel, no list-row
	// focus to capture/return, no unsaved-draft leave guard.
	const paneController = createPaneController({
		getOpenItemRef: () => openItemRef,
		cancelFollow: () => {},
		focusPaneRegion: () => paneHostEl?.focusPaneRegion(),
		captureReturnFocus: () => {},
		setBypassNavGuard: () => {},
	});
	const {
		openItemPaneByRef,
		navigatePaneTo,
		handlePaneBack,
		handlePaneNavigateAway,
		closeItemPane,
		currentPaneState,
		paneNavInFlight,
		clearPaneGo,
	} = paneController;

	// Parse a ref-shaped candidate's item NUMBER — mirrors
	// `$lib/collections/paneTarget`'s private `parseRefNumber` (case-insensitive
	// LETTERS-only prefix, hyphen, positive integer). Null for a non-ref-shaped
	// string or a zero/non-finite number.
	function refNumber(candidate: string): number | null {
		const m = /^([A-Za-z]+)-(\d+)$/.exec(candidate);
		if (!m) return null;
		const n = Number(m[2]);
		return Number.isFinite(n) && n > 0 ? n : null;
	}

	// The forbidden `?item == master` collision (PLAN-2154 D2 / Architecture E).
	// True when a candidate names the MASTER item itself — by its id, its slug,
	// its canonical ref, OR (mirroring the server's case-insensitive, item-number
	// resolution the way `isSamePaneTarget`'s `matchesRefNumber` does) any
	// ref-shaped alias that resolves to the same item NUMBER. So a hand-crafted
	// `?item=doc-5` / `DOC-005` / a stale pre-move prefix that all resolve to the
	// master are caught too, not just the byte-exact canonical ref. Never fires
	// until the master's identity resolves.
	function isMaster(candidate: string): boolean {
		if (!masterIdentity) return false;
		if (
			candidate === masterIdentity.id ||
			candidate === masterIdentity.slug ||
			candidate === masterIdentity.ref
		) {
			return true;
		}
		const n = refNumber(candidate);
		return n !== null && n === refNumber(masterIdentity.ref);
	}

	// Whether to MOUNT the pane. Gating the MOUNT (not merely stripping `?item=`
	// after the fact) closes the cold-load self-collision RACE (Codex review): on
	// a cold `?item=` load the master identity is unresolved for a beat, and a
	// `?item=` that turns out to alias the master would otherwise mount
	// `PaneHost`/`ItemDetail` and mint a SECOND collab provider on the master's
	// own room BEFORE the strip effect's `goto` removes the query. So the pane
	// mounts only once identity has resolved AND confirmed the target isn't the
	// master. For a CLICK-driven open the master identity is already resolved, so
	// there is no delay; only a cold `?item=` load waits one master-load beat
	// (master-first, which is the correct order anyway).
	let showPane = $derived(!!openItemRef && !!masterIdentity && !isMaster(openItemRef));

	// MASTER content-links FIRST-OPEN the pane (PLAN-2154 D3 / Architecture E).
	// The master's relationship / child / wiki-link / graph surfaces hand up a
	// `PaneTarget`; a first-open is `openItemPane`-semantics (a depth-0
	// `paneOwned:true` push), NOT a drill — so it routes through
	// `openItemPaneByRef`, not `navigatePaneTo`. Guarded so a link naming the
	// master itself is a clean no-op (never mount a second provider on the
	// master's room). The master's own <ItemDetail> `fireOpenTarget` already
	// drops self-links against its loaded item; this is the host-side belt.
	function handleMasterOpenTarget(target: PaneTarget) {
		const resolved = resolvePaneTarget(target);
		if (!resolved) return;
		if (isMaster(resolved)) return;
		openItemPaneByRef(resolved);
	}

	// PANE content-links DRILL in place (PLAN-2154 Architecture A/B). A link
	// clicked INSIDE the pane re-targets the pane with a back stack via
	// `navigatePaneTo` — the same-item guard drops a self-referential drill, and
	// the `?item == master` guard drops one back onto the master.
	function guardedDrill(target: PaneTarget) {
		const resolved = resolvePaneTarget(target);
		if (!resolved) return;
		if (isMaster(resolved)) return;
		navigatePaneTo(resolved);
	}

	// Cold-load strip (PLAN-2154 Architecture E). A hand-crafted / shared
	// `?item=<the master's own ref>` URL must NOT mount a pane on the master
	// itself (two providers, one room, one shared itemID-only cursor). Once the
	// master's identity resolves, strip `?item=` in place if it aliases the
	// master. Reads `masterIdentity`/`openItemRef` and WRITES neither (only a
	// `goto` — CONVE-1688 safe); the strip drops `?item=`, `openItemRef`
	// recomputes to null, and the effect re-runs to a no-op (settles, no loop).
	// It can't fight the controller: `isMaster` only ever matches the master, so
	// a legitimately-opened pane (always a non-master ref) is never touched.
	$effect(() => {
		if (!browser) return;
		if (!masterIdentity) return;
		const current = openItemRef;
		if (!current || !isMaster(current)) return;
		const url = new URL(page.url);
		url.searchParams.delete('item');
		void goto(`${url.pathname}${url.search}`, {
			replaceState: true,
			noScroll: true,
			keepFocus: true,
		});
	});

	// Text-editing target detection — copied from the collection host's ESC
	// precedence. ESC in a title edit / the Tiptap editor / a text input has
	// LOCAL meaning (cancel/blur); the pane's depth-aware ESC must not hijack it.
	// NON-text form controls (checkbox, button, …) have no local ESC semantics.
	const NON_TEXT_INPUT_TYPES = new Set([
		'checkbox', 'radio', 'button', 'submit', 'reset', 'range', 'color', 'file', 'image',
	]);
	function isTextEntryTarget(el: HTMLElement | null | undefined): boolean {
		if (!el) return false;
		if (el.isContentEditable) return true;
		const tag = el.tagName;
		if (tag === 'TEXTAREA') return true;
		if (tag === 'INPUT') return !NON_TEXT_INPUT_TYPES.has((el as HTMLInputElement).type);
		return false;
	}

	// Depth-aware ESC (PLAN-2154 Architecture C / R2, mirroring the collection
	// host's escape-stack `pane` slot — TASK-2163). The full-page host has NO
	// list, so there is NO two-level return-focus-to-list step: at depth>0 ESC
	// pops exactly ONE drill level via the controller's fenced `handlePaneBack`;
	// at depth 0 it closes the pane through the shared escape stack (the embedded
	// pane registers `onClose` = `closeItemPane` at `ESCAPE_PRIORITY.pane`). Text
	// edits, open modals/sheets, and a higher-priority stacked graph drawer all
	// win ESC first (the same guards the collection host applies).
	function handleHostKeydown(e: KeyboardEvent) {
		// A control that already handled this key (preventDefault) owns it.
		if (e.defaultPrevented) return;
		if (e.key !== 'Escape') return;
		const target = e.target as HTMLElement | null;
		// Text-editing targets own ESC locally — don't hijack into a layer-close.
		if (isTextEntryTarget(target)) return;
		// A native <dialog> / role="dialog" sheet owns its own ESC.
		if (document.querySelector('dialog[open], [role="dialog"]')) return;
		// The MASTER's full-page (non-embedded) dependency-graph drawer keeps its
		// OWN window ESC listener (ItemDetail, `!embedded`) — NOT the shared escape
		// stack. If it's open (e.g. a pane was opened from a master graph node's
		// "Open" anchor, leaving the graph up), defer this ESC to that listener so
		// one press closes ONLY the graph, not the graph AND the pane (Codex
		// review). The PANE's OWN (embedded) graph drawer registers in the escape
		// stack at the higher `graphDrawer` priority and is closed correctly by
		// `runTopEscape` below, so this bail is scoped to a `.graph-drawer` OUTSIDE
		// `.item-pane` (a master graph) via `closest`.
		for (const g of document.querySelectorAll('.graph-drawer')) {
			if (!g.closest('.item-pane')) return;
		}
		// A HELD key auto-repeats; only the initial physical press acts.
		if (e.repeat) {
			e.preventDefault();
			return;
		}
		// Detached pane (depth>0): pop exactly one drill level, consume the key.
		// Routed through the controller's fenced `handlePaneBack` (not a bare
		// `history.back()`), gated on `paneNavInFlight()` — a rapid double ESC
		// can't stack a second traversal and overshoot (R14). A no-op while a
		// traversal is armed STILL consumes the key (never falls through to close).
		if (
			openItemRef &&
			topEscapePriority() === ESCAPE_PRIORITY.pane &&
			currentPaneState().paneDepth > 0
		) {
			e.preventDefault();
			if (!paneNavInFlight()) {
				handlePaneBack();
			}
			return;
		}
		// Depth 0 (or a higher-priority layer): let the escape stack close exactly
		// one layer, innermost-first (a pane graph drawer → the pane). No-op with
		// no pane open (stack empty → returns false → native ESC untouched).
		if (runTopEscape()) e.preventDefault();
	}

	onDestroy(() => {
		// Drop any in-flight controller `history.go` continuation, and cancel a
		// settling paneMint so a late latch/settle can't write to the unmounted
		// page (PLAN-2154 / TASK-2166/2170).
		clearPaneGo();
		paneMintSettle.cancel();
	});
</script>

<svelte:window onkeydown={handleHostKeydown} />

<!--
	Full-page pane layout (PLAN-2154 Architecture E). A flex row: the master's
	own `.item-page` overflow column (always the scroll container — pane open or
	not, so scroll restoration is consistent) + the right-docked <PaneHost>. The
	host fills `.main-content` (height:100%) and clips, so `.main-content` never
	double-scrolls while `.item-page` scrolls independently — mirroring the
	collection host's `.pane-open` overflow handling. The pane is the ONLY thing
	that mounts/unmounts on open/close (its own `.item-pane` CSS docks it and,
	at ≤768px, makes it a full-screen overlay with the master left mounted +
	inert behind it).
-->
<div class="item-page-host">
	<!-- inert on the MOBILE overlay: the pane covers the viewport, so isolate the
	     master from BOTH focus order and the SR tree behind it. Desktop keeps the
	     master fully reachable (it's beside the pane, not behind it). -->
	<div class="item-page" inert={viewport.isMobile && !!openItemRef}>
		<ItemDetail
			{username}
			{wsSlug}
			{collSlug}
			{ref}
			peeking={!!openItemRef}
			onReady={(r) => (scrollReady = r)}
			onIdentity={(id) => (masterIdentity = id)}
			onOpenTarget={handleMasterOpenTarget}
		/>
	</div>
	{#if showPane}
		<PaneHost
			bind:this={paneHostEl}
			{openItemRef}
			{username}
			{wsSlug}
			{collSlug}
			{paneMintForRoute}
			onClose={closeItemPane}
			onGone={closeItemPane}
			onNavigateAway={handlePaneNavigateAway}
			onOpenTarget={guardedDrill}
			onBack={handlePaneBack}
		/>
	{/if}
</div>

<style>
	/* Fill the scrollable `.main-content` region and clip, delegating scroll to
	   the `.item-page` column so the layout's `.main-content` never
	   double-scrolls (mirrors the collection host's `.collection-page.pane-open`).
	   Always a flex row so `.item-page` is a consistent, bounded scroll container
	   whether or not the pane is docked. */
	.item-page-host {
		height: 100%;
		display: flex;
		flex-direction: row;
		align-items: stretch;
		overflow: hidden;
	}

	/* The master's OWN overflow-y:auto scroll column (the analogue of the
	   collection host's `.list-column`). No padding here — <ItemDetail>'s inner
	   `.item-page` content wrapper owns the max-width + padding, so this stays a
	   transparent, full-width, scrollable flex child. */
	.item-page {
		flex: 1 1 0;
		min-width: 0;
		overflow-y: auto;
	}
</style>
