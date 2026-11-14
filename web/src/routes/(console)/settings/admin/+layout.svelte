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
		{ label: 'Users', href: '/settings/admin' },
		{ label: 'Invitations', href: '/settings/admin/invitations' },
		{ label: 'Audit log', href: '/settings/admin/audit-log' },
		// MCP audit log (TASK-960): the full-table view of every MCP call.
		// Always shown to admins (PLAN-2310 DR-7): its API answers on every
		// install, MCP on or off, because reading past use while MCP is off
		// is how an admin audits it. No history renders the empty state.
		{ label: 'MCP audit', href: '/settings/admin/mcp-audit' },
		...(cloudMode ? [{ label: 'Billing', href: '/settings/admin/billing' }] : []),
		{ label: 'Instance', href: '/settings/admin/instance' }
	]);

	function isActive(href: string): boolean {
		const path = page.url.pathname;
		if (href === '/settings/admin') {
			return path === '/settings/admin';
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

		<nav class="admin-segments" aria-label="Admin">
			{#each tabs as tab (tab.href)}
				<a
					class="admin-segment"
					class:active={isActive(tab.href)}
					href={tab.href}
					aria-current={isActive(tab.href) ? 'page' : undefined}
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

	/* Link segments share the raised-tile styling of SegmentedControl. */
	.admin-segments { display: flex; flex-wrap: wrap; align-self: flex-start; max-width: 100%; gap: 2px; padding: 2px; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: var(--radius); }
	.admin-segment { display: inline-flex; align-items: center; justify-content: center; min-height: 28px; padding: 3px 12px; border: 1px solid transparent; border-radius: 6px; color: var(--text-secondary); font-size: 13px; font-weight: 600; line-height: 1.5; white-space: nowrap; text-decoration: none; }
	.admin-segment:hover { background: var(--bg-hover); color: var(--text-primary); }
	.admin-segment.active { background: var(--bg-raised); border-color: var(--border-strong); color: var(--text-primary); box-shadow: var(--shadow-sm); }
	.admin-segment:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: 1px; }
	@media (max-width: 640px) {
		.admin-segments { align-self: stretch; display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); }
		.admin-segment { min-height: 36px; padding-inline: 3px; }
	}
</style>
