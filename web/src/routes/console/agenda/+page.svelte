<script lang="ts">
	import { onMount, tick, untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { browserZone } from '$lib/timezone';
	import { canMove, canResolve, dateTime, dayLabel, dayWindow, weekLabel, weekWindow, targetOnDay, isAllDay, sameTarget, entryKey, filterEntries, groupEntries, localDate, shiftDay, todayIn, type AgendaAction } from '$lib/calendar/presentation';
	import PageHeader from '$lib/components/common/PageHeader.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import EmptyState from '$lib/components/common/EmptyState.svelte';
	import WorkspaceGroupsDialog from '$lib/components/calendar/WorkspaceGroupsDialog.svelte';
	import DailyCountsChart from '$lib/components/charts/DailyCountsChart.svelte';
	import ContentError from '$lib/components/common/ContentError.svelte';
	import AgendaDayList from '$lib/components/calendar/AgendaDayList.svelte';
	import AgendaWeek from '$lib/components/calendar/AgendaWeek.svelte';
	import CommitmentList from '$lib/components/calendar/CommitmentList.svelte';
	import ForecastDialog from '$lib/components/calendar/ForecastDialog.svelte';
	import MoveImpactDialog from '$lib/components/calendar/MoveImpactDialog.svelte';
	import ResolveOccurrenceDialog from '$lib/components/calendar/ResolveOccurrenceDialog.svelte';
	import type { AgendaEntry, AgendaResponse, ScheduleResolveState, CommitmentMoveResult, WorkspaceGroup, Workspace } from '$lib/types';

	const staleCopy = 'This occurrence changed. Review the updated entry and try again.';
	const permissionCopy = 'You can no longer change this item.';
	const genericCopy = 'Could not update this occurrence. Try again.';
	const preferencePrefix = 'pad-agenda-show-events:';
	const zone = $derived(authStore.user?.timezone || browserZone() || 'UTC');
	let selectedDay = $state('');
	const day = $derived(selectedDay || todayIn(zone));
	let mode = $state<'week' | 'day'>('week');
	let mobile = $state(false);
	let surface = $state<'agenda' | 'list'>('agenda');
	let periodHeading: HTMLHeadingElement | undefined;
	let showEvents = $state(true);
	let data = $state<AgendaResponse | null>(null);
	let loading = $state(true);
	let updating = $state(false);
	let loadError = $state(false);
	let savedRefreshError = $state(false);
	let actionError = $state('');
	let announcement = $state('');
	let pending = $state<Record<string, boolean>>({});
	let saved = $state<Record<string, boolean>>({});
	let forecastEntry = $state<AgendaEntry | null>(null);
	let dialogEntry = $state<AgendaEntry | null>(null);
	let dialogMode = $state<'snooze' | 'move'>('snooze');
	let dragging = $state<AgendaEntry | null>(null);
	let dragImpact = $state<CommitmentMoveResult | null>(null);
	let linkedTitles = $state<Record<string, string>>({});
	let review = $state<{ entry: AgendaEntry; to: string; preview: CommitmentMoveResult; reason?: string; sameIdentity: () => boolean } | null>(null);
	let committing = $state(false);
	let workspaceGroups = $state<WorkspaceGroup[] | null>(null);
	let workspaces = $state<Workspace[] | null>(null);
	let groupsReady = $state(false);
	let selectedGroup = $state('all');
	let manageGroups = $state(false);
	let groupsTicket = 0;
	let workspacesTicket = 0;
	let ticket = 0;
	// What the agenda request depends on: whether groups loaded, the selected id and its members.
	// A primitive, so a refetched groups array that changes neither (e.g. renaming another group) does not reload.
	const groupKey = $derived(workspaceGroups
		? `${selectedGroup}:${workspaceGroups.find(group => group.id === selectedGroup)?.workspace_ids.join(',') ?? ''}`
		: '');
	const emptyGroup = $derived(workspaces !== null && !!workspaceGroups?.some(group =>
		group.id === selectedGroup && group.kind === 'named' && !group.workspace_ids.some(id => workspaces!.some(workspace => workspace.id === id))));
	let loadedUser: string | undefined;
	let preferenceUser: string | undefined;
	let dragSerial = 0;
	let hoverDay = '';
	const dragCache = new Map<string, { to: string; promise: Promise<CommitmentMoveResult> }>();
	const entries = $derived(filterEntries(data?.entries ?? [], showEvents));
	const groups = $derived(groupEntries(entries));
	const incomplete = $derived(!!(data?.window_complete === false || data?.truncated || data?.unavailable?.length || data?.unexpanded?.length || data?.next_from));
	const status = $derived(loading ? 'Loading agenda…' : updating ? 'Updating agenda…' : announcement);

	onMount(() => {
		const query = window.matchMedia('(max-width: 768px)');
		mobile = query.matches;
		const update = (event: MediaQueryListEvent) => { mobile = event.matches; };
		query.addEventListener('change', update);
		return () => query.removeEventListener('change', update);
	});
	$effect(() => {
		const userId = authStore.user?.id;
		untrack(() => {
			preferenceUser = userId;
			let value = true;
			let savedMode: 'week' | 'day' = 'week';
			if (userId) {
				try {
					value = localStorage.getItem(`${preferencePrefix}${userId}`) !== '0';
					savedMode = localStorage.getItem(`pad-agenda-mode:${userId}`) === 'day' ? 'day' : 'week';
				} catch { /* Storage is optional. */ }
			}
			showEvents = value; mode = savedMode;
		});
	});
	function setMode(value: 'week' | 'day') {
		clearDrag(); mode = value;
		const userId = authStore.user?.id;
		if (!userId || userId !== preferenceUser) return;
		try { localStorage.setItem(`pad-agenda-mode:${userId}`, value); } catch { /* Keep the in-memory choice. */ }
	}
	function setShowEvents(value: boolean) {
		showEvents = value;
		const userId = authStore.user?.id;
		if (!userId || userId !== preferenceUser) return;
		try { localStorage.setItem(`${preferencePrefix}${userId}`, value ? '1' : '0'); } catch { /* Keep the in-memory choice. */ }
	}
	function setGroup(id: string) {
		selectedGroup = id;
		const userId = authStore.user?.id;
		if (!userId || userId !== preferenceUser) return;
		try { localStorage.setItem(`pad-agenda-group:${userId}`, id); } catch { /* Keep the in-memory choice. */ }
	}
	async function loadGroupContext() {
		const mine = ++groupsTicket;
		const sameIdentity = authStore.identityFence();
		const userId = authStore.user?.id;
		const current = () => mine === groupsTicket && sameIdentity();
		workspaceGroups = null; workspaces = null; groupsReady = false; selectedGroup = 'all'; manageGroups = false;
		if (!userId) { groupsReady = true; return; }
		let remembered = 'all';
		try { remembered = localStorage.getItem(`pad-agenda-group:${userId}`) || 'all'; } catch { /* Storage is optional. */ }
		// The workspace list only feeds the empty-group check and the dialog, so the agenda never waits on it.
		void loadWorkspaces();
		try {
			const response = await api.workspaceGroups.list();
			if (!current()) return;
			workspaceGroups = response.groups;
			selectedGroup = workspaceGroups.some(group => group.id === remembered) ? remembered : 'all';
		} catch { /* No picker; the agenda loads without a group. */ }
		if (current()) groupsReady = true;
	}
	// Refetched with the groups after each write, so members added elsewhere are known before the empty-group check reads them.
	async function loadWorkspaces() {
		const mine = ++workspacesTicket;
		const sameIdentity = authStore.identityFence();
		try {
			const response = await api.workspaces.list();
			if (mine === workspacesTicket && sameIdentity()) workspaces = response;
		} catch { /* Keep the last list; with none, the empty-group check is skipped. */ }
	}
	async function refreshGroups(resetGroup?: string) {
		const mine = ++groupsTicket;
		const sameIdentity = authStore.identityFence();
		const current = () => mine === groupsTicket && sameIdentity();
		void loadWorkspaces();
		try {
			const response = await api.workspaceGroups.list();
			if (!current()) return;
			if (selectedGroup === resetGroup || !response.groups.some(group => group.id === selectedGroup)) setGroup('all');
			workspaceGroups = response.groups;
		} catch {
			if (current() && selectedGroup === resetGroup) setGroup('all');
		}
	}
	$effect(() => {
		authStore.user?.id; authStore.identityEpoch;
		untrack(() => { void loadGroupContext(); });
	});

	async function load(date: string, timeZone: string, quiet = false): Promise<boolean | undefined> {
		const mine = ++ticket;
		const viewMode = mode;
		const requestedGroup = selectedGroup;
		const sameIdentity = authStore.identityFence();
		const userId = authStore.user?.id;
		const current = () => mine === ticket && sameIdentity();
		await Promise.resolve();
		if (!current()) return;
		if (loadedUser !== userId) {
			data = null; pending = {}; saved = {}; dialogEntry = null; forecastEntry = null; actionError = ''; announcement = '';
			review = null; committing = false; linkedTitles = {}; clearDrag(); loadedUser = userId;
		}
		if (!userId) { data = null; loading = false; updating = false; return; }
		loading = !quiet; updating = quiet; loadError = false;
		if (!quiet) { data = null; savedRefreshError = false; announcement = ''; }
		try {
			const { from, to } = viewMode === 'week' ? weekWindow(date, timeZone) : dayWindow(date, timeZone);
			const response = await (workspaceGroups ? api.getMyAgenda(from, to, timeZone, requestedGroup) : api.getMyAgenda(from, to, timeZone));
			if (!current()) return;
			data = response; saved = {}; savedRefreshError = false;
			return true;
		} catch (error) {
			if (!current()) return;
			if ((error as { code?: string })?.code === 'not_found' && workspaceGroups?.some(group => group.id === requestedGroup && group.kind === 'named')) {
				await refreshGroups(requestedGroup);
				return;
			}
			if (emptyGroup) { data = null; savedRefreshError = false; return true; }
			loadError = true; return false;
		} finally {
			if (current()) { loading = false; updating = false; }
		}
	}
	$effect(() => {
		const date = day;
		const timeZone = zone;
		mode; authStore.user?.id; authStore.identityEpoch; groupKey;
		const ready = groupsReady;
		untrack(() => {
			clearDrag();
			if (ready) void load(date, timeZone);
			else { ticket++; data = null; loading = true; updating = false; loadError = false; }
		});
	});
	$effect(() => () => { ticket++; groupsTicket++; workspacesTicket++; dragSerial++; });
	function today() {
		clearDrag();
		const next = todayIn(zone);
		if (day === next) void load(day, zone);
		else selectedDay = next;
	}
	function action(entry: AgendaEntry, state: AgendaAction) {
		if (pending[entryKey(entry)] || saved[entryKey(entry)] || !canResolve(entry)) return;
		actionError = '';
		if (state === 'forecast') { forecastEntry = entry; return; }
		if (state === 'today') { if (canMove(entry)) void moveTarget(entry, targetOnDay(entry, todayIn(zone), zone)); return; }
		if (state === 'move' && !canMove(entry)) return;
		if (state === 'move' || state === 'snooze') { dialogMode = state; dialogEntry = entry; }
		else void resolve(entry, state);
	}
	async function resolve(entry: AgendaEntry, state: Exclude<ScheduleResolveState, 'move'>, until?: string, comment?: string) {
		const key = entryKey(entry);
		if (pending[key] || saved[key] || !canResolve(entry)) return;
		const sameIdentity = authStore.identityFence();
		pending = { ...pending, [key]: true };
		actionError = ''; announcement = '';
		let viewing = true;
		try {
			const view = await api.getCommitment(entry.workspace!, entry.item_ref);
			if (!sameIdentity()) return;
			if (view.ended_at || !view.next_occurrence || !entry.wall || view.next_occurrence.wall !== entry.wall) {
				actionError = staleCopy;
				await load(day, zone, true);
				return;
			}
			viewing = false;
			await api.resolveCommitment(entry.workspace!, entry.item_ref, {
				occurrence_at: view.next_occurrence.at, state,
				...(until !== undefined ? { until } : {}),
				...(comment !== undefined ? { comment } : {})
			});
			if (!sameIdentity()) return;
			saved = { ...saved, [key]: true };
			if (dialogEntry && entryKey(dialogEntry) === key) dialogEntry = null;
			const refreshed = await load(day, zone, true);
			if (!sameIdentity()) return;
			if (refreshed === false) {
				savedRefreshError = true;
				loadError = false;
			} else {
				announcement = { done: 'Occurrence marked done.', skip: 'Occurrence skipped.', snooze: 'Occurrence snoozed.' }[state];
			}
		} catch (error) {
			if (!sameIdentity()) return;
			const code = (error as { code?: string })?.code;
			const stale = code === 'stale' || (viewing && code === 'not_found');
			const forbidden = code === 'forbidden' || code === 'permission_denied';
			const message = (error as { message?: unknown })?.message;
			// The server's own sentence says who may skip before a hard deadline; the entry is unchanged, so no reload.
			const ownerRequired = code === 'deadline_owner_required' && typeof message === 'string' && message !== '';
			actionError = ownerRequired ? message : stale ? staleCopy : forbidden ? permissionCopy : genericCopy;
			if (stale || forbidden) await load(day, zone, true);
		} finally {
			if (sameIdentity()) pending = { ...pending, [key]: false };
		}
	}
	// A move to the date the target already has changes nothing: the server returns no root change for it.
	const unchanged = (entry: AgendaEntry, to: string) => sameTarget(entry.roles?.target ?? entry.at, to, zone);
	function clearDrag() {
		dragSerial++; dragging = null; dragImpact = null; hoverDay = ''; dragCache.clear();
	}
	function startDrag(entry: AgendaEntry, event: DragEvent) {
		if (!canMove(entry) || pending[entryKey(entry)] || saved[entryKey(entry)]) { event.preventDefault(); return; }
		clearDrag(); dragging = entry;
		if (event.dataTransfer) { event.dataTransfer.effectAllowed = 'move'; event.dataTransfer.setData('text/plain', entry.item_ref); }
	}
	async function loadTitles(entry: AgendaEntry, preview: CommitmentMoveResult) {
		const sameIdentity = authStore.identityFence();
		const ids = [...new Set([...preview.changes, ...(preview.violations ?? []), ...(preview.left_in_place ?? [])].filter(change => !change.root && !(change.item_id in linkedTitles)).map(change => change.item_id))];
		if (!ids.length) return;
		const titles = await Promise.all(ids.map(async id => {
			try { const item = await api.items.get(entry.workspace!, id); return [id, item.title || 'Linked item'] as const; }
			catch { return [id, 'Linked item'] as const; }
		}));
		if (sameIdentity()) linkedTitles = { ...linkedTitles, ...Object.fromEntries(titles) };
	}
	async function enterDay(date: string) {
		const entry = dragging;
		if (!entry) return;
		if (date === hoverDay) return; // dragenter also fires on children inside the same day
		if (unchanged(entry, targetOnDay(entry, date, zone))) { hoverDay = date; dragImpact = null; return; }
		const serial = dragSerial;
		const sameIdentity = authStore.identityFence();
		hoverDay = date; dragImpact = null;
		let cached = dragCache.get(date);
		if (!cached) {
			const to = targetOnDay(entry, date, zone);
			cached = { to, promise: api.previewCommitmentMove(entry.workspace!, entry.item_ref, { role: 'target', to }) };
			dragCache.set(date, cached);
		}
		try {
			const preview = await cached.promise;
			if (serial !== dragSerial || !sameIdentity() || hoverDay !== date) return;
			dragImpact = preview; void loadTitles(entry, preview);
		} catch { /* The drop flow reports preview errors without retrying this request. */ }
	}
	function dropDay(date: string) {
		const entry = dragging;
		if (!entry) return;
		hoverDay = date;
		const to = targetOnDay(entry, date, zone);
		if (!unchanged(entry, to)) void moveTarget(entry, to);
		clearDrag();
	}
	async function moveError(error: unknown) {
		const code = (error as { code?: string })?.code;
		const stale = code === 'commitment_changed' || code === 'stale';
		const forbidden = code === 'forbidden' || code === 'permission_denied' || code === 'link_edit_required';
		const message = (error as { message?: unknown })?.message;
		const serverMessage = ['deadline_approval_required', 'deadline_owner_required', 'schedule_linked'].includes(code ?? '') && typeof message === 'string' && message.trim();
		actionError = stale ? staleCopy : forbidden ? permissionCopy : serverMessage ? message as string : genericCopy;
		if (stale || forbidden) { dialogEntry = null; review = null; await load(day, zone, true); }
	}
	async function focusMoved(entry: AgendaEntry) {
		await tick();
		const button = [...document.querySelectorAll<HTMLButtonElement>('button[data-agenda-actions]')].find(button => button.dataset.agendaActions === entry.item_ref && button.dataset.workspace === entry.workspace && button.dataset.owner === entry.owner_username);
		(button && !button.disabled ? button : periodHeading)?.focus();
	}
	async function commitTarget(entry: AgendaEntry, to: string, preview: CommitmentMoveResult, sameIdentity: () => boolean, reason?: string, acknowledge?: boolean) {
		if (!sameIdentity()) return;
		const key = entryKey(entry);
		const reviewed = review && entryKey(review.entry) === key;
		if (reviewed) committing = true;
		try {
			await api.moveCommitment(entry.workspace!, entry.item_ref, { role: 'target', to, revision: preview.revision, ...(reason ? { reason } : {}), ...(acknowledge ? { acknowledge_violations: true } : {}) });
			if (!sameIdentity()) return;
			saved = { ...saved, [key]: true };
			if (dialogEntry && entryKey(dialogEntry) === key) dialogEntry = null;
			if (review && entryKey(review.entry) === key) review = null;
			const refreshed = await load(day, zone, true);
			if (!sameIdentity()) return;
			if (refreshed === false) { savedRefreshError = true; loadError = false; }
			else announcement = `Occurrence moved to ${dateTime(to, zone, isAllDay(entry))}. Its reminder follows the target.`;
			pending = { ...pending, [key]: false };
			await focusMoved(entry);
		} catch (error) {
			if (sameIdentity()) await moveError(error);
		} finally {
			if (sameIdentity()) { if (reviewed) committing = false; pending = { ...pending, [key]: false }; }
		}
	}
	async function moveTarget(entry: AgendaEntry, to: string, reason?: string) {
		const key = entryKey(entry);
		if (!canMove(entry) || pending[key] || saved[key] || review) return;
		if (unchanged(entry, to)) { if (dialogEntry && entryKey(dialogEntry) === key) dialogEntry = null; return; }
		const sameIdentity = authStore.identityFence();
		const cached = dragging && entryKey(dragging) === key ? dragCache.get(hoverDay) : undefined;
		pending = { ...pending, [key]: true }; actionError = ''; announcement = '';
		let reviewing = false;
		try {
			const preview = await (cached?.to === to ? cached.promise : api.previewCommitmentMove(entry.workspace!, entry.item_ref, { role: 'target', to, ...(reason ? { reason } : {}) }));
			if (!sameIdentity()) return;
			const root = preview.changes.find(change => change.root);
			if (!root?.from || !unchanged(entry, root.from)) {
				actionError = staleCopy; dialogEntry = null; await load(day, zone, true); return;
			}
			if (preview.renegotiates_deadline || preview.violations?.length) {
				dialogEntry = null;
				review = { entry, to, preview, reason, sameIdentity }; reviewing = true;
				void loadTitles(entry, preview);
			} else await commitTarget(entry, to, preview, sameIdentity, reason);
		} catch (error) {
			if (sameIdentity()) await moveError(error);
		} finally {
			if (sameIdentity() && !reviewing) pending = { ...pending, [key]: false };
		}
	}
	function cancelReview() {
		if (!review || committing) return;
		pending = { ...pending, [entryKey(review.entry)]: false }; review = null; actionError = '';
	}
</script>

<svelte:head><title>Agenda</title></svelte:head>
<svelte:window onkeydown={event => { if (event.key === 'Escape') clearDrag(); }} />
<PageHeader title="Agenda" description="Commitments across your workspaces.">
	<div class="controls">
		{#if workspaceGroups}
			<label>Workspace group
				<select value={selectedGroup} onchange={event => setGroup(event.currentTarget.value)}>
					{#each workspaceGroups as group (group.id)}<option value={group.id}>{group.name}</option>{/each}
				</select>
			</label>
			<Button size="sm" onclick={() => manageGroups = true}>Manage groups</Button>
		{/if}
		<div class="mode">
			<Button size="sm" aria-pressed={mode === 'week'} onclick={() => setMode('week')}>Week</Button>
			<Button size="sm" aria-pressed={mode === 'day'} onclick={() => setMode('day')}>Day</Button>
		</div>
		<div class="period">
			<Button size="sm" aria-label={mode === 'week' ? 'Previous week' : 'Previous day'} onclick={() => { clearDrag(); selectedDay = shiftDay(day, mode === 'week' ? -7 : -1); }}>&lt;</Button>
			<Button size="sm" aria-label={mode === 'week' ? 'Next week' : 'Next day'} onclick={() => { clearDrag(); selectedDay = shiftDay(day, mode === 'week' ? 7 : 1); }}>&gt;</Button>
			<Button size="sm" onclick={today}>Today</Button>
			<h2 class="period-heading" tabindex="-1" bind:this={periodHeading}>{mode === 'week' ? weekLabel(day) : dayLabel(day)}</h2>
		</div>
		<a href="/console/settings">{zone}</a>
		<label><input type="checkbox" checked={showEvents} onchange={event => setShowEvents(event.currentTarget.checked)} /> Show created and completed</label>
	</div>
</PageHeader>

<div role="status" aria-live="polite" aria-atomic="true">{status}</div>
{#if actionError && !dialogEntry && !review}<p role="alert">{actionError}</p>{/if}
{#if !emptyGroup && savedRefreshError}
	<p role="alert">Saved. Could not refresh the view.</p>
	<Button onclick={() => load(day, zone, true)}>Try again</Button>
{:else if !emptyGroup && loadError}
	<ContentError title="Could not load agenda." />
	<Button onclick={() => load(day, zone, !!data)}>Try again</Button>
{/if}
{#if !emptyGroup && incomplete}
	<p>Agenda is incomplete.</p>
	{#if data?.next_from}<p>more after {localDate(data.next_from, zone)}</p>{/if}
{/if}
{#if emptyGroup}
	<EmptyState message="No workspaces in this group.">
		{#snippet actions()}<Button onclick={() => manageGroups = true}>Manage groups</Button>{/snippet}
	</EmptyState>
{:else if mode === 'week'}
	{#if !showEvents && data && entries.length === 0 && data.entries.length > 0}
		<p>Created and completed entries are hidden.</p>
		<Button onclick={() => setShowEvents(true)}>Show created and completed</Button>
	{/if}
	{#if mobile}
		<div class="surface-switch">
			<Button size="sm" aria-pressed={surface === 'agenda'} onclick={() => surface = 'agenda'}>Agenda</Button>
			<Button size="sm" aria-pressed={surface === 'list'} onclick={() => surface = 'list'}>List</Button>
		</div>
	{/if}
	<div class="week-layout">
		{#if !mobile || surface === 'agenda'}
			<div class="week-surface">
				<AgendaWeek start={day} {entries} {zone} complete={!incomplete && !!data} {pending} {saved} {dragging} impact={dragImpact} titles={linkedTitles}
					onaction={action} ondragstart={startDrag} ondragend={clearDrag} onenter={enterDay} ondrop={dropDay} />
			</div>
		{/if}
		{#if data && (!mobile || surface === 'list')}<CommitmentList {data} {zone} {pending} {saved} onaction={action} />{/if}
	</div>
{:else}
	{#each groups as group (group.day)}
		<AgendaDayList day={group.day} entries={group.entries} {zone} {pending} {saved} onaction={action} />
	{/each}
	{#if !loading && !loadError && !savedRefreshError && !incomplete && entries.length === 0 && data}
		{#if !showEvents && data.entries.length > 0}
			<p>Created and completed entries are hidden.</p>
			<Button onclick={() => setShowEvents(true)}>Show created and completed</Button>
		{:else}<p>No entries on this day.</p>{/if}
	{/if}
{/if}
{#if !emptyGroup}
	{#if data?.counts_truncated}
		<p>Counts unavailable for this window: too much activity to count.</p>
	{:else if data?.counts?.length}
		<DailyCountsChart data={data.counts} />
	{/if}
{/if}
{#if manageGroups && workspaceGroups}
	<WorkspaceGroupsDialog groups={workspaceGroups} {workspaces} onclose={() => manageGroups = false} onchanged={refreshGroups} />
{/if}
<ForecastDialog open={!!forecastEntry} entry={forecastEntry} {zone} {permissionCopy}
 onclose={() => forecastEntry = null}
 onsaved={() => { forecastEntry = null; void load(day, zone, true); }} />
<ResolveOccurrenceDialog open={!!dialogEntry} mode={dialogMode} {zone}
	pending={dialogEntry ? !!pending[entryKey(dialogEntry)] : false} error={actionError}
	onclose={() => dialogEntry = null}
	onsubmit={(until, comment) => { if (dialogEntry) { if (dialogMode === 'move') void moveTarget(dialogEntry, until, comment || undefined); else void resolve(dialogEntry, 'snooze', until, comment); } }} />
{#if review}
	<MoveImpactDialog entry={review.entry} preview={review.preview} {zone} titles={linkedTitles} initialReason={review.reason}
		pending={committing} error={actionError} onclose={cancelReview}
		onsubmit={(reason, acknowledge) => { if (review && !committing) void commitTarget(review.entry, review.to, review.preview, review.sameIdentity, reason, acknowledge); }} />
{/if}

<style>
	.controls { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-3); margin-top: var(--space-4); }
	.period { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-2); }
	.mode, .surface-switch { display: flex; gap: var(--space-2); }
	.surface-switch { margin: var(--space-3) 0; }
	.period-heading { font-size: 1rem; margin: 0; }
	.week-layout { display: grid; grid-template-columns: minmax(0, 1fr) minmax(15rem, 20rem); gap: var(--space-4); margin-top: var(--space-3); align-items: start; }
	.week-surface { min-width: 0; }
	.controls a { color: var(--text-secondary); overflow-wrap: anywhere; }
	label { display: flex; align-items: center; gap: var(--space-2); color: var(--text-primary); }
	input { flex-shrink: 0; }
	select { max-width: 100%; min-width: 0; padding: var(--space-2); color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); font: inherit; }
	[role="status"] { color: var(--text-secondary); }
	@media (max-width: 768px) { .week-layout { grid-template-columns: minmax(0, 1fr); } }
	@media (max-width: 767px) { .controls { align-items: flex-start; flex-direction: column; } }
</style>
