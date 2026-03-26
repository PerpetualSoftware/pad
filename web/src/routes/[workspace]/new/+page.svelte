<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import Editor from '$lib/components/editor/Editor.svelte';
	import type { Collection, FieldDef } from '$lib/types';
	import { getStatusOptions, parseSchema, itemUrlId } from '$lib/types';
	import FieldEditor from '$lib/components/fields/FieldEditor.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';

	let wsSlug = $derived(page.params.workspace ?? '');

	let collections = $state<Collection[]>([]);
	let selectedColl = $state('');
	let title = $state('');
	let statusOverride = $state<string | null>(null);
	let content = $state('');
	let creating = $state(false);

	let extraFields = $state<Record<string, any>>({});

	let activeColl = $derived(collections.find(c => c.slug === selectedColl));
	let schema = $derived(activeColl ? parseSchema(activeColl) : { fields: [] });
	let statusOptions = $derived(activeColl ? getStatusOptions(activeColl) : []);
	// Non-status, non-computed fields that should show on create form
	let editableFields = $derived(schema.fields.filter(f => f.key !== 'status' && !f.computed));
	let status = $derived(statusOverride ?? (statusOptions.length > 0 ? statusOptions[0] : ''));
	let showEditor = $derived(activeColl ? (() => {
		try { return JSON.parse(activeColl.settings).layout === 'content-primary'; } catch { return false; }
	})() : false);

	let titleInput = $state<HTMLInputElement>();

	onMount(async () => {
		try {
			collections = await api.collections.list(wsSlug);
			// Pre-select collection from query param, or default to first
			const urlColl = new URL(page.url).searchParams.get('collection');
			if (urlColl && collections.find(c => c.slug === urlColl)) {
				selectedColl = urlColl;
			} else if (collections.length > 0) {
				selectedColl = collections[0].slug;
			}
		} catch { /* ignore */ }
		requestAnimationFrame(() => titleInput?.focus());
	});

	function handleKeydown(e: KeyboardEvent) {
		if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
			e.preventDefault();
			create();
		}
		if (e.key === 'Escape') {
			history.back();
		}
	}

	async function create() {
		if (!title.trim() || !selectedColl) return;
		creating = true;
		try {
			const fields: Record<string, any> = { ...extraFields };
			if (status) fields.status = status;
			const item = await api.items.create(wsSlug, selectedColl, {
				title: title.trim(),
				content: content || '',
				fields: JSON.stringify(fields),
				source: 'web'
			});
			goto(`/${wsSlug}/${selectedColl}/${itemUrlId(item)}`);
		} catch {
			creating = false;
			toastStore.show('Failed to create item', 'error');
		}
	}
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="new-item" onkeydown={handleKeydown}>
	<h1>New Item</h1>
	<p class="hint">Press <kbd>⌘Enter</kbd> to create · <kbd>Esc</kbd> to go back</p>

	<div class="form">
		<label>
			<span>Collection</span>
			<select bind:value={selectedColl} onchange={() => { statusOverride = null; extraFields = {}; }}>
				{#each collections as coll (coll.slug)}
					<option value={coll.slug}>{coll.icon} {coll.name}</option>
				{/each}
			</select>
		</label>

		<label>
			<span>Title</span>
			<input bind:this={titleInput} bind:value={title} placeholder="What needs to be done?" onkeydown={(e) => e.key === 'Enter' && create()} />
		</label>

		{#if statusOptions.length > 0}
			<label>
				<span>Status</span>
				<select value={status} onchange={(e) => { statusOverride = (e.target as HTMLSelectElement).value; }}>
					{#each statusOptions as opt (opt)}
						<option value={opt}>{opt}</option>
					{/each}
				</select>
			</label>
		{/if}

		{#each editableFields as field (field.key)}
			<label>
				<span>{field.label}</span>
				<FieldEditor
					{field}
					value={extraFields[field.key] ?? ''}
					onchange={(v) => { extraFields[field.key] = v; }}
					wsSlug={wsSlug}
				/>
			</label>
		{/each}

		{#if showEditor}
			<div class="editor-wrap">
				<span class="editor-label">Content</span>
				<Editor content={content} onUpdate={(md) => content = md} editable={true} />
			</div>
		{/if}

		<button class="create-btn" onclick={create} disabled={!title.trim() || !selectedColl || creating}>
			{creating ? 'Creating...' : 'Create Item'}
		</button>
	</div>
</div>

<style>
	.new-item {
		max-width: 600px;
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
	}
	h1 { font-size: 1.4em; margin-bottom: var(--space-2); }
	.hint {
		font-size: 0.8em;
		color: var(--text-muted);
		margin-bottom: var(--space-6);
	}
	.hint kbd {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: 3px;
		padding: 1px 5px;
		font-size: 0.9em;
		font-family: inherit;
	}
	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}
	label span, .editor-label {
		display: block;
		font-size: 0.85em;
		color: var(--text-secondary);
		margin-bottom: var(--space-2);
		font-weight: 500;
	}
	label input, label select {
		width: 100%;
		padding: var(--space-3);
		font-size: 1em;
	}
	.editor-wrap {
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-3);
	}
	.create-btn {
		padding: var(--space-3) var(--space-6);
		background: var(--accent-blue);
		color: #fff;
		border-radius: var(--radius);
		font-weight: 600;
		font-size: 1em;
		align-self: flex-start;
	}
	.create-btn:hover { opacity: 0.9; }
	.create-btn:disabled { opacity: 0.4; cursor: not-allowed; }
</style>
