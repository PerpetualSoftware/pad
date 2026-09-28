<script lang="ts">
	// Soft-deleted workspaces still inside the restore window (TASK-1974),
	// with a Restore action per row. Moved out of WorkspaceSwitcher so the
	// TopBar "+" discovery surface (PLAN-3002 U4 / TASK-3276) and the mobile
	// switcher render the same piece instead of two copies.
	//
	// Renders NOTHING when the list is empty or the fetch failed, so it never
	// adds noise to a surface with nothing to restore.
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { DeletedWorkspace } from '$lib/types';

	interface Props {
		/** Re-fetched each time this turns true (the host surface opened). */
		active: boolean;
		/** Roomier rows for a mobile sheet. */
		roomy?: boolean;
	}

	let { active, roomy = false }: Props = $props();

	let deleted = $state<DeletedWorkspace[]>([]);
	// Collapsed by default; kept while the host stays open.
	let expanded = $state(false);
	// Slug being restored: per-row pending state, so a double-click cannot
	// fire two restores.
	let restoringSlug = $state<string | null>(null);
	// Monotonic token guarding listDeleted() responses: an older fetch cannot
	// clobber the fresher post-restore refresh and re-surface a restored
	// workspace.
	let deletedSeq = 0;

	async function loadDeleted() {
		const seq = ++deletedSeq;
		try {
			const list = await api.workspaces.listDeleted();
			if (seq === deletedSeq) deleted = list;
		} catch {
			// The restore affordance is a bonus, never a blocker. Clear so a
			// stale list does not linger and the section stays hidden.
			if (seq === deletedSeq) deleted = [];
		}
	}

	$effect(() => {
		if (active) loadDeleted();
	});

	async function restore(ws: DeletedWorkspace) {
		if (restoringSlug) return;
		restoringSlug = ws.slug;
		try {
			try {
				await api.workspaces.restore(ws.slug);
			} catch {
				// Only the restore call itself failing is a restore failure.
				toastStore.show(`Couldn't restore "${ws.name}"`, 'error');
				return;
			}
			// Confirmed before refreshing, so a failing reload cannot pass for
			// a restore failure.
			toastStore.show(`Restored "${ws.name}"`, 'success');
			try {
				await Promise.all([loadDeleted(), workspaceStore.loadAll()]);
			} catch {
				// Reload failure is non-fatal; the restore went through.
			}
		} finally {
			restoringSlug = null;
		}
	}
</script>

{#if deleted.length > 0}
	<div class="deleted-section" class:roomy>
		<button
			type="button"
			class="deleted-header"
			onclick={() => (expanded = !expanded)}
			aria-expanded={expanded}
		>
			<span class="deleted-caret" aria-hidden="true">{expanded ? '▾' : '▸'}</span>
			<span class="deleted-title">Recently deleted</span>
			<span class="deleted-count">{deleted.length}</span>
		</button>
		{#if expanded}
			<div class="deleted-list">
				{#each deleted as ws (ws.slug)}
					<div class="deleted-item">
						<span class="deleted-name" title={ws.name}>{ws.name}</span>
						<span class="deleted-days">
							{ws.days_left} {ws.days_left === 1 ? 'day' : 'days'} left
						</span>
						<button
							type="button"
							class="restore-btn"
							onclick={() => restore(ws)}
							disabled={restoringSlug === ws.slug}
						>
							{restoringSlug === ws.slug ? 'Restoring…' : 'Restore'}
						</button>
					</div>
				{/each}
			</div>
		{/if}
	</div>
{/if}

<style>
	.deleted-section { border-top: 1px solid var(--border); }
	.deleted-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		text-align: left;
		padding: var(--space-2) var(--space-4);
		background: none;
		border: none;
		cursor: pointer;
		color: var(--text-muted);
		font-size: 0.8em;
		text-transform: uppercase;
		letter-spacing: 0.03em;
	}
	.deleted-header:hover { background: var(--bg-hover); }
	.deleted-caret { font-size: 0.9em; flex-shrink: 0; }
	.deleted-title { flex: 1; min-width: 0; }
	.deleted-count {
		flex-shrink: 0;
		padding: 0 var(--space-2);
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
		font-size: 0.9em;
	}
	.deleted-list { display: flex; flex-direction: column; }
	.deleted-item {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-4);
		font-size: 0.9em;
	}
	.roomy .deleted-header { padding: var(--space-3); font-size: 0.85em; }
	.roomy .deleted-item { padding: var(--space-2) var(--space-3); }
	.deleted-name {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		color: var(--text-secondary);
	}
	.deleted-days {
		flex-shrink: 0;
		color: var(--text-muted);
		font-size: 0.85em;
		white-space: nowrap;
	}
	.restore-btn {
		flex-shrink: 0;
		padding: var(--space-1) var(--space-2);
		background: none;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--accent-blue);
		cursor: pointer;
		font-size: 0.85em;
	}
	.restore-btn:hover:not(:disabled) { background: var(--bg-hover); }
	.restore-btn:disabled { opacity: 0.6; cursor: default; }
</style>
