<!--
	YouSheet — the mobile "You" slot surface (PLAN-1694, TASK-1701).

	A docked sheet (anchored above the nav) for ACCOUNT concerns only — the
	workspace nav now lives in WorkspaceSheet. Profile header, theme toggle, and
	the account actions carried over from the retired TopBar user menu
	(Settings/Workspaces/Billing/Admin/Connect/Resources/Sign out).

	Purpose-designed rows (no reused BottomSheet / dropdown). Resources reuse the
	UserMenuResources link list (pure content) restyled to match.
-->
<script lang="ts">
	import { onMount } from 'svelte';
	import { goto, afterNavigate } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { api } from '$lib/api/client';
	import { avatarColor, avatarInitial } from '$lib/utils/avatar';
	import DockedSheet from '$lib/components/layout/DockedSheet.svelte';
	import UserMenuResources from '$lib/components/layout/UserMenuResources.svelte';
	import ConnectWorkspaceModal from '$lib/components/ConnectWorkspaceModal.svelte';

	let { open, onclose }: { open: boolean; onclose: () => void } = $props();

	let wsSlug = $derived(workspaceStore.current?.slug);
	let userName = $derived(authStore.user?.name ?? '');
	let userEmail = $derived(authStore.user?.email ?? '');

	let connectOpen = $state(false);

	// Theme toggle — mirrors the app's mechanism (key 'pad-theme', data-theme).
	let currentTheme = $state<'dark' | 'light'>('dark');
	onMount(() => {
		const fromDom = document.documentElement.getAttribute('data-theme');
		let fromLs: string | null = null;
		try {
			fromLs = localStorage.getItem('pad-theme');
		} catch {
			fromLs = null;
		}
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

	// Close on navigation (console links leave the workspace).
	afterNavigate((nav) => {
		if (nav.type !== 'enter' && open) onclose();
	});

	function go(href: string) {
		onclose();
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

<DockedSheet {open} {onclose} label="Account">
	<div class="you">
		{#if authStore.user}
			<div class="profile">
				<span class="profile-avatar" style:background={avatarColor(userName || userEmail)}>
					{avatarInitial(userName || userEmail)}
				</span>
				<span class="profile-meta">
					<span class="profile-name">{userName}</span>
					<span class="profile-email">{userEmail}</span>
				</span>
			</div>

			<div class="rows">
				<!-- Theme toggle -->
				<button class="row" type="button" onclick={toggleTheme}>
					<span class="row-icon" aria-hidden="true">{currentTheme === 'dark' ? '🌙' : '☀️'}</span>
					<span class="row-label">Dark mode</span>
					<span class="toggle" class:on={currentTheme === 'dark'} aria-hidden="true">
						<span class="knob"></span>
					</span>
				</button>

				<a class="row" href="/console/settings" onclick={onclose}>
					<span class="row-icon" aria-hidden="true">⚙️</span>
					<span class="row-label">Account settings</span>
					<span class="row-chev" aria-hidden="true">›</span>
				</a>
				<a class="row" href="/console" onclick={onclose}>
					<span class="row-icon" aria-hidden="true">🗂️</span>
					<span class="row-label">Workspaces</span>
					<span class="row-chev" aria-hidden="true">›</span>
				</a>
				{#if authStore.cloudMode}
					<a class="row" href="/console/billing" onclick={onclose}>
						<span class="row-icon" aria-hidden="true">💳</span>
						<span class="row-label">Billing</span>
						<span class="row-chev" aria-hidden="true">›</span>
					</a>
				{/if}
				{#if authStore.user?.role === 'admin'}
					<a class="row" href="/console/admin" onclick={onclose}>
						<span class="row-icon" aria-hidden="true">🛡️</span>
						<span class="row-label">Admin</span>
						<span class="row-chev" aria-hidden="true">›</span>
					</a>
				{/if}
				{#if wsSlug}
					<button
						class="row"
						type="button"
						onclick={() => {
							onclose();
							connectOpen = true;
						}}
					>
						<span class="row-icon" aria-hidden="true">🔌</span>
						<span class="row-label">Connect a project…</span>
					</button>
				{/if}
			</div>

			<!-- Resources (external links) -->
			<div class="resources user-dropdown">
				<UserMenuResources cloudMode={authStore.cloudMode} {onclose} />
			</div>

			<button class="row signout" type="button" onclick={handleLogout}>
				<span class="row-icon" aria-hidden="true">↩︎</span>
				<span class="row-label">Sign out</span>
			</button>
		{/if}
	</div>
</DockedSheet>

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
		padding: 0 var(--space-4) var(--space-2);
	}

	.profile {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-1) var(--space-4);
	}
	.profile-avatar {
		flex-shrink: 0;
		width: 44px;
		height: 44px;
		border-radius: 50%;
		display: flex;
		align-items: center;
		justify-content: center;
		color: #fff;
		font-weight: 700;
		font-size: 1.2em;
	}
	.profile-meta {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}
	.profile-name {
		font-weight: 600;
		color: var(--text-primary);
		font-size: 1.05em;
	}
	.profile-email {
		font-size: 0.82em;
		color: var(--text-muted);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.rows {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-3);
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
		text-align: left;
		text-decoration: none;
		color: var(--text-secondary);
		font-size: 0.98em;
		font-family: var(--font-ui);
	}
	.row:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
	.row-icon {
		font-size: 1.05em;
		width: 1.5em;
		text-align: center;
		flex-shrink: 0;
	}
	.row-label {
		flex: 1;
		min-width: 0;
	}
	.row-chev {
		color: var(--text-muted);
		font-size: 1.1em;
	}
	.signout {
		margin-top: var(--space-2);
		color: var(--accent-orange);
	}
	.signout:hover {
		color: var(--accent-orange);
		background: color-mix(in srgb, var(--accent-orange) 12%, transparent);
	}

	/* Theme toggle pill */
	.toggle {
		flex-shrink: 0;
		width: 38px;
		height: 22px;
		border-radius: 999px;
		background: var(--border);
		position: relative;
		transition: background 0.15s ease;
	}
	.toggle.on {
		background: var(--accent-blue);
	}
	.knob {
		position: absolute;
		top: 2px;
		left: 2px;
		width: 18px;
		height: 18px;
		border-radius: 50%;
		background: #fff;
		transition: transform 0.15s ease;
	}
	.toggle.on .knob {
		transform: translateX(16px);
	}

	/* Resources — reuse UserMenuResources content, restyle to match the sheet. */
	.resources {
		margin-top: var(--space-2);
	}
	.resources :global(.dropdown-divider) {
		height: 1px;
		background: var(--border);
		margin: var(--space-2) 0;
	}
	.resources :global(.resources-label) {
		padding: var(--space-1) var(--space-3);
		font-size: 0.7em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}
	.resources :global(.dropdown-item) {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-3);
		border-radius: var(--radius-sm);
		font-size: 0.95em;
		color: var(--text-secondary);
		text-decoration: none;
	}
	.resources :global(.dropdown-item):hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
</style>
