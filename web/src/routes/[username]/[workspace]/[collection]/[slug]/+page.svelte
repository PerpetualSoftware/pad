<script lang="ts">
	import { page } from '$app/state';
	import { goto, afterNavigate } from '$app/navigation';
	import { browser } from '$app/environment';
	import { onDestroy, onMount } from 'svelte';
	import ItemDetail from '$lib/components/items/ItemDetail.svelte';
	import PaneHost from '$lib/components/collections/PaneHost.svelte';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import { createPaneController } from '$lib/collections/paneHostController';
	import { type ResolvedPaneState } from '$lib/collections/paneController';
	import { createPaneMintSettle, PANE_MINT_SETTLE_MS } from '$lib/collections/paneMintSettle';
	import { resolvePaneTarget, isSamePaneTarget, type PaneGuardItem } from '$lib/collections/paneTarget';
	import { inExemptSurface } from '$lib/collections/paneFocus';
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
	// `null` until the master's loaded item matches its URL ref. Paired with the
	// pathname it resolved for, so a STALE identity from a since-navigated master
	// (Expand→Back, cross-collection, cross-workspace — all encoded in the
	// pathname) is dropped (see `masterItem`).
	let masterIdentity = $state<ResolvedItemIdentity | null>(null);
	let masterIdentityPathname = $state<string | null>(null);

	// The master's OWN `.item-page` overflow column element, bound directly so
	// scroll restoration targets THIS host's scroll column — not the layout's
	// `.main-content`, and not <ItemDetail>'s inner content wrapper which reuses
	// the same `.item-page` class. Binding the element removes the global
	// `querySelector('.item-page')` that would rely on ancestor DOM order to
	// disambiguate the collision (orchestrator Codex review).
	let itemPageEl = $state<HTMLElement | null>(null);

	// Scroll restoration parks on the master's own `.item-page` overflow column,
	// which is where the master content scrolls once the flex-row host is in place
	// — pane open or not (PLAN-2154 Architecture E / TASK-2171's `scrollTarget`
	// getter). Returns the BOUND element (null before mount → restore.svelte.ts
	// retries per frame).
	const scrollRestoration = createScrollRestoration({
		ready: () => scrollReady,
		persistKey: () =>
			wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null,
		scrollTarget: () => itemPageEl,
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

	// ── Focus-follows-editing (PLAN-2179 DR-2/DR-3 / TASK-2181) ────────────
	// Which side is the ACTIVE (editable) one. The master freezes while the PANE
	// is active and vice-versa (via the `peeking` derivations below) so exactly
	// ONE side is ever editable — no two-editor collision, by construction. The
	// TASK-2180 reactive freeze makes flipping this a cheap toggle (no remount).
	//
	// Cold-load INITIALIZER: no focusin fires on a `?item=` deep-load, so seed the
	// active side from viewport reality — desktop first-open/cold-load focus is the
	// MASTER (openItemPaneByRef does NOT move focus into the pane; the pane opens as
	// a read-only PREVIEW per DR-2), and mobile is pane-only (the master is inert
	// behind the full-screen overlay). Persists across same-route navigations (this
	// route component is REUSED on expand / browser Back), so a drilled-then-Back
	// pane keeps `activePane='pane'`; a fresh first-open re-seeds it (below).
	let activePane = $state<'master' | 'pane'>(viewport.isMobile ? 'pane' : 'master');

	// Mobile is pane-only: crossing INTO the mobile breakpoint must force the pane
	// active (the master goes `inert` behind the overlay — an "active" master there
	// would be an un-editable dead end). Reads only `viewport.isMobile` (a plain
	// prev-flag tracks the edge), writes only `activePane` → CONVE-1688-safe; fires
	// on the transition, not every render. Desktop→mobile only; leaving mobile keeps
	// whatever side was active.
	let wasMobile = viewport.isMobile;
	$effect(() => {
		const nowMobile = viewport.isMobile;
		if (nowMobile && !wasMobile) activePane = 'pane';
		wasMobile = nowMobile;
	});

	// ── Pane-navigation controller (PLAN-2154 Architecture A/E) ────────────
	// The SAME controller the collection page mounts — one depth/ownership state
	// machine, one three-way close. The full-page host has NO list, so the
	// collection-specific deps are no-ops: no j/k follow to cancel, no list-row
	// focus to capture/return, no unsaved-draft leave guard.
	const paneController = createPaneController({
		getOpenItemRef: () => openItemRef,
		cancelFollow: () => {},
		// Focus-follows-editing (PLAN-2179 DR-2 / TASK-2181): the controller calls
		// this on EVERY pane-internal hop — a drill (`navigatePaneTo`), an in-pane
		// Back (`handlePaneBack`), and the depth-aware ESC pop. All are pane-internal,
		// so each makes the PANE the active/editable side. Setting `activePane` here
		// (the single wire point) keeps it consistent with the synchronous
		// `focusPaneRegion` focus move — the imminent focusin lands in the pane and
		// the classifier agrees, no desync — and covers the test-hook drill path too.
		focusPaneRegion: () => {
			activePane = 'pane';
			paneHostEl?.focusPaneRegion();
		},
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

	// Parse a ref-shaped candidate's item NUMBER (mirrors paneTarget's private
	// `parseRefNumber`) — used only to derive `masterItem.item_number` from the
	// master's canonical ref below.
	function refNumber(candidate: string): number | null {
		const m = /^([A-Za-z]+)-(\d+)$/.exec(candidate);
		if (!m) return null;
		const n = Number(m[2]);
		return Number.isFinite(n) && n > 0 ? n : null;
	}

	// The master projected into the minimal identity the same-item guard reads
	// (`id` / `slug` / `item_number`), built from `onIdentity`'s {id, ref, slug}
	// with `item_number` parsed from the canonical `ref`. Reusing the SHARED,
	// unit-tested `isSamePaneTarget` / `resolvePaneTarget` (rather than a
	// hand-rolled compare) is what keeps the `?item == master` guard
	// PROVENANCE-CORRECT: a bare `?item=` value resolves REF-BEFORE-SLUG the same
	// way the server does, so a ref-shaped SLUG (e.g. master #5 slugged `plan-6`)
	// is NOT mistaken for the master when `?item=plan-6` actually resolves to
	// item #6 (Codex review).
	let masterItem = $derived.by<PaneGuardItem | null>(() => {
		if (!masterIdentity) return null;
		// `masterIdentity` (from `onIdentity`) LAGS a same-route master navigation:
		// expanding a pane item to the full page, then browser-Back to
		// `<prev-master>?item=<that item>`, REUSES this route — `masterIdentity`
		// still holds the expanded item until the master reloads the previous one
		// and re-emits identity. Compare the pathname it resolved FOR against the
		// LIVE pathname: EVERY master navigation (ref, collection, OR workspace —
		// all encoded in `/username/workspace/collection/ref`) makes them diverge,
		// so a stale identity is dropped until the master reloads and re-emits.
		// `?item=` lives in the query, not the pathname, so opening / drilling /
		// closing the pane never trips this. Because it's a `$derived` over
		// `page.url.pathname`, it re-nulls SYNCHRONOUSLY the instant the URL changes
		// — before the strip `$effect` runs — so the guard never matches a bare
		// `?item=` against the WRONG (previous) master (the R14 late-continuation
		// class; Codex review). Null until identity resolves AND still matches.
		if (masterIdentityPathname !== page.url.pathname) return null;
		return {
			id: masterIdentity.id,
			slug: masterIdentity.slug,
			item_number: refNumber(masterIdentity.ref) ?? 0,
		};
	});

	// The forbidden `?item == master` collision (PLAN-2154 D2 / Architecture E) for
	// a BARE `?item=` string (the cold-load strip + mount gate + a resolved
	// content-link ref). CONSERVATIVE by design: return true whenever the candidate
	// aliases the master under ANY of the server's resolution channels
	// (`GetItemByRef` — internal/store/items.go: UUID-first, then ref by NUMBER,
	// then slug) — an exact id, the raw slug string, OR the ref number. This is the
	// D2 "err toward a match" mandate: the forbidden outcome is a SECOND collab
	// provider on the master's own room, so when the master COULD be the target we
	// decline to open a pane. `false` while the master identity is unresolved.
	//
	// A server round-trip could distinguish the two genuinely-undecidable-by-client
	// cases (a ref/UUID-SHAPED master SLUG that either preempts to a different live
	// item or falls back to the master) — but it was evaluated and rejected: the
	// async resolve added real race surface (stale-cache-across-archive, a
	// popstate/mint-settle remount race, SSE-driven refetch) for a scenario that
	// requires a PATHOLOGICAL master slug (one shaped like a ref or a UUID — never
	// produced by an organic title). The only residual cost of the sync guard is a
	// BENIGN over-block in exactly that pathological case: a `?item=<X>` where X is
	// a live different item AND coincidentally equals the master's ref/UUID-shaped
	// slug declines to open a pane (no collision, no data loss). This matches the
	// existing shared behavior of `isSamePaneTarget` / `ItemDetail.fireOpenTarget`,
	// which already conservatively drop a slug-matching target.
	function isMasterRef(candidate: string): boolean {
		if (!masterItem) return false;
		if (candidate === masterItem.id || candidate === masterItem.slug) return true;
		return isSamePaneTarget({ href: candidate }, masterItem);
	}

	// Whether to MOUNT the pane. Gating the MOUNT (not merely stripping `?item=`
	// after the fact) closes the cold-load self-collision RACE (Codex review): on
	// a cold `?item=` load the (fresh) master identity is unresolved for a beat,
	// and a `?item=` that turns out to alias the master would otherwise mount
	// `PaneHost`/`ItemDetail` and mint a SECOND collab provider on the master's
	// own room BEFORE the strip effect's `goto` removes the query. Gating on
	// `masterItem` (fresh-for-`ref`) means the pane mounts only once the CURRENT
	// master's identity has resolved AND confirmed the target isn't the master —
	// and never against a stale prior master (the Expand→Back case). For a
	// CLICK-driven open the master identity is already resolved (and `ref` is
	// unchanged), so there is no delay; only a cold `?item=` load or a master
	// navigation waits one master-load beat (master-first, the correct order).
	let showPane = $derived(!!openItemRef && !!masterItem && !isMasterRef(openItemRef));

	// MASTER content-links FIRST-OPEN the pane (PLAN-2154 D3 / Architecture E).
	// The master's relationship / child / wiki-link / graph surfaces hand up a
	// `PaneTarget`; a first-open is `openItemPane`-semantics (a depth-0
	// `paneOwned:true` push), NOT a drill — so it routes through
	// `openItemPaneByRef`, not `navigatePaneTo`. Guarded so a link naming the
	// master itself is a clean no-op (never mount a second provider on the
	// master's room). The master's own <ItemDetail> `fireOpenTarget` already
	// drops self-links against its loaded item; this is the host-side belt.
	function handleMasterOpenTarget(target: PaneTarget) {
		// `resolvePaneTarget(target, masterItem)` returns null BOTH when the target is
		// unresolvable AND when it resolves to the master (the `isSamePaneTarget`
		// self-guard, same as `ItemDetail.fireOpenTarget`); `isMasterRef(resolved)`
		// additionally drops a resolved segment that aliases the master by id / raw
		// slug / ref-number (the conservative D2 guard — err toward a match).
		const resolved = resolvePaneTarget(target, masterItem);
		if (!resolved || isMasterRef(resolved)) return;
		// First-open re-seeds the active side (PLAN-2179 DR-2 / TASK-2181): the pane
		// opens as a read-only PREVIEW beside the still-editable master on desktop
		// (pane-only on mobile). Explicit because `activePane` PERSISTS across the
		// route's lifetime — a prior in-pane session may have left it 'pane', and a
		// brand-new open must not inherit that (it would freeze the master on open).
		// `openItemPaneByRef` does NOT move focus into the pane, so no focusin/hop
		// fights this.
		activePane = viewport.isMobile ? 'pane' : 'master';
		openItemPaneByRef(resolved);
	}

	// PANE content-links DRILL in place (PLAN-2154 Architecture A/B). A link
	// clicked INSIDE the pane re-targets the pane with a back stack via
	// `navigatePaneTo` — the same `resolvePaneTarget(target, masterItem)` +
	// `isMasterRef(resolved)` drop guards a drill back onto the master (the pane's
	// own `fireOpenTarget` already dropped drills to the pane's shown item).
	function guardedDrill(target: PaneTarget) {
		const resolved = resolvePaneTarget(target, masterItem);
		if (!resolved || isMasterRef(resolved)) return;
		navigatePaneTo(resolved);
	}

	// ── Focus-follows-editing detectors (PLAN-2179 DR-2/DR-3 / TASK-2181) ───
	// Classify which region a focus / pointer event landed in, so `activePane`
	// tracks where the user is working:
	//  • inside the MASTER column (`itemPageEl`, the bound `.item-page`) → 'master'
	//  • inside the PANE region (`paneEl`, PaneHost's bound `.item-pane`) → 'pane'
	//  • a portalled/self-trapping overlay (dialog/menu/listbox/block-context-menu,
	//    the SHARED `inExemptSurface` set) → DON'T flip (it's in NEITHER region and
	//    owns its own focus)
	//  • a bare `<body>` drop or anything else → DON'T flip
	// Membership is tested against the BOUND elements, never the `.item-page` /
	// `.item-pane` CLASSES — ItemDetail's inner content wrapper reuses `.item-page`,
	// so a class match inside the pane would misclassify (which is exactly why the
	// host binds its own column and PaneHost exposes `getPaneRegion`).
	function classifyPaneRegion(target: EventTarget | null): 'master' | 'pane' | null {
		const el = target instanceof Element ? target : null;
		if (!el) return null;
		// Exempt FIRST: an overlay portalled out of a region (or rendered above it)
		// is never a region switch, even if it happens to sit inside one in the DOM.
		if (inExemptSurface(el)) return null;
		if (itemPageEl && itemPageEl.contains(el)) return 'master';
		const paneEl = paneHostEl?.getPaneRegion() ?? null;
		if (paneEl && paneEl.contains(el)) return 'pane';
		return null;
	}

	// A pointer/focus target that is (or is inside) a navigable DRILL surface —
	// a content link, child row, relationship link, breadcrumb, or graph "Open"
	// (all `<a href>`, routed through `shouldOpenInPane` → `onOpenTarget`). NEITHER
	// detector may flip `activePane` for these: flipping mid-gesture un-freezes the
	// pane, which re-inits ChildItems' live `dndzone` (`dragDisabled` tracks the
	// freeze) and SWALLOWS the very click that would drill (PLAN-2179 DR-2 /
	// TASK-2181). Instead we let the click's own drill fire — `navigatePaneTo`
	// ALREADY sets `activePane='pane'` (via focusPaneRegion) — so a content link in
	// a frozen preview drills on the FIRST click AND activates the pane, no dndzone
	// churn. (Chromium/Firefox focus an `<a href>` on mouse-click, so the exclusion
	// must cover the focusin path too, not just pointerdown.)
	function isNavigableDrillTarget(target: EventTarget | null): boolean {
		const el = target instanceof Element ? target : null;
		return !!el?.closest('a[href]');
	}

	// The detectors flip `activePane` ONLY on a CHANGED region (no begin/end
	// transition churn on the frozen/active ItemDetail pair), and BOTH exclude
	// navigable drill targets (their own click owns the transition — see above):
	//  • focusin — the primary classifier: Tab / programmatic focus / a click that
	//    lands focus on a focusable control fires it with a real region target.
	//    Chromium/Firefox DO focus an `<a href>` on mouse-click, so this fires for a
	//    content-link drill too — hence the same navigable exclusion here, or the
	//    focus-driven flip would re-init the dndzone and swallow the drill.
	//  • pointerdown (CAPTURE) — the activator for NON-focusable text/background:
	//    clicking plain master/pane body text drops focus to `<body>` and fires NO
	//    region focusin, so a pointer landing on the region must flip regardless of
	//    the target's focusability. This is what makes "click into the master
	//    activates it". Capture-phase so a child `stopPropagation` can't swallow it.
	// Only mounted while a pane is open (`paneOpen`) — with no pane there is no
	// second side to arbitrate. Reading the boolean (not raw `openItemRef`) keeps
	// the effect from re-registering on every drill.
	let paneOpen = $derived(!!openItemRef);
	$effect(() => {
		if (!browser || !paneOpen) return;
		function onRegionEvent(e: Event) {
			if (isNavigableDrillTarget(e.target)) return;
			const region = classifyPaneRegion(e.target);
			if (region && region !== activePane) activePane = region;
		}
		document.addEventListener('focusin', onRegionEvent);
		document.addEventListener('pointerdown', onRegionEvent, true);
		return () => {
			document.removeEventListener('focusin', onRegionEvent);
			document.removeEventListener('pointerdown', onRegionEvent, true);
		};
	});

	// Cold-load strip (PLAN-2154 Architecture E). A hand-crafted / shared
	// `?item=<the master's own ref>` URL must NOT mount a pane on the master itself
	// (two providers, one room, one shared itemID-only cursor). Once the master's
	// identity resolves, strip `?item=` in place if it aliases the master. Reads
	// reactive state and WRITES none of it (only a `goto` — CONVE-1688 safe); the
	// strip drops `?item=`, `openItemRef` recomputes to null, and the effect re-runs
	// to a no-op (settles, no loop). It can't fight the controller: `isMasterRef`
	// only ever matches the master, so a legitimately-opened pane (a non-master ref)
	// is never touched.
	$effect(() => {
		if (!browser) return;
		if (!masterItem) return;
		const current = openItemRef;
		if (!current || !isMasterRef(current)) return;
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
	// pane registers `onClose` = `closeItemPane` at `ESCAPE_PRIORITY.pane`). The
	// master's dependency-graph drawer now ALSO composes in the escape stack
	// (ItemDetail routes both embedded + non-embedded graphs through it — TASK-2174),
	// so a graph drawer (master or pane) is the innermost layer `runTopEscape`
	// closes first, with no uncoordinated window listener to double-close. Text
	// edits and open modals/sheets win ESC first (the same guards the collection
	// host applies).
	function handleHostKeydown(e: KeyboardEvent) {
		// A control that already handled this key (preventDefault) owns it.
		if (e.defaultPrevented) return;
		if (e.key !== 'Escape') return;
		const target = e.target as HTMLElement | null;
		// Text-editing targets own ESC locally — don't hijack into a layer-close.
		if (isTextEntryTarget(target)) return;
		// A native <dialog> / role="dialog" sheet owns its own ESC.
		if (document.querySelector('dialog[open], [role="dialog"]')) return;
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
		// Otherwise let the escape stack close exactly one layer, innermost-first
		// (a graph drawer → the pane). No-op with nothing registered (stack empty →
		// returns false → native ESC untouched).
		if (runTopEscape()) e.preventDefault();
	}

	// Test-only hook (PLAN-2154 / TASK-2175): exposes the SAME pane-controller
	// surface the collection page's hook does (`[collection]/+page.svelte`), so
	// the full-page host's R14 async-race capstone can drive `navigatePaneTo`
	// (in-pane drill), `closeItemPane` (three-way close), and read back the
	// depth/ownership stamp — the drill/close continuations aren't otherwise
	// synchronously drivable with the adversarial timing R14 needs. Gated on the
	// same opt-in localStorage flag so it adds ZERO surface to production; the
	// e2e harness sets `pad:pane-test-hook=1` before navigating.
	interface PaneTestHook {
		navigatePaneTo: (ref: string) => void;
		closeItemPane: () => void;
		getPaneState: () => ResolvedPaneState;
	}
	function installPaneTestHook() {
		if (!browser) return;
		try {
			if (localStorage.getItem('pad:pane-test-hook') !== '1') return;
		} catch {
			return;
		}
		(window as unknown as { __padPaneController?: PaneTestHook }).__padPaneController = {
			navigatePaneTo: (r: string) => navigatePaneTo(r),
			closeItemPane: () => closeItemPane(),
			getPaneState: () => currentPaneState(),
		};
	}
	function removePaneTestHook() {
		if (!browser) return;
		delete (window as unknown as { __padPaneController?: PaneTestHook }).__padPaneController;
	}

	onMount(() => {
		installPaneTestHook();
	});

	onDestroy(() => {
		removePaneTestHook();
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
	<div class="item-page" bind:this={itemPageEl} inert={viewport.isMobile && !!openItemRef}>
		<ItemDetail
			{username}
			{wsSlug}
			{collSlug}
			{ref}
			peeking={!!openItemRef && activePane === 'pane'}
			onReady={(r) => (scrollReady = r)}
			onIdentity={(id) => {
				masterIdentity = id;
				// Stamp the pathname this identity resolved FOR (see `masterItem`);
				// `onIdentity` fires only once the loaded item matches the current
				// URL, so `page.url.pathname` is that item's route here.
				masterIdentityPathname = id ? page.url.pathname : null;
			}}
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
			{activePane}
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

	/* Print: the docked pane is a screen-only navigational surface, and the
	   flex-row host clips to the viewport for the on-screen split — neither is
	   right for print. Hide the pane + divider so Ctrl/Cmd+P captures ONLY the
	   master document (not two items side-by-side with duplicate footers), and
	   un-clip the host/column so the master flows naturally across print pages —
	   mirroring app.css's `.app-shell`/`.main-content` print unlock, which does
	   not reach these route-owned containers (Codex review). */
	@media print {
		.item-page-host {
			display: block;
			height: auto;
			overflow: visible;
		}
		.item-page {
			overflow: visible;
		}
		.item-page-host :global(.item-pane),
		.item-page-host :global(.pane-divider) {
			display: none;
		}
	}
</style>
