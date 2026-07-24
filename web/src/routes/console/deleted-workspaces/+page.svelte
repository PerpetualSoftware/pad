<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import PageHeader from '$lib/components/common/PageHeader.svelte';
	import EmptyState from '$lib/components/common/EmptyState.svelte';
	import type { DeletedWorkspace } from '$lib/types';

	// Deleted workspaces page (TASK-1975). Lists the caller's soft-deleted
	// workspaces still inside the purge window and lets them restore any of
	// them before permanent deletion. Restore un-deletes the workspace and
	// drops the row from the table.

	let workspaces = $state<DeletedWorkspace[]>([]);
	let loading = $state(true);
	let error = $state('');

	// Per-slug pending flag so a Restore button disables while its request
	// is in flight without blocking the other rows.
	let restoring = $state<Record<string, boolean>>({});

	async function load() {
		loading = true;
		error = '';
		try {
			const result = await api.workspaces.listDeleted();
			workspaces = Array.isArray(result) ? result : [];
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load deleted workspaces';
		} finally {
			loading = false;
		}
	}

	async function restore(ws: DeletedWorkspace) {
		if (restoring[ws.slug]) return;
		restoring[ws.slug] = true;
		try {
			await api.workspaces.restore(ws.slug);
			workspaces = workspaces.filter((w) => w.slug !== ws.slug);
			toastStore.show(`Restored "${ws.name}"`, 'success');
		} catch (e) {
			const msg = e instanceof Error ? e.message : 'Failed to restore workspace';
			toastStore.show(msg, 'error');
		} finally {
			restoring[ws.slug] = false;
		}
	}

	function formatDate(dateStr: string | undefined | null): string {
		if (!dateStr) return '—';
		const d = new Date(dateStr);
		if (Number.isNaN(d.getTime())) return '—';
		return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
	}

	// "N days left" until permanent deletion. When the window is nearly up
	// (0 days) we swap in a plain-language warning instead of "0 days left".
	function daysLeftLabel(days: number): string {
		if (days <= 0) return 'Permanently deleted soon';
		if (days === 1) return '1 day left';
		return `${days} days left`;
	}

	// Low-window rows get a warning accent so the deadline stands out.
	function isLowWindow(days: number): boolean {
		return days <= 3;
	}

	onMount(load);
</script>

<svelte:head>
	<title>Deleted workspaces - Pad</title>
</svelte:head>

<div class="page">
	<PageHeader
		title="Deleted workspaces"
		description="Workspaces you've deleted are kept for a short window before they're permanently removed. Restore one to bring it back with all its data."
	/>

	{#if loading}
		<div class="loading-msg">Loading&hellip;</div>
	{:else if error}
		<div class="error-msg">
			<p>{error}</p>
			<button class="btn" onclick={load}>Retry</button>
		</div>
	{:else if workspaces.length === 0}
		<div class="empty-card">
			<EmptyState
				title="No recently deleted workspaces"
				message="Workspaces you delete will appear here until they're permanently removed."
			/>
		</div>
	{:else}
		<div class="table-wrap">
			<table class="ws-table">
				<thead>
					<tr>
						<th scope="col">Name</th>
						<th scope="col">Deleted</th>
						<th scope="col">Time left</th>
						<th scope="col" class="actions-col"><span class="sr-only">Actions</span></th>
					</tr>
				</thead>
				<tbody>
					{#each workspaces as ws (ws.slug)}
						<tr>
							<td class="name-cell">
								<span class="ws-name">{ws.name}</span>
								<span class="ws-slug">{ws.slug}</span>
							</td>
							<td class="date-cell" title={ws.deleted_at ?? ''}>
								{formatDate(ws.deleted_at)}
							</td>
							<td class="days-cell">
								<span class="days-badge" class:low={isLowWindow(ws.days_left)} title={`Permanently deleted ${formatDate(ws.purge_at)}`}>
									{daysLeftLabel(ws.days_left)}
								</span>
							</td>
							<td class="actions-cell">
								<button
									class="btn"
									onclick={() => restore(ws)}
									disabled={!!restoring[ws.slug]}
								>
									{restoring[ws.slug] ? 'Restoring…' : 'Restore'}
								</button>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</div>

<style>
	/* Header spacing comes from the shared <PageHeader> primitive's own
	   margin-bottom (the old .page flex gap would have doubled it). */

	/* States */
	.loading-msg {
		color: var(--text-muted);
		padding: var(--space-6) 0;
		text-align: center;
		font-size: 0.9rem;
	}

	.error-msg {
		color: var(--accent-red);
		padding: var(--space-4) var(--space-6);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
	}

	.error-msg p {
		margin: 0;
		font-size: 0.85rem;
	}

	/* Layout-only wrapper: the boxed surface around the shared
	   <EmptyState> (which owns padding/centering/typography). */
	.empty-card {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}

	/* Table */
	.table-wrap {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		overflow: hidden;
	}

	.ws-table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.9rem;
	}

	.ws-table th {
		text-align: left;
		padding: var(--space-3) var(--space-4);
		font-size: 0.72rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-muted);
		border-bottom: 1px solid var(--border);
		background: var(--bg-tertiary);
	}

	.ws-table td {
		padding: var(--space-3) var(--space-4);
		border-bottom: 1px solid var(--border);
		color: var(--text-secondary);
		vertical-align: middle;
	}

	.ws-table tbody tr:last-child td {
		border-bottom: none;
	}

	.name-cell {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}

	.ws-name {
		font-weight: 600;
		color: var(--text-primary);
	}

	.ws-slug {
		font-size: 0.78rem;
		color: var(--text-muted);
		font-family: var(--font-mono, ui-monospace, SFMono-Regular, monospace);
	}

	.date-cell {
		white-space: nowrap;
	}

	.days-badge {
		display: inline-block;
		padding: 2px 8px;
		border-radius: 999px;
		font-size: 0.75rem;
		font-weight: 500;
		background: var(--bg-tertiary);
		color: var(--text-secondary);
		border: 1px solid var(--border);
		white-space: nowrap;
	}

	.days-badge.low {
		background: rgba(239, 68, 68, 0.1);
		color: var(--accent-red);
		border-color: rgba(239, 68, 68, 0.3);
	}

	.actions-col {
		width: 1%;
	}

	.actions-cell {
		text-align: right;
		white-space: nowrap;
	}

	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		padding: 0;
		margin: -1px;
		overflow: hidden;
		clip: rect(0, 0, 0, 0);
		white-space: nowrap;
		border: 0;
	}

	/* Buttons */
	.btn {
		padding: var(--space-2) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-family: inherit;
		font-size: 0.85rem;
		font-weight: 500;
		cursor: pointer;
		transition: background 0.15s, color 0.15s, border-color 0.15s;
	}

	.btn:hover:not(:disabled) {
		background: var(--bg-tertiary);
	}

	.btn:disabled {
		opacity: 0.5;
		cursor: default;
	}

	@media (max-width: 640px) {
		.ws-table th:nth-child(2),
		.ws-table td:nth-child(2) {
			display: none;
		}
	}
</style>
