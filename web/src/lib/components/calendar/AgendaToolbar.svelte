<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import PageHeader from '$lib/components/common/PageHeader.svelte';
	import SegmentedControl from '$lib/components/common/SegmentedControl.svelte';
	import type { Workspace, WorkspaceGroup } from '$lib/types';

	type Mode = 'day' | 'week' | 'month';

	interface Props {
		mode: Mode;
		/** The period on show, named once. */
		period: string;
		zone: string;
		/** Phone layout: the range becomes a menu button and the zone moves into view options. */
		mobile?: boolean;
		/** Null when the groups did not load: the filter is then left out. */
		groups: WorkspaceGroup[] | null;
		selectedGroup: string;
		workspaces: Workspace[] | null;
		showCompleted: boolean;
		/** The period heading, which takes focus when a moved row leaves the view. */
		heading?: HTMLHeadingElement;
		onmode: (mode: Mode) => void;
		onmove: (delta: number) => void;
		ontoday: () => void;
		ongroup: (id: string) => void;
		onmanage: () => void;
		onshowcompleted: (show: boolean) => void;
	}

	let { mode, period, zone, mobile = false, groups, selectedGroup, workspaces, showCompleted, heading = $bindable(), onmode, onmove, ontoday, ongroup, onmanage, onshowcompleted }: Props = $props();

	const RANGES: { value: Mode; label: string }[] = [{ value: 'day', label: 'Day' }, { value: 'week', label: 'Week' }, { value: 'month', label: 'Month' }];
	let open = $state<'filter' | 'view' | 'range' | null>(null);
	let trigger = $state<HTMLElement>();
	const rangeLabel = $derived(RANGES.find(range => range.value === mode)!.label);
	const selected = $derived(groups?.find(group => group.id === selectedGroup));

	const groupLabel = (group: WorkspaceGroup) => group.kind === 'all' ? 'All workspaces' : group.kind === 'mine' ? 'My workspaces' : group.name;
	function groupSize(group: WorkspaceGroup): string | undefined {
		if (group.kind === 'mine') return undefined;
		if (group.kind === 'all') return workspaces ? String(workspaces.length) : undefined;
		const count = workspaces ? group.workspace_ids.filter(id => workspaces.some(workspace => workspace.id === id)).length : group.workspace_ids.length;
		return count ? String(count) : 'empty';
	}
	function toggle(menu: NonNullable<typeof open>, event: MouseEvent) {
		trigger = event.currentTarget as HTMLElement;
		open = open === menu ? null : menu;
	}
	function pick(run: () => void) {
		open = null;
		trigger?.focus();
		run();
	}
</script>

<PageHeader title="Agenda">
	{#snippet actions()}
		{#if groups && selected}
			<button type="button" class="filter" aria-label={`Workspaces: ${groupLabel(selected)}`} aria-haspopup="menu" aria-expanded={open === 'filter'} onclick={event => toggle('filter', event)}>
				<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3l9 5-9 5-9-5 9-5z" /><path d="M3 13l9 5 9-5" /></svg>
				<span class="name">{groupLabel(selected)}</span>
				{#if groupSize(selected)}<span class="size">{groupSize(selected)}</span>{/if}
				<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 9l6 6 6-6" /></svg>
			</button>
			<Menu open={open === 'filter'} {trigger} mode="portal" width={260} sheetOnMobile sheetMenu sheetTitle="Workspaces" ariaLabel="Workspaces" onclose={() => open = null}>
				{#each groups as group (group.id)}
					<MenuItem checked={group.id === selectedGroup} hint={groupSize(group)} onclick={() => pick(() => ongroup(group.id))}>{groupLabel(group)}</MenuItem>
				{/each}
				<div class="sep" role="separator"></div>
				<MenuItem onclick={() => pick(onmanage)}>Manage groups…</MenuItem>
			</Menu>
		{/if}
		<Button class="icon" aria-label="View options" aria-haspopup="menu" aria-expanded={open === 'view'} onclick={event => toggle('view', event)}>
			<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.7" /><circle cx="12" cy="12" r="1.7" /><circle cx="19" cy="12" r="1.7" /></svg>
		</Button>
		<Menu open={open === 'view'} {trigger} mode="portal" width={260} sheetOnMobile sheetMenu sheetTitle="View options" ariaLabel="View options" onclose={() => open = null}>
			<MenuItem checked={showCompleted} onclick={() => pick(() => onshowcompleted(!showCompleted))}>Show completed</MenuItem>
			<MenuItem href="/settings/account" hint={zone}>Time zone</MenuItem>
		</Menu>
	{/snippet}
	<div class="toolbar">
		<Button class="icon" aria-label={`Previous ${mode}`} onclick={() => onmove(-1)}>
			<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 6l-6 6 6 6" /></svg>
		</Button>
		<Button class="icon" aria-label={`Next ${mode}`} onclick={() => onmove(1)}>
			<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 6l6 6-6 6" /></svg>
		</Button>
		<Button onclick={ontoday}>Today</Button>
		<h2 class="period" tabindex="-1" bind:this={heading}>{period}</h2>
		{#if !mobile}<a class="zone" href="/settings/account">{zone}</a>{/if}
		<span class="spacer"></span>
		{#if mobile}
			<Button aria-label={`Range: ${rangeLabel}`} aria-haspopup="menu" aria-expanded={open === 'range'} onclick={event => toggle('range', event)}>
				{rangeLabel}
				<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 9l6 6 6-6" /></svg>
			</Button>
			<Menu open={open === 'range'} {trigger} mode="portal" sheetOnMobile sheetMenu sheetTitle="Range" ariaLabel="Range" onclose={() => open = null}>
				{#each RANGES as range (range.value)}
					<MenuItem checked={range.value === mode} onclick={() => pick(() => onmode(range.value))}>{range.label}</MenuItem>
				{/each}
			</Menu>
		{:else}
			<SegmentedControl label="Range" value={mode} onchange={onmode} options={RANGES} />
		{/if}
	</div>
</PageHeader>

<style>
	.filter { display: inline-flex; align-items: center; gap: 8px; min-width: 0; max-width: 100%; min-height: 34px; padding: 6px 10px 6px 12px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg-secondary); color: var(--text-primary); font: inherit; font-size: 13px; font-weight: 600; cursor: pointer; }
	.filter:hover { background: var(--bg-hover); }
	.filter:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: 2px; }
	.filter svg { flex: 0 0 auto; }
	.name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.size { color: var(--text-muted); font-weight: 500; }
	.sep { height: 1px; margin: 4px 0; background: var(--border); }
	.toolbar { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-2); margin-top: var(--space-4); }
	.toolbar :global(.btn.icon), :global(.page-header .header-actions > .btn.icon) { width: 34px; height: 34px; padding: 0; }
	.period { margin: 0 0 0 var(--space-2); font-size: 16px; font-weight: 700; }
	.period:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: 2px; border-radius: var(--radius-sm); }
	.zone { margin-left: var(--space-2); color: var(--text-muted); font-size: 12px; overflow-wrap: anywhere; text-decoration: none; }
	.zone:hover { color: var(--text-secondary); text-decoration: underline; }
	.spacer { flex: 1; }
	@media (max-width: 768px) {
		.filter { max-width: 190px; min-height: 36px; }
		.size { display: none; }
		.toolbar { gap: 6px; margin-top: var(--space-3); }
		.toolbar :global(.btn) { min-height: 36px; }
		.toolbar :global(.btn.icon), :global(.page-header .header-actions > .btn.icon) { width: 36px; height: 36px; }
		.period { margin-left: 4px; font-size: 15px; }
	}
</style>
