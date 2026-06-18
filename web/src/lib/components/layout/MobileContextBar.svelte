<!--
	MobileContextBar — the unified mobile top bar (PLAN-1694 Phase 2,
	TASK-1700; generalized to all screens in IDEA-1835).

	Originally a detail-only back+title bar. IDEA-1835 promoted it to a
	persistent top bar shown on EVERY mobile screen inside a workspace so the
	workspace switcher (moved here from the bottom nav) is always visible and
	the collection/item name reads the same everywhere. Layout is
	`[back?] [title] [workspace switcher]`:
	  - back affordance shows only when there's a parent to return to (i.e.
	    not on the workspace home/root, where `segments` is empty);
	  - title is the page's own title (item ref → section → humanized last
	    segment), so collection lists show the collection name and item
	    details show the ref — no per-page in-page header needed;
	  - the workspace switcher (name + chevron) opens a BottomSheet to switch.

	Mounted in the workspace layout next to <BottomNav />. While shown it
	toggles `body.has-context-bar`, which app.css uses to pad .main-content top.
-->
<script lang="ts">
	import { onDestroy } from 'svelte';
	import { page } from '$app/state';
	import { goto, afterNavigate } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { titleStore } from '$lib/stores/title.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import WorkspaceSwitcher from '$lib/components/layout/WorkspaceSwitcher.svelte';

	// Count in-app navigations so goBack() knows whether history.back() stays
	// inside Pad. history.length is unreliable — a deep link opened from
	// another site/app still has prior history (Codex review, PLAN-1694).
	let inAppNavs = $state(0);
	afterNavigate((nav) => {
		if (nav.type !== 'enter') inAppNavs += 1;
	});

	function humanize(seg: string | undefined): string {
		if (!seg) return '';
		let s = seg;
		try {
			s = decodeURIComponent(seg);
		} catch {
			// leave as-is on malformed escape sequences
		}
		return s.replace(/[-_]/g, ' ');
	}

	let wsSlug = $derived(workspaceStore.current?.slug);
	let wsUsername = $derived(workspaceStore.current?.owner_username ?? '');
	let wsPrefix = $derived(wsUsername && wsSlug ? `/${wsUsername}/${wsSlug}` : '');

	// Segments past the workspace prefix. Empty ⇒ the workspace home/root.
	let segments = $derived.by(() => {
		const path = page.url.pathname;
		if (!wsPrefix || !path.startsWith(`${wsPrefix}/`)) return [];
		return path.slice(wsPrefix.length + 1).split('/').filter(Boolean);
	});

	// Shown on every mobile screen inside a workspace (IDEA-1835) — the home
	// (path === wsPrefix) plus any deeper route. The back affordance is gated
	// separately so the root screen doesn't offer a dead-end "back".
	let inWorkspace = $derived(
		!!wsPrefix &&
			(page.url.pathname === wsPrefix || page.url.pathname.startsWith(`${wsPrefix}/`))
	);
	let show = $derived(uiStore.isMobile && inWorkspace);
	let canGoBack = $derived(segments.length >= 1);

	// Deterministic parent (drop the last path segment) — used as a safe
	// fallback when there's no in-app history to go back to (deep link).
	let parentHref = $derived.by(() => {
		const parts = page.url.pathname.split('/');
		parts.pop();
		return parts.join('/') || wsPrefix || '/';
	});

	// Prefer the page's own title; fall back to the section, then a humanized
	// last path segment so pages that don't wire titleStore (e.g. /tags/[tag],
	// playbook detail) still show context instead of a blank bar.
	let title = $derived(titleStore.item ?? titleStore.section ?? humanize(segments.at(-1)));

	$effect(() => {
		if (typeof document === 'undefined') return;
		document.body.classList.toggle('has-context-bar', show);
	});
	onDestroy(() => {
		if (typeof document !== 'undefined') document.body.classList.remove('has-context-bar');
	});

	function goBack() {
		// history.back() restores the list's scroll/filter state when we got
		// here via in-app navigation; fall back to the computed parent on a
		// deep link (no in-app history to pop). (Codex review, PLAN-1694.)
		if (inAppNavs > 0 && typeof history !== 'undefined') {
			history.back();
		} else {
			goto(parentHref);
		}
	}
</script>

{#if show}
	<header class="context-bar">
		{#if canGoBack}
			<button class="cb-back" type="button" onclick={goBack} aria-label="Back">
				<svg width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
					<path
						d="M12.5 4L6.5 10L12.5 16"
						stroke="currentColor"
						stroke-width="1.8"
						stroke-linecap="round"
						stroke-linejoin="round"
					/>
				</svg>
			</button>
		{/if}
		<h1 class="cb-title">{title}</h1>
		<div class="cb-ws">
			<WorkspaceSwitcher mobile={true} />
		</div>
	</header>
{/if}

<style>
	.context-bar {
		position: fixed;
		top: 0;
		left: 0;
		right: 0;
		z-index: 40; /* same band as the bottom nav; opposite edge */
		display: flex;
		align-items: center;
		gap: var(--space-2);
		height: calc(var(--context-bar-height) + env(safe-area-inset-top, 0px));
		padding-top: env(safe-area-inset-top, 0px);
		padding-left: var(--space-2);
		padding-right: var(--space-2);
		background: var(--bg-secondary);
		border-bottom: 1px solid var(--border);
	}
	.cb-back {
		flex: 0 0 auto;
		display: flex;
		align-items: center;
		justify-content: center;
		width: 40px;
		height: 40px;
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		cursor: pointer;
	}
	.cb-back:hover {
		background: var(--bg-hover);
	}
	.cb-title {
		flex: 1;
		min-width: 0;
		margin: 0;
		font-size: 1.15em;
		font-weight: 600;
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	/* Workspace switcher, right-anchored. Capped so a long workspace name
	   can't crowd out the title — the switcher's own `.name` ellipsizes. */
	.cb-ws {
		flex: 0 1 auto;
		min-width: 0;
		max-width: 62%;
	}
	/* The embedded <WorkspaceSwitcher> paints its trigger with the same
	   --bg-secondary as this bar, so it'd vanish. Lift it onto --bg-tertiary
	   and size it to fill the bar's height (without growing the bar) so it
	   reads as a substantial control rather than a small floating chip. */
	.cb-ws :global(.current) {
		max-width: 100%;
		min-height: 34px;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		font-size: 1em;
	}
	.cb-ws :global(.current:hover) {
		background: var(--bg-hover);
	}
</style>
