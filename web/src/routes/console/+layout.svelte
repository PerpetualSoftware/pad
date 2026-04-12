<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { authStore } from '$lib/stores/auth.svelte';
	import { api } from '$lib/api/client';
	import { onMount } from 'svelte';

	let { children } = $props();
	let ready = $state(false);

	onMount(async () => {
		try {
			const session = await authStore.load();
			if (!session?.authenticated) {
				goto('/login', { replaceState: true });
				return;
			}
		} catch {
			goto('/login', { replaceState: true });
			return;
		}
		ready = true;
	});

	let currentPath = $derived(page.url.pathname as string);

	function isActive(path: string): boolean {
		if (path === '/console') return currentPath === '/console';
		return currentPath.startsWith(path);
	}

	async function logout() {
		await api.auth.logout();
		authStore.clear();
		goto('/login');
	}

	let initial = $derived(
		authStore.user?.name?.charAt(0)?.toUpperCase() ??
		authStore.user?.email?.charAt(0)?.toUpperCase() ?? '?'
	);
</script>

{#if ready}
	<div class="console-layout">
		<nav class="console-nav">
			<div class="nav-left">
				<a href="/console" class="nav-logo">Pad</a>
				<div class="nav-links">
					<a href="/console" class="nav-link" class:active={isActive('/console') && !isActive('/console/settings') && !isActive('/console/billing') && !isActive('/console/admin')}>
						Workspaces
					</a>
					<a href="/console/settings" class="nav-link" class:active={isActive('/console/settings')}>
						Settings
					</a>
					{#if authStore.cloudMode}
						<a href="/console/billing" class="nav-link" class:active={isActive('/console/billing')}>
							Billing
						</a>
					{/if}
					{#if authStore.user?.role === 'admin'}
						<a href="/console/admin" class="nav-link" class:active={isActive('/console/admin')}>
							Admin
						</a>
					{/if}
				</div>
			</div>
			<div class="nav-right">
				<div class="user-info">
					<span class="avatar">{initial}</span>
					<span class="user-name">{authStore.user?.name || authStore.user?.email || ''}</span>
				</div>
				<button class="logout-btn" onclick={logout}>Sign out</button>
			</div>
		</nav>
		<main class="console-main">
			{@render children()}
		</main>
	</div>
{/if}

<style>
	.console-layout {
		min-height: 100vh;
		background: var(--bg-primary);
	}

	.console-nav {
		display: flex;
		align-items: center;
		justify-content: space-between;
		height: 56px;
		padding: 0 var(--space-6);
		background: var(--bg-secondary);
		border-bottom: 1px solid var(--border);
	}

	.nav-left {
		display: flex;
		align-items: center;
		gap: var(--space-8);
	}

	.nav-logo {
		font-size: 1.25rem;
		font-weight: 700;
		color: var(--text-primary);
		text-decoration: none;
		letter-spacing: -0.02em;
	}

	.nav-logo:hover {
		text-decoration: none;
	}

	.nav-links {
		display: flex;
		align-items: center;
		gap: var(--space-1);
	}

	.nav-link {
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.9rem;
		font-weight: 500;
		text-decoration: none;
		transition: color 0.15s, background 0.15s;
	}

	.nav-link:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
		text-decoration: none;
	}

	.nav-link.active {
		color: var(--text-primary);
		background: var(--bg-tertiary);
	}

	.nav-right {
		display: flex;
		align-items: center;
		gap: var(--space-4);
	}

	.user-info {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.avatar {
		width: 28px;
		height: 28px;
		border-radius: 50%;
		background: var(--accent-blue);
		color: #fff;
		display: flex;
		align-items: center;
		justify-content: center;
		font-size: 0.8rem;
		font-weight: 600;
	}

	.user-name {
		color: var(--text-secondary);
		font-size: 0.85rem;
	}

	.logout-btn {
		padding: var(--space-1) var(--space-3);
		background: transparent;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.8rem;
		cursor: pointer;
		transition: color 0.15s, border-color 0.15s;
	}

	.logout-btn:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
	}

	.console-main {
		max-width: 960px;
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
	}

	@media (max-width: 640px) {
		.console-nav {
			padding: 0 var(--space-4);
		}

		.nav-left {
			gap: var(--space-4);
		}

		.user-name {
			display: none;
		}

		.console-main {
			padding: var(--space-6) var(--space-4);
		}
	}
</style>
