<script lang="ts">
	// The item pane on home (TASK-4902): a ref clicked in the Agents panel opens
	// the item in the right-docked PaneHost beside the panel, in its OWN
	// workspace's context. Home is outside every workspace route, so the URL
	// carries the item's owner, workspace and collection in `item_in` beside
	// `?item=`, and the pane gets a workspace scope fetched for that workspace
	// (HomePaneScope) rather than the shell workspace the global stores describe.
	//
	// Always mounted on home. It owns the pane controller, the mint settle, ESC,
	// focus return, click-away and the `{#key}` that remounts the pane when the
	// minted workspace changes. A same-workspace item switch stays a prop update
	// (PLAN-2105: no `{#key}` across item switches).
	import { page, navigating } from '$app/state';
	import { afterNavigate, goto } from '$app/navigation';
	import { browser } from '$app/env';
	import { onDestroy, untrack } from 'svelte';
	import PaneHost from '$lib/components/collections/PaneHost.svelte';
	import HomePaneScope from './HomePaneScope.svelte';
	import { createPaneController } from '$lib/collections/paneHostController';
	import { planFullPageNavigateAway } from '$lib/collections/paneController';
	import { createPaneMintSettle, PANE_MINT_SETTLE_MS } from '$lib/collections/paneMintSettle';
	import { dismissPaneOnBackgroundClick } from '$lib/collections/paneDismiss';
	import { isTextEntryTarget } from '$lib/a11y/textEntryTarget';
	import { hasForeignEscapeOwner } from '$lib/a11y/viewerBackdrop';
	import { runTopEscape, topEscapePriority, ESCAPE_PRIORITY } from '$lib/stores/escapeStack';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { ITEM_IN_PARAM, formatItemIn, homePaneKey, parseItemIn, type HomePaneTarget } from './homePane';

	let openItemRef = $derived(page.url.searchParams.get('item'));
	let itemIn = $derived(parseItemIn(page.url.searchParams.get(ITEM_IN_PARAM)));

	// The mint settle (TASK-2166) coalesces a Back/Forward burst. It is fed the
	// PAIR `item_in + NUL + item`, so a burst can never pair one entry's
	// workspace with another entry's ref.
	const SEP = '\u0000';
	function mintOf(url: { searchParams: { get(name: string): string | null } }): string | null {
		const ref = url.searchParams.get('item');
		return ref ? `${url.searchParams.get(ITEM_IN_PARAM) ?? ''}${SEP}${ref}` : null;
	}
	let paneMint = $state<string | null>(mintOf(page.url));
	let paneMintPathname: string = page.url.pathname;
	const paneMintSettle = createPaneMintSettle({
		settleMs: PANE_MINT_SETTLE_MS,
		onSettle: (m) => {
			paneMint = m;
		},
		deferWhile: () => navigating.type !== null,
	});
	afterNavigate((nav) => {
		if (nav.shallow) return;
		const samePathname = !!nav.from && nav.to?.url.pathname === nav.from.url.pathname;
		paneMintPathname = nav.to?.url.pathname ?? paneMintPathname;
		paneMintSettle.onNavigate(samePathname ? nav.type : 'goto', nav.to ? mintOf(nav.to.url) : null);
	});
	let mintForRoute = $derived(page.url.pathname === paneMintPathname ? paneMint : mintOf(page.url));
	let mintRef = $derived(mintForRoute ? mintForRoute.slice(mintForRoute.indexOf(SEP) + 1) : null);
	let mintIn = $derived(mintForRoute ? parseItemIn(mintForRoute.slice(0, mintForRoute.indexOf(SEP))) : null);
	let mintScopeKey = $derived(mintIn ? `${mintIn.owner}/${mintIn.workspace}` : '');

	// Missing or malformed `item_in`: no pane, and the URL is left alone.
	let showPane = $derived(!!openItemRef && !!itemIn && !!mintRef && !!mintIn);

	let paneHostEl = $state<PaneHost | undefined>();

	// Focus return (TASK-2122's rules, home's targets). Plain lets: written by
	// the controller's first-open capture and by the close tracker below, never
	// read reactively.
	let pendingTarget: HomePaneTarget | null = null;
	let returnFocusEl: HTMLElement | null = null;
	let returnAgent: string | null = null;

	function agentToggleFor(el: Element | null): string | null {
		for (let n: Element | null = el; n; n = n.parentElement) {
			const toggle = n.querySelector(':scope > [data-agent-toggle]');
			if (toggle) return toggle.getAttribute('data-agent-toggle');
		}
		return null;
	}

	function refLinkFor(key: string): HTMLElement | null {
		return document.querySelector<HTMLElement>(`[data-pane-key="${CSS.escape(key)}"]`);
	}

	function captureReturnFocus() {
		if (!browser) return;
		const active = document.activeElement as HTMLElement | null;
		returnFocusEl = active && active !== document.body ? active : null;
		const t = pendingTarget;
		const link = returnFocusEl?.closest('[data-pane-key]') ?? (t ? refLinkFor(homePaneKey(t.owner, t.workspace, t.ref)) : null);
		returnAgent = agentToggleFor(link ?? null);
	}

	function returnFocus(closedKey: string | null) {
		const pane = paneHostEl?.getPaneRegion() ?? null;
		const captured = returnFocusEl && returnFocusEl.isConnected && !pane?.contains(returnFocusEl) ? returnFocusEl : null;
		const link = closedKey ? refLinkFor(closedKey) : null;
		const agent = agentToggleFor(link) ?? returnAgent;
		const toggle = agent ? document.querySelector<HTMLElement>(`[data-agent-toggle="${CSS.escape(agent)}"]`) : null;
		const target =
			captured ??
			link ??
			toggle ??
			document.querySelector<HTMLElement>('section[aria-label="Agents"][tabindex="-1"]');
		returnFocusEl = null;
		returnAgent = null;
		target?.focus();
	}

	const paneController = createPaneController({
		getOpenItemRef: () => openItemRef,
		cancelFollow: () => {},
		focusPaneRegion: () => paneHostEl?.focusPaneRegion(),
		captureReturnFocus,
		setBypassNavGuard: () => {},
		companionParam: ITEM_IN_PARAM,
	});
	const { openItemPaneByRef, navigatePaneTo, handleOpenTarget, handlePaneBack, closeItemPane, currentPaneState, paneNavInFlight, clearPaneGo } =
		paneController;

	/** Open `target` in the pane (a desktop ref click on home). */
	export function open(target: HomePaneTarget) {
		pendingTarget = target;
		openItemPaneByRef(target.ref, formatItemIn(target));
		pendingTarget = null;
	}

	/** The click-away dismiss, for the home column's `onclick`. */
	export function dismissOnBackgroundClick(e: MouseEvent) {
		dismissPaneOnBackgroundClick(e, { open: showPane, mobile: viewport.isMobile, close: closeItemPane });
	}

	// An item move inside the pane retargets it; a collection rename needs no
	// URL change, since the pane trusts the item's own collection.
	function handleNavigateAway(url: string) {
		const plan = planFullPageNavigateAway(url);
		if (plan.kind === 'retarget') navigatePaneTo(plan.target);
	}

	// Close tracker: on a truthy -> null `?item=` on home (ESC, the close
	// button, click-away, Back, a deleted item), return focus. Plain-let
	// trackers; the effect writes no state.
	const hostPathname = page.url.pathname;
	// The effect's first run records the opening state.
	let prevOpen: string | null = null;
	let prevKey: string | null = null;
	$effect(() => {
		const current = openItemRef;
		const inn = itemIn;
		const onHome = page.url.pathname === hostPathname;
		const justClosed = !!prevOpen && !current && onHome;
		const closedKey = prevKey;
		prevOpen = current;
		if (current && inn) prevKey = homePaneKey(inn.owner, inn.workspace, current);
		if (!justClosed || !browser) return;
		prevKey = null;
		untrack(() => returnFocus(closedKey));
	});

	// An identity change closes the pane in place; the scope drops its answer
	// on the same change.
	function closeForIdentity() {
		const url = new URL(page.url.href);
		if (!url.searchParams.has('item') && !url.searchParams.has(ITEM_IN_PARAM)) return;
		url.searchParams.delete('item');
		url.searchParams.delete(ITEM_IN_PARAM);
		void goto(`${url.pathname}${url.search}`, { replaceState: true, reset: false });
	}
	onDestroy(authStore.onIdentityChange(closeForIdentity));

	// ESC, copied from the full-page item host: text entry, chat and foreign
	// owners keep their own ESC; repeats are swallowed; at depth > 0 ESC goes
	// Back one drill, otherwise the escape stack (the pane's layer) closes.
	function handleHostKeydown(e: KeyboardEvent) {
		if (e.defaultPrevented) return;
		if (e.key !== 'Escape') return;
		const target = e.target as HTMLElement | null;
		if (isTextEntryTarget(target)) return;
		if (target?.closest?.('[data-chat-host]')) return;
		if (hasForeignEscapeOwner()) return;
		if (e.repeat) {
			e.preventDefault();
			return;
		}
		if (openItemRef && topEscapePriority() === ESCAPE_PRIORITY.pane && currentPaneState().paneDepth > 0) {
			e.preventDefault();
			if (!paneNavInFlight()) handlePaneBack();
			return;
		}
		if (runTopEscape(e)) e.preventDefault();
	}

	onDestroy(() => {
		clearPaneGo();
		paneMintSettle.cancel();
	});
</script>

<svelte:window onkeydown={handleHostKeydown} />

{#if showPane && mintIn && mintRef}
	{#key mintScopeKey}
		<HomePaneScope owner={mintIn.owner} workspace={mintIn.workspace}>
			<PaneHost
				bind:this={paneHostEl}
				{openItemRef}
				username={mintIn.owner}
				wsSlug={mintIn.workspace}
				collSlug={mintIn.collection}
				paneMintForRoute={mintRef}
				onClose={closeItemPane}
				onGone={closeItemPane}
				onNavigateAway={handleNavigateAway}
				onOpenTarget={handleOpenTarget}
				onBack={handlePaneBack}
			/>
		</HomePaneScope>
	{/key}
{/if}
