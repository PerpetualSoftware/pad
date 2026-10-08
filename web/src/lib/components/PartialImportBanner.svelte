<script lang="ts">
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import type { WorkspaceImportStatus } from '$lib/types';

	// TASK-896: a workspace a bundle import created and KEPT after an error
	// partway through. The server stores the marker, so this shows on every
	// visit until an owner keeps the workspace (clears the marker) or deletes
	// it. The reason (`note`) is sent to owners and admins only, and only by
	// GET /workspaces/{slug}; everyone else sees that it is partly imported.
	let {
		slug,
		status,
		isOwner,
		deleteHref = null,
		ondelete
	}: {
		slug: string;
		status: WorkspaceImportStatus | undefined;
		isOwner: boolean;
		// Where "Delete workspace" leads (the landing page links to the
		// settings danger zone).
		deleteHref?: string | null;
		// The settings page's alternative: switch to its danger-zone tab.
		ondelete?: () => void;
	} = $props();

	// The slug this banner cleared, so it hides at once, before the store
	// re-resolves `current` from the PATCH answer.
	let clearedFor = $state<string | null>(null);
	let note = $state<string | null>(null);
	let noteFor = $state<string | null>(null);
	let keeping = $state(false);
	let error = $state<string | null>(null);

	let visible = $derived(!!status && clearedFor !== slug);

	$effect(() => {
		const s = slug;
		if (!visible || !isOwner || noteFor === s) return;
		noteFor = s;
		note = null;
		api.workspaces
			.get(s)
			.then((ws) => {
				if (slug === s) note = ws.import_status?.note ?? null;
			})
			.catch(() => {
				// The banner stands without the reason.
			});
	});

	async function keep() {
		if (keeping) return;
		const s = slug;
		const epoch = authStore.identityEpoch;
		keeping = true;
		error = null;
		try {
			const updated = await api.workspaces.update(s, { clear_import_status: true });
			// The store is shared: never commit one session's answer into the
			// next signed-in identity's app (the settings page's rule).
			if (authStore.identityEpoch !== epoch) return;
			clearedFor = s;
			if (workspaceStore.current?.slug === s) await workspaceStore.setCurrent(updated);
			if (authStore.identityEpoch !== epoch) return;
			void workspaceStore.loadAll().catch(() => {});
		} catch (e) {
			if (authStore.identityEpoch !== epoch) return;
			error = e instanceof Error ? e.message : 'Could not update the workspace';
		} finally {
			keeping = false;
		}
	}
</script>

{#if visible}
	<div class="partial-import-banner" role="status" data-testid="partial-import-banner">
		<span class="pi-icon" aria-hidden="true">⚠</span>
		<div class="pi-body">
			<p class="pi-title">This workspace was only partly imported.</p>
			{#if isOwner}
				{#if note}
					<p class="pi-note" data-testid="partial-import-note">{note}</p>
				{/if}
				<p class="pi-help">
					Check that what arrived is what you need. Keep it as it is, or delete it and import the
					bundle again.
				</p>
				{#if error}
					<p class="pi-error" role="alert">{error}</p>
				{/if}
				<div class="pi-actions">
					<button type="button" class="pi-keep" onclick={keep} disabled={keeping}>
						{keeping ? 'Saving…' : 'Keep it'}
					</button>
					{#if deleteHref}
						<a class="pi-delete" href={deleteHref}>Delete workspace…</a>
					{:else if ondelete}
						<button type="button" class="pi-delete pi-link" onclick={ondelete}>Delete workspace…</button>
					{/if}
				</div>
			{/if}
		</div>
	</div>
{/if}

<style>
	.partial-import-banner {
		display: flex;
		gap: var(--space-3);
		align-items: flex-start;
		padding: var(--space-3) var(--space-4);
		margin-bottom: var(--space-4);
		border: 1px solid color-mix(in srgb, var(--accent-amber) 45%, transparent);
		background: color-mix(in srgb, var(--accent-amber) 10%, var(--bg-secondary));
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.9em;
	}
	.pi-icon {
		color: var(--accent-amber);
		line-height: 1.4;
	}
	.pi-body {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		min-width: 0;
	}
	.pi-body p {
		margin: 0;
	}
	.pi-title {
		font-weight: 600;
	}
	.pi-note {
		overflow-wrap: anywhere;
	}
	.pi-help {
		color: var(--text-secondary);
	}
	.pi-error {
		color: var(--accent-red);
	}
	.pi-actions {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3);
		align-items: center;
	}
	.pi-keep {
		padding: var(--space-1) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		background: var(--bg-primary);
		color: var(--text-primary);
		font: inherit;
		cursor: pointer;
	}
	.pi-keep:disabled {
		opacity: 0.6;
		cursor: default;
	}
	.pi-delete {
		color: var(--accent-red);
	}
	.pi-link {
		padding: 0;
		border: none;
		background: none;
		font: inherit;
		text-decoration: underline;
		cursor: pointer;
	}
</style>
