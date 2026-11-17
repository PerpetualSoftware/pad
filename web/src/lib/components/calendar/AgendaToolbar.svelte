<script lang="ts">
	import AudienceToggle from './AudienceToggle.svelte';
	import type { AudienceView, AudienceCounts } from '$lib/calendar/audience';
	import Button from '$lib/components/common/Button.svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import PageHeader from '$lib/components/common/PageHeader.svelte';
	import SegmentedControl from '$lib/components/common/SegmentedControl.svelte';
	import type { AgendaWorkspaceSelection, Workspace, WorkspaceGroup } from '$lib/types';

	type Mode = 'day' | 'week' | 'month';

	interface Props {
		audience: AudienceView;
		counts: AudienceCounts;
		onaudience: (value: AudienceView) => void;
		mode: Mode;
		/** The period on show, named once. */
		period: string;
		zone: string;
		/** Phone layout: the range becomes a menu button and the zone moves into view options. */
		mobile?: boolean;
		/** Null when groups have not loaded. Workspace selections remain usable. */
		groups: WorkspaceGroup[] | null;
		selection: AgendaWorkspaceSelection;
		workspaceLabel: (workspace: Workspace) => string;
		workspacesLoading: boolean;
		workspacesError: boolean;
		onretryworkspaces: () => void;
		onretrygroups: () => void;
		onworkspace: (id: string) => void;
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

	let { audience, counts, onaudience, mode, period, zone, mobile = false, groups, selection, workspaces, workspaceLabel, workspacesLoading, workspacesError, onretryworkspaces, onretrygroups, onworkspace, showCompleted, heading = $bindable(), onmode, onmove, ontoday, ongroup, onmanage, onshowcompleted }: Props = $props();

	const RANGES: { value: Mode; label: string }[] = [{ value: 'day', label: 'Day' }, { value: 'week', label: 'Week' }, { value: 'month', label: 'Month' }];
	let open = $state<'filter' | 'view' | 'range' | null>(null);
	let trigger = $state<HTMLElement>();
	const rangeLabel = $derived(RANGES.find(range => range.value === mode)!.label);
	let workspaceView = $state<'root' | 'workspaces'>('root');
	let workspaceList = $state<HTMLDivElement>();
	let moreWorkspacesBelow = $state(false);
	const builtins: WorkspaceGroup[] = [
		{ id: 'all', kind: 'all', name: 'All', workspace_ids: [] },
		{ id: 'mine', kind: 'mine', name: 'Mine', workspace_ids: [] }
	];
	// Keep built-in choices and retries available after leaving a workspace when groups failed.
	let workspacePickerAvailable = $state(false);
	$effect(() => { if (selection.kind === 'workspace') workspacePickerAvailable = true; });
	const selected = $derived(selection.kind === 'group' ? (groups ?? builtins).find(group => group.id === selection.id) : undefined);
	const selectedWorkspace = $derived(workspaces?.find(workspace => workspace.id === selection.id));
	const activeLabel = $derived(selection.kind === 'workspace' ? selectedWorkspace ? workspaceLabel(selectedWorkspace) : 'Selected workspace' : selected ? groupLabel(selected) : 'All workspaces');
	const listed = $derived([...(workspaces ?? [])].sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }) || (a.owner_username ?? '').localeCompare(b.owner_username ?? '') || a.id.localeCompare(b.id)));

	$effect(() => { if (!open) workspaceView = 'root'; });
	$effect(() => {
		// Recheck after list content changes as well as container resizes.
		listed; workspacesLoading; workspacesError;
		if (!workspaceList) return;
		updateWorkspaceScroll();
		const observer = new ResizeObserver(updateWorkspaceScroll);
		observer.observe(workspaceList);
		return () => observer.disconnect();
	});
	function updateWorkspaceScroll() {
		moreWorkspacesBelow = !!workspaceList && workspaceList.scrollHeight - workspaceList.clientHeight - workspaceList.scrollTop > 1;
	}

	function groupLabel(group: WorkspaceGroup) { return group.kind === 'all' ? 'All workspaces' : group.kind === 'mine' ? 'My workspaces' : group.name; }
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

{#snippet workspaceOptions()}
	{#if workspaceView === 'workspaces'}
		<div class="workspace-picker" class:phone={mobile}>
			<MenuItem onclick={() => workspaceView = 'root'}>Back</MenuItem>
			<div class="workspace-list" bind:this={workspaceList} onscroll={updateWorkspaceScroll} data-scroll-more={moreWorkspacesBelow || undefined}>
				{#if workspacesLoading}<p role="status">Loading workspaces…</p>{/if}
				{#if workspacesError}
					<p role="status">Could not load workspaces.</p>
					<MenuItem onclick={onretryworkspaces}>Retry</MenuItem>
				{/if}
				{#if !workspacesLoading && !workspacesError && listed.length === 0}<p>No workspaces available.</p>{/if}
				{#each listed as workspace (workspace.id)}
					<MenuItem checked={selection.kind === 'workspace' && selection.id === workspace.id} title={workspaceLabel(workspace)} onclick={() => pick(() => onworkspace(workspace.id))}>
						<span class="workspace-name">{workspaceLabel(workspace)}</span>
						<span class="workspace-identity">{workspace.owner_username ? `@${workspace.owner_username}/` : ''}{workspace.slug}</span>
					</MenuItem>
				{/each}
			</div>
		</div>
	{:else}
		{#each groups?.filter(group => group.kind !== 'named') ?? builtins as group (group.id)}
			<MenuItem checked={selection.kind === 'group' && group.id === selection.id} hint={groupSize(group)} onclick={() => pick(() => ongroup(group.id))}>{groupLabel(group)}</MenuItem>
		{/each}
		<div class="workspace-chooser">
			<MenuItem hint={selection.kind === 'workspace' ? activeLabel : undefined} title={selection.kind === 'workspace' ? activeLabel : undefined} onclick={() => workspaceView = 'workspaces'}>
				Choose a workspace… <span aria-hidden="true">›</span>
			</MenuItem>
		</div>
		{#if groups}
			{#each groups.filter(group => group.kind === 'named') as group (group.id)}
				<MenuItem checked={selection.kind === 'group' && group.id === selection.id} hint={groupSize(group)} onclick={() => pick(() => ongroup(group.id))}>{groupLabel(group)}</MenuItem>
			{/each}
			<div class="sep" role="separator"></div>
			<MenuItem onclick={() => pick(onmanage)}>Manage groups…</MenuItem>
		{:else}
			<p class="list-status">Groups unavailable</p>
			<MenuItem onclick={onretrygroups}>Retry groups</MenuItem>
		{/if}
	{/if}
{/snippet}

<PageHeader title="Agenda">
	{#snippet actions()}
		{#if mobile}
			<div class="phone-audience"><AudienceToggle value={audience} {counts} onchange={onaudience} /></div>
		{:else if groups || workspacePickerAvailable || selection.kind === 'workspace'}
			<button type="button" class="filter" title={activeLabel} aria-label={`Workspaces: ${activeLabel}`} aria-haspopup="menu" aria-expanded={open === 'filter'} onclick={event => toggle('filter', event)}>
				<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3l9 5-9 5-9-5 9-5z" /><path d="M3 13l9 5 9-5" /></svg>
				<span class="name">{activeLabel}</span>
				{#if selected && groupSize(selected)}<span class="size">{groupSize(selected)}</span>{/if}
				<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 9l6 6 6-6" /></svg>
			</button>
			<Menu open={open === 'filter'} {trigger} mode="portal" width={260} sheetOnMobile sheetMenu sheetTitle="Workspaces" ariaLabel="Workspaces" focusKey={workspaceView} bodyScroll={workspaceView === 'workspaces'} onclose={() => open = null}>
				{@render workspaceOptions()}
			</Menu>
		{/if}
		<Button class="icon" aria-label={mobile && selection.kind === 'workspace' ? `View options: ${activeLabel}` : 'View options'} aria-haspopup="menu" aria-expanded={open === 'view'} onclick={event => toggle('view', event)}>
			<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.7" /><circle cx="12" cy="12" r="1.7" /><circle cx="19" cy="12" r="1.7" /></svg>
		</Button>
		<Menu open={open === 'view'} {trigger} mode="portal" width={260} sheetOnMobile sheetMenu sheetTitle="View options" ariaLabel="View options" focusKey={workspaceView} onclose={() => open = null}>
			{#if mobile && (groups || workspacePickerAvailable || selection.kind === 'workspace')}
				{@render workspaceOptions()}
				<div class="sep" role="separator"></div>
			{/if}
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
			<span class="show">Show</span>
			<AudienceToggle value={audience} {counts} onchange={onaudience} />
			<SegmentedControl label="Range" value={mode} onchange={onmode} options={RANGES} />
		{/if}
	</div>
</PageHeader>

<style>
	.workspace-chooser :global(.mi-hint) { max-width: 50%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.workspace-picker { position: relative; display: flex; flex-direction: column; min-height: 0; --scroll-cue-bg: var(--bg-raised, var(--bg-tertiary)); --scroll-cue-height: 24px; }
	.workspace-picker:has(.workspace-list[data-scroll-more])::after { content: ''; position: absolute; inset: auto 0 0; height: var(--scroll-cue-height); pointer-events: none; background: linear-gradient(transparent, var(--scroll-cue-bg)); }
	/* Phone rows are about 58px tall with roomy padding, so a 24px fade lands on the gap between rows and dims nothing, and one that dims only the muted slug line reads as styling (TASK-4584). Fade past a full row so the last name line dims too. */
	.workspace-picker.phone { --scroll-cue-bg: var(--bg-secondary); --scroll-cue-height: 72px; }
	.workspace-picker > :global(.mi) { flex-shrink: 0; }
	.workspace-list { min-height: 0; overflow-y: scroll; scrollbar-gutter: stable; border-top: 1px solid var(--border); }
	.workspace-picker.phone .workspace-list { max-height: 50dvh; }
	.workspace-name, .workspace-identity { display: block; overflow: hidden; text-overflow: ellipsis; }
	.workspace-identity { color: var(--text-muted); font-size: 11.5px; }
	.workspace-list p, .list-status { padding: 7px 9px; color: var(--text-muted); font-size: 13px; }

	.show { font-size: 12px; color: var(--text-muted); }
	.phone-audience { flex: 1; min-width: 0; }
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
		:global(.page-header .header-actions:has(.phone-audience)) { width: 100%; }
		.filter { max-width: 190px; min-height: 36px; }
		.size { display: none; }
		.toolbar { gap: 6px; margin-top: var(--space-3); }
		.toolbar :global(.btn) { min-height: 36px; }
		.toolbar :global(.btn.icon), :global(.page-header .header-actions > .btn.icon) { width: 36px; height: 36px; }
		.period { margin-left: 4px; font-size: 15px; }
	}
</style>
