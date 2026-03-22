<script lang="ts">
	import { goto } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { api } from '$lib/api/client';
	import type { WorkspaceTemplate } from '$lib/types';

	let open = $state(false);
	let creating = $state(false);
	let newName = $state('');
	let selectedTemplate = $state('startup');
	let templates = $state<WorkspaceTemplate[]>([]);
	let loadingTemplates = $state(false);

	function select(slug: string) {
		open = false;
		goto(`/${slug}`);
	}

	async function startCreating() {
		creating = true;
		if (templates.length === 0) {
			loadingTemplates = true;
			try {
				templates = await api.templates.list();
			} catch {
				templates = [];
			} finally {
				loadingTemplates = false;
			}
		}
	}

	async function createWorkspace() {
		if (!newName.trim()) return;
		const ws = await workspaceStore.create({
			name: newName.trim(),
			template: selectedTemplate || undefined
		});
		newName = '';
		creating = false;
		selectedTemplate = 'startup';
		open = false;
		goto(`/${ws.slug}`);
	}

	function cancelCreate() {
		creating = false;
		newName = '';
		selectedTemplate = 'startup';
	}
</script>

<div class="switcher">
	<button class="current" onclick={() => open = !open}>
		<span class="name">{workspaceStore.current?.name ?? 'Select workspace'}</span>
		<span class="chevron">{open ? '▲' : '▼'}</span>
	</button>

	{#if open}
		<!-- svelte-ignore a11y_click_events_have_key_events -->
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div class="backdrop" onclick={() => { open = false; creating = false; }}></div>
		<div class="dropdown">
			{#each workspaceStore.workspaces as ws}
				<button
					class="item"
					class:active={ws.slug === workspaceStore.current?.slug}
					onclick={() => select(ws.slug)}
				>
					{ws.name}
				</button>
			{/each}

			{#if creating}
				<div class="create-section">
					<input
						bind:value={newName}
						placeholder="Workspace name"
						onkeydown={(e) => e.key === 'Enter' && createWorkspace()}
					/>
					{#if templates.length > 0}
						<div class="template-label">Template</div>
						<div class="template-list">
							{#each templates as tpl (tpl.name)}
								<button
									class="template-card"
									class:selected={selectedTemplate === tpl.name}
									onclick={() => (selectedTemplate = tpl.name)}
								>
									<span class="tpl-name">{tpl.name}</span>
									<span class="tpl-desc">{tpl.collections.join(', ')}</span>
								</button>
							{/each}
							<button
								class="template-card"
								class:selected={selectedTemplate === ''}
								onclick={() => (selectedTemplate = '')}
							>
								<span class="tpl-name">blank</span>
								<span class="tpl-desc">Empty workspace</span>
							</button>
						</div>
					{/if}
					<div class="create-actions">
						<button class="create-btn" onclick={createWorkspace} disabled={!newName.trim()}>Create</button>
						<button class="cancel-btn" onclick={cancelCreate}>Cancel</button>
					</div>
				</div>
			{:else}
				<button class="item create-trigger" onclick={startCreating}>
					+ New Workspace
				</button>
			{/if}
		</div>
	{/if}
</div>

<style>
	.switcher { position: relative; }
	.current {
		width: 100%;
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border-radius: var(--radius);
		font-weight: 600;
		font-size: 0.9em;
	}
	.name {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.current:hover { background: var(--bg-tertiary); }
	.chevron { font-size: 0.7em; color: var(--text-muted); flex-shrink: 0; }
	.backdrop { position: fixed; inset: 0; z-index: 10; }
	.dropdown {
		position: absolute;
		top: 100%;
		left: 0;
		min-width: 240px;
		margin-top: 4px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 8px 24px rgba(0, 0, 0, 0.3);
		z-index: 11;
		overflow: hidden;
	}
	.item {
		display: block;
		width: 100%;
		text-align: left;
		padding: var(--space-2) var(--space-4);
	}
	.item:hover { background: var(--bg-hover); }
	.item.active { background: var(--bg-active); color: var(--accent-blue); }
	.create-trigger { color: var(--text-muted); border-top: 1px solid var(--border); }
	.create-section {
		padding: var(--space-3);
		border-top: 1px solid var(--border);
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.create-section input { font-size: 0.9em; }
	.template-label {
		font-size: 0.75em;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		margin-top: var(--space-1);
	}
	.template-list {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.template-card {
		display: flex;
		flex-direction: column;
		gap: 1px;
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius-sm);
		background: var(--bg-tertiary);
		text-align: left;
		cursor: pointer;
		border: 1px solid transparent;
		transition: border-color 0.1s;
	}
	.template-card:hover { border-color: var(--border); }
	.template-card.selected {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 8%, var(--bg-tertiary));
	}
	.tpl-name {
		font-size: 0.85em;
		font-weight: 600;
		color: var(--text-primary);
		text-transform: capitalize;
	}
	.tpl-desc {
		font-size: 0.75em;
		color: var(--text-muted);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.create-actions {
		display: flex;
		gap: var(--space-2);
		margin-top: var(--space-1);
	}
	.create-btn {
		flex: 1;
		background: var(--accent-blue);
		color: #fff;
		padding: var(--space-1) var(--space-3);
		border-radius: var(--radius-sm);
		font-size: 0.85em;
	}
	.create-btn:disabled { opacity: 0.5; cursor: not-allowed; }
	.cancel-btn {
		padding: var(--space-1) var(--space-3);
		border-radius: var(--radius-sm);
		font-size: 0.85em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
	}
	.cancel-btn:hover { background: var(--bg-hover); }
</style>
