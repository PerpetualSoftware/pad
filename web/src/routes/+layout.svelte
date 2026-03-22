<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import Sidebar from '$lib/components/layout/Sidebar.svelte';
	import CommandPalette from '$lib/components/search/CommandPalette.svelte';
	import ToastContainer from '$lib/components/common/ToastContainer.svelte';
	import { isMod } from '$lib/utils/keyboard';

	let { children } = $props();

	onMount(async () => {
		await workspaceStore.loadAll();
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
		if (e.key === 'Escape' && uiStore.searchOpen) {
			uiStore.closeSearch();
		}
	}
</script>

<svelte:head>
	<title>Pad</title>
</svelte:head>

<svelte:window onkeydown={handleKeydown} />

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
				<span class="mobile-title">{workspaceStore.current?.name ?? 'Pad'}</span>
			</div>
		{/if}
		{@render children()}
	</main>
</div>

<CommandPalette />
<ToastContainer />

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
	}
</style>
