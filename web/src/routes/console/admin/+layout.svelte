<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { authStore } from '$lib/stores/auth.svelte';
	import { adminStore } from '$lib/stores/admin.svelte';

	let { children } = $props();

	let isAdmin = $derived(authStore.user?.role === 'admin');

	onMount(() => {
		if (isAdmin) {
			adminStore.loadStats();
		}
	});

	// Billing is cloud-only — the underlying endpoint returns 404 in
	// self-host. Hide the tab when adminStore.stats reports cloud_mode=false
	// so a self-host operator doesn't see a tab that always 404s on click.
	let cloudMode = $derived(adminStore.stats?.cloud_mode ?? false);

	let tabs = $derived([
		{ label: 'Users', href: '/console/admin' },
		{ label: 'Invitations', href: '/console/admin/invitations' },
		{ label: 'Audit Log', href: '/console/admin/audit-log' },
		// MCP audit log (TASK-960) — full-table view of every MCP call.
		// Cloud-only: the /api/v1/admin/mcp-audit endpoint backs the
		// MCP transport, which only mounts in cloud mode. Hiding the
		// tab in self-host avoids a "the page always errors" UX.
		...(cloudMode ? [{ label: 'MCP Audit', href: '/console/admin/mcp-audit' }] : []),
		...(cloudMode ? [{ label: 'Billing', href: '/console/admin/billing' }] : []),
		{ label: 'Settings', href: '/console/admin/settings' }
	]);

	function isActive(href: string): boolean {
		const path = page.url.pathname;
		if (href === '/console/admin') {
			return path === '/console/admin';
		}
		return path.startsWith(href);
	}
</script>

<svelte:head>
	<title>Admin - Pad</title>
</svelte:head>

<div class="admin-layout">
	{#if !isAdmin}
		<div class="denied">Admin access required</div>
	{:else if adminStore.loading}
		<div class="loading-msg">Loading admin data...</div>
	{:else if adminStore.error}
		<div class="error-msg">{adminStore.error}</div>
	{:else}
		{#if adminStore.stats}
			<div class="stats-bar">
				<div class="stat">
					<span class="stat-value">{adminStore.stats.users}</span>
					<span class="stat-label">Users</span>
				</div>
				{#each Object.entries(adminStore.stats.users_by_plan) as [plan, count] (plan)}
					<div class="stat">
						<span class="stat-value">{count}</span>
						<span class="stat-label">{plan}</span>
					</div>
				{/each}
				<div class="stat">
					<span class="stat-value">{adminStore.stats.workspaces}</span>
					<span class="stat-label">Workspaces</span>
				</div>
			</div>
		{/if}

		<nav class="admin-tabs">
			{#each tabs as tab (tab.href)}
				<a
					class="admin-tab"
					class:active={isActive(tab.href)}
					href={tab.href}
				>
					{tab.label}
				</a>
			{/each}
		</nav>

		{@render children()}
	{/if}
</div>

<style>
	.admin-layout { display: flex; flex-direction: column; gap: var(--space-6); }

	.denied, .loading-msg {
		color: var(--text-muted); padding: var(--space-10) 0; text-align: center; font-size: 0.95rem;
	}
	.error-msg {
		color: var(--accent-red); padding: var(--space-6); background: var(--bg-secondary);
		border: 1px solid var(--border); border-radius: var(--radius);
	}

	/* Stats */
	.stats-bar { display: flex; gap: var(--space-4); flex-wrap: wrap; }
	.stat {
		flex: 1; min-width: 120px; padding: var(--space-4) var(--space-5);
		background: var(--bg-secondary); border: 1px solid var(--border);
		border-radius: var(--radius-lg); display: flex; flex-direction: column; gap: var(--space-1);
	}
	.stat-value { font-size: 1.5rem; font-weight: 700; color: var(--text-primary); }
	.stat-label { font-size: 0.8rem; color: var(--text-muted); text-transform: capitalize; }

	/* Tabs */
	/* TASK-2979: wraps rather than scrolls, for the reason C82 (TASK-2245) gives
	   on the workspace settings strip — a scrollport whose scrollbar is hidden
	   ends after a tab with clean trailing whitespace and looks complete, so the
	   tabs past the fold are not merely awkward to reach, they are unadvertised.
	   Measured before this change: 4 tabs (self-host) clip `Settings` to 61% at
	   320px, and with the two cloudMode tabs present the strip is 478px intrinsic
	   against a 288-398px box — `Settings` 0% visible at 320/360/390/412/430 and
	   `Billing` invisible or nearly so throughout.
	   No media query: `flex-wrap` is inert while the row fits, so the rule cannot
	   reach a width where the tabs were fine. */
	.admin-tabs {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1);
		border-bottom: 1px solid var(--border);
		margin-bottom: var(--space-6);
	}
	.admin-tab {
		padding: var(--space-2) var(--space-4);
		font-size: 0.85rem;
		font-weight: 500;
		color: var(--text-muted);
		text-decoration: none;
		border-bottom: 2px solid transparent;
		margin-bottom: -1px;
		transition: color 0.15s, border-color 0.15s;
		/* Hoisted out of the deleted mobile block: a wrapped row must still break
		   BETWEEN tabs, never inside a label. */
		flex-shrink: 0;
		white-space: nowrap;
	}
	.admin-tab:hover {
		color: var(--text-primary);
		text-decoration: none;
	}
	.admin-tab.active {
		color: var(--text-primary);
		border-bottom-color: var(--accent-blue);
	}

	/* The mobile block that used to live here scrolled the strip with its
	   scrollbar hidden, and its own comment claimed that made every tab
	   "reachable ... without wrapping or clipping". The first half was true and
	   the second was not: reachable by a swipe nothing advertises is what BUG-1118
	   bought, and measurement (on TASK-2979's trail) shows tabs clipped to 0% at
	   every phone width once the cloud tab set is present. Wrapping is what the
	   comment was describing; now it is what the code does. */
</style>
