<script lang="ts">
	import { page } from '$app/state';
	import { api } from '$lib/api/client';
	import type { LibraryCategory, LibraryConvention, PlaybookCategory, LibraryPlaybook, Item } from '$lib/types';

	let wsSlug = $derived(page.params.workspace ?? '');

	let categories = $state<LibraryCategory[]>([]);
	let playbookCategories = $state<PlaybookCategory[]>([]);
	let activeConventionTitles = $state<Set<string>>(new Set());
	let activePlaybookTitles = $state<Set<string>>(new Set());
	let loading = $state(true);
	let activatingTitle = $state<string | null>(null);
	let toast = $state<string | null>(null);
	let activeTab = $state<'conventions' | 'playbooks'>(
		(page.url.searchParams.get('tab') === 'playbooks') ? 'playbooks' : 'conventions'
	);

	const categoryIcons: Record<string, string> = {
		git: '\u{1F500}',
		quality: '\u{2705}',
		pm: '\u{1F4CB}',
		docs: '\u{1F4DD}',
		build: '\u{1F527}',
		workflow: '\u{2699}\u{FE0F}',
		planning: '\u{1F4C5}',
		operations: '\u{1F680}',
	};

	const priorityColors: Record<string, string> = {
		must: 'var(--accent-orange)',
		should: 'var(--accent-amber)',
		'nice-to-have': 'var(--accent-gray)',
	};

	$effect(() => {
		if (wsSlug) loadData(wsSlug);
	});

	async function loadData(ws: string) {
		loading = true;
		try {
			const [libraryRes, playbookRes, existingConventions, existingPlaybooks] = await Promise.all([
				api.library.get(),
				api.library.getPlaybooks(),
				api.items.listByCollection(ws, 'conventions', { all: true }).catch(() => [] as Item[]),
				api.items.listByCollection(ws, 'playbooks', { all: true }).catch(() => [] as Item[]),
			]);
			categories = libraryRes.categories;
			playbookCategories = playbookRes.categories;
			activeConventionTitles = new Set(existingConventions.map((item) => item.title));
			activePlaybookTitles = new Set(existingPlaybooks.map((item) => item.title));
		} catch {
			categories = [];
			playbookCategories = [];
		} finally {
			loading = false;
		}
	}

	async function activateConvention(convention: LibraryConvention) {
		if (activeConventionTitles.has(convention.title) || activatingTitle) return;
		activatingTitle = convention.title;
		try {
			await api.library.activate(wsSlug, convention);
			activeConventionTitles = new Set([...activeConventionTitles, convention.title]);
			toast = `Activated: ${convention.title}`;
			setTimeout(() => (toast = null), 3000);
		} catch {
			toast = `Failed to activate: ${convention.title}`;
			setTimeout(() => (toast = null), 3000);
		} finally {
			activatingTitle = null;
		}
	}

	async function activatePlaybook(playbook: LibraryPlaybook) {
		if (activePlaybookTitles.has(playbook.title) || activatingTitle) return;
		activatingTitle = playbook.title;
		try {
			await api.library.activatePlaybook(wsSlug, playbook);
			activePlaybookTitles = new Set([...activePlaybookTitles, playbook.title]);
			toast = `Activated: ${playbook.title}`;
			setTimeout(() => (toast = null), 3000);
		} catch {
			toast = `Failed to activate: ${playbook.title}`;
			setTimeout(() => (toast = null), 3000);
		} finally {
			activatingTitle = null;
		}
	}

	function truncate(text: string, max: number): string {
		return text.length > max ? text.slice(0, max) + '...' : text;
	}

	function previewSteps(content: string): string {
		const lines = content.split('\n').filter((l) => l.match(/^\d+\./));
		return lines.slice(0, 3).join('\n');
	}
</script>

<div class="library">
	{#if loading}
		<div class="loading">Loading library...</div>
	{:else}
		<header class="library-header">
			<h1>Library</h1>
			<p class="subtitle">Pre-built conventions and playbooks to guide agent behavior. Activate the ones that fit your workflow.</p>
		</header>

		<div class="tabs">
			<button
				class="tab"
				class:active={activeTab === 'conventions'}
				onclick={() => (activeTab = 'conventions')}
			>
				Conventions
			</button>
			<button
				class="tab"
				class:active={activeTab === 'playbooks'}
				onclick={() => (activeTab = 'playbooks')}
			>
				Playbooks
			</button>
		</div>

		{#if activeTab === 'conventions'}
			{#if categories.length === 0}
				<p class="empty">No conventions available.</p>
			{/if}

			{#each categories as category (category.name)}
				<section class="category">
					<div class="category-header">
						<span class="category-icon">{categoryIcons[category.name] ?? '\u{1F4E6}'}</span>
						<div>
							<h2>{category.name}</h2>
							{#if category.description}
								<p class="category-desc">{category.description}</p>
							{/if}
						</div>
					</div>

					<div class="card-grid">
						{#each category.conventions as convention (convention.title)}
							{@const isActive = activeConventionTitles.has(convention.title)}
							{@const isActivating = activatingTitle === convention.title}
							<div class="card">
								<div class="card-body">
									<h3 class="card-title">{convention.title}</h3>
									<div class="badges">
										<span class="badge trigger">{convention.trigger}</span>
										<span class="badge scope">{convention.scope}</span>
										<span class="badge priority" style="background: {priorityColors[convention.priority] ?? 'var(--accent-gray)'}">
											{convention.priority}
										</span>
									</div>
									<p class="card-content">{truncate(convention.content, 100)}</p>
								</div>
								<div class="card-action">
									{#if isActive}
										<span class="active-badge">Active</span>
									{:else}
										<button
											class="activate-btn"
											disabled={isActivating}
											onclick={() => activateConvention(convention)}
										>
											{#if isActivating}
												<span class="spinner"></span>
											{:else}
												Activate
											{/if}
										</button>
									{/if}
								</div>
							</div>
						{/each}
					</div>
				</section>
			{/each}
		{:else}
			{#if playbookCategories.length === 0}
				<p class="empty">No playbooks available.</p>
			{/if}

			{#each playbookCategories as category (category.name)}
				<section class="category">
					<div class="category-header">
						<span class="category-icon">{categoryIcons[category.name] ?? '\u{1F4D8}'}</span>
						<div>
							<h2>{category.name}</h2>
							{#if category.description}
								<p class="category-desc">{category.description}</p>
							{/if}
						</div>
					</div>

					<div class="card-grid">
						{#each category.playbooks as playbook (playbook.title)}
							{@const isActive = activePlaybookTitles.has(playbook.title)}
							{@const isActivating = activatingTitle === playbook.title}
							<div class="card">
								<div class="card-body">
									<h3 class="card-title">{playbook.title}</h3>
									<div class="badges">
										<span class="badge trigger">{playbook.trigger}</span>
										<span class="badge scope">{playbook.scope}</span>
									</div>
									<p class="card-content card-steps">{previewSteps(playbook.content)}</p>
								</div>
								<div class="card-action">
									{#if isActive}
										<span class="active-badge">Active</span>
									{:else}
										<button
											class="activate-btn"
											disabled={isActivating}
											onclick={() => activatePlaybook(playbook)}
										>
											{#if isActivating}
												<span class="spinner"></span>
											{:else}
												Activate
											{/if}
										</button>
									{/if}
								</div>
							</div>
						{/each}
					</div>
				</section>
			{/each}
		{/if}
	{/if}

	{#if toast}
		<div class="toast">{toast}</div>
	{/if}
</div>

<style>
	.library { max-width: var(--content-max-width); margin: 0 auto; padding: var(--space-8) var(--space-6); }
	.loading { text-align: center; padding-top: 20vh; color: var(--text-muted); }
	.empty { color: var(--text-muted); text-align: center; padding-top: 10vh; }
	.library-header { margin-bottom: var(--space-6); }
	.library-header h1 { font-size: 1.6em; margin-bottom: var(--space-2); }
	.subtitle { color: var(--text-secondary); font-size: 0.95em; }

	.tabs {
		display: flex;
		gap: var(--space-1);
		margin-bottom: var(--space-8);
		border-bottom: 1px solid var(--border);
		padding-bottom: 0;
	}
	.tab {
		padding: var(--space-2) var(--space-4);
		background: none;
		border: none;
		border-bottom: 2px solid transparent;
		color: var(--text-secondary);
		font-size: 0.95em;
		font-weight: 600;
		cursor: pointer;
		transition: color 0.15s, border-color 0.15s;
		margin-bottom: -1px;
	}
	.tab:hover { color: var(--text-primary); }
	.tab.active {
		color: var(--accent-blue);
		border-bottom-color: var(--accent-blue);
	}

	.category { margin-bottom: var(--space-8); }
	.category-header { display: flex; align-items: center; gap: var(--space-3); margin-bottom: var(--space-4); }
	.category-icon { font-size: 1.4em; }
	.category-header h2 { font-size: 1.1em; text-transform: capitalize; }
	.category-desc { font-size: 0.85em; color: var(--text-muted); margin-top: 2px; }

	.card-grid { display: grid; grid-template-columns: 1fr; gap: var(--space-3); }
	@media (min-width: 640px) {
		.card-grid { grid-template-columns: 1fr 1fr; }
	}

	.card {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-4);
		display: flex;
		flex-direction: column;
		justify-content: space-between;
		gap: var(--space-3);
		transition: border-color 0.15s;
	}
	.card:hover { border-color: var(--accent-blue); }

	.card-body { display: flex; flex-direction: column; gap: var(--space-2); }
	.card-title { font-size: 0.95em; font-weight: 600; }
	.card-content { font-size: 0.85em; color: var(--text-secondary); line-height: 1.5; }
	.card-steps { white-space: pre-line; }

	.badges { display: flex; flex-wrap: wrap; gap: var(--space-1); }
	.badge {
		font-size: 0.7em;
		padding: 2px 8px;
		border-radius: 10px;
		font-weight: 600;
		white-space: nowrap;
	}
	.badge.trigger { background: color-mix(in srgb, var(--accent-blue) 20%, transparent); color: var(--accent-blue); }
	.badge.scope { background: color-mix(in srgb, var(--accent-purple) 20%, transparent); color: var(--accent-purple); }
	.badge.priority { color: #fff; }

	.card-action { display: flex; justify-content: flex-end; }

	.activate-btn {
		padding: var(--space-1) var(--space-4);
		background: var(--accent-blue);
		color: #fff;
		border-radius: var(--radius);
		font-size: 0.8em;
		font-weight: 600;
		cursor: pointer;
		border: none;
		display: flex;
		align-items: center;
		gap: var(--space-2);
		transition: opacity 0.15s;
	}
	.activate-btn:hover { opacity: 0.9; }
	.activate-btn:disabled { opacity: 0.6; cursor: not-allowed; }

	.active-badge {
		padding: var(--space-1) var(--space-4);
		background: color-mix(in srgb, var(--accent-green) 20%, transparent);
		color: var(--accent-green);
		border-radius: var(--radius);
		font-size: 0.8em;
		font-weight: 600;
	}

	.spinner {
		width: 14px;
		height: 14px;
		border: 2px solid rgba(255, 255, 255, 0.3);
		border-top-color: #fff;
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
	}
	@keyframes spin { to { transform: rotate(360deg); } }

	.toast {
		position: fixed;
		bottom: var(--space-6);
		left: 50%;
		transform: translateX(-50%);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		padding: var(--space-3) var(--space-6);
		border-radius: var(--radius-lg);
		font-size: 0.85em;
		color: var(--text-primary);
		z-index: 100;
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
		animation: fade-in 0.2s ease;
	}
	@keyframes fade-in { from { opacity: 0; transform: translateX(-50%) translateY(8px); } }
</style>
