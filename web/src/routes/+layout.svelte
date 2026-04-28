<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { titleStore } from '$lib/stores/title.svelte';
	import Sidebar from '$lib/components/layout/Sidebar.svelte';
	import TopBar from '$lib/components/layout/TopBar.svelte';
	import WorkspaceSwitcher from '$lib/components/layout/WorkspaceSwitcher.svelte';
	import CommandPalette from '$lib/components/search/CommandPalette.svelte';
	import ToastContainer from '$lib/components/common/ToastContainer.svelte';
	import CreateWorkspaceModal from '$lib/components/layout/CreateWorkspaceModal.svelte';
	import { isMod, isInputFocused } from '$lib/utils/keyboard';
	import KeyboardShortcuts from '$lib/components/common/KeyboardShortcuts.svelte';

	let { children } = $props();

	let showShortcuts = $state(false);
	let authReady = $state(false);
	let workspacesLoaded = $state(false);
	let authLoadFailed = $state(false);
	let isAuthPage = $derived(
		page.url.pathname === '/login'
		|| page.url.pathname === '/register'
		|| page.url.pathname === '/forgot-password'
		|| page.url.pathname.startsWith('/reset-password/')
		|| page.url.pathname.startsWith('/join/')
		|| page.url.pathname.startsWith('/auth/cli/')
	);
	let isSharePage = $derived(page.url.pathname.startsWith('/s/'));
	let isConsolePage = $derived(page.url.pathname.startsWith('/console'));

	onMount(async () => {
		// Initialize theme
		const savedTheme = localStorage.getItem('pad-theme');
		if (savedTheme === 'light' || savedTheme === 'dark') {
			document.documentElement.setAttribute('data-theme', savedTheme);
		} else if (window.matchMedia('(prefers-color-scheme: light)').matches) {
			document.documentElement.setAttribute('data-theme', 'light');
		}

		// Share pages bypass auth entirely
		if (isSharePage) {
			authReady = true;
			return;
		}

		// Check auth status before loading the app
		try {
			const auth = await authStore.load();
			if (!auth) {
				if (!isAuthPage) {
					goto('/login', { replaceState: true });
				}
				authReady = true;
				return;
			}
			if (auth.setup_required) {
				if (!isAuthPage) {
					goto('/login', { replaceState: true });
				}
				authReady = true;
				return;
			}
			if (!auth.authenticated) {
				if (!isAuthPage) {
					goto('/login', { replaceState: true });
				}
				authReady = true;
				return;
			}
		} catch {
			// If auth check fails, proceed anyway (server may not support it).
			// Track this so the workspace loader effect below can still fire — in
			// this case authStore.authenticated stays false but we still want to
			// load the workspace list.
			authLoadFailed = true;
		}

		authReady = true;
	});

	$effect(() => {
		// Load workspaces once auth is resolved and we're on an app page.
		// This runs after onMount AND on subsequent navigation (e.g., post-login
		// when the user moves from /login → /console → /{user}/{workspace}),
		// which a one-shot onMount would miss. Fixes BUG-584.
		//
		// We gate on (authenticated || authLoadFailed) rather than !isAuthPage
		// alone: authReady flips true inside the unauthenticated branches of
		// onMount BEFORE the /login redirect completes, so during that window a
		// logged-out user on a protected route would otherwise fire loadAll()
		// and latch workspacesLoaded=true, blocking the retry after login.
		// authLoadFailed covers the deployment case where the auth endpoint is
		// unavailable and authStore.authenticated stays false by design.
		if (
			authReady &&
			(authStore.authenticated || authLoadFailed) &&
			!isAuthPage &&
			!isSharePage &&
			!workspacesLoaded &&
			!workspaceStore.loading
		) {
			workspacesLoaded = true;
			workspaceStore.loadAll();
		}
	});

	function handleKeydown(e: KeyboardEvent) {
		if (isMod(e) && e.key === 'k') {
			e.preventDefault();
			uiStore.toggleSearch();
			return;
		}
		if (isMod(e) && e.key === '\\') {
			e.preventDefault();
			uiStore.toggleSidebar();
			uiStore.toggleTopbar();
			return;
		}
		if (isMod(e) && e.key === ']') {
			e.preventDefault();
			uiStore.toggleDetailPanel();
			return;
		}
		if (isMod(e) && e.key === 'n') {
			e.preventDefault();
			uiStore.requestQuickAdd();
			return;
		}
		if (isMod(e) && e.key === 'f') {
			e.preventDefault();
			uiStore.requestCollectionSearch();
			return;
		}
		if (e.key === '?' && !isInputFocused()) {
			e.preventDefault();
			showShortcuts = !showShortcuts;
			return;
		}
		if (e.key === 'Escape' && showShortcuts) {
			showShortcuts = false;
			return;
		}
		if (e.key === 'Escape' && uiStore.searchOpen) {
			uiStore.closeSearch();
		}
	}
</script>

<svelte:head>
	<title>{titleStore.title}</title>
	<meta name="description" content="Collaborate with your AI agents" />
	<meta property="og:title" content="Pad" />
	<meta property="og:description" content="Collaborate with your AI agents" />
	<meta property="og:image" content="/padicon.png" />
	<meta property="og:type" content="website" />
</svelte:head>

<svelte:window onkeydown={handleKeydown} />

{#if !authReady}
	<!-- Auth check in progress — blank screen to avoid flash -->
{:else if isAuthPage || isSharePage || isConsolePage}
	{@render children()}
{:else}
	{#if uiStore.isMobile && uiStore.sidebarOpen}
		<TopBar mobile />
	{/if}
	<div class="app-layout">
		{#if !uiStore.isMobile && uiStore.topbarOpen}
			<TopBar />
		{/if}
		{#if !uiStore.isMobile && !uiStore.topbarOpen}
			<button
				class="topbar-expand-btn"
				onclick={() => uiStore.openTopbar()}
				aria-label="Show workspace bar"
				title="Show workspace bar (⌘\)"
			>
				<svg width="16" height="16" viewBox="0 0 16 16" fill="none">
					<path d="M3 6L8 11L13 6" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
				</svg>
			</button>
		{/if}
		<div class="app-shell">
			<Sidebar />
			{#if !uiStore.isMobile && !uiStore.sidebarOpen}
				<button
					class="sidebar-expand-btn"
					onclick={() => uiStore.openSidebar()}
					aria-label="Open sidebar"
					title="Open sidebar (⌘\)"
				>
					<svg width="16" height="16" viewBox="0 0 16 16" fill="none">
						<path d="M6 3L11 8L6 13" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
					</svg>
				</button>
			{/if}
			<main class="main-content">
				{#if uiStore.isMobile && !uiStore.sidebarOpen}
					<div class="mobile-header">
						<button class="hamburger" onclick={() => uiStore.openSidebar()} aria-label="Open sidebar">
							<svg width="20" height="20" viewBox="0 0 20 20" fill="none">
								<rect y="3" width="20" height="2" rx="1" fill="currentColor"/>
								<rect y="9" width="20" height="2" rx="1" fill="currentColor"/>
								<rect y="15" width="20" height="2" rx="1" fill="currentColor"/>
							</svg>
						</button>
						<div class="mobile-switcher-slot">
							<WorkspaceSwitcher mobile />
						</div>
					</div>
				{/if}
				{@render children()}
			</main>
		</div>
	</div>

	<CommandPalette />
	<CreateWorkspaceModal />
	<ToastContainer />
	<KeyboardShortcuts visible={showShortcuts} onclose={() => showShortcuts = false} />
{/if}

<style>
	.app-layout {
		position: relative;
		display: flex;
		flex-direction: column;
		height: 100vh;
		overflow: hidden;
	}
	.app-shell {
		position: relative;
		display: flex;
		flex: 1;
		min-height: 0;
		overflow: hidden;
	}
	.main-content {
		flex: 1;
		overflow-y: auto;
		min-width: 0;
	}
	.mobile-header {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		border-bottom: 1px solid var(--border);
		background: var(--bg-secondary);
		position: sticky;
		top: 0;
		z-index: 5;
	}
	.hamburger {
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		color: var(--text-secondary);
		display: flex;
		align-items: center;
		justify-content: center;
	}
	.hamburger:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
	.mobile-switcher-slot {
		flex: 1;
		min-width: 0;
		display: flex;
		align-items: center;
	}
	/*
		Expand tabs (both .topbar-expand-btn and .sidebar-expand-btn).
		IDEA-757: persistent low-opacity affordance. ⌘\ toggles BOTH the
		sidebar and the topbar at once — a user who hits it accidentally
		needs to see *something* clickable to recover, even before they
		move the mouse. Idle opacity (0.5) keeps the tabs faintly visible
		so the affordance never disappears; hover amplifies to full. The
		button tooltip ("Show workspace bar (⌘\)" / "Open sidebar (⌘\)")
		teaches the shortcut once the user notices the tab.
	*/
	.topbar-expand-btn {
		position: absolute;
		top: 0;
		left: 50%;
		transform: translateX(-50%);
		z-index: 10;
		display: flex;
		align-items: center;
		justify-content: center;
		width: 48px;
		height: 20px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-top: none;
		border-radius: 0 0 var(--radius) var(--radius);
		color: var(--text-muted);
		cursor: pointer;
		padding: 0;
		opacity: 0.5;
		transition: opacity 0.2s ease, color 0.15s ease, background 0.15s ease;
	}
	.app-layout:hover .topbar-expand-btn {
		opacity: 1;
	}
	.topbar-expand-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}
	.sidebar-expand-btn {
		position: absolute;
		left: 0;
		top: 50%;
		transform: translateY(-50%);
		z-index: 10;
		display: flex;
		align-items: center;
		justify-content: center;
		width: 20px;
		height: 48px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-left: none;
		border-radius: 0 var(--radius) var(--radius) 0;
		color: var(--text-muted);
		cursor: pointer;
		padding: 0;
		opacity: 0.5;
		transition: opacity 0.2s ease, color 0.15s ease, background 0.15s ease;
	}
	.app-shell:hover .sidebar-expand-btn {
		opacity: 1;
	}
	.sidebar-expand-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}
</style>
