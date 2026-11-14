<script lang="ts">
	import { page } from '$app/state';
	import { authStore } from '$lib/stores/auth.svelte';

	let { children } = $props();
	const tabs = $derived([
		{ label: 'Account', href: '/settings/account' },
		...(authStore.oauthAvailable ? [{ label: 'Connected apps', href: '/settings/connected-apps' }] : []),
		...(authStore.commerceAllowed ? [{ label: 'Billing', href: '/settings/billing' }] : []),
		{ label: 'Deleted workspaces', href: '/settings/deleted-workspaces' },
		...(authStore.user?.role === 'admin' ? [{ label: 'Admin', href: '/settings/admin' }] : [])
	]);
	function isActive(href: string): boolean {
		return page.url.pathname === href || page.url.pathname.startsWith(href + '/');
	}
</script>

<svelte:head><title>Settings - Pad</title></svelte:head>

<header class="settings-header">
	<h1>Settings</h1>
	<p>Your account on this Pad. A workspace's name, members and collections are in its own Workspace settings.</p>
</header>
<nav class="settings-tabs" aria-label="Settings">
	{#each tabs as tab (tab.href)}
		<a href={tab.href} aria-current={isActive(tab.href) ? 'page' : undefined}>
			{tab.label}{#if tab.label === 'Admin'}{' '}<span class="admins-pill" aria-hidden="true">admins</span>{/if}
		</a>
	{/each}
</nav>
{@render children()}

<style>
	.settings-header h1 { font-size: 1.4rem; font-weight: 700; color: var(--text-primary); }
	.settings-header p { margin: var(--space-3) 0 var(--space-6); color: var(--text-secondary); font-size: 0.9rem; }
	.settings-tabs { display: flex; flex-wrap: wrap; gap: var(--space-1); border-bottom: 1px solid var(--border); margin-bottom: var(--space-6); }
	.settings-tabs a { display: inline-flex; align-items: center; gap: var(--space-1); padding: var(--space-3) var(--space-4); color: var(--text-secondary); font-size: 0.85rem; font-weight: 600; text-decoration: none; border-bottom: 2px solid transparent; margin-bottom: -1px; white-space: nowrap; }
	.settings-tabs a:hover { color: var(--text-primary); background: var(--bg-hover); }
	.settings-tabs a[aria-current="page"] { color: var(--text-primary); border-bottom-color: var(--accent-primary); }
	.admins-pill { padding: 1px 6px; border-radius: var(--radius); background: var(--bg-tertiary); color: var(--text-secondary); font-size: 0.7rem; font-weight: 500; }
	@media (max-width: 640px) { .settings-tabs a { padding-inline: var(--space-3); } }
</style>
