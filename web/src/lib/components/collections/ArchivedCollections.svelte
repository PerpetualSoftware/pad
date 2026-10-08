<script lang="ts">
	// TASK-2189: the workspace's archived collections, each restorable with its
	// items (an archive never deleted them). Owner-only, like the archive; the
	// caller renders it only for owners and the server enforces it.
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { ArchivedCollection } from '$lib/types';

	interface Props {
		wsSlug: string;
		/** Changes whenever the live collection list does, so an archive or a
		 *  restore elsewhere refreshes this list. */
		refreshKey: string;
		onrestored?: () => void;
	}
	let { wsSlug, refreshKey, onrestored }: Props = $props();

	let archived = $state<ArchivedCollection[]>([]);
	let loaded = $state(false);
	let restoring = $state<string | null>(null);
	let latest = 0;

	$effect(() => {
		const ws = wsSlug;
		void refreshKey;
		const token = ++latest;
		const isSameIdentity = authStore.identityFence();
		if (!ws) return;
		api.collections
			.archived(ws)
			.then((list) => {
				if (token !== latest || !isSameIdentity()) return;
				archived = list;
				loaded = true;
			})
			.catch(() => {
				if (token !== latest || !isSameIdentity()) return;
				loaded = true;
			});
		return () => {
			latest++;
		};
	});

	async function restore(a: ArchivedCollection) {
		const ws = wsSlug;
		const isSameIdentity = authStore.identityFence();
		restoring = a.id;
		try {
			await api.collections.restore(ws, a.id);
			if (!isSameIdentity() || ws !== wsSlug) return;
			archived = archived.filter((x) => x.id !== a.id);
			toastStore.show(`Restored "${a.name}" with its ${a.item_count} ${a.item_count === 1 ? 'item' : 'items'}`, 'success');
			onrestored?.();
		} catch {
			if (!isSameIdentity()) return;
			toastStore.show(`Failed to restore "${a.name}"`, 'error');
		} finally {
			if (restoring === a.id) restoring = null;
		}
	}
</script>

{#if loaded && archived.length > 0}
	<div class="archived" data-testid="archived-collections">
		<h3 class="archived-title">Archived collections</h3>
		<p class="archived-hint">Archived collections and their items are hidden, not deleted. Restore brings both back.</p>
		<ul class="archived-list">
			{#each archived as a (a.id)}
				<li class="archived-row">
					<span class="archived-icon" aria-hidden="true">{a.icon || '#'}</span>
					<span class="archived-name">{a.name}</span>
					<span class="archived-slug mono">/{a.slug}</span>
					<span class="archived-count">{a.item_count} {a.item_count === 1 ? 'item' : 'items'}</span>
					<span class="archived-date" title={new Date(a.archived_at).toLocaleString()}
						>archived {new Date(a.archived_at).toLocaleDateString()}</span
					>
					<button
						type="button"
						class="btn btn-restore"
						disabled={restoring === a.id}
						onclick={() => restore(a)}
						aria-label={`Restore ${a.name}`}
					>
						{restoring === a.id ? 'Restoring…' : 'Restore'}
					</button>
				</li>
			{/each}
		</ul>
	</div>
{/if}

<style>
	.archived {
		margin-top: var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.archived-title {
		margin: 0;
		font-size: 0.95em;
	}
	.archived-hint {
		margin: 0;
		font-size: 0.8em;
		color: var(--text-muted);
	}
	.archived-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.archived-row {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		border: 1px dashed var(--border);
		border-radius: var(--radius-sm, 6px);
		color: var(--text-secondary);
	}
	.archived-name {
		color: var(--text-primary);
		font-weight: 500;
	}
	.archived-slug,
	.archived-count,
	.archived-date {
		font-size: 0.8em;
		color: var(--text-muted);
	}
	.btn-restore {
		margin-left: auto;
	}
</style>
