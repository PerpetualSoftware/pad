<!--
	BottomNav — the phone bar, everywhere a signed-in phone can be (PLAN-1694,
	reworked in TASK-4546).

	Five slots: Home · Workspace · Quick-capture (center ＋) · Chat · Search.
	  - Home links to / and carries the Needs you count; it is active on the
	    You pages (/, /needs, /agenda).
	  - Workspace opens WorkspaceSheet (current workspace + Switch, Navigate,
	    Collections) for the current or last workspace; with no workspace it
	    goes to the workspace picker.
	  - ＋ opens QuickCaptureSheet (BUG-1765); outside a workspace it captures
	    into the last workspace.
	  - Chat is ChatSlot (ChatHost's phone sheet); no slot while Chat is off.
	  - Search opens the command palette.
	The Account sheet opens from the avatar in the top bar (AccountButton), not
	from this bar. phoneNav holds which docked sheet is open, so only one is.

	The workspace layout mounts it directly; GlobalBottomBar mounts the same bar
	outside a workspace. Shows only on mobile (uiStore.isMobile) when signed in.
	Toggles `body.has-bottom-nav` so app.css reserves the bar's height.
-->
<script lang="ts">
	import { onDestroy, untrack } from 'svelte';
	import { goto, afterNavigate } from '$app/navigation';
	import { authStore } from '$lib/stores/auth.svelte';
	import { homeOverview } from '$lib/stores/homeOverview.svelte';
	import { chatSetting } from '$lib/stores/chatSetting.svelte';
	import { lastWorkspace } from '$lib/stores/lastWorkspace.svelte';
	import { tabsStore } from '$lib/stores/tabs.svelte';
	import { pickShellWorkspace } from '$lib/stores/shellWorkspace';
	import { phoneNav } from './phoneNav.svelte';
	import { page } from '$app/state';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { avatarColor, avatarInitial } from '$lib/utils/avatar';
	import { getActiveKey } from '$lib/nav/destinations';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';
	import ChatSlot from '$lib/components/layout/ChatSlot.svelte';
	import QuickCaptureSheet from '$lib/components/layout/QuickCaptureSheet.svelte';
	import WorkspaceSheet from '$lib/components/layout/WorkspaceSheet.svelte';


	let workspace = $derived(workspaceStore.current ?? pickShellWorkspace(workspaceStore.workspaces, lastWorkspace.current, tabsStore.tabs));
	let inWorkspace = $derived(!!workspace && (page.url.pathname === `/${workspace.owner_username}/${workspace.slug}` || page.url.pathname.startsWith(`/${workspace.owner_username}/${workspace.slug}/`)));
	let wsSlug = $derived(workspace?.slug);
	let wsUsername = $derived(workspace?.owner_username ?? '');
	let wsPrefix = $derived(wsUsername && wsSlug ? `/${wsUsername}/${wsSlug}` : '');
	let wsName = $derived(workspace?.name ?? 'Workspace');
	let activeKey = $derived(getActiveKey(page.url.pathname, wsPrefix));

	let workspaceOpen = $derived(phoneNav.sheet === 'workspace');
	let captureOpen = $derived(phoneNav.sheet === 'capture');
	let homeActive = $derived(['/', '/needs', '/agenda'].includes(page.url.pathname));

	// "Workspace" owns all in-workspace navigation, Activity included, so light it
	// whenever the active page is workspace content.
	let onWorkspaceContent = $derived(!!activeKey && !uiStore.searchOpen && !captureOpen && !chatSurface.isOpen);

	// Tap toggles: a second tap on the active slot closes its surface. Opening
	// any of the surfaces (Workspace / Account / Search / Quick-capture / Chat)
	// closes the others (BUG-1765).
	function closeAllSurfaces() {
		phoneNav.sheet = null;
		uiStore.closeSearch();
	}
	// With the Chat sheet open, another slot waits for Chat to close (its history
	// entries must not stay under the next surface), then runs. Chat closed: now.
	function afterChat(run: () => void) {
		if (!chatSurface.closeThen(run)) run();
	}
	function toggleWorkspace() {
		afterChat(() => {
			const next = !workspaceOpen;
			closeAllSurfaces();
			if (next) { if (workspace) phoneNav.sheet = 'workspace'; else void goto('/'); }
		});
	}
	function toggleSearch() {
		afterChat(() => {
			const next = !uiStore.searchOpen;
			closeAllSurfaces();
			if (next) {
				uiStore.openSearch();
				uiStore.onNavigate();
			}
		});
	}
	function toggleCapture() {
		afterChat(() => {
			const next = !captureOpen;
			closeAllSurfaces();
			if (next) phoneNav.sheet = 'capture';
		});
	}

	function openHome(event: MouseEvent) {
		closeAllSurfaces();
		if (chatSurface.closeThen(() => void goto('/'))) event.preventDefault();
	}
	// Match YouNav's refresh policy without tracking loader-owned state.
	let lastPath = '';
	$effect(() => {
		const path = page.url.pathname;
		if (!uiStore.isMobile || !authStore.authenticated) return;
		const arrived = path !== lastPath;
		lastPath = path;
		untrack(() => {
			if (homeActive) { if (arrived) void homeOverview.load(path, true); }
			else if (!homeOverview.needs && !homeOverview.agenda && !homeOverview.loading) void homeOverview.load('shell');
		});
	});
	afterNavigate(nav => { if (!nav.shallow && nav.type !== 'enter') phoneNav.sheet = null; });

	// Drive the .main-content reflow only while the bar is actually shown —
	// mobile AND no keyboard up. app.css owns the media-gated padding rule;
	// dropping the class while the keyboard hides the bar avoids a dead gap.
	$effect(() => {
		if (typeof document === 'undefined') return;
		document.body.classList.toggle('has-bottom-nav', uiStore.isMobile && authStore.authenticated && !uiStore.keyboardVisible);
	});
	onDestroy(() => {
		if (typeof document !== 'undefined') document.body.classList.remove('has-bottom-nav');
		phoneNav.sheet = null;
	});
</script>

{#if uiStore.isMobile && authStore.authenticated}
	<!--
		Hide the bar while the on-screen keyboard is up so it doesn't sit
		stranded above the keyboard (PLAN-1694 Phase 1). The docked sheets
		below stay mounted regardless — QuickCaptureSheet's title input
		raises the keyboard itself, and it must survive that.
	-->
	{#if !uiStore.keyboardVisible}
	<nav class="bottom-nav" aria-label="Primary">
		<a class="bn-item" class:active={homeActive} href="/" aria-current={homeActive ? 'page' : undefined} onclick={openHome}>
			<span class="bn-icon bn-home" aria-hidden="true">🏠
				{#if homeOverview.needs?.total}<span class="needs-count">{homeOverview.needs.total}</span>{/if}
			</span>
			<span class="bn-label">Home</span>
			{#if homeOverview.needs?.total}<span class="sr-only">{homeOverview.needs.total} need you</span>{/if}
		</a>
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

		{#if chatSetting.enabled}<ChatSlot onopen={closeAllSurfaces} />{/if}

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

	</nav>
	{/if}

	<WorkspaceSheet open={workspaceOpen} onclose={() => (phoneNav.sheet = null)} />

	<QuickCaptureSheet
		open={captureOpen}
		onclose={() => (phoneNav.sheet = null)}
		wsSlug={inWorkspace ? wsSlug : undefined}
		wsPrefix={inWorkspace ? wsPrefix : ''}
	/>
{/if}

<style>
	.bn-home { position: relative; }
	.needs-count { position: absolute; top: -7px; left: 60%; font-size: .65rem; font-weight: 700; min-width: 18px; padding: 2px 4px; border-radius: 99px; background: var(--accent-amber); color: var(--bg-primary); }

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
