<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import type { Collection } from '$lib/types';
	import { parseSchema } from '$lib/types';
	import CreateCollectionModal from '$lib/components/collections/CreateCollectionModal.svelte';
	import EditCollectionModal from '$lib/components/collections/EditCollectionModal.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';

	let wsSlug = $derived(page.params.workspace);
	let loading = $state(true);
	let collections = $state<Collection[]>([]);
	let wsName = $state('');
	let savingName = $state(false);
	let nameStatus = $state<'idle' | 'saved' | 'error'>('idle');
	let theme = $state<'light' | 'dark'>('dark');
	let showCreateModal = $state(false);
	let editingCollection = $state<Collection | null>(null);

	$effect(() => {
		if (wsSlug) load(wsSlug);
	});

	onMount(() => {
		const stored = localStorage.getItem('pad-theme');
		if (stored === 'light' || stored === 'dark') {
			theme = stored;
		} else {
			theme = document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
		}
	});
	async function load(slug: string) {
		loading = true;
		try {
			await workspaceStore.setCurrent(slug);
			wsName = workspaceStore.current?.name ?? '';
			collections = await api.collections.list(slug);
		} catch { /* allow partial render */
		} finally {
			loading = false;
		}
	}
	async function saveName() {
		if (!wsName.trim() || savingName) return;
		savingName = true;
		nameStatus = 'idle';
		try {
			await api.workspaces.update(wsSlug, { name: wsName.trim() });
			nameStatus = 'saved';
			setTimeout(() => (nameStatus = 'idle'), 2000);
		} catch {
			nameStatus = 'error';
		} finally {
			savingName = false;
		}
	}
	function toggleTheme() {
		theme = theme === 'dark' ? 'light' : 'dark';
		document.documentElement.setAttribute('data-theme', theme);
		localStorage.setItem('pad-theme', theme);
	}
	async function handleCollectionCreated() {
		collections = await api.collections.list(wsSlug);
		collectionStore.loadCollections(wsSlug);
		showCreateModal = false;
	}
	async function handleCollectionUpdated() {
		collections = await api.collections.list(wsSlug);
		collectionStore.loadCollections(wsSlug);
		editingCollection = null;
	}
	let createdDate = $derived(
		workspaceStore.current?.created_at
			? new Date(workspaceStore.current.created_at).toLocaleDateString('en-US', {
					year: 'numeric',
					month: 'long',
					day: 'numeric'
				})
			: ''
	);
</script>

<div class="settings">
	{#if loading}
		<div class="loading">Loading settings...</div>
	{:else}
		<header class="settings-header">
			<h1>Settings</h1>
		</header>
		<section class="section">
			<h2>Workspace</h2>
			<div class="card">
				<div class="field-row">
					<label for="ws-name">Name</label>
					<div class="inline-edit">
						<input id="ws-name" type="text" bind:value={wsName} onkeydown={(e) => e.key === 'Enter' && saveName()} />
						<button class="btn btn-small" onclick={saveName} disabled={savingName}>
							{savingName ? 'Saving...' : 'Save'}
						</button>
						{#if nameStatus === 'saved'}
							<span class="status-saved">Saved</span>
						{:else if nameStatus === 'error'}
							<span class="status-error">Error</span>
						{/if}
					</div>
				</div>
				<div class="field-row">
					<span class="field-label">Slug</span>
					<span class="field-value mono">{wsSlug}</span>
				</div>
				{#if createdDate}
					<div class="field-row">
						<span class="field-label">Created</span>
						<span class="field-value">{createdDate}</span>
					</div>
				{/if}
			</div>
		</section>
		<section class="section">
			<h2>Collections</h2>
			{#if collections.length === 0}
				<p class="empty-text">No collections yet.</p>
			{:else}
				<div class="coll-list">
					{#each collections as coll (coll.id)}
						{@const schema = parseSchema(coll)}
						<button class="card coll-card coll-card-btn" onclick={() => (editingCollection = coll)}>
							<div class="coll-header">
								<span class="coll-icon">{coll.icon || '#'}</span>
								<span class="coll-name">{coll.name}</span>
								<span class="coll-slug mono">/{coll.slug}</span>
								<span class="coll-count">{coll.item_count ?? 0} items</span>
								{#if coll.is_default}
									<span class="badge">default</span>
								{/if}
								<span class="edit-hint">Edit</span>
							</div>
							{#if schema.fields.length > 0}
								<div class="field-tags">
									{#each schema.fields as field (field.key)}
										<span class="field-tag">{field.key}: {field.type}</span>
									{/each}
								</div>
							{/if}
						</button>
					{/each}
				</div>
			{/if}
			<button class="btn btn-create" onclick={() => (showCreateModal = true)}>
				+ Create Collection
			</button>
			<CreateCollectionModal
				open={showCreateModal}
				{wsSlug}
				oncreated={handleCollectionCreated}
				onclose={() => (showCreateModal = false)}
			/>
			{#if editingCollection}
				<EditCollectionModal
					open={true}
					collection={editingCollection}
					{wsSlug}
					onupdated={handleCollectionUpdated}
					onclose={() => (editingCollection = null)}
				/>
			{/if}
		</section>
		<section class="section">
			<h2>Theme</h2>
			<div class="card">
				<div class="theme-row">
					<span>Appearance</span>
					<button class="theme-toggle" onclick={toggleTheme}>
						<span class="theme-option" class:active={theme === 'light'}>Light</span>
						<span class="theme-option" class:active={theme === 'dark'}>Dark</span>
					</button>
				</div>
			</div>
		</section>
	{/if}
</div>

<style>
	.settings { max-width: var(--content-max-width); margin: 0 auto; padding: var(--space-8) var(--space-6); }
	.loading { text-align: center; padding-top: 20vh; color: var(--text-muted); }
	.settings-header { margin-bottom: var(--space-8); }
	.settings-header h1 { font-size: 1.6em; }
	.section { margin-bottom: var(--space-8); }
	.section h2 { font-size: 1.1em; color: var(--text-secondary); margin-bottom: var(--space-4); }
	.card { background: var(--bg-secondary); border: 1px solid var(--border); border-radius: var(--radius); padding: var(--space-4); }
	.card + .card { margin-top: var(--space-3); }
	.field-row { display: flex; align-items: center; gap: var(--space-3); padding: var(--space-2) 0; flex-wrap: wrap; }
	.field-row + .field-row { border-top: 1px solid var(--border-subtle); }
	.field-row label, .field-label { width: 80px; font-size: 0.85em; color: var(--text-secondary); flex-shrink: 0; }
	.field-value { font-size: 0.9em; }
	.mono { font-family: var(--font-mono); font-size: 0.85em; }
	.inline-edit { display: flex; align-items: center; gap: var(--space-2); flex: 1; min-width: 0; flex-wrap: wrap; }
	.inline-edit input { flex: 1; min-width: 120px; max-width: 300px; }
	.status-saved { font-size: 0.8em; color: var(--accent-green); }
	.status-error { font-size: 0.8em; color: var(--accent-orange); }
	.btn { padding: var(--space-2) var(--space-4); background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: var(--radius); font-size: 0.85em; cursor: pointer; color: var(--text-primary); }
	.btn:hover { background: var(--bg-hover); }
	.btn:disabled { opacity: 0.5; cursor: not-allowed; }
	.btn-small { padding: var(--space-1) var(--space-3); font-size: 0.8em; }
	.btn-primary { background: var(--accent-blue); border-color: var(--accent-blue); color: #fff; }
	.btn-primary:hover { opacity: 0.9; }
	.btn-create { margin-top: var(--space-3); width: 100%; padding: var(--space-3); background: var(--bg-secondary); border: 1px dashed var(--border); border-radius: var(--radius); color: var(--text-secondary); font-size: 0.85em; cursor: pointer; }
	.btn-create:hover { border-color: var(--accent-blue); color: var(--accent-blue); }
	.coll-list { display: flex; flex-direction: column; gap: var(--space-3); }
	.coll-card { padding: var(--space-3) var(--space-4); }
	.coll-header { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
	.coll-icon { font-size: 1.1em; }
	.coll-name { font-weight: 600; font-size: 0.95em; }
	.coll-slug { color: var(--text-muted); font-size: 0.8em; }
	.coll-card-btn { cursor: pointer; text-align: left; width: 100%; transition: border-color 0.15s; }
	.coll-card-btn:hover { border-color: var(--accent-blue); }
	.edit-hint { font-size: 0.75em; color: var(--text-muted); opacity: 0; transition: opacity 0.15s; }
	.coll-card-btn:hover .edit-hint { opacity: 1; color: var(--accent-blue); }
	.coll-count { margin-left: auto; font-size: 0.8em; color: var(--text-muted); }
	.badge { font-size: 0.7em; background: var(--accent-blue); color: #fff; padding: 1px 6px; border-radius: 10px; font-weight: 600; }
	.field-tags { display: flex; flex-wrap: wrap; gap: var(--space-1); margin-top: var(--space-2); }
	.field-tag { font-size: 0.75em; font-family: var(--font-mono); background: var(--bg-tertiary); color: var(--text-secondary); padding: 1px 8px; border-radius: 10px; }
	.empty-text { color: var(--text-muted); font-size: 0.9em; }
	.theme-row { display: flex; align-items: center; justify-content: space-between; font-size: 0.9em; }
	.theme-toggle { display: flex; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: var(--radius); overflow: hidden; cursor: pointer; }
	.theme-option { padding: var(--space-1) var(--space-4); font-size: 0.85em; transition: background 0.15s, color 0.15s; }
	.theme-option.active { background: var(--accent-blue); color: #fff; }
</style>
