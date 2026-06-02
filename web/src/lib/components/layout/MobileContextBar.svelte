<!--
	MobileContextBar — slim contextual top bar for detail screens (PLAN-1694
	Phase 2, TASK-1700).

	With the global mobile <TopBar /> retired inside workspaces, root/tab
	screens render full-height (no top chrome). Drill-in "detail" screens still
	need a back affordance + title — that's this bar. Shown only on mobile and
	only when the path is ≥2 segments deep past the workspace prefix (item
	detail `/coll/slug`, tag detail `/tags/tag`, playbook editor, …). Root tabs
	and collection lists (depth 0-1) have their own in-page headers.

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

	// Segments past the workspace prefix. depth ≥ 2 ⇒ a drill-in detail screen.
	let segments = $derived.by(() => {
		const path = page.url.pathname;
		if (!wsPrefix || !path.startsWith(`${wsPrefix}/`)) return [];
		return path.slice(wsPrefix.length + 1).split('/').filter(Boolean);
	});
	let isDetail = $derived(segments.length >= 2);
	let show = $derived(uiStore.isMobile && isDetail);

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
		<h1 class="cb-title">{title}</h1>
		<span class="cb-spacer" aria-hidden="true"></span>
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
		width: 36px;
		height: 36px;
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
		font-size: 1em;
		font-weight: 600;
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	/* Balance the back button so the title sits optically centered. */
	.cb-spacer {
		flex: 0 0 auto;
		width: 36px;
	}
</style>
