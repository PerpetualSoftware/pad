<!--
	BottomNav — mobile-only persistent bottom navigation (PLAN-1694).

	Five slots: Workspace · Search · Quick-capture (center ＋) · Activity · You.
	"Workspace", "You" and the center ＋ open docked sheets (DockedSheet) that
	anchor ABOVE this bar, so the nav stays visible and the active slot stays lit:
	  - WorkspaceSheet — switcher card + Navigate + Collections.
	  - YouSheet — account (profile, theme, settings, sign out, …).
	  - QuickCaptureSheet — pick a collection, type a title, create (BUG-1765).
	Together with the contextual bar they replace the retired mobile <TopBar />
	inside a workspace (PLAN-1694 Phase 2-3, redesigned in TASK-1701).

	Rendered in the workspace layout; shows only on mobile (uiStore.isMobile).
	Toggles `body.has-bottom-nav` so app.css reflows .main-content.
-->
<script lang="ts">
	import { onDestroy } from 'svelte';
	import { page } from '$app/state';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { avatarColor, avatarInitial } from '$lib/utils/avatar';
	import { getActiveKey } from '$lib/nav/destinations';
	import QuickCaptureSheet from '$lib/components/layout/QuickCaptureSheet.svelte';
	import WorkspaceSheet from '$lib/components/layout/WorkspaceSheet.svelte';
	import YouSheet from '$lib/components/layout/YouSheet.svelte';

	let wsSlug = $derived(workspaceStore.current?.slug);
	let wsUsername = $derived(workspaceStore.current?.owner_username ?? '');
	let wsPrefix = $derived(wsUsername && wsSlug ? `/${wsUsername}/${wsSlug}` : '');
	let wsName = $derived(workspaceStore.current?.name ?? 'Workspace');
	let activeKey = $derived(getActiveKey(page.url.pathname, wsPrefix));

	let workspaceOpen = $state(false);
	let youOpen = $state(false);
	let captureOpen = $state(false);

	// "Workspace" owns all in-workspace navigation except Activity (its own
	// slot), so light it whenever the active page is workspace content.
	let onWorkspaceContent = $derived(!!activeKey && activeKey !== 'activity');

	// Tap toggles: a second tap on the active slot closes its surface. Opening
	// any of the four surfaces (Workspace / You / Search / Quick-capture)
	// closes the others (BUG-1765).
	function closeAllSurfaces() {
		workspaceOpen = false;
		youOpen = false;
		captureOpen = false;
		uiStore.closeSearch();
	}
	function toggleWorkspace() {
		const next = !workspaceOpen;
		closeAllSurfaces();
		workspaceOpen = next;
	}
	function toggleYou() {
		const next = !youOpen;
		closeAllSurfaces();
		youOpen = next;
	}
	function toggleSearch() {
		const next = !uiStore.searchOpen;
		closeAllSurfaces();
		if (next) {
			uiStore.openSearch();
			uiStore.onNavigate();
		}
	}
	function toggleCapture() {
		const next = !captureOpen;
		closeAllSurfaces();
		captureOpen = next;
	}

	// Drive the .main-content reflow only while the bar is actually shown —
	// mobile AND no keyboard up. app.css owns the media-gated padding rule;
	// dropping the class while the keyboard hides the bar avoids a dead gap.
	$effect(() => {
		if (typeof document === 'undefined') return;
		document.body.classList.toggle('has-bottom-nav', uiStore.isMobile && !uiStore.keyboardVisible);
	});
	onDestroy(() => {
		if (typeof document !== 'undefined') document.body.classList.remove('has-bottom-nav');
	});
</script>

{#if uiStore.isMobile && wsPrefix}
	<!--
		Hide the bar while the on-screen keyboard is up so it doesn't sit
		stranded above the keyboard (PLAN-1694 Phase 1). The docked sheets
		below stay mounted regardless — QuickCaptureSheet's title input
		raises the keyboard itself, and it must survive that.
	-->
	{#if !uiStore.keyboardVisible}
	<nav class="bottom-nav" aria-label="Primary">
		<button
			class="bn-item"
			class:active={onWorkspaceContent || workspaceOpen}
			type="button"
			onclick={toggleWorkspace}
			aria-haspopup="dialog"
			aria-expanded={workspaceOpen}
		>
			<span class="bn-avatar" style:background={avatarColor(wsName)} aria-hidden="true">
				{avatarInitial(wsName)}
			</span>
			<span class="bn-label">Workspace</span>
		</button>

		<button
			class="bn-item"
			class:active={uiStore.searchOpen}
			type="button"
			onclick={toggleSearch}
			aria-haspopup="dialog"
			aria-expanded={uiStore.searchOpen}
		>
			<span class="bn-icon" aria-hidden="true">🔍</span>
			<span class="bn-label">Search</span>
		</button>

		<button
			class="bn-item bn-capture"
			class:active={captureOpen}
			type="button"
			onclick={toggleCapture}
			aria-label="Quick capture"
			aria-haspopup="dialog"
			aria-expanded={captureOpen}
		>
			<span class="bn-capture-plus" aria-hidden="true">＋</span>
		</button>

		<a
			class="bn-item"
			class:active={activeKey === 'activity'}
			href={`${wsPrefix}/activity`}
			onclick={() => {
				closeAllSurfaces();
				uiStore.onNavigate();
			}}
		>
			<span class="bn-icon" aria-hidden="true">📋</span>
			<span class="bn-label">Activity</span>
		</a>

		<button
			class="bn-item"
			class:active={youOpen}
			type="button"
			onclick={toggleYou}
			aria-haspopup="dialog"
			aria-expanded={youOpen}
		>
			<span class="bn-icon" aria-hidden="true">👤</span>
			<span class="bn-label">You</span>
		</button>
	</nav>
	{/if}

	<WorkspaceSheet open={workspaceOpen} onclose={() => (workspaceOpen = false)} />
	<YouSheet open={youOpen} onclose={() => (youOpen = false)} />

	<QuickCaptureSheet
		open={captureOpen}
		onclose={() => (captureOpen = false)}
		{wsSlug}
		{wsPrefix}
	/>
{/if}

<style>
	.bottom-nav {
		position: fixed;
		bottom: 0;
		left: 0;
		right: 0;
		z-index: 40; /* above content; sheets dock above at 45/46 but never cover this bar */
		display: flex;
		align-items: stretch;
		height: calc(var(--bottom-nav-height) + env(safe-area-inset-bottom, 0px));
		padding-bottom: env(safe-area-inset-bottom, 0px);
		background: var(--bg-secondary);
		border-top: 1px solid var(--border);
	}
	.bn-item {
		flex: 1;
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: 3px;
		background: none;
		border: none;
		cursor: pointer;
		padding: 0;
		color: var(--text-muted);
		text-decoration: none;
		font-family: var(--font-ui);
		min-width: 0;
	}
	.bn-item.active {
		color: var(--text-primary);
	}
	.bn-icon {
		font-size: 1.2em;
		line-height: 1;
	}
	.bn-avatar {
		width: 22px;
		height: 22px;
		border-radius: var(--radius-sm);
		display: flex;
		align-items: center;
		justify-content: center;
		color: #fff;
		font-size: 0.72em;
		font-weight: 700;
		line-height: 1;
	}
	.bn-item.active .bn-avatar {
		box-shadow: 0 0 0 2px color-mix(in srgb, var(--accent-blue) 55%, transparent);
	}
	.bn-label {
		font-size: 0.68em;
		font-weight: 500;
		max-width: 100%;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	/* Center capture action — a raised pill so it reads as the primary action. */
	.bn-capture {
		flex: 0 0 auto;
		width: 56px;
	}
	.bn-capture-plus {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 44px;
		height: 44px;
		border-radius: 50%;
		background: var(--accent-blue);
		color: #fff;
		font-size: 1.4em;
		line-height: 1;
		box-shadow: 0 2px 8px rgba(0, 0, 0, 0.35);
	}
	/* Lit while its sheet is open — same ring the Workspace avatar uses. */
	.bn-capture.active .bn-capture-plus {
		box-shadow:
			0 0 0 2px color-mix(in srgb, var(--accent-blue) 55%, transparent),
			0 2px 8px rgba(0, 0, 0, 0.35);
	}
</style>
