<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import Sidebar from '$lib/components/layout/Sidebar.svelte';
	import TopBar from '$lib/components/layout/TopBar.svelte';
	import CommandPalette from '$lib/components/search/CommandPalette.svelte';
	import ToastContainer from '$lib/components/common/ToastContainer.svelte';
	import CreateWorkspaceModal from '$lib/components/layout/CreateWorkspaceModal.svelte';
	import { isMod, isInputFocused } from '$lib/utils/keyboard';
	import KeyboardShortcuts from '$lib/components/common/KeyboardShortcuts.svelte';

	let { children } = $props();

	let showShortcuts = $state(false);
	let authReady = $state(false);
	let isAuthPage = $derived(page.url.pathname === '/login' || page.url.pathname === '/register' || page.url.pathname.startsWith('/join/'));

	onMount(async () => {
		// Initialize theme
		const savedTheme = localStorage.getItem('pad-theme');
		if (savedTheme === 'light' || savedTheme === 'dark') {
			document.documentElement.setAttribute('data-theme', savedTheme);
		} else if (window.matchMedia('(prefers-color-scheme: light)').matches) {
			document.documentElement.setAttribute('data-theme', 'light');
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
			// If auth check fails, proceed anyway (server may not support it)
		}

		authReady = true;
		if (!isAuthPage) {
			await workspaceStore.loadAll();
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
	<title>Pad</title>
	<meta name="description" content="Project management for developers and AI agents" />
	<meta property="og:title" content="Pad" />
	<meta property="og:description" content="Project management for developers and AI agents" />
	<meta property="og:image" content="/padicon.png" />
	<meta property="og:type" content="website" />
</svelte:head>

<svelte:window onkeydown={handleKeydown} />

{#if !authReady}
	<!-- Auth check in progress — blank screen to avoid flash -->
{:else if isAuthPage}
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
						<a href="/{workspaceStore.current?.slug ?? ''}" class="mobile-title">{workspaceStore.current?.name ?? 'Pad'}</a>
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
	.mobile-title {
		font-weight: 600;
		font-size: 0.95em;
		color: var(--text-primary);
		text-decoration: none;
	}
	.mobile-title:hover {
		color: var(--accent-blue);
		text-decoration: none;
	}
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
		opacity: 0;
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
		opacity: 0;
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
