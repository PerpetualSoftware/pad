<script lang="ts">
	import type { Item, Collection } from '$lib/types';
	import { parseSchema } from '$lib/types';
	import { SORT_OPTIONS, priorityField, type SortMode } from '$lib/collections/itemSort';

	interface Props {
		/** The lane's CURRENTLY-FILTERED items — every action operates on these. */
		items: Item[];
		/** The lane's group value (e.g. the status) — used to label the lane. */
		groupValue: string;
		/** Which field the board groups by. "Move all to" only applies when 'status'. */
		groupField: string;
		collection: Collection;
		/** True when a filter/search is narrowing the lane — surfaced in labels. */
		filtered: boolean;
		members: { user_id: string; user_name?: string }[];
		/** Workspace tag suggestions for "Tag all". */
		tagSuggestions: string[];
		// Page-wide sort default + this lane's ephemeral override (TASK-1673).
		// `laneSort` undefined = follow the page default. onSetLaneSort(null)
		// clears the override. Sorting is a view preference, available to
		// everyone (including viewers) — not gated on edit permission.
		sortMode?: SortMode;
		laneSort?: SortMode;
		onSetLaneSort?: (mode: SortMode | null) => void;
		onClose: () => void;
		// Each action is optional: the caller passes only the ones the
		// current user is permitted to perform (the single `+` create is
		// grant-aware; the bulk verbs require workspace owner/editor — see
		// the page). An undefined callback hides its menu entry.
		onAddItem?: () => void;
		onArchive?: () => void;
		onMove?: (status: string) => void;
		onTag?: (tag: string) => void;
		onUntag?: (tag: string) => void;
		onSetPriority?: (priority: string) => void;
		onAssign?: (userId: string) => void;
	}

	let {
		items,
		groupValue,
		groupField,
		collection,
		filtered,
		members,
		tagSuggestions,
		sortMode = 'manual',
		laneSort = undefined,
		onSetLaneSort,
		onClose,
		onAddItem,
		onArchive,
		onMove,
		onTag,
		onUntag,
		onSetPriority,
		onAssign
	}: Props = $props();

	type View = 'root' | 'move' | 'tag' | 'untag' | 'priority' | 'assign' | 'sort';
	let view = $state<View>('root');
	let confirmArchive = $state(false);
	let tagInput = $state('');

	let count = $derived(items.length);
	let scopeNote = $derived(filtered ? ' (filtered)' : '');

	let schema = $derived(parseSchema(collection));
	let statusField = $derived(schema.fields.find((f) => f.key === 'status'));
	let priorityFieldDef = $derived(
		schema.fields.find((f) => f.key === 'priority' && f.type === 'select')
	);

	// Move targets = the other lanes. Only offered when the board groups by
	// status, since the bulk `move` op sets the status field (TASK-1668).
	let moveTargets = $derived(
		groupField === 'status'
			? (statusField?.options ?? []).filter((o) => o !== groupValue)
			: []
	);
	let priorityOptions = $derived(priorityFieldDef?.options ?? []);

	// Sort options offered in the "Sort lane by" submenu — same set the
	// page toolbar uses, minus Priority when the collection has no priority
	// field (TASK-1673). `effectiveSort` is the lane's current order: its
	// override if set, else the page default.
	let sortOptions = $derived(
		priorityField(collection) ? SORT_OPTIONS : SORT_OPTIONS.filter((o) => o.value !== 'priority')
	);
	let effectiveSort = $derived(laneSort ?? sortMode);
	let sortLabel = $derived(
		SORT_OPTIONS.find((o) => o.value === effectiveSort)?.label ?? 'Manual'
	);

	// Untag offers only the tags actually present on the lane's items.
	let laneTags = $derived.by(() => {
		const set = new Set<string>();
		for (const it of items) {
			try {
				for (const t of JSON.parse(it.tags || '[]')) set.add(t);
			} catch {
				/* malformed tags JSON — skip */
			}
		}
		return [...set].sort();
	});

	// Section presence — drives separators so none dangle when a section
	// (e.g. the bulk verbs for a viewer) is empty. Declared after laneTags
	// since hasVerbs references it.
	let hasSort = $derived(count > 0 && !!onSetLaneSort);
	let hasVerbs = $derived(
		count > 0 &&
			(!!(onMove && moveTargets.length) ||
				!!onTag ||
				!!(onUntag && laneTags.length) ||
				!!(onSetPriority && priorityOptions.length) ||
				!!(onAssign && members.length))
	);
	let hasArchive = $derived(count > 0 && !!onArchive);

	function fmt(v: string): string {
		return v.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}

	// stopPropagation on every click: a click that mutates state (drill-down,
	// archive confirm) re-renders and detaches the clicked node before the
	// event bubbles to BoardView's <svelte:window> outside-click handler,
	// where closest() on the orphan returns null and slams the menu shut.
	// Same Svelte 5 same-click issue documented in console/+layout.svelte.
	function run(e: MouseEvent, fn: () => void) {
		e.stopPropagation();
		fn();
	}
	function submitTag() {
		const t = tagInput.trim();
		if (t) {
			onTag?.(t);
			onClose();
		}
	}
</script>

<div class="lane-menu" role="menu" onkeydown={(e) => { if (e.key === 'Escape') onClose(); }} tabindex="-1">
	{#if view === 'root'}
		{#if onAddItem}
			<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => { onAddItem?.(); onClose(); })}>
				<span class="lmi-icon" aria-hidden="true">＋</span> Add item here
			</button>
		{/if}

		{#if hasSort}
			{#if onAddItem}<div class="lane-menu-sep"></div>{/if}
			<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => (view = 'sort'))}>
				<span class="lmi-icon" aria-hidden="true">↕</span> Sort lane by
				<span class="lmi-chevron">{sortLabel} ›</span>
			</button>
		{/if}

		{#if hasVerbs}
			{#if onAddItem || hasSort}<div class="lane-menu-sep"></div>{/if}
			{#if onMove && moveTargets.length > 0}
				<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => (view = 'move'))}>
					<span class="lmi-icon" aria-hidden="true">→</span> Move all to <span class="lmi-chevron">›</span>
				</button>
			{/if}

			{#if onTag}
				<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => (view = 'tag'))}>
					<span class="lmi-icon" aria-hidden="true">🏷</span> Tag all <span class="lmi-chevron">›</span>
				</button>
			{/if}

			{#if onUntag && laneTags.length > 0}
				<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => (view = 'untag'))}>
					<span class="lmi-icon" aria-hidden="true">⌫</span> Untag all <span class="lmi-chevron">›</span>
				</button>
			{/if}

			{#if onSetPriority && priorityOptions.length > 0}
				<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => (view = 'priority'))}>
					<span class="lmi-icon" aria-hidden="true">⚑</span> Set priority <span class="lmi-chevron">›</span>
				</button>
			{/if}

			{#if onAssign && members.length > 0}
				<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => (view = 'assign'))}>
					<span class="lmi-icon" aria-hidden="true">👤</span> Assign all <span class="lmi-chevron">›</span>
				</button>
			{/if}
		{/if}

		{#if hasArchive}
			{#if onAddItem || hasSort || hasVerbs}<div class="lane-menu-sep"></div>{/if}
			{#if confirmArchive}
				<div class="lane-menu-confirm">
					<span>Archive {count} item{count === 1 ? '' : 's'}{scopeNote}?</span>
					<div class="lmc-actions">
						<button class="lmc-yes" onclick={(e) => run(e, () => { onArchive?.(); onClose(); })}>Archive</button>
						<button class="lmc-no" onclick={(e) => run(e, () => (confirmArchive = false))}>Cancel</button>
					</div>
				</div>
			{:else}
				<button class="lane-menu-item lmi-danger" role="menuitem" onclick={(e) => run(e, () => (confirmArchive = true))}>
					<span class="lmi-icon" aria-hidden="true">🗃</span> Archive all ({count}{scopeNote})
				</button>
			{/if}
		{/if}
	{:else if view === 'move'}
		<button class="lane-menu-back" onclick={(e) => run(e, () => (view = 'root'))}>‹ Move all to{scopeNote}</button>
		{#each moveTargets as target (target)}
			<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => { onMove?.(target); onClose(); })}>
				{fmt(target)}
			</button>
		{/each}
	{:else if view === 'priority'}
		<button class="lane-menu-back" onclick={(e) => run(e, () => (view = 'root'))}>‹ Set priority{scopeNote}</button>
		{#each priorityOptions as p (p)}
			<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => { onSetPriority?.(p); onClose(); })}>
				{fmt(p)}
			</button>
		{/each}
	{:else if view === 'assign'}
		<button class="lane-menu-back" onclick={(e) => run(e, () => (view = 'root'))}>‹ Assign all{scopeNote}</button>
		{#each members as m (m.user_id)}
			<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => { onAssign?.(m.user_id); onClose(); })}>
				{m.user_name || m.user_id}
			</button>
		{/each}
	{:else if view === 'untag'}
		<button class="lane-menu-back" onclick={(e) => run(e, () => (view = 'root'))}>‹ Untag all{scopeNote}</button>
		{#each laneTags as t (t)}
			<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => { onUntag?.(t); onClose(); })}>
				{t}
			</button>
		{/each}
	{:else if view === 'tag'}
		<button class="lane-menu-back" onclick={(e) => run(e, () => (view = 'root'))}>‹ Tag all{scopeNote}</button>
		<div class="lane-menu-tag-input">
			<input
				type="text"
				placeholder="Add tag…"
				bind:value={tagInput}
				onclick={(e) => e.stopPropagation()}
				onkeydown={(e) => { if (e.key === 'Enter') { e.stopPropagation(); submitTag(); } }}
			/>
			<button class="lmc-yes" disabled={!tagInput.trim()} onclick={(e) => run(e, submitTag)}>Add</button>
		</div>
		{#if tagSuggestions.length > 0}
			<div class="lane-menu-sep"></div>
			{#each tagSuggestions as t (t)}
				<button class="lane-menu-item" role="menuitem" onclick={(e) => run(e, () => { onTag?.(t); onClose(); })}>
					{t}
				</button>
			{/each}
		{/if}
	{:else if view === 'sort'}
		<button class="lane-menu-back" onclick={(e) => run(e, () => (view = 'root'))}>‹ Sort lane by</button>
		<!-- Clear the ephemeral override → follow the page-wide sort. -->
		<button
			class="lane-menu-item"
			role="menuitemradio"
			aria-checked={laneSort === undefined}
			onclick={(e) => run(e, () => { onSetLaneSort?.(null); onClose(); })}
		>
			<span class="lmi-check" aria-hidden="true">{laneSort === undefined ? '✓' : ''}</span>
			Page default
			<span class="lmi-chevron">{SORT_OPTIONS.find((o) => o.value === sortMode)?.label}</span>
		</button>
		<div class="lane-menu-sep"></div>
		{#each sortOptions as opt (opt.value)}
			<button
				class="lane-menu-item"
				role="menuitemradio"
				aria-checked={laneSort === opt.value}
				onclick={(e) => run(e, () => { onSetLaneSort?.(opt.value); onClose(); })}
			>
				<span class="lmi-check" aria-hidden="true">{laneSort === opt.value ? '✓' : ''}</span>
				{opt.label}
			</button>
		{/each}
	{/if}
</div>

<style>
	.lane-menu {
		position: absolute;
		top: calc(100% + 4px);
		right: 0;
		z-index: 20;
		min-width: 200px;
		max-height: 320px;
		overflow-y: auto;
		padding: var(--space-1);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		box-shadow: var(--shadow-md, 0 4px 12px rgba(0, 0, 0, 0.15));
		display: flex;
		flex-direction: column;
		gap: 2px;
	}

	.lane-menu-item {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: 8px 10px;
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.875em;
		text-align: left;
		cursor: pointer;
	}

	.lane-menu-item:hover {
		background: var(--bg-hover);
	}

	.lane-menu-item.lmi-danger {
		color: var(--accent-red, #ef4444);
	}

	.lane-menu-item.lmi-danger:hover {
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
	}

	.lmi-icon {
		width: 1.1em;
		text-align: center;
		flex-shrink: 0;
	}

	.lmi-check {
		width: 1.1em;
		flex-shrink: 0;
		color: var(--accent-blue);
		font-size: 0.9em;
	}

	.lmi-chevron {
		margin-left: auto;
		color: var(--text-muted);
	}

	.lane-menu-back {
		display: flex;
		align-items: center;
		width: 100%;
		padding: 6px 10px;
		margin-bottom: 2px;
		background: none;
		border: none;
		border-bottom: 1px solid var(--border);
		border-radius: 0;
		color: var(--text-muted);
		font-size: 0.8125em;
		font-weight: 600;
		text-align: left;
		cursor: pointer;
	}

	.lane-menu-back:hover {
		color: var(--text-primary);
	}

	.lane-menu-sep {
		height: 1px;
		margin: 2px 0;
		background: var(--border);
	}

	.lane-menu-confirm {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: 8px 10px;
		font-size: 0.8125em;
		color: var(--text-secondary);
	}

	.lmc-actions {
		display: flex;
		gap: var(--space-2);
	}

	.lmc-yes {
		flex: 1;
		padding: 6px 10px;
		background: var(--accent-red, #ef4444);
		border: none;
		border-radius: var(--radius-sm);
		color: #fff;
		font-size: 0.8125em;
		cursor: pointer;
	}

	.lmc-yes:disabled {
		opacity: 0.5;
		cursor: default;
	}

	.lmc-no {
		flex: 1;
		padding: 6px 10px;
		background: var(--bg-tertiary);
		border: none;
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.8125em;
		cursor: pointer;
	}

	.lane-menu-tag-input {
		display: flex;
		gap: var(--space-2);
		padding: 6px 8px;
	}

	.lane-menu-tag-input input {
		flex: 1;
		min-width: 0;
		padding: 6px 8px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.8125em;
	}

	.lane-menu-tag-input .lmc-yes {
		flex: 0 0 auto;
		background: var(--accent-blue);
	}
</style>
