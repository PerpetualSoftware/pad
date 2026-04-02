<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import Sidebar from '$lib/components/layout/Sidebar.svelte';
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
		// Check auth status before loading the app
		try {
			const auth = await api.auth.session();
			if (auth.setup_required && auth.setup_method === 'open_register') {
				if (page.url.pathname !== '/register') {
					goto('/register', { replaceState: true });
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
			return;
		}
		if (isMod(e) && e.key === ']') {
			e.preventDefault();
			uiStore.toggleDetailPanel();
			return;
		}
		if (isMod(e) && e.key === 'n') {
			e.preventDefault();
			if (workspaceStore.current) {
				goto(`/${workspaceStore.current.slug}/new`);
			}
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
	<div class="app-shell">
		<Sidebar />
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

	<CommandPalette />
	<CreateWorkspaceModal />
	<ToastContainer />
	<KeyboardShortcuts visible={showShortcuts} onclose={() => showShortcuts = false} />
{/if}

<style>
	.app-shell {
		display: flex;
		height: 100vh;
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
</style>
