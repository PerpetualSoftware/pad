<script lang="ts">
	import { goto } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { api } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { WorkspaceTemplate } from '$lib/types';

	let mode = $state<'create' | 'import'>('create');
	let newName = $state('');
	let selectedTemplate = $state('startup');
	let templates = $state<WorkspaceTemplate[]>([]);
	let loadingTemplates = $state(false);
	let importing = $state(false);
	let importFile = $state<File | null>(null);
	let fileInputEl = $state<HTMLInputElement>();
	let nameInputEl = $state<HTMLInputElement>();
	let dragging = $state(false);
	let dragCounter = 0;

	$effect(() => {
		if (uiStore.createWorkspaceOpen) {
			// Reset state on open
			mode = 'create';
			newName = '';
			selectedTemplate = 'startup';
			importFile = null;
			importing = false;
			// Load templates
			if (templates.length === 0) {
				loadingTemplates = true;
				api.templates.list().then(t => templates = t).catch(() => {}).finally(() => loadingTemplates = false);
			}
			// Focus name input
			requestAnimationFrame(() => nameInputEl?.focus());
		}
	});

	function close() {
		uiStore.closeCreateWorkspace();
	}

	async function createWorkspace() {
		if (!newName.trim()) return;
		try {
			const ws = await workspaceStore.create({
				name: newName.trim(),
				template: selectedTemplate || undefined
			});
			close();
			goto(`/${ws.slug}`);
		} catch {
			toastStore.show('Failed to create workspace', 'error');
		}
	}

	async function importWorkspace() {
		if (!importFile) return;
		importing = true;
		try {
			const text = await importFile.text();
			const data = JSON.parse(text);
			const nameOverride = newName.trim() || '';
			const url = nameOverride
				? `/workspaces/import?name=${encodeURIComponent(nameOverride)}`
				: '/workspaces/import';
			const ws = await api.raw.post(url, data);
			await workspaceStore.loadAll();
			close();
			toastStore.show(`Imported workspace "${ws.name}"`, 'success');
			goto(`/${ws.slug}`);
		} catch (err) {
			toastStore.show(`Import failed: ${err instanceof Error ? err.message : 'Unknown error'}`, 'error');
		} finally {
			importing = false;
		}
	}

	function setFile(file: File) {
		importFile = file;
		mode = 'import';
		if (!newName.trim()) {
			newName = file.name.replace(/-export\.json$|\.json$/i, '');
		}
	}

	function handleFileSelect(e: Event) {
		const input = e.target as HTMLInputElement;
		const file = input.files?.[0];
		if (file) setFile(file);
	}

	function handleDrop(e: DragEvent) {
		e.preventDefault();
		dragCounter = 0;
		dragging = false;
		const file = e.dataTransfer?.files[0];
		if (file && file.name.endsWith('.json')) setFile(file);
	}

	function handleDragOver(e: DragEvent) {
		e.preventDefault();
	}

	function handleDragEnter(e: DragEvent) {
		e.preventDefault();
		dragCounter++;
		dragging = true;
	}

	function handleDragLeave() {
		dragCounter--;
		if (dragCounter <= 0) {
			dragCounter = 0;
			dragging = false;
		}
	}
</script>

{#if uiStore.createWorkspaceOpen}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="modal-backdrop" onclick={close}></div>
	<div class="modal" role="dialog">
		<div class="modal-header">
			<h2>New Workspace</h2>
			<button class="modal-close" onclick={close}>✕</button>
		</div>

		<div class="modal-tabs">
			<button class="tab" class:active={mode === 'create'} onclick={() => mode = 'create'}>Create</button>
			<button class="tab" class:active={mode === 'import'} onclick={() => mode = 'import'}>Import</button>
		</div>

		<div class="modal-body">
			<label class="field-label" for="ws-create-name">Name</label>
			<input
				id="ws-create-name"
				bind:this={nameInputEl}
				bind:value={newName}
				placeholder="Workspace name"
				onkeydown={(e) => e.key === 'Enter' && (mode === 'create' ? createWorkspace() : importWorkspace())}
			/>

			{#if mode === 'create'}
				{#if templates.length > 0}
					<label class="field-label">Template</label>
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
			{:else}
				<label class="field-label">Export file</label>
				<!-- svelte-ignore a11y_no_static_element_interactions -->
				<div
					class="drop-zone"
					class:dragging
					class:has-file={!!importFile}
					onclick={() => fileInputEl?.click()}
					ondrop={handleDrop}
					ondragover={handleDragOver}
					ondragenter={handleDragEnter}
					ondragleave={handleDragLeave}
				>
					{#if importFile}
						<span class="drop-file-name">{importFile.name}</span>
						<span class="drop-hint">Click or drop to replace</span>
					{:else}
						<span class="drop-icon" class:drop-icon-active={dragging}>↓</span>
						<span class="drop-text">{dragging ? 'Drop JSON file here' : 'Drop export file here or click to browse'}</span>
					{/if}
					<input
						bind:this={fileInputEl}
						type="file"
						accept=".json"
						oninput={handleFileSelect}
						style="display:none"
					/>
				</div>
				{#if importFile}
					<p class="import-hint">Creates a new workspace with regenerated IDs. Original data is unchanged.</p>
				{/if}
			{/if}
		</div>

		<div class="modal-footer">
			<button class="cancel-btn" onclick={close}>Cancel</button>
			{#if mode === 'create'}
				<button class="action-btn" onclick={createWorkspace} disabled={!newName.trim()}>
					Create Workspace
				</button>
			{:else}
				<button class="action-btn" onclick={importWorkspace} disabled={!importFile || importing}>
					{importing ? 'Importing...' : 'Import Workspace'}
				</button>
			{/if}
		</div>
	</div>
{/if}

<style>
	.modal-backdrop {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		z-index: 200;
	}
	.modal {
		position: fixed;
		top: 50%;
		left: 50%;
		transform: translate(-50%, -50%);
		width: 90%;
		max-width: 480px;
		max-height: 85vh;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 16px 48px rgba(0, 0, 0, 0.4);
		z-index: 201;
		display: flex;
		flex-direction: column;
		overflow: hidden;
		animation: modal-in 0.15s ease-out;
	}
	@keyframes modal-in {
		from { opacity: 0; transform: translate(-50%, -48%); }
		to { opacity: 1; transform: translate(-50%, -50%); }
	}
	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--border);
	}
	.modal-header h2 { margin: 0; font-size: 1.1em; }
	.modal-close {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 1.1em;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
	}
	.modal-close:hover { background: var(--bg-hover); color: var(--text-primary); }

	.modal-tabs {
		display: flex;
		border-bottom: 1px solid var(--border);
	}
	.tab {
		flex: 1;
		padding: var(--space-2) var(--space-4);
		background: none;
		border: none;
		font-size: 0.88em;
		font-weight: 500;
		color: var(--text-muted);
		cursor: pointer;
		border-bottom: 2px solid transparent;
		transition: color 0.15s, border-color 0.15s;
	}
	.tab:hover { color: var(--text-secondary); }
	.tab.active { color: var(--accent-blue); border-bottom-color: var(--accent-blue); }

	.modal-body {
		padding: var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		overflow-y: auto;
	}
	.field-label {
		font-size: 0.8em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}
	.modal-body input[type="text"],
	.modal-body input:not([type]) {
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.9em;
	}
	.modal-body input:focus { outline: none; border-color: var(--accent-blue); }

	.template-list { display: flex; flex-direction: column; gap: 4px; }
	.template-card {
		display: flex; flex-direction: column; gap: 1px;
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius-sm);
		background: var(--bg-tertiary);
		text-align: left; cursor: pointer;
		border: 1px solid transparent;
		transition: border-color 0.1s;
	}
	.template-card:hover { border-color: var(--border); }
	.template-card.selected {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 8%, var(--bg-tertiary));
	}
	.tpl-name { font-size: 0.88em; font-weight: 600; color: var(--text-primary); text-transform: capitalize; }
	.tpl-desc { font-size: 0.78em; color: var(--text-muted); }

	.drop-zone {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: var(--space-2);
		min-height: 90px;
		padding: var(--space-4);
		background: var(--bg-tertiary);
		border: 2px dashed var(--border);
		border-radius: var(--radius);
		cursor: pointer;
		transition: border-color 0.15s, background 0.15s;
		text-align: center;
	}
	.drop-zone:hover { border-color: var(--accent-blue); }
	.drop-zone.dragging {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 8%, var(--bg-tertiary));
	}
	.drop-zone.has-file { border-style: solid; border-color: var(--accent-green); }
	.drop-icon { font-size: 1.4em; color: var(--text-muted); transition: color 0.15s, transform 0.15s; }
	.drop-icon-active { color: var(--accent-blue); transform: translateY(2px); }
	.drop-text { font-size: 0.82em; color: var(--text-muted); }
	.drop-file-name { font-size: 0.88em; font-weight: 500; color: var(--text-primary); }
	.drop-hint { font-size: 0.75em; color: var(--text-muted); }
	.import-hint { font-size: 0.8em; color: var(--text-muted); margin: 0; }

	.modal-footer {
		display: flex; align-items: center; justify-content: flex-end;
		gap: var(--space-3);
		padding: var(--space-4) var(--space-5);
		border-top: 1px solid var(--border);
	}
	.action-btn {
		background: var(--accent-blue); color: #fff;
		padding: var(--space-2) var(--space-5);
		border-radius: var(--radius);
		font-size: 0.88em; font-weight: 500; cursor: pointer;
	}
	.action-btn:hover:not(:disabled) { filter: brightness(1.1); }
	.action-btn:disabled { opacity: 0.5; cursor: not-allowed; }
	.cancel-btn {
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius);
		font-size: 0.88em; color: var(--text-muted);
		background: var(--bg-tertiary);
		border: 1px solid var(--border); cursor: pointer;
	}
	.cancel-btn:hover { background: var(--bg-hover); color: var(--text-primary); }
</style>
