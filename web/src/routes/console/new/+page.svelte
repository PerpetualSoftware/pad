<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import type { Workspace, WorkspaceTemplate } from '$lib/types';
	import { groupTemplatesByCategory } from '$lib/utils/templates';

	let name = $state('');
	let selectedTemplate = $state('startup');
	let templates = $state<WorkspaceTemplate[]>([]);
	let creating = $state(false);
	let error = $state('');

	// PLAN-1496 / TASK-1506: after a successful create we swap to a
	// success state that surfaces the /pad onboard guidance instead of
	// immediately redirecting. The pre-task flow goto'd straight to
	// the new workspace dashboard, which hid the canonical entry point
	// users need to actually adapt the workspace.
	let createdWorkspace = $state<Workspace | null>(null);
	let createdTemplate = $state('');

	// Blank is rendered as a leading "featured" card so the
	// agent-driven flow is the first option the user sees. Filter it
	// out of the grouped iteration so it doesn't render twice. If the
	// server doesn't ship blank (older versions, fallback list), the
	// featured card silently disappears and the picker degrades to
	// the pre-task layout.
	let blankTemplate = $derived(templates.find((t) => t.name === 'blank'));
	let grouped = $derived(
		groupTemplatesByCategory(templates.filter((t) => t.name !== 'blank'))
	);

	let slug = $derived(
		name
			.toLowerCase()
			.replace(/[^a-z0-9\s-]/g, '')
			.replace(/\s+/g, '-')
			.replace(/-+/g, '-')
			.replace(/^-|-$/g, '')
	);

	let username = $derived(authStore.user?.username ?? '');

	let workspaceUrl = $derived.by(() => {
		if (!createdWorkspace) return '';
		const owner = createdWorkspace.owner_username || username;
		return `/${owner}/${createdWorkspace.slug}`;
	});

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
			// Stash the response + which template was picked so the
			// success state can branch on "blank → prominent onboard
			// guidance" vs. "non-blank → subtle onboard mention".
			createdWorkspace = ws;
			createdTemplate = selectedTemplate;
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
	{#if !createdWorkspace}
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
					{#if blankTemplate}
						<button
							class="template-option featured"
							class:selected={selectedTemplate === blankTemplate.name}
							onclick={() => (selectedTemplate = blankTemplate!.name)}
							disabled={creating}
							type="button"
						>
							{#if blankTemplate.icon}
								<span class="template-icon">{blankTemplate.icon}</span>
							{/if}
							<span class="template-text">
								<span class="template-name">
									{blankTemplate.name}
									<span class="featured-badge">Agent-driven</span>
								</span>
								<span class="template-desc">{blankTemplate.description}</span>
							</span>
						</button>
					{/if}
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
	{:else}
		<a href="/console" class="back-link">Back to workspaces</a>

		<h1 class="page-title">Workspace created</h1>

		<div class="success">
			<p class="success-summary">
				<strong>{createdWorkspace.name}</strong> is ready at
				<a class="workspace-link" href={workspaceUrl}>{workspaceUrl}</a>.
			</p>

			{#if createdTemplate === 'blank'}
				<div class="onboard-card primary">
					<h2 class="onboard-heading">Now run <code>/pad onboard</code></h2>
					<p>
						Your workspace ships empty by design. Open an agent session
						in your project directory and run:
					</p>
					<pre class="onboard-snippet"><code>/pad onboard</code></pre>
					<p class="onboard-help">
						The <code>onboard</code> playbook walks you through an
						interview, inspects your codebase if it can, and adapts
						collections, conventions, roles, and playbooks to match
						the project — all conversationally.
					</p>
					<p class="onboard-help">
						Don't have an agent connected yet?
						<a href="/docs/agents" class="inline-link">Connect Claude Code, Cursor, or any MCP client</a>.
					</p>
				</div>
			{:else}
				<div class="onboard-card subtle">
					<p>
						Want to adapt the seeded collections / conventions /
						playbooks to your project? Open an agent session and run
						<code>/pad onboard</code> — it'll walk an interview and
						rewrite anything that doesn't fit.
					</p>
				</div>
			{/if}

			<a class="submit-btn" href={workspaceUrl}>Open {createdWorkspace.name}</a>
		</div>
	{/if}
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

	/* PLAN-1496 / TASK-1506: the "Blank" template renders above the
	   grouped category list as a featured card. Slightly heavier than
	   a normal option (thicker border accent, an "Agent-driven" badge)
	   to signal it's the recommended entry point for users who want
	   the /pad onboard flow. */
	.template-option.featured {
		border-color: color-mix(in srgb, var(--accent-blue) 35%, var(--border));
		background: color-mix(in srgb, var(--accent-blue) 4%, var(--bg-tertiary));
		margin-bottom: var(--space-3);
	}

	.template-option.featured.selected {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 10%, var(--bg-tertiary));
	}

	.featured-badge {
		display: inline-block;
		margin-left: var(--space-2);
		padding: 1px 6px;
		font-size: 0.65rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
		border-radius: 4px;
		vertical-align: 1px;
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

	/* PLAN-1496 / TASK-1506: post-create success state. Replaces the
	   pre-task immediate redirect so /pad onboard has somewhere to
	   surface. Layout mirrors the form's vertical rhythm so the page
	   doesn't visually jump when the swap happens. */
	.success {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
	}

	.success-summary {
		font-size: 0.95rem;
		color: var(--text-primary);
	}

	.workspace-link {
		color: var(--accent-blue);
		font-family: var(--font-mono);
		font-size: 0.9rem;
	}

	.workspace-link:hover {
		text-decoration: underline;
	}

	.onboard-card {
		padding: var(--space-5) var(--space-5);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-tertiary);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.onboard-card.primary {
		border-color: color-mix(in srgb, var(--accent-blue) 35%, var(--border));
		background: color-mix(in srgb, var(--accent-blue) 4%, var(--bg-tertiary));
	}

	.onboard-card.subtle {
		font-size: 0.9rem;
		color: var(--text-secondary);
	}

	.onboard-heading {
		font-size: 1.1rem;
		font-weight: 600;
		color: var(--text-primary);
		margin: 0;
	}

	.onboard-card p {
		margin: 0;
		font-size: 0.9rem;
		color: var(--text-primary);
	}

	.onboard-card.subtle p {
		color: var(--text-secondary);
	}

	.onboard-card code {
		font-family: var(--font-mono);
		font-size: 0.88em;
		padding: 1px 5px;
		background: var(--bg-secondary);
		border-radius: 4px;
	}

	.onboard-snippet {
		margin: 0;
		padding: var(--space-3) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		font-family: var(--font-mono);
		font-size: 0.95rem;
		color: var(--text-primary);
		overflow-x: auto;
	}

	.onboard-snippet code {
		padding: 0;
		background: transparent;
	}

	.onboard-help {
		font-size: 0.85rem !important;
		color: var(--text-secondary) !important;
	}

	.inline-link {
		color: var(--accent-blue);
	}

	.inline-link:hover {
		text-decoration: underline;
	}

	/* The success state's CTA button is an <a>, not a <button>, so it
	   needs the same baseline styles the .submit-btn class wires up
	   for the form submit. text-decoration:none and align-self:flex-start
	   prevent the link rendering as a full-width underlined block. */
	a.submit-btn {
		display: inline-block;
		text-align: center;
		text-decoration: none;
		align-self: flex-start;
	}
</style>
