<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api, isPlanLimitError, planLimitMessage } from '$lib/api/client';
	import { parseFields, parseSchema, itemUrlId, type Collection, type Item } from '$lib/types';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import PlaybookFormFields from '$lib/components/playbooks/PlaybookFormFields.svelte';
	import {
		PLAYBOOK_SKELETON_BODY,
		argumentsToJSON,
		type PlaybookArgument
	} from '$lib/playbooks/arguments';

	const TRIGGERS = ['on-implement', 'on-triage', 'on-release', 'on-plan', 'on-review', 'on-deploy', 'manual'] as const;
	const SCOPES = ['all', 'backend', 'frontend', 'mobile', 'devops'] as const;
	const STATUSES = ['active', 'draft', 'deprecated'] as const;
	const STATUS_ORDER: Record<string, number> = { active: 0, draft: 1, deprecated: 2 };
	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	let playbooks = $state<Item[]>([]);
	let playbooksCollection = $state<Collection | null>(null);
	let loading = $state(true);

	// Scroll position restoration (BUG-1425).
	const scrollRestoration = createScrollRestoration({
		// `playbooksCollection` flips null on workspace change. Length
		// gate omitted (Codex P2 round 2).
		ready: () => !loading && playbooksCollection !== null,
		persistKey: () =>
			wsSlug
				? `pad-last-scroll-${wsSlug}-${page.url.pathname}${page.url.search}`
				: null,
	});
	export const snapshot = scrollRestoration.snapshot;

	let expandedId = $state<string | null>(null);
	let showNewForm = $state(false);
	let deleting = $state<string | null>(null);
	let confirmDeleteSlug = $state<string | null>(null);
	let togglingStatus = $state<string | null>(null);
	let duplicating = $state<string | null>(null);
	let searchQuery = $state('');
	let filterTrigger = $state<string>('');
	let filterScope = $state<string>('');
	let newTitle = $state('');
	let newTrigger = $state<string>('manual');
	let newScope = $state<string>('all');
	let newStatus = $state<string>('draft');
	let newContent = $state('');
	let newInvocationSlug = $state('');
	let newArgs = $state<PlaybookArgument[]>([]);
	// Tracks whether we've already seeded the skeleton for the current
	// "open new form" session — so re-opens without `resetForm` (shouldn't
	// happen but defensively) don't clobber edited content.
	let skeletonSeeded = $state(false);

	// Pre-fill the textarea with the skeleton body when the user opens
	// the new-form panel from a closed state. Only seed when content is
	// empty so subsequent renders don't clobber a draft.
	$effect(() => {
		if (showNewForm && !skeletonSeeded && newContent === '') {
			newContent = PLAYBOOK_SKELETON_BODY;
			skeletonSeeded = true;
		}
		if (!showNewForm) {
			skeletonSeeded = false;
		}
	});

	$effect(() => {
		if (wsSlug) {
			loadPlaybooks(wsSlug);
			loadPlaybooksCollection(wsSlug);
		}
	});
	async function loadPlaybooks(ws: string) {
		loading = true;
		try { playbooks = await api.items.listByCollection(ws, 'playbooks', {}); }
		catch { playbooks = []; }
		finally { loading = false; }
	}

	async function loadPlaybooksCollection(ws: string) {
		// Clear any previous workspace's schema before the fetch. Until the
		// new response lands, createTriggers/createScopes fall back to the
		// hardcoded software defaults — correct for a workspace whose schema
		// we have not yet observed. Prevents the in-flight window from
		// rendering the previous workspace's vocabulary on the new page.
		playbooksCollection = null;
		try {
			const coll = await api.collections.get(ws, 'playbooks');
			// Stale-response guard: if the user has since moved to another
			// workspace, drop the result rather than overwriting state with
			// schema from a workspace we are no longer on.
			if (ws !== wsSlug) return;
			playbooksCollection = coll;
		} catch {
			if (ws !== wsSlug) return;
			playbooksCollection = null;
		}
	}

	let schemaTriggers = $derived.by<readonly string[]>(() => {
		if (!playbooksCollection) return [];
		const schema = parseSchema(playbooksCollection);
		const field = schema.fields.find((f) => f.key === 'trigger');
		return field?.options ?? [];
	});

	let schemaScopes = $derived.by<readonly string[]>(() => {
		if (!playbooksCollection) return [];
		const schema = parseSchema(playbooksCollection);
		const field = schema.fields.find((f) => f.key === 'scope');
		return field?.options ?? [];
	});

	let createTriggers = $derived<readonly string[]>(
		schemaTriggers.length > 0 ? schemaTriggers : (TRIGGERS as readonly string[])
	);
	let createScopes = $derived<readonly string[]>(
		schemaScopes.length > 0 ? schemaScopes : (SCOPES as readonly string[])
	);

	// Snap the create-form selections into the effective list when it changes,
	// so the <select> never displays a phantom value that isn't in its <option>s.
	// Only snap while the new-form is CLOSED — once the user opens the form,
	// they may type a custom trigger (via PlaybookFormFields' "Other…" mode)
	// that isn't in `createTriggers`. Snapping during an open form silently
	// overwrites that custom value (Codex round 1 P2).
	$effect(() => {
		if (!showNewForm && createTriggers.length > 0 && !createTriggers.includes(newTrigger)) {
			newTrigger = createTriggers[0];
		}
	});
	$effect(() => {
		if (!showNewForm && createScopes.length > 0 && !createScopes.includes(newScope)) {
			newScope = createScopes[0];
		}
	});

	let hasActiveFilters = $derived(searchQuery !== '' || filterTrigger !== '' || filterScope !== '');

	let allTriggers = $derived.by(() => {
		const known = createTriggers;
		const knownSet = new Set<string>(known);
		const discovered = new Set<string>();
		for (const p of playbooks) {
			const t = parseFields(p).trigger;
			if (typeof t === 'string' && t && !knownSet.has(t)) discovered.add(t);
		}
		return [...known, ...Array.from(discovered).sort((a, b) => a.localeCompare(b))];
	});

	let allScopes = $derived.by(() => {
		const known = createScopes;
		const knownSet = new Set<string>(known);
		const discovered = new Set<string>();
		for (const p of playbooks) {
			const s = parseFields(p).scope;
			if (typeof s === 'string' && s && !knownSet.has(s)) discovered.add(s);
		}
		return [...known, ...Array.from(discovered).sort((a, b) => a.localeCompare(b))];
	});

	let sorted = $derived.by(() => {
		let items = [...playbooks];
		if (searchQuery) {
			const q = searchQuery.toLowerCase();
			items = items.filter(i => i.title.toLowerCase().includes(q) || (i.content ?? '').toLowerCase().includes(q));
		}
		if (filterTrigger) {
			items = items.filter(i => (parseFields(i).trigger ?? 'manual') === filterTrigger);
		}
		if (filterScope) {
			items = items.filter(i => (parseFields(i).scope ?? 'all') === filterScope);
		}
		return items.sort((a, b) => {
			const fa = parseFields(a), fb = parseFields(b);
			const sa = STATUS_ORDER[fa.status] ?? 1, sb = STATUS_ORDER[fb.status] ?? 1;
			if (sa !== sb) return sa - sb;
			const ta = fa.trigger ?? '', tb = fb.trigger ?? '';
			if (ta !== tb) return ta.localeCompare(tb);
			return a.title.localeCompare(b.title);
		});
	});

	function countSteps(content: string): number {
		if (!content) return 0;
		return content.split('\n').filter(l => {
			const t = l.trim();
			return /^\d+[\.\)]\s/.test(t) || /^[-*]\s/.test(t);
		}).length;
	}

	function isCodeLine(line: string): boolean {
		const t = line.trim();
		if (t.includes('`')) return true;
		return /^(git |pad |npm |make |go |cargo |docker |kubectl |curl |wget |ssh |cd |ls |cat |rm |cp |mv |mkdir |chmod |chown |brew |apt |pip |yarn |pnpm |bun |deno |node |python |ruby )/.test(t);
	}

	function toggleExpand(id: string) { expandedId = expandedId === id ? null : id; }

	async function createPlaybook(status: string) {
		if (!newTitle.trim()) return;
		try {
			const fieldsObj: Record<string, unknown> = {
				status,
				trigger: newTrigger,
				scope: newScope,
				// `arguments` is stored as a JSON value (array of objects),
				// NOT a stringified string — the field type is `json`.
				// Mirrors internal/collections/templates_startup_ship.go.
				arguments: JSON.parse(argumentsToJSON(newArgs))
			};
			// Omit empty slug entirely rather than emitting "".
			if (newInvocationSlug.trim()) {
				fieldsObj.invocation_slug = newInvocationSlug.trim();
			}
			await api.items.create(wsSlug, 'playbooks', {
				title: newTitle.trim(),
				content: newContent,
				fields: JSON.stringify(fieldsObj)
			});
			resetForm();
			toastStore.show(`Playbook created as ${status}`, 'success');
			await loadPlaybooks(wsSlug);
		} catch (err: unknown) {
			if (isPlanLimitError(err)) {
				toastStore.show(planLimitMessage(err) + ' Upgrade to Pro at /console/billing', 'error');
			} else {
				toastStore.show('Failed to create playbook', 'error');
			}
		}
	}

	async function toggleStatus(item: Item) {
		const fields = parseFields(item);
		const cur = fields.status ?? 'draft';
		const next = cur === 'active' ? 'draft' : cur === 'draft' ? 'active' : 'draft';
		togglingStatus = item.slug;
		try {
			fields.status = next;
			const updated = await api.items.update(wsSlug, item.slug, { fields: JSON.stringify(fields) });
			const idx = playbooks.findIndex(p => p.id === item.id);
			if (idx !== -1) playbooks[idx] = updated;
			toastStore.show(`Status changed to ${next}`, 'success');
		} catch { toastStore.show('Failed to update status', 'error'); }
		finally { togglingStatus = null; }
	}

	async function deletePlaybook(slug: string) {
		deleting = slug;
		try {
			await api.items.delete(wsSlug, slug);
			playbooks = playbooks.filter(p => p.slug !== slug);
			confirmDeleteSlug = null; expandedId = null;
			toastStore.show('Playbook deleted', 'success');
		} catch { toastStore.show('Failed to delete playbook', 'error'); }
		finally { deleting = null; }
	}
	async function duplicatePlaybook(item: Item) {
		duplicating = item.slug;
		try {
			const fields = parseFields(item);
			const dupFields: Record<string, unknown> = {
				status: 'draft',
				trigger: fields.trigger ?? 'manual',
				scope: fields.scope ?? 'all'
			};
			// Carry forward the structured `arguments` contract so a duplicated
			// playbook keeps its arg spec — otherwise the copy's body still
			// describes arguments but the queryable form is empty (Codex round
			// 1 P3). invocation_slug is intentionally NOT carried; the original
			// owns the slug and a duplicate would clash on the unique index.
			if (fields.arguments !== undefined && fields.arguments !== null) {
				dupFields.arguments = fields.arguments;
			}
			await api.items.create(wsSlug, 'playbooks', {
				title: `${item.title} (copy)`,
				content: item.content,
				fields: JSON.stringify(dupFields)
			});
			toastStore.show('Playbook duplicated as draft', 'success');
			await loadPlaybooks(wsSlug);
		} catch (err: unknown) {
			if (isPlanLimitError(err)) {
				toastStore.show(planLimitMessage(err) + ' Upgrade to Pro at /console/billing', 'error');
			} else {
				toastStore.show('Failed to duplicate playbook', 'error');
			}
		} finally { duplicating = null; }
	}

	function clearFilters() { searchQuery = ''; filterTrigger = ''; filterScope = ''; }

	function resetForm() {
		showNewForm = false;
		newTitle = '';
		newTrigger = 'manual';
		newScope = 'all';
		newStatus = 'draft';
		newContent = '';
		newInvocationSlug = '';
		newArgs = [];
		skeletonSeeded = false;
	}
	function statusLabel(s: string) { return s === 'active' ? 'Active' : s === 'deprecated' ? 'Deprecated' : 'Draft'; }
	function nextStatusLabel(s: string) { return s === 'active' ? 'Mark as Draft' : s === 'draft' ? 'Mark as Active' : 'Mark as Draft'; }
</script>

<div class="playbooks-page">
	{#if loading}
		<div class="loading">Loading playbooks...</div>
	{:else}
		<header class="page-header">
			<div class="header-text">
				<h1>Playbooks</h1>
				<p class="subtitle">Multi-step workflows that agents follow for specific actions</p>
			</div>
			{#if !showNewForm}
				<div style="display:flex;gap:var(--space-2);align-items:center;">
					<a href="/{username}/{wsSlug}/library?tab=playbooks" class="new-btn" style="background:var(--bg-secondary);color:var(--text-primary);border:1px solid var(--border);">📚 Browse Library</a>
					<button class="new-btn" onclick={() => (showNewForm = true)}>+ New Playbook</button>
				</div>
			{/if}
		</header>

		{#if showNewForm}
			<div class="new-form">
				<h2>New Playbook</h2>
				<div class="form-fields">
					<div class="form-row">
						<label class="form-label" for="pb-title">Title</label>
						<input id="pb-title" type="text" bind:value={newTitle} placeholder="e.g. Implementation Playbook" class="form-input" />
					</div>

					<div class="new-form-grid">
						<div class="new-form-sidebar">
							<PlaybookFormFields
								{wsSlug}
								selfItemId={null}
								invocationSlug={newInvocationSlug}
								trigger={newTrigger}
								scope={newScope}
								status={newStatus}
								args={newArgs}
								bodyContent={newContent}
								triggers={createTriggers}
								scopes={createScopes}
								statuses={STATUSES}
								hideStatus={true}
								existingPlaybooks={playbooks}
								onSlugChange={(s) => (newInvocationSlug = s)}
								onTriggerChange={(t) => (newTrigger = t)}
								onScopeChange={(s) => (newScope = s)}
								onStatusChange={(s) => (newStatus = s)}
								onArgumentsChange={(a) => (newArgs = a)}
								onBodyContentChange={(b) => (newContent = b)}
							/>
						</div>
						<div class="new-form-main">
							<label class="form-label" for="pb-content">Body</label>
							<textarea
								id="pb-content"
								bind:value={newContent}
								placeholder="Describe what this playbook does, its arguments, steps, defaults, and stop conditions."
								class="form-textarea"
								rows="20"
							></textarea>
						</div>
					</div>
				</div>
				<div class="form-actions">
					<button class="btn btn-secondary" onclick={resetForm}>Cancel</button>
					<button class="btn btn-draft" disabled={!newTitle.trim()} onclick={() => createPlaybook('draft')}>Create as Draft</button>
					<button class="btn btn-primary" disabled={!newTitle.trim()} onclick={() => createPlaybook('active')}>Create as Active</button>
				</div>
			</div>
		{/if}

		{#if playbooks.length > 0 && !showNewForm}
			<div class="filter-bar">
				<input type="text" class="search-input" placeholder="Search playbooks..." bind:value={searchQuery} />
				<select class="filter-select" bind:value={filterTrigger}>
					<option value="">All triggers</option>
					{#each allTriggers as t (t)}<option value={t}>{t}</option>{/each}
				</select>
				<select class="filter-select" bind:value={filterScope}>
					<option value="">All scopes</option>
					{#each allScopes as s (s)}<option value={s}>{s}</option>{/each}
				</select>
				{#if hasActiveFilters}
					<button class="action-btn" onclick={clearFilters}>Clear</button>
				{/if}
			</div>
		{/if}

		{#if sorted.length === 0 && hasActiveFilters && !showNewForm}
			<div class="empty-state">
				<p>No playbooks match your filters.</p>
				<button class="btn btn-secondary" onclick={clearFilters}>Clear filters</button>
			</div>
		{:else if sorted.length === 0 && !showNewForm}
			<div class="empty-state">
				<div class="empty-icon">&#x1F4D8;</div>
				<h2>No playbooks yet</h2>
				<p>Playbooks are multi-step workflows that guide agents through complex tasks.</p>
				<button class="btn btn-primary" onclick={() => (showNewForm = true)}>Create Your First Playbook</button>
			</div>
		{:else}
			<div class="cards">
				{#each sorted as item (item.id)}
					{@const fields = parseFields(item)}
					{@const status = fields.status ?? 'draft'}
					{@const trigger = fields.trigger ?? 'manual'}
					{@const scope = fields.scope ?? 'all'}
					{@const steps = countSteps(item.content)}
					{@const isExpanded = expandedId === item.id}
					<div class="card" class:card-draft={status === 'draft'} class:card-deprecated={status === 'deprecated'}>
						<button class="card-header" onclick={() => toggleExpand(item.id)} aria-expanded={isExpanded}>
							<div class="card-title-row">
								<span class="card-title" class:deprecated-title={status === 'deprecated'}>{item.title}</span>
								<span class="status-badge status-{status}">{statusLabel(status)}</span>
							</div>
							<div class="card-meta">
								<span class="badge badge-trigger">{trigger}</span>
								<span class="meta-sep">&middot;</span>
								<span class="badge badge-scope">{scope}</span>
								{#if steps > 0}
									<span class="meta-sep">&middot;</span>
									<span class="step-count">{steps} step{steps !== 1 ? 's' : ''}</span>
								{/if}
								<span class="chevron" class:chevron-open={isExpanded}>&#9660;</span>
							</div>
						</button>
						{#if isExpanded}
							<div class="card-body">
								<div class="card-divider"></div>
								<div class="content-block">
									{#if item.content}
										{#each item.content.split('\n') as line, i (i)}
											{#if line.trim() === ''}
												<div class="content-line empty-line">&nbsp;</div>
											{:else if isCodeLine(line)}
												<div class="content-line code-line">{line}</div>
											{:else}
												<div class="content-line">{line}</div>
											{/if}
										{/each}
									{:else}
										<p class="no-content">No workflow steps defined yet.</p>
									{/if}
								</div>
								<div class="card-divider"></div>
								<div class="card-actions">
									<button class="action-btn" onclick={() => goto(`/${username}/${wsSlug}/playbooks/${itemUrlId(item)}`)}>Edit</button>
									<button class="action-btn" disabled={togglingStatus === item.slug} onclick={() => toggleStatus(item)}>
										{togglingStatus === item.slug ? '...' : nextStatusLabel(status)}
									</button>
									<button class="action-btn" disabled={duplicating === item.slug} onclick={() => duplicatePlaybook(item)}>
										{duplicating === item.slug ? '...' : 'Duplicate'}
									</button>
									{#if confirmDeleteSlug === item.slug}
										<span class="delete-confirm">
											Delete?
											<button class="action-btn action-danger" disabled={deleting === item.slug} onclick={() => deletePlaybook(item.slug)}>{deleting === item.slug ? '...' : 'Yes'}</button>
											<button class="action-btn" onclick={() => (confirmDeleteSlug = null)}>No</button>
										</span>
									{:else}
										<button class="action-btn action-danger" onclick={() => (confirmDeleteSlug = item.slug)}>Delete</button>
									{/if}
								</div>
							</div>
						{/if}
					</div>
				{/each}
			</div>
		{/if}
	{/if}
</div>

<style>
	.playbooks-page { max-width: var(--content-max-width); margin: 0 auto; padding: var(--space-8) var(--space-6); }
	.loading { text-align: center; padding-top: 20vh; color: var(--text-muted); }
	.page-header { display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: var(--space-8); gap: var(--space-4); }
	.page-header h1 { font-size: 1.6em; margin-bottom: var(--space-1); }
	.subtitle { color: var(--text-secondary); font-size: 0.95em; }
	.new-btn { background: var(--accent-blue); color: #fff; padding: var(--space-2) var(--space-5); border-radius: var(--radius); font-size: 0.85em; font-weight: 600; white-space: nowrap; flex-shrink: 0; transition: opacity 0.15s; }
	.new-btn:hover { opacity: 0.85; }
	.empty-state { text-align: center; padding: var(--space-10) var(--space-6); color: var(--text-secondary); }
	.empty-icon { font-size: 3em; margin-bottom: var(--space-4); opacity: 0.6; }
	.empty-state h2 { font-size: 1.2em; font-weight: 600; margin-bottom: var(--space-2); color: var(--text-primary); }
	.empty-state p { font-size: 0.9em; color: var(--text-muted); margin-bottom: var(--space-5); }
	.filter-bar { display: flex; gap: var(--space-2); align-items: center; margin-bottom: var(--space-4); flex-wrap: wrap; }
	.search-input { flex: 1; min-width: 160px; padding: var(--space-1) var(--space-3); background: var(--bg-secondary); border: 1px solid var(--border); border-radius: var(--radius); font-size: 0.85em; color: var(--text-primary); }
	.search-input::placeholder { color: var(--text-muted); }
	.search-input:focus { border-color: var(--accent-blue); outline: none; }
	.filter-select { padding: var(--space-1) var(--space-3); background: var(--bg-secondary); border: 1px solid var(--border); border-radius: var(--radius); font-size: 0.82em; color: var(--text-primary); cursor: pointer; }
	.filter-select:focus { border-color: var(--accent-blue); outline: none; }
	.cards { display: flex; flex-direction: column; gap: var(--space-3); }
	.card { background: var(--bg-secondary); border: 1px solid var(--border); border-radius: var(--radius-lg); transition: border-color 0.15s; }
	.card:hover { border-color: var(--accent-blue); }
	.card-draft { opacity: 0.75; }
	.card-deprecated { opacity: 0.5; }
	.card-header { width: 100%; display: flex; flex-direction: column; gap: var(--space-2); padding: var(--space-4); text-align: left; cursor: pointer; background: none; color: inherit; }
	.card-header:hover { background: var(--bg-hover); border-radius: var(--radius-lg); }
	.card-title-row { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
	.card-title { font-size: 1.05em; font-weight: 600; }
	.deprecated-title { text-decoration: line-through; color: var(--text-muted); }
	.status-badge { font-size: 0.7em; padding: 2px 10px; border-radius: 10px; font-weight: 600; white-space: nowrap; text-transform: uppercase; letter-spacing: 0.03em; }
	.status-active { background: color-mix(in srgb, var(--accent-green) 20%, transparent); color: var(--accent-green); }
	.status-draft { background: color-mix(in srgb, var(--accent-gray) 20%, transparent); color: var(--accent-gray); }
	.status-deprecated { background: color-mix(in srgb, var(--accent-orange) 20%, transparent); color: var(--accent-orange); }
	.card-meta { display: flex; align-items: center; gap: var(--space-2); font-size: 0.85em; color: var(--text-secondary); }
	.badge { font-size: 0.8em; padding: 1px 8px; border-radius: 10px; font-weight: 600; white-space: nowrap; }
	.badge-trigger { background: color-mix(in srgb, var(--accent-blue) 20%, transparent); color: var(--accent-blue); }
	.badge-scope { background: color-mix(in srgb, var(--accent-purple) 20%, transparent); color: var(--accent-purple); }
	.meta-sep { color: var(--text-muted); }
	.step-count { color: var(--text-muted); font-size: 0.9em; }
	.chevron { margin-left: auto; font-size: 0.65em; color: var(--text-muted); transition: transform 0.2s; }
	.chevron-open { transform: rotate(180deg); }
	.card-body { padding: 0 var(--space-4) var(--space-4); }
	.card-divider { height: 1px; background: var(--border); margin: var(--space-3) 0; }
	.content-block { padding: var(--space-2) 0; }
	.content-line { font-size: 0.9em; line-height: 1.7; color: var(--text-primary); padding: 1px 0; white-space: pre-wrap; word-break: break-word; }
	.empty-line { height: 0.5em; }
	.code-line { font-family: var(--font-mono); font-size: 0.85em; background: var(--bg-tertiary); padding: 2px var(--space-2); border-radius: var(--radius-sm); margin: 2px 0; }
	.no-content { color: var(--text-muted); font-size: 0.9em; font-style: italic; }
	.card-actions { display: flex; align-items: center; gap: var(--space-3); flex-wrap: wrap; }
	.action-btn { padding: var(--space-1) var(--space-3); font-size: 0.8em; font-weight: 600; border-radius: var(--radius); background: var(--bg-tertiary); color: var(--text-secondary); border: 1px solid var(--border); cursor: pointer; transition: background 0.15s, color 0.15s; }
	.action-btn:hover { background: var(--bg-hover); color: var(--text-primary); }
	.action-btn:disabled { opacity: 0.5; cursor: not-allowed; }
	.action-danger { color: var(--accent-orange); }
	.action-danger:hover { background: color-mix(in srgb, var(--accent-orange) 15%, var(--bg-tertiary)); color: var(--accent-orange); }
	.delete-confirm { display: flex; align-items: center; gap: var(--space-2); font-size: 0.8em; color: var(--accent-orange); font-weight: 600; }
	.new-form { background: var(--bg-secondary); border: 1px solid var(--accent-blue); border-radius: var(--radius-lg); padding: var(--space-5); margin-bottom: var(--space-6); }
	.new-form-grid { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1.2fr); gap: var(--space-4); align-items: start; margin-top: var(--space-2); }
	.new-form-sidebar { min-width: 0; }
	.new-form-main { display: flex; flex-direction: column; gap: var(--space-1); min-width: 0; }
	.new-form h2 { font-size: 1.1em; margin-bottom: var(--space-4); }
	.form-fields { display: flex; flex-direction: column; gap: var(--space-4); }
	.form-row { display: flex; flex-direction: column; gap: var(--space-1); }
	.form-row-pair { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-4); }
	.form-label { font-size: 0.8em; font-weight: 600; color: var(--text-secondary); text-transform: uppercase; letter-spacing: 0.04em; }
	.form-input, .form-select { padding: var(--space-2) var(--space-3); background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: var(--radius); color: var(--text-primary); font-size: 0.95em; }
	.form-input:focus, .form-select:focus, .form-textarea:focus { border-color: var(--accent-blue); outline: none; }
	.form-textarea { padding: var(--space-3); background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: var(--radius); color: var(--text-primary); font-family: var(--font-mono); font-size: 0.9em; line-height: 1.6; min-height: 200px; resize: vertical; }
	.form-actions { display: flex; justify-content: flex-end; gap: var(--space-3); margin-top: var(--space-4); }
	.btn { padding: var(--space-2) var(--space-5); border-radius: var(--radius); font-size: 0.85em; font-weight: 600; cursor: pointer; border: none; transition: opacity 0.15s; }
	.btn:disabled { opacity: 0.5; cursor: not-allowed; }
	.btn:hover:not(:disabled) { opacity: 0.85; }
	.btn-primary { background: var(--accent-blue); color: #fff; }
	.btn-draft { background: var(--bg-tertiary); color: var(--text-primary); border: 1px solid var(--border); }
	.btn-secondary { background: transparent; color: var(--text-secondary); }
	.btn-secondary:hover:not(:disabled) { color: var(--text-primary); opacity: 1; }
	@media (max-width: 1024px) {
		.new-form-grid { grid-template-columns: 1fr; }
	}
	@media (max-width: 768px) {
		.page-header { flex-direction: column; }
		.form-row-pair { grid-template-columns: 1fr; }
		.form-actions { flex-direction: column; }
		.form-actions .btn { width: 100%; text-align: center; }
	}
</style>
