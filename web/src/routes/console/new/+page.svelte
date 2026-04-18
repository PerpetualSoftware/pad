<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import type { WorkspaceTemplate } from '$lib/types';
	import { groupTemplatesByCategory } from '$lib/utils/templates';

	let name = $state('');
	let selectedTemplate = $state('startup');
	let templates = $state<WorkspaceTemplate[]>([]);
	let creating = $state(false);
	let error = $state('');

	let grouped = $derived(groupTemplatesByCategory(templates));

	let slug = $derived(
		name
			.toLowerCase()
			.replace(/[^a-z0-9\s-]/g, '')
			.replace(/\s+/g, '-')
			.replace(/-+/g, '-')
			.replace(/^-|-$/g, '')
	);

	let username = $derived(authStore.user?.username ?? '');

	onMount(async () => {
		try {
			templates = await api.templates.list();
		} catch {
			// Templates are optional; fall back to defaults
			templates = [
				{ name: 'startup', description: 'Default template for general projects', collections: [], category: 'software' },
				{ name: 'scrum', description: 'Scrum-style sprints and backlogs', collections: [], category: 'software' },
				{ name: 'product', description: 'Product development workflow', collections: [], category: 'software' }
			];
		}
	});

	async function handleCreate() {
		error = '';
		if (!name.trim()) {
			error = 'Please enter a workspace name.';
			return;
		}

		creating = true;
		try {
			const ws = await api.workspaces.create({
				name: name.trim(),
				template: selectedTemplate
			});
			const owner = ws.owner_username || username;
			await goto(`/${owner}/${ws.slug}`, { replaceState: true });
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to create workspace';
		} finally {
			creating = false;
		}
	}

	function handleKeydown(event: KeyboardEvent) {
		if (event.key === 'Enter' && !creating) {
			handleCreate();
		}
	}
</script>

<svelte:head>
	<title>New Workspace - Pad</title>
</svelte:head>

<div class="new-page">
	<a href="/console" class="back-link">Back to workspaces</a>

	<h1 class="page-title">Create a new workspace</h1>

	<div class="form">
		<div class="field">
			<label for="ws-name">Workspace name</label>
			<input
				id="ws-name"
				type="text"
				placeholder="My Project"
				bind:value={name}
				onkeydown={handleKeydown}
				disabled={creating}
			/>
			{#if slug}
				<p class="slug-preview">/{username}/{slug}</p>
			{/if}
		</div>

		<div class="field">
			<span class="field-label">Template</span>
			<div class="template-grid">
				{#each grouped as group (group.category)}
					<div class="category-group">
						<span class="category-label">{group.label}</span>
						{#each group.templates as tmpl (tmpl.name)}
							<button
								class="template-option"
								class:selected={selectedTemplate === tmpl.name}
								onclick={() => (selectedTemplate = tmpl.name)}
								disabled={creating}
								type="button"
							>
								{#if tmpl.icon}
									<span class="template-icon">{tmpl.icon}</span>
								{/if}
								<span class="template-text">
									<span class="template-name">{tmpl.name}</span>
									<span class="template-desc">{tmpl.description}</span>
								</span>
							</button>
						{/each}
					</div>
				{/each}
			</div>
		</div>

		{#if error}
			<p class="error">{error}</p>
		{/if}

		<button class="submit-btn" onclick={handleCreate} disabled={creating || !name.trim()}>
			{#if creating}
				Creating...
			{:else}
				Create Workspace
			{/if}
		</button>
	</div>
</div>

<style>
	.new-page {
		max-width: 520px;
	}

	.back-link {
		display: inline-block;
		color: var(--text-muted);
		font-size: 0.85rem;
		margin-bottom: var(--space-6);
	}

	.back-link:hover {
		color: var(--text-primary);
	}

	.page-title {
		font-size: 1.4rem;
		font-weight: 700;
		color: var(--text-primary);
		margin-bottom: var(--space-8);
	}

	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
	}

	.field {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	label, .field-label {
		font-size: 0.85rem;
		font-weight: 500;
		color: var(--text-secondary);
	}

	input {
		padding: var(--space-3) var(--space-4);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.95rem;
		font-family: var(--font-ui);
		outline: none;
		transition: border-color 0.15s;
	}

	input::placeholder {
		color: var(--text-muted);
	}

	input:focus {
		border-color: var(--accent-blue);
	}

	input:disabled {
		opacity: 0.6;
	}

	.slug-preview {
		color: var(--text-muted);
		font-size: 0.8rem;
		font-family: var(--font-mono);
	}

	.template-grid {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.category-group {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.category-group + .category-group {
		margin-top: var(--space-3);
	}

	.category-label {
		font-size: 0.72rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--text-muted);
		opacity: 0.85;
	}

	.template-option {
		display: flex;
		flex-direction: row;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		text-align: left;
		cursor: pointer;
		transition: border-color 0.15s;
	}

	.template-icon {
		font-size: 1.2em;
		flex-shrink: 0;
	}

	.template-text {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}

	.template-option:hover {
		border-color: var(--text-muted);
	}

	.template-option.selected {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 8%, var(--bg-tertiary));
	}

	.template-option:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.template-name {
		font-weight: 600;
		font-size: 0.9rem;
		color: var(--text-primary);
		text-transform: capitalize;
	}

	.template-desc {
		font-size: 0.8rem;
		color: var(--text-muted);
	}

	.error {
		color: #ef4444;
		font-size: 0.85rem;
	}

	.submit-btn {
		padding: var(--space-3) var(--space-4);
		background: var(--accent-blue);
		color: #fff;
		border: none;
		border-radius: var(--radius);
		font-size: 0.95rem;
		font-weight: 500;
		font-family: var(--font-ui);
		cursor: pointer;
		transition: opacity 0.15s;
	}

	.submit-btn:hover:not(:disabled) {
		opacity: 0.9;
	}

	.submit-btn:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}
</style>
