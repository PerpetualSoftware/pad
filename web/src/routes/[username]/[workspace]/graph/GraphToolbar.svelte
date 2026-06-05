<script lang="ts">
	// Toolbar for the 3D workspace graph (PLAN-1730 / TASK-1735).
	//
	// Owns the show-completed toggle, the filter controls (collection chips, status
	// chips, optional role dropdown), the filtered node/edge readout, and the search
	// fly-to box. It is PURE PRESENTATION + LOCAL UI STATE: the page owns the
	// authoritative filter $state (read by its renderer-sync effect) and the
	// selection machinery. This component receives the current filter values + the
	// distinct option lists, and emits callbacks the page applies.
	//
	// Styling mirrors the page's old inline .toolbar (backdrop-blur, color-mix
	// surfaces) so the control layer reads as the same UI.

	// Minimal node shape the search box needs — a structural subset of the page's
	// GraphNode3D so the page can pass its mapped/filtered nodes straight through.
	interface SearchNode {
		ref: string;
		title: string;
		collection: string;
	}

	let {
		// show-completed toggle (two-way bound to the page).
		showCompleted = $bindable(),
		// Filtered vs total counts for the readout.
		nodeCount,
		edgeCount,
		totalNodeCount,
		totalEdgeCount,
		filtered,
		// Distinct option lists, derived by the page from the loaded payload.
		collections,
		statuses,
		roles,
		// Current filter selections (page-owned $state, passed down read-only).
		selectedCollections,
		selectedStatuses,
		selectedRole,
		// POST-filter nodes, for the search type-ahead.
		searchNodes,
		// Palette accessor — shared with the renderer so chips match node colors.
		colorForCollection,
		// Filter callbacks.
		ontogglecollection,
		ontogglestatus,
		onselectrole,
		// Search fly-to: page resolves the ref to a live renderer node + selects it.
		onsearchpick
	}: {
		showCompleted: boolean;
		nodeCount: number;
		edgeCount: number;
		totalNodeCount: number;
		totalEdgeCount: number;
		filtered: boolean;
		collections: string[];
		statuses: string[];
		roles: string[];
		selectedCollections: string[];
		selectedStatuses: string[];
		selectedRole: string | null;
		searchNodes: SearchNode[];
		colorForCollection: (slug: string) => string;
		ontogglecollection: (slug: string) => void;
		ontogglestatus: (status: string) => void;
		onselectrole: (role: string | null) => void;
		onsearchpick: (ref: string) => void;
	} = $props();

	// ── Filters disclosure ──────────────────────────────────────────────────────
	// Collapsed by default to keep the toolbar tidy; a count badge hints at active
	// filters without expanding.
	let filtersOpen = $state(false);
	const activeFilterCount = $derived(
		selectedCollections.length + selectedStatuses.length + (selectedRole ? 1 : 0)
	);

	// ── Search box (local UI state only) ─────────────────────────────────────────
	let query = $state('');
	let searchOpen = $state(false);
	// Active descendant in the type-ahead list, -1 when none. Reset on every fresh
	// match set so the highlight never points past the list.
	let highlight = $state(-1);

	// Case-insensitive match on ref or title, capped at 8 results. $derived so the
	// list tracks both the query and any payload/filter change underneath it.
	const MAX_RESULTS = 8;
	const matches = $derived.by<SearchNode[]>(() => {
		const q = query.trim().toLowerCase();
		if (!q) return [];
		const out: SearchNode[] = [];
		for (const n of searchNodes) {
			if (n.ref.toLowerCase().includes(q) || n.title.toLowerCase().includes(q)) {
				out.push(n);
				if (out.length >= MAX_RESULTS) break;
			}
		}
		return out;
	});

	// Dropdown is visible only when focused AND there are matches to show.
	const dropdownVisible = $derived(searchOpen && matches.length > 0);

	function pick(ref: string) {
		onsearchpick(ref);
		query = '';
		searchOpen = false;
		highlight = -1;
	}

	function onInput() {
		searchOpen = true;
		// Reset the active row whenever the query changes; the match set just shifted.
		highlight = matches.length > 0 ? 0 : -1;
	}

	// Keyboard nav. Escape closes the dropdown WITHOUT bubbling to the page's
	// window-level Escape-deselect handler — but only when the dropdown is open, so
	// a stray Escape on an empty/closed box still reaches the page (CONVE-639 spirit).
	function onSearchKeydown(e: KeyboardEvent) {
		if (e.key === 'ArrowDown') {
			if (!dropdownVisible) return;
			e.preventDefault();
			highlight = (highlight + 1) % matches.length;
		} else if (e.key === 'ArrowUp') {
			if (!dropdownVisible) return;
			e.preventDefault();
			highlight = (highlight - 1 + matches.length) % matches.length;
		} else if (e.key === 'Enter') {
			if (!dropdownVisible) return;
			e.preventDefault();
			const target = highlight >= 0 ? matches[highlight] : matches[0];
			if (target) pick(target.ref);
		} else if (e.key === 'Escape') {
			if (searchOpen) {
				e.preventDefault();
				e.stopPropagation();
				searchOpen = false;
				highlight = -1;
			}
		}
	}
</script>

<div class="toolbar">
	<div class="row primary">
		<label class="toggle">
			<input type="checkbox" bind:checked={showCompleted} />
			<span>Show completed</span>
		</label>

		<button
			type="button"
			class="filters-btn"
			class:open={filtersOpen}
			aria-expanded={filtersOpen}
			onclick={() => (filtersOpen = !filtersOpen)}
		>
			Filters{#if activeFilterCount > 0}<span class="badge">{activeFilterCount}</span>{/if}
		</button>

		<span class="counts">
			{#if filtered}
				<span class="count">{nodeCount} of {totalNodeCount} nodes</span>
				<span class="count-sep">·</span>
				<span class="count">{edgeCount} of {totalEdgeCount} edges</span>
			{:else}
				<span class="count">{nodeCount} node{nodeCount === 1 ? '' : 's'}</span>
				<span class="count-sep">·</span>
				<span class="count">{edgeCount} edge{edgeCount === 1 ? '' : 's'}</span>
			{/if}
		</span>
	</div>

	<!-- Search fly-to. The wrapper is relative so the type-ahead anchors to it. -->
	<div class="row search-row">
		<div class="search">
			<input
				type="text"
				class="search-input"
				placeholder="Search items…"
				bind:value={query}
				oninput={onInput}
				onfocus={() => (searchOpen = true)}
				onkeydown={onSearchKeydown}
				role="combobox"
				aria-expanded={dropdownVisible}
				aria-controls="graph-search-list"
				aria-autocomplete="list"
			/>
			{#if dropdownVisible}
				<ul class="search-results" id="graph-search-list" role="listbox">
					{#each matches as m, i (m.ref)}
						<li role="option" aria-selected={i === highlight}>
							<button
								type="button"
								class="result"
								class:active={i === highlight}
								onmousedown={(e) => {
									// mousedown (not click) so the pick fires before the input's
									// blur closes the dropdown out from under it.
									e.preventDefault();
									pick(m.ref);
								}}
								onmouseenter={() => (highlight = i)}
							>
								<span
									class="dot"
									style:background-color={colorForCollection(m.collection)}
									aria-hidden="true"
								></span>
								<span class="result-ref">{m.ref}</span>
								<span class="result-title">{m.title}</span>
							</button>
						</li>
					{/each}
				</ul>
			{/if}
		</div>
	</div>

	{#if filtersOpen}
		<div class="row filters">
			{#if collections.length > 0}
				<div class="filter-group" role="group" aria-label="Filter by collection">
					<span class="filter-label">Collection</span>
					<div class="chips">
						{#each collections as slug (slug)}
							<button
								type="button"
								class="chip"
								class:active={selectedCollections.includes(slug)}
								aria-pressed={selectedCollections.includes(slug)}
								onclick={() => ontogglecollection(slug)}
							>
								<span
									class="dot"
									style:background-color={colorForCollection(slug)}
									aria-hidden="true"
								></span>
								{slug}
							</button>
						{/each}
					</div>
				</div>
			{/if}

			{#if statuses.length > 0}
				<div class="filter-group" role="group" aria-label="Filter by status">
					<span class="filter-label">Status</span>
					<div class="chips">
						{#each statuses as status (status)}
							<button
								type="button"
								class="chip"
								class:active={selectedStatuses.includes(status)}
								aria-pressed={selectedStatuses.includes(status)}
								onclick={() => ontogglestatus(status)}
							>
								{status}
							</button>
						{/each}
					</div>
				</div>
			{/if}

			{#if roles.length > 0}
				<div class="filter-group">
					<span class="filter-label" id="graph-role-label">Role</span>
					<select
						class="role-select"
						aria-labelledby="graph-role-label"
						value={selectedRole ?? ''}
						onchange={(e) => onselectrole(e.currentTarget.value || null)}
					>
						<option value="">All roles</option>
						{#each roles as role (role)}
							<option value={role}>{role}</option>
						{/each}
					</select>
				</div>
			{/if}
		</div>
	{/if}
</div>

<style>
	.toolbar {
		position: absolute;
		top: var(--space-4);
		left: var(--space-4);
		z-index: 10;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		max-width: min(28rem, calc(100% - var(--space-8)));
		padding: var(--space-2) var(--space-4);
		background: color-mix(in srgb, var(--bg-secondary) 88%, transparent);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 2px 8px rgba(0, 0, 0, 0.25);
		backdrop-filter: blur(6px);
	}

	.row {
		display: flex;
		align-items: center;
		gap: var(--space-4);
	}
	.row.primary {
		flex-wrap: wrap;
	}

	.toggle {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.82em;
		font-weight: 600;
		color: var(--text-secondary);
		cursor: pointer;
		user-select: none;
	}
	.toggle input {
		cursor: pointer;
	}

	.filters-btn {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		padding: var(--space-1) var(--space-3);
		font-size: 0.8em;
		font-weight: 600;
		color: var(--text-secondary);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: 999px;
		cursor: pointer;
		transition: border-color 0.15s, color 0.15s;
	}
	.filters-btn:hover,
	.filters-btn.open {
		border-color: var(--text-muted);
		color: var(--text-primary);
	}
	.badge {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-width: 1.1rem;
		height: 1.1rem;
		padding: 0 0.3rem;
		font-size: 0.85em;
		font-variant-numeric: tabular-nums;
		color: var(--btn-primary-text, #fff);
		background: var(--accent, #6366f1);
		border-radius: 999px;
	}

	.counts {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.78em;
		color: var(--text-muted);
		font-variant-numeric: tabular-nums;
	}
	.count-sep {
		opacity: 0.5;
	}

	/* ── Search ───────────────────────────────────────────────────────────────── */
	.search-row {
		align-items: stretch;
	}
	.search {
		position: relative;
		flex: 1;
	}
	.search-input {
		width: 100%;
		padding: var(--space-1) var(--space-3);
		font-size: 0.82em;
		color: var(--text-primary);
		background: var(--bg-primary, #0a0a1a);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}
	.search-input::placeholder {
		color: var(--text-muted);
	}
	.search-input:focus {
		outline: none;
		border-color: var(--accent, #6366f1);
	}

	.search-results {
		position: absolute;
		top: calc(100% + 4px);
		left: 0;
		right: 0;
		z-index: 11;
		margin: 0;
		padding: var(--space-1);
		list-style: none;
		max-height: 16rem;
		overflow-y: auto;
		background: color-mix(in srgb, var(--bg-secondary) 96%, transparent);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 4px 16px rgba(0, 0, 0, 0.35);
		backdrop-filter: blur(8px);
	}
	.result {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-1) var(--space-2);
		text-align: left;
		background: transparent;
		border: none;
		border-radius: var(--radius-sm, 4px);
		cursor: pointer;
	}
	.result.active {
		background: color-mix(in srgb, var(--accent, #6366f1) 18%, transparent);
	}
	.result-ref {
		font-family: var(--font-mono, ui-monospace, monospace);
		font-size: 0.72em;
		font-weight: 600;
		color: var(--text-secondary);
		flex: 0 0 auto;
	}
	.result-title {
		font-size: 0.78em;
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	/* ── Filters ──────────────────────────────────────────────────────────────── */
	.filters {
		flex-direction: column;
		align-items: stretch;
		gap: var(--space-3);
		padding-top: var(--space-2);
		border-top: 1px solid var(--border);
	}
	.filter-group {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.filter-label {
		font-size: 0.7em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-muted);
	}
	.chips {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}
	.chip {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		padding: var(--space-1) var(--space-3);
		font-size: 0.78em;
		font-weight: 500;
		text-transform: capitalize;
		color: var(--text-secondary);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: 999px;
		cursor: pointer;
		transition: background 0.15s, border-color 0.15s, color 0.15s;
	}
	.chip:hover {
		border-color: var(--text-muted);
		color: var(--text-primary);
	}
	.chip.active {
		background: color-mix(in srgb, var(--accent, #6366f1) 15%, transparent);
		border-color: var(--accent, #6366f1);
		color: var(--text-primary);
	}

	.dot {
		width: 0.6rem;
		height: 0.6rem;
		border-radius: 50%;
		flex: 0 0 auto;
	}

	.role-select {
		padding: var(--space-1) var(--space-2);
		font-size: 0.8em;
		color: var(--text-primary);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		cursor: pointer;
	}
</style>
