<script lang="ts">
	import { page } from '$app/state';
	import { api } from '$lib/api/client';
	import type { Item, ItemCreate } from '$lib/types';
	import { parseFields } from '$lib/types';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { SvelteSet, SvelteMap } from 'svelte/reactivity';

	const TRIGGERS = ['always','on-task-start','on-task-complete','on-implement','on-commit','on-pr-create','on-phase-start','on-phase-complete','on-plan'] as const;
	type Trigger = typeof TRIGGERS[number];

	const TRIGGER_META: Record<Trigger, { icon: string; label: string }> = {
		'always':            { icon: '\u{1F504}', label: 'Always' },
		'on-task-start':     { icon: '\u25B6\uFE0F',  label: 'On Task Start' },
		'on-task-complete':  { icon: '\u2705',   label: 'On Task Complete' },
		'on-implement':      { icon: '\u{1F528}', label: 'On Implement' },
		'on-commit':         { icon: '\u{1F4BE}', label: 'On Commit' },
		'on-pr-create':      { icon: '\u{1F500}', label: 'On PR Create' },
		'on-phase-start':    { icon: '\u{1F3C1}', label: 'On Phase Start' },
		'on-phase-complete': { icon: '\u{1F389}', label: 'On Phase Complete' },
		'on-plan':           { icon: '\u{1F4CB}', label: 'On Plan' },
	};

	const SCOPES = ['all','backend','frontend','mobile','docs','devops'] as const;
	const PRIORITIES = ['must','should','nice-to-have'] as const;

	let workspace = $derived(page.params.workspace);
	let conventions = $state<Item[]>([]);
	let loading = $state(true);
	let expandedSlug = $state<string | null>(null);
	let collapsedGroups = new SvelteSet<string>();
	let showCreate = $state(false);
	let creating = $state(false);
	let confirmDelete = $state<string | null>(null);

	// Inline create form state
	let newTitle = $state('');
	let newTrigger = $state<Trigger>('always');
	let newScope = $state<typeof SCOPES[number]>('all');
	let newPriority = $state<typeof PRIORITIES[number]>('should');
	let newContent = $state('');

	$effect(() => {
		if (workspace) loadConventions(workspace);
	});

	async function loadConventions(ws: string) {
		loading = true;
		try {
			conventions = await api.items.listByCollection(ws, 'conventions', { include_archived: false });
		} catch {
			conventions = [];
		} finally {
			loading = false;
		}
	}

	let grouped = $derived.by(() => {
		const groups: { trigger: Trigger; items: Item[]; activeCount: number }[] = [];
		const byTrigger = new SvelteMap<Trigger, Item[]>();
		for (const item of conventions) {
			const fields = parseFields(item);
			const t = (fields.trigger as Trigger) || 'always';
			if (!byTrigger.has(t)) byTrigger.set(t, []);
			byTrigger.get(t)!.push(item);
		}
		for (const trigger of TRIGGERS) {
			const items = byTrigger.get(trigger);
			if (!items || items.length === 0) continue;
			const activeCount = items.filter(i => parseFields(i).status === 'active').length;
			groups.push({ trigger, items, activeCount });
		}
		return groups;
	});

	async function toggleStatus(item: Item) {
		if (!workspace) return;
		const fields = parseFields(item);
		const wasActive = fields.status === 'active';
		const newStatus = wasActive ? 'disabled' : 'active';

		// Optimistic update
		const oldFields = item.fields;
		fields.status = newStatus;
		item.fields = JSON.stringify(fields);
		conventions = [...conventions];

		try {
			await api.items.update(workspace, item.slug, { fields: JSON.stringify(fields) });
			toastStore.show(wasActive ? 'Convention disabled' : 'Convention enabled', 'success');
		} catch {
			// Revert
			item.fields = oldFields;
			conventions = [...conventions];
			toastStore.show('Failed to update convention', 'error');
		}
	}

	function toggleGroup(trigger: string) {
		if (collapsedGroups.has(trigger)) collapsedGroups.delete(trigger);
		else collapsedGroups.add(trigger);
	}

	function toggleExpand(slug: string) {
		expandedSlug = expandedSlug === slug ? null : slug;
	}

	async function handleCreate() {
		if (!newTitle.trim() || creating || !workspace) return;
		creating = true;
		try {
			const data: ItemCreate = {
				title: newTitle.trim(),
				content: newContent.trim(),
				fields: JSON.stringify({
					status: 'active',
					trigger: newTrigger,
					scope: newScope,
					priority: newPriority,
				}),
			};
			const created = await api.items.create(workspace, 'conventions', data);
			conventions = [...conventions, created];
			toastStore.show('Convention created', 'success');
			resetForm();
		} catch {
			toastStore.show('Failed to create convention', 'error');
		} finally {
			creating = false;
		}
	}

	function resetForm() {
		showCreate = false;
		newTitle = '';
		newTrigger = 'always';
		newScope = 'all';
		newPriority = 'should';
		newContent = '';
	}

	async function deleteConvention(item: Item) {
		if (!workspace) return;
		try {
			await api.items.delete(workspace, item.slug);
			conventions = conventions.filter(c => c.id !== item.id);
			toastStore.show('Convention deleted', 'success');
			confirmDelete = null;
			expandedSlug = null;
		} catch {
			toastStore.show('Failed to delete convention', 'error');
		}
	}

	function isActive(item: Item): boolean {
		return parseFields(item).status === 'active';
	}

	function getScope(item: Item): string {
		return parseFields(item).scope || 'all';
	}

	function getPriority(item: Item): string {
		return parseFields(item).priority || 'should';
	}
</script>

<div class="conventions-page">
	{#if loading}
		<div class="loading">Loading conventions...</div>
	{:else}
		<header class="page-header">
			<div class="header-text">
				<h1>Conventions</h1>
				<p class="subtitle">Rules that guide agent behavior in this project</p>
			</div>
			<div class="header-actions">
				<a href="/{workspace}/library" class="btn btn-secondary">Browse Library</a>
				<button class="btn btn-primary" onclick={() => (showCreate = !showCreate)}>
					{showCreate ? 'Cancel' : '+ New Convention'}
				</button>
			</div>
		</header>

		{#if showCreate}
			<form class="create-form" onsubmit={(e) => { e.preventDefault(); handleCreate(); }}>
				<input type="text" bind:value={newTitle} placeholder="Convention title..." class="input-title" required />
				<div class="form-row">
					<label class="form-field">
						<span>Trigger</span>
						<select bind:value={newTrigger}>
							{#each TRIGGERS as t (t)}
								<option value={t}>{TRIGGER_META[t].icon} {TRIGGER_META[t].label}</option>
							{/each}
						</select>
					</label>
					<label class="form-field">
						<span>Scope</span>
						<select bind:value={newScope}>
							{#each SCOPES as s (s)}
								<option value={s}>{s}</option>
							{/each}
						</select>
					</label>
					<label class="form-field">
						<span>Priority</span>
						<select bind:value={newPriority}>
							{#each PRIORITIES as p (p)}
								<option value={p}>{p}</option>
							{/each}
						</select>
					</label>
				</div>
				<textarea bind:value={newContent} placeholder="Instruction the agent should follow..." rows="3"></textarea>
				<div class="form-actions">
					<button type="button" class="btn btn-secondary" onclick={resetForm}>Cancel</button>
					<button type="submit" class="btn btn-primary" disabled={creating || !newTitle.trim()}>
						{creating ? 'Creating...' : 'Create'}
					</button>
				</div>
			</form>
		{/if}

		{#if conventions.length === 0 && !showCreate}
			<div class="empty-state">
				<div class="empty-icon">📏</div>
				<h2>No conventions yet</h2>
				<p>Add rules from the library or create your own.</p>
			</div>
		{:else}
			<div class="groups">
				{#each grouped as group (group.trigger)}
					{@const meta = TRIGGER_META[group.trigger]}
					{@const collapsed = collapsedGroups.has(group.trigger)}
					<section class="trigger-group">
						<button class="group-header" onclick={() => toggleGroup(group.trigger)}>
							<span class="group-chevron" class:collapsed>{collapsed ? '\u25B6' : '\u25BC'}</span>
							<span class="group-icon">{meta.icon}</span>
							<span class="group-label">{meta.label}</span>
							<span class="group-count">{group.activeCount} active</span>
						</button>

						{#if !collapsed}
							<div class="group-items">
								{#each group.items as item (item.id)}
									{@const active = isActive(item)}
									{@const expanded = expandedSlug === item.slug}
									<div class="convention-row" class:disabled={!active}>
										<div
										class="row-main"
										role="button"
										tabindex="0"
										onclick={() => toggleExpand(item.slug)}
										onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleExpand(item.slug); } }}
									>
											<button
												class="toggle-switch"
												type="button"
												class:on={active}
												onclick={(e) => { e.stopPropagation(); toggleStatus(item); }}
												onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') e.stopPropagation(); }}
												aria-label={active ? 'Disable convention' : 'Enable convention'}
											>
												<span class="toggle-knob"></span>
											</button>
											<span class="row-title">{item.title}</span>
											<span class="badge scope-badge">{getScope(item)}</span>
											<span class="priority-dot priority-{getPriority(item)}" title={getPriority(item)}></span>
											<span class="row-chevron">{expanded ? '\u25B4' : '\u25BE'}</span>
										</div>

										{#if expanded}
											<div class="row-expanded">
												{#if item.content}
													<p class="convention-content">{item.content}</p>
												{:else}
													<p class="convention-content muted">No instruction content.</p>
												{/if}
												<div class="expanded-actions">
													<a href="/{workspace}/conventions/{item.slug}" class="btn btn-small btn-secondary">Edit</a>
													{#if confirmDelete === item.slug}
														<span class="confirm-text">Delete this convention?</span>
														<button class="btn btn-small btn-danger" onclick={() => deleteConvention(item)}>Confirm</button>
														<button class="btn btn-small btn-secondary" onclick={() => (confirmDelete = null)}>Cancel</button>
													{:else}
														<button class="btn btn-small btn-danger-outline" onclick={() => (confirmDelete = item.slug)}>Delete</button>
													{/if}
												</div>
											</div>
										{/if}
									</div>
								{/each}
							</div>
						{/if}
					</section>
				{/each}
			</div>
		{/if}
	{/if}
</div>

<style>
	.conventions-page { max-width: var(--content-max-width); margin: 0 auto; padding: var(--space-8) var(--space-6); }
	.loading { text-align: center; padding-top: 20vh; color: var(--text-muted); }

	/* Header */
	.page-header { display: flex; justify-content: space-between; align-items: flex-start; gap: var(--space-4); margin-bottom: var(--space-6); flex-wrap: wrap; }
	.header-text h1 { font-size: 1.6em; margin-bottom: var(--space-1); }
	.subtitle { color: var(--text-secondary); font-size: 0.9em; }
	.header-actions { display: flex; gap: var(--space-2); flex-shrink: 0; }

	/* Buttons */
	.btn { padding: var(--space-1) var(--space-4); border-radius: var(--radius); font-size: 0.85em; font-weight: 600; cursor: pointer; border: none; white-space: nowrap; text-decoration: none; display: inline-flex; align-items: center; }
	.btn-primary { background: var(--accent-blue); color: #fff; }
	.btn-primary:hover { opacity: 0.9; text-decoration: none; }
	.btn-primary:disabled { opacity: 0.5; cursor: not-allowed; }
	.btn-secondary { background: var(--bg-tertiary); color: var(--text-primary); border: 1px solid var(--border); }
	.btn-secondary:hover { background: var(--bg-hover); text-decoration: none; }
	.btn-small { padding: 2px var(--space-3); font-size: 0.8em; }
	.btn-danger { background: #dc2626; color: #fff; }
	.btn-danger-outline { background: none; color: #dc2626; border: 1px solid #dc2626; }
	.btn-danger-outline:hover { background: rgba(220,38,38,0.1); }

	/* Create form */
	.create-form { background: var(--bg-secondary); border: 1px solid var(--border); border-radius: var(--radius-lg); padding: var(--space-4); margin-bottom: var(--space-6); display: flex; flex-direction: column; gap: var(--space-3); }
	.input-title { font-size: 1em; padding: var(--space-2) var(--space-3); }
	.form-row { display: flex; gap: var(--space-3); flex-wrap: wrap; }
	.form-field { display: flex; flex-direction: column; gap: var(--space-1); flex: 1; min-width: 120px; }
	.form-field span { font-size: 0.8em; color: var(--text-secondary); font-weight: 600; }
	.form-actions { display: flex; gap: var(--space-2); justify-content: flex-end; }

	/* Empty state */
	.empty-state { text-align: center; padding: var(--space-10) var(--space-6); }
	.empty-icon { font-size: 3em; margin-bottom: var(--space-4); opacity: 0.6; }
	.empty-state h2 { font-size: 1.2em; font-weight: 600; margin-bottom: var(--space-2); }
	.empty-state p { color: var(--text-muted); font-size: 0.9em; }

	/* Trigger groups */
	.groups { display: flex; flex-direction: column; gap: var(--space-4); }
	.trigger-group { border: 1px solid var(--border); border-radius: var(--radius-lg); overflow: hidden; }
	.group-header { width: 100%; display: flex; align-items: center; gap: var(--space-2); padding: var(--space-3) var(--space-4); background: var(--bg-secondary); cursor: pointer; font-weight: 600; font-size: 0.95em; }
	.group-header:hover { background: var(--bg-hover); }
	.group-chevron { font-size: 0.7em; width: 14px; color: var(--text-muted); }
	.group-icon { font-size: 1.1em; }
	.group-label { flex: 1; text-align: left; }
	.group-count { font-size: 0.8em; font-weight: 400; color: var(--text-muted); }

	/* Convention rows */
	.group-items { border-top: 1px solid var(--border); }
	.convention-row { border-bottom: 1px solid var(--border-subtle); }
	.convention-row:last-child { border-bottom: none; }
	.convention-row.disabled { opacity: 0.6; }
	.row-main { display: flex; align-items: center; gap: var(--space-3); padding: var(--space-2) var(--space-4); cursor: pointer; }
	.row-main:hover { background: var(--bg-hover); }
	.row-title { flex: 1; font-size: 0.9em; font-weight: 500; }
	.row-chevron { font-size: 0.8em; color: var(--text-muted); }

	/* Toggle switch */
	.toggle-switch { position: relative; width: 36px; height: 20px; border-radius: 10px; background: var(--bg-tertiary); border: 1px solid var(--border); cursor: pointer; flex-shrink: 0; transition: background 0.2s; }
	.toggle-switch.on { background: var(--accent-green); border-color: var(--accent-green); }
	.toggle-knob { position: absolute; top: 2px; left: 2px; width: 14px; height: 14px; border-radius: 50%; background: #fff; transition: transform 0.2s; }
	.toggle-switch.on .toggle-knob { transform: translateX(16px); }

	/* Badges */
	.scope-badge { font-size: 0.7em; padding: 1px 8px; border-radius: 10px; background: color-mix(in srgb, var(--accent-purple) 20%, transparent); color: var(--accent-purple); font-weight: 600; white-space: nowrap; }

	/* Priority dots */
	.priority-dot { width: 8px; height: 8px; border-radius: 50%; flex-shrink: 0; }
	.priority-must { background: var(--accent-orange); }
	.priority-should { background: var(--accent-amber); }
	.priority-nice-to-have { background: var(--accent-gray); }

	/* Expanded content */
	.row-expanded { padding: var(--space-3) var(--space-4) var(--space-3) calc(var(--space-4) + 36px + var(--space-3)); background: var(--bg-secondary); border-top: 1px solid var(--border-subtle); }
	.convention-content { font-size: 0.85em; line-height: 1.6; color: var(--text-secondary); margin-bottom: var(--space-3); white-space: pre-wrap; }
	.convention-content.muted { font-style: italic; color: var(--text-muted); }
	.expanded-actions { display: flex; gap: var(--space-2); align-items: center; }
	.confirm-text { font-size: 0.8em; color: #dc2626; }

	@media (max-width: 640px) {
		.page-header { flex-direction: column; }
		.header-actions { width: 100%; }
		.scope-badge { display: none; }
		.form-row { flex-direction: column; }
	}
</style>
