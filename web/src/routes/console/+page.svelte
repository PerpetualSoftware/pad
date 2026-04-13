<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import type { Workspace } from '$lib/types';

	let workspaces = $state<Workspace[]>([]);
	let loading = $state(true);
	let error = $state('');

	let ownedWorkspaces = $derived(
		workspaces.filter((w) => w.owner_id === authStore.userId)
	);
	let sharedWorkspaces = $derived(
		workspaces.filter((w) => w.owner_id !== authStore.userId)
	);

	onMount(async () => {
		try {
			workspaces = await api.workspaces.list();
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to load workspaces';
		} finally {
			loading = false;
		}
	});

	function workspaceUrl(ws: Workspace): string {
		const owner = ws.owner_username || authStore.user?.username || '';
		return `/${owner}/${ws.slug}`;
	}

	function formatDate(dateStr: string): string {
		const d = new Date(dateStr);
		return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
	}
</script>

<svelte:head>
	<title>Console - Pad</title>
</svelte:head>

<div class="console-page">
	{#if loading}
		<div class="loading">Loading workspaces...</div>
	{:else if error}
		<div class="error-msg">{error}</div>
	{:else}
		<section class="section">
			<div class="section-header">
				<h2 class="section-title">Your Workspaces</h2>
				<a href="/console/new" class="create-btn">Create Workspace</a>
			</div>

			{#if ownedWorkspaces.length === 0}
				<div class="empty-state">
					<p class="empty-text">You don't have any workspaces yet.</p>
					<a href="/console/new" class="empty-action">Create your first workspace</a>
				</div>
			{:else}
				<div class="workspace-grid">
					{#each ownedWorkspaces as ws (ws.id)}
						<a href={workspaceUrl(ws)} class="workspace-card">
							<div class="card-top">
								<span class="card-name">{ws.name}</span>
							</div>
							{#if ws.description}
								<p class="card-desc">{ws.description}</p>
							{/if}
							<div class="card-meta">
								<span>Created {formatDate(ws.created_at)}</span>
							</div>
						</a>
					{/each}
				</div>
			{/if}
		</section>

		<section class="section">
			<div class="section-header">
				<h2 class="section-title">Shared With Me</h2>
			</div>

			{#if sharedWorkspaces.length === 0}
				<div class="empty-state">
					<p class="empty-text">No workspaces have been shared with you yet.</p>
				</div>
			{:else}
				<div class="workspace-grid">
					{#each sharedWorkspaces as ws (ws.id)}
						<a href={workspaceUrl(ws)} class="workspace-card shared">
							<div class="card-top">
								<span class="card-name">{ws.name}</span>
								{#if ws.is_guest}
									<span class="badge guest">Guest</span>
								{:else}
									<span class="badge member">Member</span>
								{/if}
							</div>
							{#if ws.description}
								<p class="card-desc">{ws.description}</p>
							{/if}
							<div class="card-meta">
								<span>{ws.owner_username ?? 'Unknown owner'}</span>
							</div>
						</a>
					{/each}
				</div>
			{/if}
		</section>
	{/if}
</div>

<style>
	.console-page {
		display: flex;
		flex-direction: column;
		gap: var(--space-10);
	}

	.loading {
		color: var(--text-muted);
		padding: var(--space-10) 0;
		text-align: center;
	}

	.error-msg {
		color: #ef4444;
		padding: var(--space-6);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}

	.section {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.section-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
	}

	.section-title {
		font-size: 1.1rem;
		font-weight: 600;
		color: var(--text-primary);
	}

	.create-btn {
		padding: var(--space-2) var(--space-4);
		background: var(--accent-blue);
		color: #fff;
		border-radius: var(--radius);
		font-size: 0.85rem;
		font-weight: 500;
		text-decoration: none;
		transition: opacity 0.15s;
	}

	.create-btn:hover {
		opacity: 0.9;
		text-decoration: none;
	}

	.workspace-grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
		gap: var(--space-4);
	}

	.workspace-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-5);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		text-decoration: none;
		transition: border-color 0.15s, background 0.15s;
	}

	.workspace-card:hover {
		border-color: var(--accent-blue);
		background: var(--bg-tertiary);
		text-decoration: none;
	}

	.card-top {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-2);
	}

	.card-name {
		font-weight: 600;
		color: var(--text-primary);
		font-size: 0.95rem;
	}

	.card-desc {
		color: var(--text-secondary);
		font-size: 0.85rem;
		line-height: 1.4;
		display: -webkit-box;
		-webkit-line-clamp: 2;
		line-clamp: 2;
		-webkit-box-orient: vertical;
		overflow: hidden;
	}

	.card-meta {
		color: var(--text-muted);
		font-size: 0.8rem;
		margin-top: var(--space-1);
	}

	.badge {
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-weight: 500;
		flex-shrink: 0;
	}

	.badge.member {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}

	.badge.guest {
		background: color-mix(in srgb, var(--accent-amber) 15%, transparent);
		color: var(--accent-amber);
	}

	.empty-state {
		padding: var(--space-10) var(--space-6);
		text-align: center;
		background: var(--bg-secondary);
		border: 1px dashed var(--border);
		border-radius: var(--radius-lg);
	}

	.empty-text {
		color: var(--text-muted);
		font-size: 0.9rem;
		margin-bottom: var(--space-3);
	}

	.empty-action {
		color: var(--accent-blue);
		font-size: 0.9rem;
		font-weight: 500;
	}
</style>
