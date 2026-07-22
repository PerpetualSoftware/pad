<script lang="ts">
	// Pane SHELL (PLAN-2154 Architecture E, bullet 1 / TASK-2170). The
	// right-docked detail-pane `<aside>` + its resize divider, the pane-width
	// persistence, and the focus machinery (focus-per-hop region focus, the
	// mobile focus trap, the desktop focusin backstop) were inline in the
	// collection route (`[collection]/+page.svelte`) through Phase 1; this
	// extracts the SHELL so BOTH the collection page AND the full-page item host
	// (TASK-2174) mount ONE implementation — no duplicated resize/focus glue.
	//
	// The pane NAVIGATION controller (depth/ownership state machine, the fenced
	// `history.go` traversals, the three-way close) lives separately in
	// `$lib/collections/paneHostController`; the host owns it and hands the pane
	// callbacks (`onClose`/`onGone`/`onNavigateAway`/`onOpenTarget`/`onBack`) in
	// as props. The `paneMint` provider-mint settle stays in the host too (its
	// always-mounted lifecycle is load-bearing for TASK-2166's popstate-burst
	// coalescing); the host computes `paneMintForRoute` and passes it as a PROP,
	// which this shell's inner render keys the `<ItemDetail>` `ref` off.
	//
	// CRITICAL (PLAN-2105 / TASK-2112): NO `{#key}` around the pane
	// `<ItemDetail>`. An A→B item switch must be a PROP UPDATE (`ref` change
	// re-drives ItemDetail's loadData + collabKey via its own reactive effects,
	// reusing the one mounted instance + its single collab provider + SSE
	// subscription). The ONLY mount/unmount is the host's outer `{#if
	// openItemRef}` gate, which mounts/unmounts THIS whole component.
	import { browser } from '$app/environment';
	import { onDestroy, untrack } from 'svelte';
	import ItemDetail from '$lib/components/items/ItemDetail.svelte';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import { paneOverlay } from '$lib/stores/paneOverlay.svelte';
	import { paneFocusables, nextTrapTarget, inExemptSurface } from '$lib/collections/paneFocus';
	import type { PaneTarget } from '$lib/types';

	interface Props {
		/** The immediate `?item=` URL truth — used for focus/observer guards. Always
		 *  truthy while this shell is mounted (the host gates it behind `{#if openItemRef}`). */
		openItemRef: string | null;
		username: string;
		wsSlug: string;
		collSlug: string;
		/** The coalesced provider-mint ref (TASK-2166) the host computes and passes
		 *  in; the inner `<ItemDetail>` `ref` keys off THIS, not raw `openItemRef`. */
		paneMintForRoute: string | null;
		/** Focus-follows-editing (PLAN-2179 DR-2/DR-3, TASK-2181) — OPTIONAL. The
		 *  full-page item host owns an `activePane: 'master'|'pane'` model and passes
		 *  it here so the pane's inner ItemDetail freezes (`peeking`) while the MASTER
		 *  is the active side, and the desktop focusin backstop stands down instead of
		 *  fighting a click into the master. LEFT UNSET by the collection route (its
		 *  master is the non-editable list) → pane stays always-editable + the backstop
		 *  keeps its original always-pull-back behavior there (byte-identical). */
		activePane?: 'master' | 'pane';
		onClose: () => void;
		onGone: () => void;
		onNavigateAway: (url: string) => void;
		onOpenTarget: (target: PaneTarget) => void;
		onBack: () => void;
	}

	let {
		openItemRef,
		username,
		wsSlug,
		collSlug,
		paneMintForRoute,
		activePane,
		onClose,
		onGone,
		onNavigateAway,
		onOpenTarget,
		onBack,
	}: Props = $props();

	// Focus per hop (PLAN-2154 Architecture C / R1, TASK-2162). Move keyboard
	// focus INTO the stable pane region on each in-pane hop — a drill
	// (`navigatePaneTo`, the clicked link), an in-pane Back (`handlePaneBack`,
	// the clicked Back chevron), or a depth-aware ESC pop. Each hop changes
	// `?item=`, which remounts ItemDetail's `{#key itemSlug}` subtrees (and
	// swaps its loaded↔minimal header) and DESTROYS whatever link/row/button
	// was focused; `keepFocus` then has nothing to restore, so focus falls to
	// `<body>`, where the NEXT `j`/`k` runs list-nav (the host's
	// `handlePageKeydown` `.item-pane` bail doesn't match `<body>`) and
	// laterally re-targets the drilled `?item=`, corrupting the stack (R1).
	//
	// We focus the aria-labeled `<aside>` (`paneEl`) itself — it lives OUTSIDE
	// every `{#key}` and the header swap, so it's STABLE across the remount.
	// Called SYNCHRONOUSLY at the hop (while the just-activated control is still
	// in the DOM), so focus moves onto the stable region BEFORE the imminent
	// remount removes that control — the removed control never had focus to
	// drop. Focus now sits inside `.item-pane`, so `handlePageKeydown` bails and
	// `j`/`k` stay inert. This is the plan's serializable "focus per hop"
	// target: no stored element ref (keyed remounts destroy those) — `paneEl`
	// is re-found via its binding, never captured across a hop.
	//
	// The plan's "focus the originating link if still in the DOM, else the pane
	// heading" reduces to this region focus in practice: the originating link
	// always sits inside a `{#key itemSlug}` subtree that remounts on the very
	// item change the hop triggers, so it is NEVER still in the DOM once the pop
	// lands — the "else the pane heading" fallback is the operative branch, and
	// re-finding the link across TASK-2166's mint settle is the unbounded timing
	// tail we deliberately don't chase.
	//
	// Runs on BOTH desktop and the mobile overlay: on mobile `j`/`k` is already
	// inert (the list is hidden), but focus must still not strand on `<body>`
	// behind the overlay after a Back/drill — the synchronous belt this
	// provides is what TASK-2164's now-retired `pendingBackFocus` used to give
	// the mobile Back path, and the mobile focus trap's `focusin` is the async
	// net on top. `{preventScroll:true}` so the programmatic focus never jumps
	// the pane's scroll position (nor summons the mobile keyboard — `paneEl` is
	// a non-editable region, not an input).
	//
	// Exposed as a Svelte 5 instance export (PLAN-2154 Architecture E / TASK-2170)
	// so the host can wire it as the controller's `focusPaneRegion` dep AND its
	// list→pane Tab bridge, both via `paneHostEl?.focusPaneRegion()`.
	export function focusPaneRegion() {
		if (!browser) return;
		if (!openItemRef || !paneEl) return;
		paneEl.focus({ preventScroll: true });
	}

	// The stable `.item-pane` region element (PLAN-2179 / TASK-2181). Exposed so
	// the full-page host's focus-follows classifier can test membership with
	// `paneEl.contains(target)` against the BOUND element — the `.item-page` /
	// `.item-pane` class pair alone can't disambiguate (ItemDetail's inner content
	// wrapper reuses `.item-page`), which is exactly why the host binds its own
	// column too. Returns null before mount.
	export function getPaneRegion(): HTMLElement | null {
		return paneEl;
	}

	// ── Resizable detail pane + persisted width (PLAN-2105 / TASK-2114) ─
	// No resize primitive existed in the repo, so this is built from
	// scratch: a draggable divider between the list column and the pane,
	// pointer-captured so the drag can't select text or fall through to a
	// row-click. The width persists to localStorage under a GLOBAL key that
	// deliberately does NOT collide with the vestigial `pad-detail-panel`
	// key in ui.svelte.ts, reusing the read-on-init / write-on-change idiom
	// from that store (the `pad-topbar` pattern).
	const PANE_WIDTH_KEY = 'pad-pane-width';
	const PANE_WIDTH_MIN = 360; // mirrors the CSS clamp() floor
	const PANE_WIDTH_MAX = 720; // generous ceiling (CSS clamp maxed at 640)
	const PANE_WIDTH_DEFAULT = 460;
	const PANE_DIVIDER_WIDTH = 6; // keep in sync with .pane-divider flex-basis
	// Keep at least this much room for the list column so a wide drag (or a
	// stored width carried over from a bigger screen) can never crush the
	// list to nothing.
	const LIST_MIN_WIDTH = 360;

	// Min/max pane width for the CURRENT container. Measures the ACTUAL
	// flex-row container (`.collection-page` — which spans `.main-content`,
	// i.e. EXCLUDES the sidebar + page chrome) once the pane is mounted, so
	// the clamp reflects real available space and a drag can never crush the
	// list. Before the pane mounts (init / keyboard) it falls back to the
	// viewport as a loose upper bound; the ResizeObserver below refits once
	// the real container is measurable (Codex round 1 P2).
	//
	// When the container is too narrow to honor BOTH the pane floor and the
	// list floor (e.g. a ~800px viewport with the sidebar expanded), the fixed
	// 360px floors would force the list down to ~140px. In that regime both
	// floors relax to 40% of the usable width so neither column is crushed —
	// the pane can shrink below PANE_WIDTH_MIN and the list keeps ≥40% (Codex
	// round 2 P2).
	function paneBounds(): { min: number; max: number } {
		let containerW = 0;
		const container = paneEl?.parentElement ?? null;
		if (container) containerW = container.getBoundingClientRect().width;
		else if (browser) containerW = window.innerWidth;
		if (containerW <= 0) return { min: PANE_WIDTH_MIN, max: PANE_WIDTH_MAX };
		const usable = containerW - PANE_DIVIDER_WIDTH;
		const listFloor = Math.min(LIST_MIN_WIDTH, usable * 0.4);
		const paneFloor = Math.min(PANE_WIDTH_MIN, usable * 0.4);
		const max = Math.max(paneFloor, Math.min(PANE_WIDTH_MAX, usable - listFloor));
		return { min: paneFloor, max };
	}
	function clampPaneWidth(px: number): number {
		const { min, max } = paneBounds();
		return Math.max(min, Math.min(max, px));
	}

	// The RAW persisted preference (unclamped) — the width the user last
	// chose. Kept separate from the APPLIED width so a temporarily narrow
	// container (small window / open sidebar) refits the pane down without
	// destroying the preference, and widening restores it.
	function readStoredPaneWidth(): number | null {
		if (!browser) return null;
		try {
			const raw = localStorage.getItem(PANE_WIDTH_KEY);
			if (!raw) return null;
			const n = Number(raw);
			return Number.isFinite(n) ? n : null;
		} catch {
			return null;
		}
	}

	// storedPaneWidth = the raw preference; paneWidth = that preference
	// clamped to what currently fits. BOTH read synchronously from
	// localStorage on the first client tick. These authenticated routes are
	// CSR-only (adapter-static `fallback` ⇒ no runtime SSR), so the
	// `!browser` → null branch only runs at build-time fallback generation,
	// never for a real user: the persisted width is present on the very first
	// client paint and a direct `?item=REF` landing shows no jump from the
	// CSS default to the stored width (PLAN-2105 reflow mitigation). Applied
	// as the `--pane-width` CSS var on the pane below; `null` → the CSS
	// clamp() default.
	let paneEl = $state<HTMLElement | null>(null);
	let resizingPane = $state(false);
	// Read once into a plain const so neither $state initializer references the
	// other reactive value (which would only capture its initial value anyway).
	const initialStoredWidth = readStoredPaneWidth();
	let storedPaneWidth = $state<number | null>(initialStoredWidth);
	let paneWidth = $state<number | null>(
		initialStoredWidth != null ? clampPaneWidth(initialStoredWidth) : null,
	);
	// The pane's real rendered width, tracked by the ResizeObserver below.
	// Fallback for `aria-valuenow` in the brief window before the observer
	// first fires and sets `paneWidth`.
	let measuredPaneWidth = $state<number | null>(null);
	// The current bounds, mirrored into state so the divider's ARIA range
	// tracks the relaxed floors on a cramped container (Codex round 3 P3).
	let ariaMin = $state(PANE_WIDTH_MIN);
	let ariaMax = $state(PANE_WIDTH_MAX);

	// Commit a new user-chosen width: clamp, apply, persist the preference.
	function setPaneWidth(px: number) {
		const clamped = clampPaneWidth(px);
		storedPaneWidth = clamped;
		paneWidth = clamped;
		if (browser) {
			try {
				localStorage.setItem(PANE_WIDTH_KEY, String(clamped));
			} catch {}
		}
	}

	// The width to APPLY for the current container: the user's saved
	// preference if set, otherwise the CSS `clamp(360px, 38%, 640px)` default
	// computed in JS — both clamped to what currently fits. Mirroring the CSS
	// default means applying it as `--pane-width` matches what CSS renders on
	// roomy layouts (so the observer's first run causes no reflow) while a
	// cramped container gets a fitted width that doesn't crush the list, even
	// with NO saved preference (Codex round 3 P2).
	function fittedPaneWidth(): number {
		const { min, max } = paneBounds();
		let containerW = 0;
		const container = paneEl?.parentElement ?? null;
		if (container) containerW = container.getBoundingClientRect().width;
		else if (browser) containerW = window.innerWidth;
		const cssDefault = Math.min(640, Math.max(360, containerW * 0.38));
		return Math.max(min, Math.min(max, storedPaneWidth ?? cssDefault));
	}

	// Re-fit the applied width + ARIA bounds to the current container. Called
	// from the ResizeObserver on mount, window resize, and sidebar toggle.
	function refitPaneWidth() {
		paneWidth = fittedPaneWidth();
		const b = paneBounds();
		ariaMin = Math.round(b.min);
		ariaMax = Math.round(b.max);
	}

	// Fit the pane to its real container: once synchronously before paint (so a
	// direct `?item=` load never flashes an over-wide width) and then on every
	// container resize via a ResizeObserver. Also samples the rendered width for
	// ARIA. Effect re-runs when the pane mounts (paneEl binds).
	$effect(() => {
		if (!browser || !openItemRef || !paneEl) return;
		const el = paneEl;
		const container = el.parentElement;
		if (!container) return;
		// Synchronous FIRST fit against the real container, BEFORE the browser
		// paints. This effect runs after the pane is in the DOM but within the
		// same update cycle (pre-paint), so on a direct `/{ws}/{coll}?item=REF`
		// load it corrects the init clamp — which used `window.innerWidth` and
		// therefore over-counted by the ~260px sidebar — against the true
		// flex-row width with NO visible jump. Waiting for the observer's first
		// (async, post-paint) callback is what let an over-wide stored width
		// flash before snapping (TASK-2114 hydration-reflow criterion; coord P2).
		// Runs regardless of ResizeObserver support (needs only
		// getBoundingClientRect). `untrack` so reading storedPaneWidth/paneEl
		// inside the fit doesn't add them as deps of this observer effect (which
		// would re-create the observer on every drag).
		untrack(() => {
			refitPaneWidth();
			measuredPaneWidth = el.getBoundingClientRect().width;
		});
		// Observe the flex-row container so pane width refits on window resize
		// AND sidebar toggles (which resize the container without a window
		// `resize` event). Resizing the pane itself doesn't change the container
		// width, so this can't loop.
		if (typeof ResizeObserver === 'undefined') return;
		const ro = new ResizeObserver(() => {
			refitPaneWidth();
			measuredPaneWidth = el.getBoundingClientRect().width;
		});
		ro.observe(container);
		return () => ro.disconnect();
	});

	// Move focus INTO the pane on open — MOBILE ONLY (PLAN-2105 / TASK-2122).
	// The mobile overlay is a modal, so focus enters it (and the trap below keeps
	// it there). On the DESKTOP split we deliberately do NOT move focus: it stays
	// on the list so arrow/j-k navigation (with the pane following) is
	// uninterrupted; the user bridges INTO the pane with Tab and back with ESC
	// (handled in the keydown handler). Focus lands on the `<aside>` region
	// itself (tabindex=-1) — a screen reader announces the aria-labeled region
	// and Tab proceeds into its controls. Re-runs on mount/unmount and on a
	// breakpoint crossing (desktop→mobile enters the modal); `preventScroll`
	// avoids a jump on a direct `?item=` deep-link; skips if focus is already
	// inside.
	$effect(() => {
		if (!browser) return;
		const el = paneEl;
		if (!el || !viewport.isMobile) return;
		if (!el.contains(document.activeElement)) {
			el.focus({ preventScroll: true });
		}
	});

	// Mobile focus trap (PLAN-2105 / TASK-2122). At ≤768px the pane is a
	// full-screen overlay (modal semantics), so focus must stay WITHIN it. On the
	// desktop split we deliberately do NOT trap — the list must stay reachable
	// (Tab-out + j/k depend on it). Re-runs when the pane mounts/unmounts OR the
	// viewport crosses the breakpoint, engaging/releasing the trap accordingly.
	//
	// Two backstops, both WINDOW/DOCUMENT-level (not pane-scoped) so they still
	// fire after focus has already escaped the pane:
	//  • Tab keydown — cycles focus within the pane's tabbables (smooth, no
	//    flicker) via `nextTrapTarget`.
	//  • focusin — the catch-all: ANY programmatic focus escape (Cmd+F focusing
	//    the search box behind the overlay, a removed control dropping focus to
	//    <body>) is pulled straight back into the pane (Codex P1 — Tab alone
	//    can't see these).
	// Both DEFER to a native modal <dialog> (Share / Edit Collection / the
	// Open-Children confirm — `Modal.svelte` uses `showModal()`), which renders
	// in the top layer above the overlay and owns its own focus cycle. The pane
	// itself now carries `role="dialog"` on mobile (TASK-2131), so the exempt
	// selector excludes it via `:not(.item-pane)` — the trap must NOT treat the
	// region it's guarding as an exempt surface; role="dialog" sheets (BottomSheet)
	// sit BELOW the pane and stay behind it, so focus belongs on the pane, not them.
	$effect(() => {
		if (!browser) return;
		if (!paneEl || !viewport.isMobile) return;
		// Bind the narrowed, non-null element so the nested handlers capture
		// `HTMLElement` (TS re-widens a `$state` read back to `| null` across a
		// closure boundary otherwise).
		const region = paneEl;
		// Surfaces that OWN their own focus/keyboard and legitimately overlay the
		// pane are exempt from the trap so it neither hijacks their Tab nor yanks
		// focus off them the instant they open (native <dialog>, [role="dialog"]
		// BottomSheets, [role="menu"] / [role="listbox"], the editor block context
		// menu). Recognised by role/tag/class so pane-owned popups that portal out
		// to <body> are all covered — the shared `inExemptSurface` set (paneFocus.ts)
		// is the SAME one the host's focus-follows classifier reuses (PLAN-2179).
		// (Focus already inside `.item-pane` is handled by the `region.contains`
		// check at each call site.)
		function onTrapKeydown(e: KeyboardEvent) {
			if (e.key !== 'Tab' || e.defaultPrevented) return;
			if (inExemptSurface(e.target as Element | null)) return;
			const target = nextTrapTarget(
				paneFocusables(region),
				document.activeElement,
				e.shiftKey,
				region,
			);
			if (target) {
				e.preventDefault();
				target.focus({ preventScroll: true });
			}
		}
		function onFocusIn(e: FocusEvent) {
			const t = e.target as Element | null;
			if (!t || region.contains(t) || inExemptSurface(t)) return;
			region.focus({ preventScroll: true });
		}
		window.addEventListener('keydown', onTrapKeydown);
		document.addEventListener('focusin', onFocusIn);
		return () => {
			window.removeEventListener('keydown', onTrapKeydown);
			document.removeEventListener('focusin', onFocusIn);
		};
	});

	// App-shell isolation for the mobile modal (PLAN-2105 / TASK-2131). The
	// focus trap + `aria-modal` above keep KEYBOARD focus in the overlay, but a
	// JS trap can't constrain a screen-reader virtual cursor and `aria-modal`
	// alone is unevenly honored — so the app-shell chrome the layout renders
	// (`MobileContextBar` + `BottomNav`) must physically leave the a11y tree
	// while the overlay is up. That chrome lives in `+layout.svelte`, ABOVE this
	// host, so we can't `inert` it by prop; instead register the active overlay
	// in a shared store the layout reads (the list column, in-DOM here, is
	// already inerted directly). Gated on the SAME condition as the trap
	// (`viewport.isMobile && openItemRef`); the cleanup releases the ref on
	// unmount and on any dep change (breakpoint crossing, `openItemRef`
	// clearing), so desktop and closed states never inert the chrome.
	$effect(() => {
		if (!browser) return;
		if (!openItemRef || !viewport.isMobile) return;
		paneOverlay.enter();
		return () => paneOverlay.leave();
	});

	// Desktop focusin backstop (PLAN-2154 Architecture C / R1, TASK-2162) — the
	// NARROWED desktop mirror of the mobile trap's focusin catch-all above.
	// `focusPaneRegion` (fired synchronously at each hop) is the primary R1
	// mechanism; this is the async safety net for a focus drop that lands
	// LATER than that synchronous focus — e.g. an inner `{#key itemSlug}`
	// subtree, or EditorLinkPopover's anchor, being removed a tick after the
	// hop. It re-pulls focus into the pane ONLY when focus DROPS to `<body>`
	// (the R1 removed-control case) AND the focus it dropped FROM was inside
	// `.item-pane`. It must NOT fire for a deliberate Tab / click OUT to the
	// list — the desktop split keeps the list reachable — so any outside
	// target that ISN'T bare `<body>` (a real row/control) is left untouched.
	//
	// `focusin`'s own `relatedTarget` (the blurring element) is unreliable in
	// exactly the R1 case — the element was REMOVED, so it's already gone — so
	// we remember whether the last settled focus was inside the pane ourselves
	// rather than reading it off the event.
	//
	// Focus-follows-editing reconciliation (PLAN-2179 DR-2 / TASK-2181): on the
	// full-page host this backstop must DEFER to `activePane`. Clicking
	// non-focusable MASTER text drops focus to `<body>`; if the last focus was in
	// the pane this backstop would yank it straight back — fighting "click master
	// → activate master". So the pull-back only fires while the PANE is the active
	// side (`activePane !== 'master'`). When `activePane` is UNSET (the collection
	// route never passes it) `undefined !== 'master'` is true → the original
	// always-pull-back R1 behavior is byte-identical there.
	$effect(() => {
		if (!browser) return;
		if (!paneEl || viewport.isMobile || !openItemRef) return;
		const region = paneEl;
		let lastFocusWasInPane = region.contains(document.activeElement);
		function onFocusIn(e: FocusEvent) {
			const t = e.target as Element | null;
			if (t && region.contains(t)) {
				lastFocusWasInPane = true;
				return;
			}
			// Dropped to <body> out of the pane → pull it back into the stable
			// region, but ONLY while the pane is the active side (else a click into
			// the master would be immediately overridden). `region.focus()` re-fires
			// focusin with the region as target, which the branch above treats as
			// "inside" (no loop). Read `activePane` live at event time (not tracked
			// by this effect) so the listener isn't re-registered on every flip.
			if (t === document.body && lastFocusWasInPane && activePane !== 'master') {
				region.focus({ preventScroll: true });
				return;
			}
			// A real element outside the pane (a list row/control the user Tabbed
			// or clicked to), OR a body drop while the MASTER is active — legitimate
			// on the desktop split; stand down.
			lastFocusWasInPane = false;
		}
		document.addEventListener('focusin', onFocusIn);
		return () => document.removeEventListener('focusin', onFocusIn);
	});

	// Safety net for an interrupted drag: if the pane closes (ESC / Back / nav)
	// mid-drag, this whole shell unmounts before pointerup/lostpointercapture
	// fires, leaving text selection disabled and the col-resize cursor stuck
	// across the whole app. Restore the global drag chrome on destroy if a drag
	// was still in flight (PLAN-2105 / TASK-2114; the pre-extraction route did
	// this from an `openItemRef`→null effect — the shell's unmount IS that
	// transition now).
	onDestroy(() => {
		if (resizingPane && browser) {
			document.body.style.userSelect = '';
			document.body.style.cursor = '';
		}
	});

	function onDividerPointerDown(e: PointerEvent) {
		// Left button / touch / pen only; ignore right-click etc.
		if (e.button !== 0) return;
		// Stop the browser from starting a text selection or handing the
		// gesture to the list (row-click nav) mid-drag.
		e.preventDefault();
		resizingPane = true;
		const divider = e.currentTarget as HTMLElement;
		divider.setPointerCapture(e.pointerId);
		document.body.style.userSelect = 'none';
		document.body.style.cursor = 'col-resize';
	}

	function onDividerPointerMove(e: PointerEvent) {
		if (!resizingPane || !paneEl) return;
		// The pane is right-docked, so its width is (its right edge − pointer
		// x). Reading the live rect keeps this correct regardless of page
		// padding or the pane's current size.
		const rect = paneEl.getBoundingClientRect();
		setPaneWidth(rect.right - e.clientX);
	}

	function endPaneResize(e: PointerEvent) {
		if (!resizingPane) return;
		resizingPane = false;
		const divider = e.currentTarget as HTMLElement;
		try {
			divider.releasePointerCapture(e.pointerId);
		} catch {}
		document.body.style.userSelect = '';
		document.body.style.cursor = '';
	}

	// The pane's actual current width — the applied width if set, else the
	// real rendered size (the CSS clamp() default), so the FIRST keyboard
	// nudge moves from where the pane visibly is rather than an assumed
	// default that could be on the wrong side of it (Codex round 1 P2).
	function currentPaneWidth(): number {
		if (paneWidth != null) return paneWidth;
		if (browser && paneEl) return paneEl.getBoundingClientRect().width;
		return PANE_WIDTH_DEFAULT;
	}

	// Keyboard resize for the focusable separator (arrows nudge the width).
	// ArrowLeft grows the pane (divider moves left), ArrowRight shrinks it,
	// matching the drag direction.
	function onDividerKeydown(e: KeyboardEvent) {
		const step = e.shiftKey ? 32 : 16;
		const current = currentPaneWidth();
		if (e.key === 'ArrowLeft') {
			e.preventDefault();
			setPaneWidth(current + step);
		} else if (e.key === 'ArrowRight') {
			e.preventDefault();
			setPaneWidth(current - step);
		}
	}
</script>

<!-- Draggable divider (PLAN-2105 / TASK-2114). Pointer-captured so a
     drag stays on the handle (never selects text or fires a row-click)
     and also keyboard-adjustable via the arrow keys. This is the ARIA
     "window splitter" pattern — a focusable separator IS interactive,
     but Svelte's a11y heuristic treats every separator as
     non-interactive, so the tabindex + pointer/key handlers are
     legitimately ignored here. -->
<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<div
	class="pane-divider"
	class:resizing={resizingPane}
	role="separator"
	tabindex="0"
	aria-orientation="vertical"
	aria-label="Resize detail pane"
	aria-valuemin={ariaMin}
	aria-valuemax={ariaMax}
	aria-valuenow={Math.min(ariaMax, Math.max(ariaMin, Math.round(paneWidth ?? measuredPaneWidth ?? PANE_WIDTH_DEFAULT)))}
	onpointerdown={onDividerPointerDown}
	onpointermove={onDividerPointerMove}
	onpointerup={endPaneResize}
	onlostpointercapture={endPaneResize}
	onkeydown={onDividerKeydown}
></div>
<!--
	The detail pane. CRITICAL (PLAN-2105 / TASK-2112): NO {#key} wrapper.
	A→B item switch must be a PROP UPDATE (`ref` change re-drives
	ItemDetail's loadData + collabKey via its own reactive effects,
	reusing the mounted instance + its single SSE subscription). A
	{#key} would silently full-remount on every switch and defeat the
	whole design. Open/close (the host's outer {#if openItemRef}) is the
	ONLY mount/unmount. `onNavigateAway` handles the collection-rename case;
	onClose/onGone clear only `?item=`, preserving view/sort/filter/tags/search.

	`--pane-width` carries the persisted width (TASK-2114); undefined
	until localStorage is read so the CSS clamp() default applies on
	first paint when there's no stored value.
-->
<!--
	`aria-label` names the pane so a screen reader announces the region on
	entry (TASK-2122). `tabindex="-1"` makes it a programmatic focus target
	(not a Tab stop): Tab from the list moves focus here, and the mobile
	overlay moves focus here on open (then traps it). Desktop keeps focus on
	the list so j/k navigation is uninterrupted. Focus-in (mobile), the Tab
	bridge, ESC return-to-list, and the mobile focus trap are wired in the
	effects + the host's keydown handler.

	Role is viewport-dependent (PLAN-2105 / TASK-2131). On the DESKTOP split
	the pane is a non-modal companion to the list, so it stays a bare `<aside>`
	(implicit `complementary` landmark) with no `aria-modal`. On MOBILE it's a
	full-screen overlay covering everything, so it becomes a `role="dialog"`
	`aria-modal="true"` modal — paired with the app-shell chrome being inerted
	(the overlay effect above) and the mobile focus trap for a complete modal.
	`aria-label` supplies the required accessible name in the dialog case.
-->
<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
<aside
	class="item-pane"
	bind:this={paneEl}
	tabindex="-1"
	aria-label="Item detail"
	role={viewport.isMobile ? 'dialog' : undefined}
	aria-modal={viewport.isMobile ? 'true' : undefined}
	style={paneWidth != null ? `--pane-width: ${paneWidth}px` : undefined}
>
	{#if paneMintForRoute}
		<!--
			Nested (not `{#key}`) on `paneMintForRoute`, not raw `openItemRef`
			(PLAN-2154 / TASK-2166; Codex review): while the pane is ALREADY
			open on the SAME route, `paneMintForRoute` (== `paneMintRef`) stays
			pinned at the last-settled ref through an entire popstate burst, so
			this branch stays mounted and `ref` never flips mid-burst — the
			coalescing itself. It only actually mounts/unmounts at a genuine
			open (closed → first settle) or close (`openItemRef` going null
			tears down the whole host `{#if}` around this shell) — OR a route
			change, where `paneMintForRoute` falls through to the live
			`openItemRef` instead (see the host's declaration). The host owns
			`paneMintForRoute`; this shell just renders it.
		-->
		<ItemDetail
			ref={paneMintForRoute}
			embedded
			peeking={activePane === 'master'}
			{username}
			{wsSlug}
			{collSlug}
			onClose={onClose}
			onGone={onGone}
			onNavigateAway={onNavigateAway}
			onOpenTarget={onOpenTarget}
			onBack={onBack}
		/>
	{/if}
</aside>

<style>
	.item-pane {
		/* Width comes from the persisted `--pane-width` CSS var (TASK-2114);
		   the clamp() is the fallback before localStorage is read / when no
		   width was ever stored. The .pane-divider (not this border) is the
		   resting separator on desktop. */
		flex: 0 0 var(--pane-width, clamp(360px, 38%, 640px));
		min-width: 0;
		overflow-y: auto;
		background: var(--bg-primary);
	}

	/* Draggable resize handle between the list column and the pane
	   (PLAN-2105 / TASK-2114). The 6px-wide element is the grab target; a
	   centered pseudo-element draws the resting 1px separator (replacing the
	   pane's old border-left) and thickens to an accent line on hover, focus,
	   or active drag. */
	.pane-divider {
		flex: 0 0 6px;
		align-self: stretch;
		position: relative;
		background: transparent;
		cursor: col-resize;
		/* No native text selection or touch scroll/gesture while dragging. */
		user-select: none;
		touch-action: none;
	}
	.pane-divider:focus-visible {
		outline: none;
	}
	.pane-divider::after {
		content: '';
		position: absolute;
		top: 0;
		bottom: 0;
		left: 50%;
		width: 1px;
		transform: translateX(-50%);
		background: var(--border);
		transition: background 0.12s, width 0.12s;
	}
	.pane-divider:hover::after,
	.pane-divider:focus-visible::after,
	.pane-divider.resizing::after {
		background: var(--accent-blue);
		width: 2px;
	}

	/* ── Mobile: full-screen overlay (PLAN-2105 Phase 4 / TASK-2121) ──────
	   At ≤768px (the app-wide mobile breakpoint from breakpoint.svelte.ts,
	   kept in lockstep with this media query) there is no room to split, so
	   the detail pane covers the viewport as a full-screen overlay with the
	   list column left mounted BEHIND it — its state + scroll position are
	   preserved (the PLAN-2105 no-remount invariant), it's just hidden by
	   the opaque overlay. Mirrors the graph drawer's mobile pattern
	   (ItemDetail `.graph-drawer`: fixed, `width: 100vw`, no border).

	   Pane visibility stays URL-derived (`?item=`); this overlay is pure
	   presentation keyed off the viewport, NOT the vestigial
	   `uiStore.detailPanelOpen` boolean — whose mobile-entry force-close
	   must never reach `?item=` (see the reconciliation note in
	   ui.svelte.ts). So crossing the 768px boundary only swaps
	   split ⇄ overlay; it never closes the pane or drops the open item.

	   `position: fixed` is viewport-relative here because no ancestor
	   (.collection-page / .main-content / .app-shell) establishes a
	   containing block via transform/filter/contain. z-index sits above
	   the mobile chrome (BottomNav + MobileContextBar at 40) and below app
	   modals / toasts / lightbox (99–100) so their global feedback still
	   stacks over the item view. */
	@media (max-width: 768px) {
		.item-pane {
			position: fixed;
			inset: 0;
			width: 100vw;
			z-index: 60;
			border-left: 0;
		}
		/* The vertical resize handle can't split a full-screen overlay. */
		.pane-divider {
			display: none;
		}
	}
</style>
