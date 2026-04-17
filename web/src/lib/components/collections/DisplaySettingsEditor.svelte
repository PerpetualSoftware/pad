<script lang="ts">
	/**
	 * Display settings block shared by CreateCollectionModal (inside its
	 * "Advanced" reveal) and EditCollectionModal (as its Display tab
	 * content). Pure presentation — all state is bindable and owned by
	 * the parent modal.
	 *
	 * Select-field-dependent controls (board / list group-by) are only
	 * rendered when the parent passes in at least one select-type field;
	 * otherwise there's nothing sensible to group by.
	 */

	export interface DisplayFieldOption {
		key: string;
		label: string;
	}

	type DefaultView = 'list' | 'board' | 'table';
	type Layout = 'fields-primary' | 'content-primary' | 'balanced';

	interface Props {
		defaultView: DefaultView;
		layout: Layout;
		boardGroupBy: string;
		listGroupBy: string;
		listSortBy: string;
		/** Keys of select / multi_select fields — used to populate group-by options. */
		selectFieldKeys: DisplayFieldOption[];
		/** Keys available for sorting — typically all fields + created/updated/manual. */
		sortableFieldKeys: DisplayFieldOption[];
	}

	let {
		defaultView = $bindable(),
		layout = $bindable(),
		boardGroupBy = $bindable(),
		listGroupBy = $bindable(),
		listSortBy = $bindable(),
		selectFieldKeys,
		sortableFieldKeys
	}: Props = $props();
</script>

<div class="settings-grid">
	<div class="setting-item">
		<label class="setting-label" for="ds-default-view">Default view</label>
		<select id="ds-default-view" class="setting-select" bind:value={defaultView}>
			<option value="list">List</option>
			<option value="board">Board</option>
			<option value="table">Table</option>
		</select>
	</div>

	<div class="setting-item">
		<label class="setting-label" for="ds-layout">Item layout</label>
		<select id="ds-layout" class="setting-select" bind:value={layout}>
			<option value="balanced">Balanced</option>
			<option value="fields-primary">Fields primary</option>
			<option value="content-primary">Content primary</option>
		</select>
	</div>

	{#if selectFieldKeys.length > 0}
		<div class="setting-item">
			<label class="setting-label" for="ds-board-group">Board group by</label>
			<select id="ds-board-group" class="setting-select" bind:value={boardGroupBy}>
				{#each selectFieldKeys as f (f.key)}
					<option value={f.key}>{f.label}</option>
				{/each}
			</select>
			<p class="setting-hint">
				Also determines which field's terminal options drive "done" in
				dashboards, progress bars, and changelog.
			</p>
		</div>

		<div class="setting-item">
			<label class="setting-label" for="ds-list-group">List group by</label>
			<select id="ds-list-group" class="setting-select" bind:value={listGroupBy}>
				<option value="">None</option>
				{#each selectFieldKeys as f (f.key)}
					<option value={f.key}>{f.label}</option>
				{/each}
			</select>
		</div>
	{/if}

	<div class="setting-item">
		<label class="setting-label" for="ds-list-sort">List sort by</label>
		<select id="ds-list-sort" class="setting-select" bind:value={listSortBy}>
			<option value="">Default</option>
			{#each sortableFieldKeys as f (f.key)}
				<option value={f.key}>{f.label}</option>
			{/each}
		</select>
	</div>
</div>

<style>
	.settings-grid {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-4);
	}

	.setting-item {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.setting-label {
		font-size: 0.75em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}

	.setting-hint {
		margin: var(--space-1) 0 0;
		font-size: 0.72em;
		line-height: 1.4;
		color: var(--text-muted);
	}

	.setting-select {
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 0.88em;
		color: var(--text-primary);
		cursor: pointer;
	}

	.setting-select:hover {
		border-color: var(--border);
	}

	.setting-select:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	@media (max-width: 640px) {
		.settings-grid {
			grid-template-columns: 1fr;
		}
	}
</style>
