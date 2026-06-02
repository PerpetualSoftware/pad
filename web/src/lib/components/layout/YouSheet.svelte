<!--
	YouSheet — the mobile BottomNav's "You" slot (PLAN-1694, TASK-1699).

	Consolidates everything the retired mobile <TopBar /> offered into one
	BottomSheet, in three sections:
	  1. Workspace — <WorkspaceSwitcher mobile /> (current workspace + switch).
	  2. Navigate — the sidebar overflow not promoted to a primary BottomNav
	     slot (Insights/Roles/Starred/Tags + Settings) and the collections.
	  3. Account — identity, console links (Workspaces/Settings/Billing/Admin),
	     theme toggle, Resources, Connect a project, Sign out.

	Self-contained (reads stores directly) like QuickCaptureSheet. The account
	block mirrors TopBar.svelte's user menu so retiring the header loses nothing
	(Codex consensus on PLAN-1694 Phase 2-3).
-->
<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto, afterNavigate } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { api } from '$lib/api/client';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';
	import WorkspaceSwitcher from '$lib/components/layout/WorkspaceSwitcher.svelte';
	import UserMenuResources from '$lib/components/layout/UserMenuResources.svelte';
	import ConnectWorkspaceModal from '$lib/components/ConnectWorkspaceModal.svelte';
	import { getPrimaryDestinations, getActiveKey } from '$lib/nav/destinations';

	let { open, onclose }: { open: boolean; onclose: () => void } = $props();

	// Close the sheet on any navigation. Most rows close explicitly before
	// goto(), but the embedded <WorkspaceSwitcher /> navigates on its own
	// (workspace switch) without notifying us — without this it'd stay open
	// over the new workspace (Codex review, PLAN-1694).
	afterNavigate((nav) => {
		if (nav.type !== 'enter' && open) onclose();
	});

	let wsSlug = $derived(workspaceStore.current?.slug);
	let wsUsername = $derived(workspaceStore.current?.owner_username ?? '');
	let wsPrefix = $derived(wsUsername && wsSlug ? `/${wsUsername}/${wsSlug}` : '');
	let isGuest = $derived(workspaceStore.current?.is_guest ?? false);
	let activeKey = $derived(getActiveKey(page.url.pathname, wsPrefix));

	// Same primary-slot exclusion as the BottomNav bar.
	const PRIMARY_SLOT_KEYS = ['dashboard', 'activity'];
	let overflowDestinations = $derived(
		getPrimaryDestinations(wsPrefix)
			.filter((d) => !PRIMARY_SLOT_KEYS.includes(d.key))
			.filter((d) => !(d.guestHidden && isGuest))
	);

	const agentSlugs = ['conventions', 'playbooks'];
	let regularCollections = $derived(
		collectionStore.collections.filter((c) => !agentSlugs.includes(c.slug))
	);
	let agentCollections = $derived(
		collectionStore.collections.filter((c) => agentSlugs.includes(c.slug))
	);

	let connectOpen = $state(false);

	// Theme toggle — mirrors the Sidebar/TopBar toggle (key 'pad-theme',
	// data-theme attribute). Read the live value when the sheet mounts.
	let currentTheme = $state<'dark' | 'light'>('dark');
	onMount(() => {
		const fromDom = document.documentElement.getAttribute('data-theme');
		const fromLs = (() => {
			try {
				return localStorage.getItem('pad-theme');
			} catch {
				return null;
			}
		})();
		currentTheme = (fromDom || fromLs) === 'light' ? 'light' : 'dark';
	});
	function toggleTheme() {
		currentTheme = currentTheme === 'dark' ? 'light' : 'dark';
		document.documentElement.setAttribute('data-theme', currentTheme);
		try {
			localStorage.setItem('pad-theme', currentTheme);
		} catch {
			// storage unavailable; in-memory toggle still applies.
		}
	}

	function navigate(href: string) {
		onclose();
		uiStore.onNavigate();
		goto(href);
	}

	async function handleLogout() {
		try {
			await api.auth.logout();
		} finally {
			window.location.href = '/login';
		}
	}
</script>

<BottomSheet {open} {onclose} title="You">
	<div class="you">
		{#if wsSlug}
			<div class="you-workspace">
				<WorkspaceSwitcher mobile />
			</div>
		{/if}

		<div class="you-section">Navigate</div>
		{#each overflowDestinations as dest (dest.key)}
			<button
				class="you-item"
				class:active={activeKey === dest.key}
				type="button"
				onclick={() => navigate(dest.href)}
			>
				<span class="you-icon" aria-hidden="true">{dest.icon}</span>
				<span>{dest.label}</span>
			</button>
		{/each}
		{#each regularCollections as coll (coll.id)}
			<button
				class="you-item"
				class:active={activeKey === `collection:${coll.slug}`}
				type="button"
				onclick={() => navigate(`${wsPrefix}/${coll.slug}`)}
			>
				<span class="you-icon" aria-hidden="true">{coll.icon}</span>
				<span>{coll.name}</span>
			</button>
		{/each}
		{#each agentCollections as coll (coll.id)}
			<button
				class="you-item"
				class:active={activeKey === `collection:${coll.slug}`}
				type="button"
				onclick={() => navigate(`${wsPrefix}/${coll.slug}`)}
			>
				<span class="you-icon" aria-hidden="true">{coll.icon}</span>
				<span>{coll.name}</span>
			</button>
		{/each}

		{#if authStore.user}
			<div class="you-section">Account</div>
			<div class="you-identity">
				<span class="you-name">{authStore.user.name}</span>
				<span class="you-email">{authStore.user.email}</span>
			</div>
			<!-- Mirrors the retired TopBar user menu so nothing is lost. -->
			<div class="user-dropdown">
				<a href="/console" class="dropdown-item" onclick={onclose}>Workspaces</a>
				<a href="/console/settings" class="dropdown-item" onclick={onclose}>Settings</a>
				{#if authStore.cloudMode}
					<a href="/console/billing" class="dropdown-item" onclick={onclose}>Billing</a>
				{/if}
				{#if authStore.user?.role === 'admin'}
					<a href="/console/admin" class="dropdown-item" onclick={onclose}>Admin</a>
				{/if}
				<button class="dropdown-item" type="button" onclick={toggleTheme}>
					{currentTheme === 'dark' ? 'Light mode' : 'Dark mode'}
				</button>

				<UserMenuResources cloudMode={authStore.cloudMode} {onclose} />

				{#if wsSlug}
					<button
						class="dropdown-item"
						type="button"
						onclick={() => {
							onclose();
							connectOpen = true;
						}}
					>
						Connect a project…
					</button>
				{/if}
				<div class="dropdown-divider"></div>
				<button class="dropdown-item logout" type="button" onclick={handleLogout}>
					Sign out
				</button>
			</div>
		{/if}
	</div>
</BottomSheet>

{#if wsSlug}
	<ConnectWorkspaceModal
		bind:open={connectOpen}
		serverUrl={typeof window !== 'undefined' ? window.location.origin : ''}
		workspaceSlug={wsSlug}
		workspaceName={workspaceStore.current?.name ?? ''}
		mcpPublicUrl={authStore.mcpPublicUrl}
	/>
{/if}

<style>
	.you {
		display: flex;
		flex-direction: column;
		padding: 0 var(--space-3) var(--space-2);
	}
	.you-workspace {
		padding: 0 var(--space-2) var(--space-2);
	}
	.you-section {
		padding: var(--space-3) var(--space-3) var(--space-1);
		font-size: 0.7em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}
	.you-item {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		width: 100%;
		background: none;
		border: none;
		cursor: pointer;
		padding: var(--space-3);
		border-radius: var(--radius-sm);
		color: var(--text-secondary);
		font-size: 0.98em;
		font-family: var(--font-ui);
		text-align: left;
	}
	.you-item:hover {
		background: var(--bg-hover);
	}
	.you-item.active {
		color: var(--text-primary);
		background: var(--bg-hover);
		font-weight: 600;
	}
	.you-icon {
		font-size: 1.1em;
		width: 1.4em;
		text-align: center;
		flex-shrink: 0;
	}
	.you-identity {
		display: flex;
		flex-direction: column;
		gap: 1px;
		padding: var(--space-1) var(--space-3) var(--space-2);
	}
	.you-name {
		font-weight: 600;
		color: var(--text-primary);
		font-size: 0.95em;
	}
	.you-email {
		font-size: 0.8em;
		color: var(--text-muted);
	}
	/* .dropdown-item / .dropdown-divider come from UserMenuResources' global
	   .user-dropdown rules; restyle the buttons to match full-width sheet rows
	   and add the touch-friendly sizing the sheet wants. */
	.user-dropdown {
		display: flex;
		flex-direction: column;
	}
	.user-dropdown :global(.dropdown-item) {
		display: block;
		width: 100%;
		text-align: left;
		background: none;
		border: none;
		cursor: pointer;
		padding: var(--space-3);
		font-size: 0.95em;
		font-family: var(--font-ui);
		color: var(--text-secondary);
		border-radius: var(--radius-sm);
	}
	.user-dropdown :global(.dropdown-item):hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
	.user-dropdown :global(.dropdown-divider) {
		height: 1px;
		background: var(--border);
		margin: var(--space-1) 0;
	}
	.user-dropdown :global(.logout) {
		color: var(--accent-orange);
	}
</style>
