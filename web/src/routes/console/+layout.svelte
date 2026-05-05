<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { authStore } from '$lib/stores/auth.svelte';
	import { api } from '$lib/api/client';
	import { onMount } from 'svelte';

	let { children } = $props();
	let ready = $state(false);
	let mobileMenuOpen = $state(false);

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

	// Auto-close the mobile menu whenever the route changes. Kept as its own
	// single-purpose effect per CONVE-606 (split reactive effects) so the
	// route-change concern doesn't get tangled with anything else.
	$effect(() => {
		void currentPath;
		mobileMenuOpen = false;
	});

	function isActive(path: string): boolean {
		if (path === '/console') return currentPath === '/console';
		return currentPath.startsWith(path);
	}

	async function logout() {
		await api.auth.logout();
		authStore.clear();
		goto('/login');
	}

	function closeMobileMenu() {
		mobileMenuOpen = false;
	}

	function handleWindowKeydown(event: KeyboardEvent) {
		if (event.key === 'Escape' && mobileMenuOpen) {
			mobileMenuOpen = false;
		}
	}

	function handleWindowClick(event: MouseEvent) {
		if (!mobileMenuOpen) return;
		const target = event.target as HTMLElement | null;
		if (!target) return;
		if (!target.closest('.console-nav')) {
			mobileMenuOpen = false;
		}
	}

	let initial = $derived(
		authStore.user?.name?.charAt(0)?.toUpperCase() ??
		authStore.user?.email?.charAt(0)?.toUpperCase() ?? '?'
	);
</script>

<svelte:window onkeydown={handleWindowKeydown} onclick={handleWindowClick} />

{#if ready}
	<div class="console-layout">
		<nav class="console-nav">
			<div class="nav-left">
				<a href="/console" class="nav-logo">Pad</a>
				<button
					class="mobile-hamburger"
					onclick={() => (mobileMenuOpen = !mobileMenuOpen)}
					aria-label={mobileMenuOpen ? 'Close menu' : 'Open menu'}
					aria-expanded={mobileMenuOpen}
					aria-controls="console-nav-links"
					title={mobileMenuOpen ? 'Close menu' : 'Open menu'}
				>
					{#if mobileMenuOpen}
						<svg width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
							<path d="M4 4L16 16" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>
							<path d="M16 4L4 16" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>
						</svg>
					{:else}
						<svg width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
							<rect y="3" width="20" height="2" rx="1" fill="currentColor"/>
							<rect y="9" width="20" height="2" rx="1" fill="currentColor"/>
							<rect y="15" width="20" height="2" rx="1" fill="currentColor"/>
						</svg>
					{/if}
				</button>
				<div
					id="console-nav-links"
					class="nav-links"
					class:open={mobileMenuOpen}
					role="menu"
				>
					<a
						href="/console"
						class="nav-link"
						role="menuitem"
						onclick={closeMobileMenu}
						class:active={isActive('/console') &&
							!isActive('/console/settings') &&
							!isActive('/console/billing') &&
							!isActive('/console/admin') &&
							!isActive('/console/connected-apps')}
					>
						Workspaces
					</a>
					<a
						href="/console/settings"
						class="nav-link"
						role="menuitem"
						onclick={closeMobileMenu}
						class:active={isActive('/console/settings')}
					>
						Settings
					</a>
					{#if authStore.cloudMode}
						<a
							href="/console/connected-apps"
							class="nav-link"
							role="menuitem"
							onclick={closeMobileMenu}
							class:active={isActive('/console/connected-apps')}
						>
							Connected Apps
						</a>
						<a
							href="/console/billing"
							class="nav-link"
							role="menuitem"
							onclick={closeMobileMenu}
							class:active={isActive('/console/billing')}
						>
							Billing
						</a>
					{/if}
					{#if authStore.user?.role === 'admin'}
						<a
							href="/console/admin"
							class="nav-link"
							role="menuitem"
							onclick={closeMobileMenu}
							class:active={isActive('/console/admin')}
						>
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
		position: relative;
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

	/*
		Hamburger button — hidden on desktop, shown on mobile. Mirrors
		TopBar.svelte's `.mobile-hamburger` (32×32, --text-secondary, hover
		with --bg-hover) so the chrome feels consistent with the main app
		shell. See BUG-1118.
	*/
	.mobile-hamburger {
		display: none;
		align-items: center;
		justify-content: center;
		width: 32px;
		height: 32px;
		border-radius: var(--radius-sm);
		color: var(--text-secondary);
		cursor: pointer;
		padding: 0;
		background: none;
		border: none;
		transition: color 0.15s, background 0.15s;
	}

	.mobile-hamburger:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
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

	@keyframes dropdown-in {
		from { opacity: 0; transform: translateY(-4px); }
		to { opacity: 1; transform: translateY(0); }
	}

	@media (max-width: 640px) {
		.console-nav {
			padding: 0 var(--space-4);
		}

		.nav-left {
			gap: var(--space-2);
		}

		.user-name {
			display: none;
		}

		.console-main {
			padding: var(--space-6) var(--space-4);
		}

		.mobile-hamburger {
			display: flex;
		}

		/*
			On mobile the .nav-links row turns into a dropdown panel below
			the navbar. Hidden by default; .open shows it. Visual style
			mirrors TopBar.svelte's `.user-dropdown` (BUG-1118).
		*/
		.nav-links {
			display: none;
			position: absolute;
			top: calc(100% + 6px);
			left: 0;
			right: 0;
			flex-direction: column;
			align-items: stretch;
			gap: 0;
			background: var(--bg-secondary);
			border: 1px solid var(--border);
			border-radius: var(--radius-lg);
			box-shadow: 0 8px 24px rgba(0, 0, 0, 0.3);
			z-index: 50;
			overflow: hidden;
			padding: var(--space-1) 0;
		}

		.nav-links.open {
			display: flex;
			animation: dropdown-in 0.12s ease-out;
		}

		.nav-link {
			display: block;
			width: 100%;
			padding: var(--space-3) var(--space-4);
			border-radius: 0;
			font-size: 0.9rem;
		}

		.nav-link:hover {
			background: var(--bg-hover);
		}

		.nav-link.active {
			color: var(--text-primary);
			background: var(--bg-tertiary);
		}
	}
</style>
