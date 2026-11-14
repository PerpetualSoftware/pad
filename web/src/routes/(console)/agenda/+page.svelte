<script lang="ts">
	import { onMount, tick, untrack } from 'svelte';
	import { afterNavigate, goto } from '$app/navigation';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';
	import { currentHref, currentSearch } from '$lib/utils/pageLocation';
	import { audienceCounts, defaultAudience, filterAudience, parseAudience, splitAudience, type AudienceView } from '$lib/calendar/audience';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { browserZone } from '$lib/timezone';
	import { agendaRows, attentionEntries, endedKey, roleDay, roleMarkers, canAcknowledge, canMove, canResolve, isOpenState, outstandingMatches, workState, dateTime, dayLabel, daySummary, dayWindow, longDay, weekLabel, weekWindow, targetOnDay, isAllDay, sameTarget, entryKey, filterEntries, groupEntries, isCompleted, localTime, shiftDay, shortDay, shortRoleDate, todayIn, workOccurrence, type AgendaAction } from '$lib/calendar/presentation';
	import Button from '$lib/components/common/Button.svelte';
	import SegmentedControl from '$lib/components/common/SegmentedControl.svelte';
	import EmptyState from '$lib/components/common/EmptyState.svelte';
	import WorkspaceGroupsDialog from '$lib/components/calendar/WorkspaceGroupsDialog.svelte';
	import { attentionFirst, dayLabel as calendarDayLabel, entryCount, monthLabel, monthWindow, moveMonth } from '$lib/calendar/monthGrid';
	import MonthGrid from '$lib/components/calendar/MonthGrid.svelte';
	import CalendarEntry from '$lib/components/calendar/CalendarEntry.svelte';
	import AgendaAttention from '$lib/components/calendar/AgendaAttention.svelte';
	import AgendaDayDialog from '$lib/components/calendar/AgendaDayDialog.svelte';
	import AgendaCompleted from '$lib/components/calendar/AgendaCompleted.svelte';
	import AgendaDayGroup from '$lib/components/calendar/AgendaDayGroup.svelte';
	import AgendaRow from '$lib/components/calendar/AgendaRow.svelte';
	import AgendaToolbar from '$lib/components/calendar/AgendaToolbar.svelte';
	import AgendaWeekStrip from '$lib/components/calendar/AgendaWeekStrip.svelte';
	import ForecastDialog from '$lib/components/calendar/ForecastDialog.svelte';
	import MoveImpactDialog from '$lib/components/calendar/MoveImpactDialog.svelte';
	import OutstandingPanel from '$lib/components/calendar/OutstandingPanel.svelte';
	import ResolveOccurrenceDialog from '$lib/components/calendar/ResolveOccurrenceDialog.svelte';
	import ScheduleEndDialog, { type ScheduleEndChoice } from '$lib/components/calendar/ScheduleEndDialog.svelte';
	import WeekSummary from '$lib/components/calendar/WeekSummary.svelte';
	import type { AgendaEntry, AgendaForecast, AgendaResponse, AgendaWorkEntry, ItemStillOpenDetails, ScheduleResolveState, CommitmentMoveResult, WorkspaceGroup, Workspace } from '$lib/types';

	const staleCopy = 'This occurrence changed. Review the updated entry and try again.';
	const permissionCopy = 'You can no longer change this item.';
	const genericCopy = 'Could not update this occurrence. Try again.';
	const notFiredCopy = 'This occurrence has not fired yet, so there is nothing to acknowledge.';
	const preferencePrefix = 'pad-agenda-completed:';
	const zone = $derived(authStore.user?.timezone || browserZone() || 'UTC');
	let selectedDay = $state('');
	const day = $derived(selectedDay || todayIn(zone));
	const isAgent = false;
	let audience = $state<AudienceView>(defaultAudience(isAgent));
	function readAudience() { audience = parseAudience(new URLSearchParams(currentSearch()).get('audience'), isAgent); }
	onMount(readAudience);
	afterNavigate(navigation => { if (!navigation.shallow) readAudience(); });
	function setAudience(value: AudienceView) {
		if (chatSurface.closeThen(() => setAudience(value))) return;
		audience = value;
		const url = new URL(currentHref());
		if (value === defaultAudience(isAgent)) url.searchParams.delete('audience');
		else url.searchParams.set('audience', value);
		void goto(url, { replace: true, reset: false });
	}
	let mode = $state<'week' | 'day' | 'month'>('week');
	const month = $derived(day.slice(0, 7));
	let mobile = $state(false);
	const periodLabel = $derived(mode === 'month' ? monthLabel(month) : mode === 'week' ? weekLabel(day, mobile) : mobile ? longDay(day, 'short', 'short') : dayLabel(day));
	// One clock for every deadline and forecast pill, so an open page moves them from soon to passed.
	let now = $state(new Date(Date.now()));
	const today = $derived(todayIn(zone, now));
	// The strip holds still while Day range steps through it; it follows the selected day otherwise.
	let stripAnchor = $state('');
	// Past either end of the anchored week it pages by whole weeks, so it never creeps a day at a time.
	const stripStart = $derived(mode === 'day' && stripAnchor
		? shiftDay(stripAnchor, 7 * Math.floor((Date.parse(`${day}T00:00:00Z`) - Date.parse(`${stripAnchor}T00:00:00Z`)) / 604_800_000))
		: day);
	const stripDays = $derived(Array.from({ length: 7 }, (_, index) => shiftDay(stripStart, index)));
	let openedDay = $state<string | null>(null);
	let monthGrid = $state<{ cancelFocus: () => void }>();
	let lastFocusScope = '';
	let lastAnnouncedMonth = '';
	let surface = $state<'agenda' | 'list'>('agenda');
	// Where each phone surface was scrolled to when it was last left. Plain storage: nothing renders from it.
	const surfaceScroll = { agenda: 0, list: 0 };
	const surfaceId = $props.id();
	let periodHeading = $state<HTMLHeadingElement>();
	let loadingRest = $state(false);
	// Whether "Load the rest" was asked for in the window on screen; a quiet reload of that window then pages on too.
	let wantRest = false;
	const REST_PAGES = 10;
	let showCompleted = $state(true);
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
	// A done or skip the server refused because it would end an open item's schedule (TASK-4537).
	let endChoice = $state<{ entry: AgendaEntry; details: ItemStillOpenDetails; comment?: string } | null>(null);
	let endError = $state('');
	let dragging = $state<AgendaEntry | null>(null);
	let dragImpact = $state<CommitmentMoveResult | null>(null);
	let linkedTitles = $state<Record<string, string>>({});
	let review = $state<{ entry: AgendaEntry; to: string; preview: CommitmentMoveResult; reason?: string; sameIdentity: () => boolean } | null>(null);
	let committing = $state(false);
	let workspaceGroups = $state<WorkspaceGroup[] | null>(null);
	let workspaces = $state<Workspace[] | null>(null);
	let groupsReady = $state(false);
	let selectedGroup = $state('all');
	// The groups dialog, and the group whose edit step it opens at.
	let manageGroups = $state<{ edit?: string } | null>(null);
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
	let hoverDay = $state('');
	let toastedError = '';
	const dragCache = new Map<string, { to: string; promise: Promise<CommitmentMoveResult> }>();
	// Created entries never reach the page: creation is intake, not a commitment.
	const unfiltered = $derived(filterEntries(data?.entries ?? [], showCompleted));
	const audienceData = $derived(splitAudience(unfiltered, data?.priority_list ?? [], audience));
	const entries = $derived(audienceData.entries);
	const counts = $derived(audienceCounts(data?.entries ?? []));
	const groups = $derived(groupEntries(entries));
	const incomplete = $derived(!!(data?.window_complete === false || data?.truncated || data?.unavailable?.length || data?.unexpanded?.length || data?.next_from));
	const status = $derived(loading ? 'Loading agenda…' : updating ? 'Updating agenda…' : announcement);
	const selectedGroupName = $derived.by(() => {
		const group = workspaceGroups?.find(candidate => candidate.id === selectedGroup);
		return !group || group.kind === 'all' ? '' : group.kind === 'mine' ? 'My workspaces' : group.name;
	});

	// What needs attention is listed once, above the days, and left out of its own day's rows.
	const attention = $derived.by(() => {
		if (!data || mode === 'month') return [];
		return audienceData.attention.flatMap(item => {
			const row = item.entry as AgendaWorkEntry;
			const entry = item.inWindow ? item.entry as AgendaEntry
				: workOccurrence(row, zone) ?? (row.at && row.day ? { ...row, type: 'schedule' as const, state: workState(row), at: row.at, day: row.day } : null);
			return entry ? [{ key: item.key, entry, inWindow: item.inWindow }] : [];
		});
	});
	const lifted = $derived(new Set(attention.filter(item => item.inWindow).map(item => entryKey(item.entry))));
	const markers = $derived(new Map([...roleMarkers(unfiltered, zone, data?.forecasts ?? [], data?.next_from)].map(([date, roles]) => [date, { deadlines: filterAudience(roles.deadlines, audience), forecasts: filterAudience(roles.forecasts, audience) }])));
	const dayRows = (date: string) => groups.find(group => group.day === date)?.entries ?? [];
	function summary(date: string) {
		const deadlines = markers.get(date)?.deadlines.length ?? 0;
		return [daySummary(dayRows(date)), ...(deadlines ? [`${deadlines} deadline${deadlines === 1 ? '' : 's'}`] : [])].filter(Boolean).join(' · ');
	}
	const listDays = $derived(mode === 'week' ? stripDays : groups.map(group => group.day));
	const hiddenCompleted = $derived(!showCompleted && !!data && entries.length === 0 && filterAudience(data.entries, audience).some(isCompleted));
	const nothing = $derived(!!data && !loading && !loadError && !incomplete && !hiddenCompleted && entries.length === 0 && attention.length === 0);
	// What a drag over a day would do, in words.
	const dragNote = $derived.by(() => {
		if (!dragging || !hoverDay || unchanged(dragging, targetOnDay(dragging, hoverDay, zone))) return null;
		const linked = dragImpact?.changes.filter(change => !change.root).length ?? 0;
		const deadline = dragging.roles?.deadline;
		return {
			headline: `Moving “${dragging.item_title}” to ${shortDay(hoverDay, today)}${isAllDay(dragging) ? '' : `, ${localTime(dragging.at, zone)}`}.`,
			review: !!dragImpact?.renegotiates_deadline,
			detail: !dragImpact ? '' : [
				...(linked ? [`${linked} linked item${linked === 1 ? ' follows' : 's follow'}`] : []),
				...(deadline && !dragImpact.renegotiates_deadline ? [`deadline ${shortRoleDate(deadline, zone, now)} unchanged`] : [])
			].join(' · ')
		};
	});

	const windowOf = (viewMode: 'week' | 'day' | 'month', date: string, timeZone: string) =>
		viewMode === 'month' ? monthWindow(date.slice(0, 7), timeZone) : viewMode === 'week' ? weekWindow(date, timeZone) : dayWindow(date, timeZone);
	// next_from is a place to continue from only while it is before the window's end; at the end, the window was read through.
	const cursorIn = (next: string | undefined, to: string) => next && Date.parse(next) < Date.parse(to) ? next : null;
	const cursor = $derived(cursorIn(data?.next_from, windowOf(mode, day, zone).to));
	const nextDay = $derived(cursor ? roleDay(cursor, zone) : null);
	const monthEntries = $derived(entries.filter(entry => !nextDay || entry.day <= nextDay));
	// What needs attention in the month, by the derivation the week's band uses.
	const monthAttention = $derived(new Set(data ? attentionEntries(unfiltered.filter(entry => !nextDay || entry.day <= nextDay), data.priority_list).filter(item => item.inWindow).map(item => entryKey(item.entry as AgendaEntry)) : []));
	const monthAttentionCounts = $derived(new Map(groupEntries(unfiltered.filter(entry => monthAttention.has(entryKey(entry)))).map(group => [group.day, group.entries.length])));
	// Each day's commitments, what needs attention first, so it is never behind "+N more".
	// Completed entries are never rows or counted: each day's fold into one line that ends its list.
	const monthCompleted = $derived(new Map(groupEntries(monthEntries.filter(isCompleted)).map(group => [group.day, group.entries])));
	const monthDays = $derived(new Map(groupEntries(monthEntries.filter(entry => !isCompleted(entry))).map(group => [group.day, attentionFirst(group.entries, entry => monthAttention.has(entryKey(entry)))])));
	// agendaRows leads with all-day rows, so the attention order is applied to its result.
	const monthRows = $derived(new Map([...monthDays].map(([date, rows]) => [date, attentionFirst(agendaRows(rows).map(row => ({ ...row, attention: monthAttention.has(row.key) })), row => row.attention)])));
	function monthCount(date: string) {
		const rows = monthDays.get(date) ?? [];
		const owed = rows.filter(entry => entry.type === 'schedule' && isOpenState(entry.state)).length;
		return rows.length ? `${rows.length}${owed ? ` · ${owed} owed` : ''}` : '';
	}
	function dayIncomplete(date: string) {
		return incomplete && (!nextDay || date >= nextDay || !!data?.unavailable?.length || !!data?.unexpanded?.length);
	}
	// How much of a strip day is on screen. Day range loads the selected day alone; a cut window stops on the next_from day.
	function stripLoad(date: string): 'full' | 'partial' | 'none' {
		if (!data || (mode === 'day' && date !== day)) return 'none';
		if (!dayIncomplete(date)) return 'full';
		return nextDay && date > nextDay ? 'none' : 'partial';
	}
	function monthCellLabel(date: string) {
		return `${calendarDayLabel(date, true)}${date === todayIn(zone) ? ', Today' : ''}, ${entryCount(monthRows.get(date)?.length ?? 0)}${monthAttentionCounts.get(date) ? `, ${monthAttentionLabel(monthAttentionCounts.get(date)!)}` : ''}${dayIncomplete(date) ? ', Incomplete' : ''}`;
	}
	function monthAttentionLabel(count: number) {
		return `${count} ${count === 1 ? 'needs' : 'need'} attention`;
	}
	async function setSurface(value: 'agenda' | 'list') {
		if (value === surface) return;
		surfaceScroll[surface] = window.scrollY;
		surface = value;
		await tick();
		window.scrollTo(0, surfaceScroll[value]);
	}
	function openDay(date: string) { selectedDay = date; openedDay = date; }
	function movePeriod(delta: number) {
		clearDrag();
		if (mode === 'day') stripAnchor = stripStart;
		selectedDay = mode === 'month' ? moveMonth(day, delta) : shiftDay(day, delta * (mode === 'week' ? 7 : 1));
	}
	async function dayAction(entry: AgendaEntry, state: AgendaAction) {
		const sameIdentity = authStore.identityFence();
		if (state === 'snooze' || state === 'move' || state === 'forecast') {
			openedDay = null; await tick();
		}
		if (sameIdentity()) action(entry, state);
	}

	onMount(() => {
		const query = window.matchMedia('(max-width: 768px)');
		mobile = query.matches;
		const update = (event: MediaQueryListEvent) => { mobile = event.matches; };
		query.addEventListener('change', update);
		const clock = setInterval(() => { now = new Date(Date.now()); }, 60_000);
		return () => { query.removeEventListener('change', update); clearInterval(clock); };
	});
	// An action error with no dialog open to show it goes to the toast container, once.
	$effect(() => {
		const message = actionError;
		// A dialog shows the error itself, and that counts as shown: closing the dialog does not repeat it.
		const shown = !!(dialogEntry || review || openedDay);
		untrack(() => {
			if (message && !shown && message !== toastedError) toastStore.show(message, 'error');
			toastedError = message;
		});
	});
	// The toast container is a polite live region, so a success is announced there and not in the page status as well.
	function succeed(message: string) {
		toastStore.show(message, 'success');
	}
	$effect(() => {
		const userId = authStore.user?.id;
		untrack(() => {
			preferenceUser = userId;
			let value = true;
			let savedMode: 'week' | 'day' | 'month' = 'week';
			if (userId) {
				try {
					value = localStorage.getItem(`${preferencePrefix}${userId}`) !== '0';
					const stored = localStorage.getItem(`pad-agenda-mode:${userId}`);
					savedMode = stored === 'day' || stored === 'month' ? stored : 'week';
				} catch { /* Storage is optional. */ }
			}
			showCompleted = value; mode = savedMode;
		});
	});
	function setMode(value: 'week' | 'day' | 'month') {
		clearDrag();
		if (value === 'day') stripAnchor = stripStart;
		mode = value;
		if (value !== 'month') lastAnnouncedMonth = '';
		const userId = authStore.user?.id;
		if (!userId || userId !== preferenceUser) return;
		try { localStorage.setItem(`pad-agenda-mode:${userId}`, value); } catch { /* Keep the in-memory choice. */ }
	}
	function setShowCompleted(value: boolean) {
		showCompleted = value;
		const userId = authStore.user?.id;
		if (!userId || userId !== preferenceUser) return;
		try { localStorage.setItem(`${preferencePrefix}${userId}`, value ? '1' : '0'); } catch { /* Keep the in-memory choice. */ }
	}
	async function pickDay(date: string) {
		if (mode === 'day') { clearDrag(); stripAnchor = stripStart; selectedDay = date; return; }
		await tick();
		const heading = document.querySelector<HTMLElement>(`[data-day-heading="${date}"]`);
		heading?.focus({ preventScroll: true });
		heading?.scrollIntoView?.({ block: 'start' });
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
		workspaceGroups = null; workspaces = null; groupsReady = false; selectedGroup = 'all'; manageGroups = null;
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
		if (!quiet) wantRest = false;
		const resume = quiet && wantRest;
		await Promise.resolve();
		if (!current()) return;
		if (loadedUser !== userId) {
			data = null; openedDay = null; lastAnnouncedMonth = ''; pending = {}; saved = {}; dialogEntry = null; forecastEntry = null; endChoice = null; endError = ''; actionError = ''; announcement = '';
			review = null; committing = false; linkedTitles = {}; clearDrag(); loadedUser = userId;
		}
		if (!userId) { data = null; loading = false; updating = false; return; }
		loading = !quiet; updating = quiet; loadError = false; loadingRest = false;
		if (!quiet) { data = null; savedRefreshError = false; announcement = ''; }
		else if (announcement === lastAnnouncedMonth) announcement = '';
		try {
			const { from, to } = windowOf(viewMode, date, timeZone);
			const read = (start: string) => workspaceGroups ? api.getMyAgenda(start, to, timeZone, requestedGroup, ['completed']) : api.getMyAgenda(start, to, timeZone, undefined, ['completed']);
			let response = await read(from);
			if (!current()) return;
			// The rest was on screen before this reload: page on before publishing, so its rows never drop out.
			for (let page = 0, next = resume && cursorIn(response.next_from, to); next && page < REST_PAGES; page++, next = cursorIn(response.next_from, to)) {
				let rest: AgendaResponse;
				// A page that fails leaves what was joined so far, and its next_from offers the rest again.
				try { rest = await read(next); } catch { break; }
				if (!current()) return;
				response = joinRest(response, rest);
			}
			data = response; saved = {}; savedRefreshError = false;
			if (!quiet && viewMode === 'month' && lastAnnouncedMonth !== monthLabel(date.slice(0, 7))) {
				lastAnnouncedMonth = monthLabel(date.slice(0, 7)); announcement = lastAnnouncedMonth;
			}
			return true;
		} catch (error) {
			if (!current()) return;
			if ((error as { code?: string })?.code === 'not_found' && workspaceGroups?.some(group => group.id === requestedGroup && group.kind === 'named')) {
				await refreshGroups(requestedGroup);
				return;
			}
			if (emptyGroup) { data = null; savedRefreshError = false; return true; }
			monthGrid?.cancelFocus(); loadError = true; return false;
		} finally {
			if (current()) { loading = false; updating = false; }
		}
	}
	$effect(() => {
		const date = mode === 'month' ? `${month}-01` : day;
		const timeZone = zone;
		mode; authStore.user?.id; authStore.identityEpoch; groupKey;
		const ready = groupsReady;
		const focusScope = JSON.stringify([mode, timeZone, authStore.user?.id, authStore.identityEpoch, groupKey]);
		untrack(() => {
			if (focusScope !== lastFocusScope) { monthGrid?.cancelFocus(); lastFocusScope = focusScope; }
			openedDay = null;
			clearDrag();
			if (ready) void load(date, timeZone);
			else { ticket++; wantRest = false; data = null; loading = true; updating = false; loadError = false; }
		});
	});
	$effect(() => () => { ticket++; groupsTicket++; workspacesTicket++; dragSerial++; });
	// The window stopped at next_from: ask for the remainder and join it to what is shown.
	function joinRest(shown: AgendaResponse, rest: AgendaResponse): AgendaResponse {
		const seen = new Set(shown.entries.map(entryKey));
		const forecastKey = (row: AgendaForecast) => JSON.stringify([row.workspace, row.owner_username, row.schedule_id]);
		const forecastSeen = new Set((shown.forecasts ?? []).map(forecastKey));
		return {
			...shown, entries: [...shown.entries, ...rest.entries.filter(entry => !seen.has(entryKey(entry)))],
			window_complete: rest.window_complete, truncated: rest.truncated, next_from: rest.next_from, events_truncated: shown.events_truncated || rest.events_truncated,
			unavailable: [...(shown.unavailable ?? []), ...(rest.unavailable ?? [])], unexpanded: [...(shown.unexpanded ?? []), ...(rest.unexpanded ?? [])],
			forecasts: [...(shown.forecasts ?? []), ...(rest.forecasts ?? []).filter(row => !forecastSeen.has(forecastKey(row)))]
		};
	}
	async function loadRest() {
		const from = cursor;
		if (!from || loadingRest) return;
		wantRest = true;
		const mine = ticket;
		const sameIdentity = authStore.identityFence();
		const current = () => mine === ticket && sameIdentity();
		const { to } = windowOf(mode, day, zone);
		loadingRest = true; actionError = '';
		try {
			const rest = await (workspaceGroups ? api.getMyAgenda(from, to, zone, selectedGroup, ['completed']) : api.getMyAgenda(from, to, zone, undefined, ['completed']));
			if (!current() || !data) return;
			data = joinRest(data, rest);
		} catch {
			if (current()) actionError = 'Could not load the rest. Try again.';
		} finally {
			if (current()) loadingRest = false;
		}
	}
	function goToday() {
		clearDrag();
		const next = todayIn(zone);
		if (day === next) void load(day, zone);
		else selectedDay = next;
	}
	// A lapsed occurrence is history, and the one thing it still takes is being recorded as done.
	const lapsed = (entry: AgendaEntry) => entry.type === 'schedule' && entry.state === 'lapsed';
	function action(entry: AgendaEntry, state: AgendaAction) {
		if (pending[entryKey(entry)] || saved[entryKey(entry)] || !(canResolve(entry) || (lapsed(entry) && state === 'done'))) return;
		actionError = '';
		if (state === 'forecast') { forecastEntry = entry; return; }
		if (state === 'today') { if (canMove(entry)) void moveTarget(entry, targetOnDay(entry, todayIn(zone), zone)); return; }
		if (state === 'move' && !canMove(entry)) return;
		if (state === 'ack' && !canAcknowledge(entry)) return;
		if (state === 'move' || state === 'snooze') { dialogMode = state; dialogEntry = entry; }
		else void resolve(entry, state);
	}
	// end: the caller confirmed ending the schedule of an open item. closeAs then closes the item with that value.
	async function resolve(entry: AgendaEntry, state: Exclude<ScheduleResolveState, 'move'>, until?: string, comment?: string, end?: { closeAs?: string }): Promise<boolean> {
		const key = entryKey(entry);
		const history = lapsed(entry) && state === 'done';
		if (pending[key] || saved[key] || !(canResolve(entry) || history)) return false;
		const sameIdentity = authStore.identityFence();
		pending = { ...pending, [key]: true };
		actionError = ''; announcement = '';
		let viewing = !history;
		try {
			// The outstanding occurrence is named by the instant the schedule holds now; a lapsed one by its own.
			let occurrenceAt = entry.at;
			if (!history) {
				const view = await api.getCommitment(entry.workspace!, entry.item_ref);
				if (!sameIdentity()) return false;
				if (!outstandingMatches(entry, view) || !view.next_occurrence) {
					actionError = staleCopy;
					await load(day, zone, true);
					return false;
				}
				occurrenceAt = view.next_occurrence.at;
			}
			viewing = false;
			const resolved = await api.resolveCommitment(entry.workspace!, entry.item_ref, {
				occurrence_at: occurrenceAt, state,
				...(until !== undefined ? { until } : {}),
				...(comment !== undefined ? { comment } : {}),
				...(end ? { end_schedule: true } : {})
			});
			if (!sameIdentity()) return false;
			saved = { ...saved, [key]: true };
			if (dialogEntry && entryKey(dialogEntry) === key) dialogEntry = null;
			// end_schedule has no effect when a concurrent edit gave the schedule another
			// occurrence: only a resolution that ended it with the item open closes the item.
			const ended = resolved?.resolution?.item_still_open === true;
			// The schedule has ended; closing the item is a second write that can be refused on its own.
			let closeError = '';
			let itemClosed = false;
			if (ended && end?.closeAs !== undefined && endChoice) {
				try {
					await api.items.update(entry.workspace!, entry.item_ref, { fields_patch: { [endChoice.details.done_field]: end.closeAs } });
					itemClosed = true;
				} catch (error) {
					const message = (error as { message?: unknown })?.message;
					closeError = `The schedule has ended, but closing the item failed: ${typeof message === 'string' && message ? message : 'try again'}. It stays open in Outstanding.`;
				}
				if (!sameIdentity()) return false;
			}
			const refreshed = await load(day, zone, true);
			if (!sameIdentity()) return false;
			if (closeError) {
				actionError = closeError;
			} else if (refreshed === false) {
				savedRefreshError = true;
				loadError = false;
			} else if (end && !ended && !resolved?.commitment?.ended_at) {
				const verb = state === 'skip' ? 'Occurrence skipped' : 'Occurrence marked done';
				succeed(end.closeAs !== undefined ? `${verb}. The schedule continues, so the item was not closed.` : `${verb}. The schedule continues.`);
			} else if (end && !ended) {
				// The schedule ended, but not with the item open: it was closed concurrently,
				// or its collection can no longer close it.
				succeed(`${state === 'skip' ? 'Occurrence skipped' : 'Occurrence marked done'}; the schedule has ended.`);
			} else if (end) {
				const verb = state === 'skip' ? 'Occurrence skipped' : 'Occurrence marked done';
				succeed(itemClosed ? `${verb}; the schedule has ended and the item is closed.` : `${verb}; the schedule has ended. The item stays open in Outstanding.`);
			} else {
				succeed({ done: 'Occurrence marked done.', skip: 'Occurrence skipped.', snooze: 'Occurrence snoozed.', ack: 'Acknowledged. The work stays outstanding.' }[state]);
			}
			return true;
		} catch (error) {
			if (!sameIdentity()) return false;
			const code = (error as { code?: string })?.code;
			// The last occurrence of an open item: nothing was written. Ask how to end it.
			if (code === 'item_still_open' && (state === 'done' || state === 'skip') && !end) {
				const details = (error as { details?: Partial<ItemStillOpenDetails> })?.details ?? {};
				endError = '';
				endChoice = { entry, comment, details: { state, occurrence_at: details.occurrence_at ?? '', done_field: details.done_field || 'status', item_status: details.item_status ?? '', close_options: details.close_options ?? [], ack_available: !!details.ack_available } };
				return false;
			}
			const stale = code === 'stale' || (viewing && code === 'not_found');
			// The alert this ack named was re-armed (snoozed or moved) before it landed: show the agenda as it is now.
			const notFired = state === 'ack' && code === 'not_fired';
			const forbidden = code === 'forbidden' || code === 'permission_denied';
			const message = (error as { message?: unknown })?.message;
			// The server's own sentence says who may skip before a hard deadline; the entry is unchanged, so no reload.
			const ownerRequired = code === 'deadline_owner_required' && typeof message === 'string' && message !== '';
			actionError = ownerRequired ? message : stale ? staleCopy : notFired ? notFiredCopy : forbidden ? permissionCopy : genericCopy;
			if (stale || notFired || forbidden) await load(day, zone, true);
			return false;
		} finally {
			if (sameIdentity()) pending = { ...pending, [key]: false };
		}
	}
	async function chooseEnd(choice: ScheduleEndChoice) {
		const asked = endChoice;
		if (!asked) return;
		const { entry, details, comment } = asked;
		endError = '';
		const done = choice.kind === 'ack'
			? await resolve(entry, 'ack')
			: await resolve(entry, details.state, undefined, comment, choice.kind === 'close' ? { closeAs: choice.value } : {});
		if (endChoice !== asked) return;
		if (done || actionError === staleCopy) endChoice = null;
		else { endError = actionError || genericCopy; actionError = ''; }
	}
	async function dismissEnded(row: AgendaWorkEntry) {
		const key = endedKey(row);
		if (pending[key] || !row.workspace) return;
		const sameIdentity = authStore.identityFence();
		pending = { ...pending, [key]: true };
		actionError = '';
		try {
			await api.dismissEndedCommitment(row.workspace, row.item_ref);
			if (!sameIdentity()) return;
			await load(day, zone, true);
			if (sameIdentity()) succeed(`Dismissed. ${row.item_ref} stays open.`);
		} catch (error) {
			if (!sameIdentity()) return;
			const code = (error as { code?: string })?.code;
			// It is live again (a timing edit revived it): show the agenda as it is now.
			if (code === 'not_ended' || code === 'not_found') { await load(day, zone, true); return; }
			actionError = code === 'forbidden' || code === 'permission_denied' ? permissionCopy : 'Could not dismiss this. Try again.';
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
			else succeed(`Occurrence moved to ${dateTime(to, zone, isAllDay(entry))}. Its reminder follows the target.`);
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
				if (openedDay) { openedDay = null; await tick(); if (!sameIdentity()) return; }
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
<AgendaToolbar {audience} {counts} onaudience={setAudience} {mode} period={periodLabel} {zone} {mobile} groups={workspaceGroups} {selectedGroup} {workspaces} {showCompleted} bind:heading={periodHeading}
	onmode={setMode} onmove={movePeriod} ontoday={goToday} ongroup={setGroup} onmanage={() => manageGroups = {}} onshowcompleted={setShowCompleted} />

<div class="sr-only" role="status" aria-live="polite" aria-atomic="true">{status}</div>
<div class="progress" class:busy={updating || loadingRest} aria-hidden="true"></div>

{#snippet failed()}
	<div class="card state" role="alert">
		<h3>Could not load this {mode}</h3>
		<p>The server did not answer.</p>
		<div class="acts">
			<Button variant="primary" onclick={() => load(mode === 'month' ? `${month}-01` : day, zone, !!data)}>Try again</Button>
			{#if mode === 'month' ? month !== today.slice(0, 7) : day !== today}<Button variant="ghost" onclick={goToday}>Back to this {mode}</Button>{/if}
		</div>
	</div>
{/snippet}

{#if !emptyGroup}
	{#if savedRefreshError}
		<div class="notice info">
			<span>Saved. Could not refresh the view.</span><span class="spacer"></span>
			<Button size="sm" onclick={() => load(day, zone, true)}>Try again</Button>
		</div>
	{/if}
	{#if incomplete}
		<div class="notice">
			{#if cursor && nextDay}
				<span><b>Showing entries up to {longDay(nextDay, 'short')}.</b> This {mode} has more than the agenda can load at once.</span><span class="spacer"></span>
				<Button size="sm" disabled={loadingRest} onclick={loadRest}>Load the rest</Button>
			{:else if data?.window_complete === false || data?.truncated}
				<span>Too much to load at once. Choose a smaller range or a workspace group.</span>
			{:else}
				<span>Some schedules could not be shown.</span>
			{/if}
		</div>
	{/if}
	{#if showCompleted && data?.events_truncated}
		<div class="notice info">
			<span>Some completed items in this {mode} aren't shown.</span>
		</div>
	{/if}
	{#if hiddenCompleted}
		<div class="notice info">
			<span>Completed work is hidden.</span><span class="spacer"></span>
			<Button size="sm" variant="ghost" onclick={() => setShowCompleted(true)}>Show it</Button>
		</div>
	{/if}
{/if}

{#if emptyGroup}
	<EmptyState title="No workspaces in this group." message="Add a workspace to see its commitments here.">
		{#snippet actions()}<Button variant="primary" onclick={() => manageGroups = { edit: selectedGroup }}>Add workspaces</Button>{/snippet}
	</EmptyState>
{:else if mode === 'month'}
	{#if loadError && !savedRefreshError}{@render failed()}{/if}
	<MonthGrid bind:this={monthGrid} {month} today={todayIn(zone)} selected={day} {mobile} loaded={data}
		cellLabel={monthCellLabel} heading={longDay} summary={(date) => dayIncomplete(date) ? 'Not fully loaded' : daySummary(monthDays.get(date) ?? [])}
		count={monthCount} empty={(date) => !dayIncomplete(date) && !monthDays.get(date)?.length && !monthCompleted.get(date)?.length && !monthAttentionCounts.get(date)}
		onopen={openDay} onchoose={(date) => selectedDay = date}>
		{#snippet cell(date: string, phone: boolean)}
			{#if monthAttentionCounts.get(date)}<span class="month-attention">{monthAttentionLabel(monthAttentionCounts.get(date)!)}</span>{/if}
			{#if phone}
				<!-- The phone month is the week's list: every entry of the day, with its actions. -->
				{#if monthDays.get(date)?.length || monthCompleted.get(date)?.length}
					<ul class="card month-day">
						{#each monthDays.get(date) ?? [] as entry (entryKey(entry))}
							<AgendaRow {audience} {isAgent} {entry} {zone} {now} compact pending={pending[entryKey(entry)]} saved={saved[entryKey(entry)]} onaction={action} />
						{/each}
						<AgendaCompleted {audience} {isAgent} completed={monthCompleted.get(date) ?? []} {zone} {now} compact onaction={action} />
					</ul>
				{/if}
			{:else}
				{#each (monthRows.get(date) ?? []).slice(0, 3) as row (row.key)}<CalendarEntry {audience} {isAgent} audienceEntry={monthEntries.find(entry => entryKey(entry) === row.key)} {row} {zone} preview keyboard={false} />{/each}
				{#if (monthRows.get(date)?.length ?? 0) > 3}<button class="more" tabindex="-1" onclick={() => openDay(date)}>+{monthRows.get(date)!.length - 3} more</button>{/if}
			{/if}
			{#if dayIncomplete(date)}<p class="incomplete-day">Not fully loaded</p>{/if}
		{/snippet}
	</MonthGrid>
	{#if nothing}<p class="quiet-note">No entries this month.</p>{/if}
	<AgendaDayDialog {audience} {isAgent} day={openedDay} today={todayIn(zone)} entries={openedDay ? monthDays.get(openedDay) ?? [] : []} completed={openedDay ? monthCompleted.get(openedDay) ?? [] : []} {zone} {now} {mobile}
		incomplete={!!openedDay && dayIncomplete(openedDay)} error={actionError} {pending} {saved} onclose={() => openedDay = null} onaction={dayAction} />
	{#if data && !(loadError && !savedRefreshError)}<div class="card below"><WeekSummary range="month" {data} {today} /></div>{/if}
{:else}
	{@const switching = mobile && mode === 'week'}
	{@const schedule = !switching || surface === 'agenda'}
	{#if switching}
		<div class="surface-switch">
			<SegmentedControl mode="tablist" label="Show" fill value={surface} onchange={setSurface}
				options={[
					{ value: 'agenda', label: 'Schedule', id: `${surfaceId}-tab-agenda`, controls: `${surfaceId}-surface` },
					{ value: 'list', label: 'Outstanding', count: data ? `${data.priority_list.length}${data.priority_list_complete ? '' : '+'}` : undefined, id: `${surfaceId}-tab-list`, controls: `${surfaceId}-surface` }
				]} />
		</div>
	{/if}
	<!-- On a phone in Week range this is the one panel the two tabs show; elsewhere it is a plain wrapper. -->
	<div id={`${surfaceId}-surface`} role={switching ? 'tabpanel' : undefined} aria-labelledby={switching ? `${surfaceId}-tab-${surface}` : undefined}>
		{#if schedule}
			<AgendaWeekStrip days={stripDays} {today} selected={mode === 'day' ? day : undefined} {entries} loadState={stripLoad} {zone} {now} compact={mobile}
				{dragging} {hoverDay} onselect={pickDay} onenter={enterDay} ondrop={dropDay} />
		{/if}
		<div class="layout">
			{#if schedule}
				<div class="main" aria-busy={loading ? 'true' : undefined}>
					{#if loading}
						{#each [[62, 48, 55], [70, 44, 58, 50], [52, 66]] as rows, index (index)}
							<div class="skeleton" aria-hidden="true">
								<i class="bar head"></i>
								<div class="card">{#each rows as width, at (at)}<div class="sk"><i class="bar"></i><i class="dot"></i><i class="bar" style:width={`${width}%`}></i><i class="bar"></i></div>{/each}</div>
							</div>
						{/each}
					{:else if loadError && !savedRefreshError}
						{@render failed()}
					{:else if nothing}
						<div class="card">
							<EmptyState icon="📅" title={`Nothing scheduled for ${mode === 'week' ? weekLabel(day) : longDay(day)}`}
								message={`No commitments or dated items${selectedGroupName ? ` in “${selectedGroupName}”` : ''} this ${mode}.`}>
								{#snippet actions()}
									{#if selectedGroupName}<Button onclick={() => setGroup('all')}>Show all workspaces</Button>{/if}
								{/snippet}
							</EmptyState>
						</div>
					{:else if data && (entries.length > 0 || attention.length > 0)}
						{#if attention.length}<AgendaAttention {audience} {isAgent} items={attention} {zone} {now} {pending} {saved} compact={mobile} onaction={action} />{/if}
						{#each listDays as date (date)}
							<AgendaDayGroup {audience} {isAgent} day={date} {today} entries={dayRows(date).filter(entry => !lifted.has(entryKey(entry)))} lifted={dayRows(date).filter(entry => lifted.has(entryKey(entry))).length} summary={summary(date)}
								deadlines={markers.get(date)?.deadlines} forecasts={markers.get(date)?.forecasts} {zone} {now} complete={!dayIncomplete(date)}
								{pending} {saved} compact={mobile} movable={mode === 'week' && !mobile} {dragging} hover={hoverDay === date} impact={dragImpact} titles={linkedTitles}
								onaction={action} ondragstart={startDrag} ondragend={clearDrag} onenter={enterDay} ondrop={dropDay} />
						{/each}
					{/if}
					{#if dragNote && !mobile}
						<div class="dragbar">
							<span class="what"><b>{dragNote.headline}</b>{#if dragNote.detail}{' '}{dragNote.detail}{/if}</span>
							{#if dragNote.review}<span class="review">Review needed: this move changes a hard deadline.</span>{/if}
							<span class="esc"><kbd>Esc</kbd> to cancel</span>
						</div>
					{/if}
				</div>
			{/if}
			<!-- On a phone in Week range the rail is the Outstanding surface; the schedule surface is the schedule alone. -->
			{#if !switching || surface === 'list'}
				<aside class="rail" class:single={mode === 'day'}>
					{#if loading}
						{#each (mode === 'week' ? [[70, 52], [64, 80, 58, 72]] : [[70, 52]]) as rows, index (index)}
							<div class="card rail-skeleton" aria-hidden="true">
								<i class="bar head"></i>
								{#each rows as width, at (at)}<i class="bar" style:width={`${width}%`}></i>{/each}
							</div>
						{/each}
					{:else if loadError && !savedRefreshError}
						<div class="card rail-note">Not loaded.</div>
					{:else if data}
						<div class="card"><WeekSummary range={mode} {data} {today} /></div>
						{#if mode === 'week'}<div class="card"><OutstandingPanel {data} {zone} {now} {pending} {saved} compact={mobile} onaction={action} ondismiss={dismissEnded} /></div>{/if}
					{/if}
				</aside>
			{/if}
		</div>
	</div>
{/if}
{#if manageGroups && workspaceGroups}
	<WorkspaceGroupsDialog groups={workspaceGroups} {workspaces} {mobile} initialEdit={manageGroups.edit} onclose={() => manageGroups = null} onchanged={refreshGroups} />
{/if}
<ForecastDialog open={!!forecastEntry} entry={forecastEntry} {zone} {mobile} {permissionCopy}
 onclose={() => forecastEntry = null}
 onsaved={() => { forecastEntry = null; void load(day, zone, true); }} />
<ResolveOccurrenceDialog open={!!dialogEntry} mode={dialogMode} entry={dialogEntry} {zone} {mobile}
	pending={dialogEntry ? !!pending[entryKey(dialogEntry)] : false} error={actionError}
	onclose={() => dialogEntry = null}
	onsubmit={(until, comment) => { if (dialogEntry) { if (dialogMode === 'move') void moveTarget(dialogEntry, until, comment || undefined); else void resolve(dialogEntry, 'snooze', until, comment); } }} />
<ScheduleEndDialog open={!!endChoice} entry={endChoice?.entry ?? null} details={endChoice?.details ?? null} {mobile}
	pending={endChoice ? !!pending[entryKey(endChoice.entry)] : false} error={endError}
	onclose={() => { endChoice = null; endError = ''; }}
	onchoose={choice => void chooseEnd(choice)} />
{#if review}
	<MoveImpactDialog entry={review.entry} preview={review.preview} {zone} {mobile} titles={linkedTitles} initialReason={review.reason}
		pending={committing} error={actionError} onclose={cancelReview}
		onsubmit={(reason, acknowledge) => { if (review && !committing) void commitTarget(review.entry, review.to, review.preview, review.sameIdentity, reason, acknowledge); }} />
{/if}

<style>
	.month-attention { display: block; font-size: 11px; color: var(--accent-red); padding: 0 4px; }
	.more { font: inherit; color: var(--text-secondary); border: 0; background: transparent; cursor: pointer; border-radius: 4px; text-align: left; font-size: 11.5px; font-weight: 600; padding: 1px 4px; }
	.month-day { list-style: none; margin: 0; padding: 0; overflow: hidden; }
	.month-day > :global(* + *) { border-top: 1px solid var(--border-subtle); }
	.more:hover { background: var(--bg-hover); }
	.more:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: -2px; }
	.incomplete-day { color: var(--text-secondary); font-size: 11px; margin: 0; padding: 0 4px; }
	@media (pointer: coarse) { .more { min-height: 44px; min-width: 44px; } }

	/* A quiet reload keeps the content and runs a thin line under the toolbar. */
	.progress { height: 2px; margin-top: calc(-1 * var(--space-4)); border-radius: 1px; overflow: hidden; }
	.progress.busy { background: linear-gradient(90deg, transparent, var(--accent-primary), transparent) 0 0 / 40% 100% no-repeat; animation: slide 1.2s linear infinite; }
	@keyframes slide { from { background-position: -40% 0; } to { background-position: 140% 0; } }
	@media (prefers-reduced-motion: reduce) { .progress.busy { animation: none; background: var(--accent-primary); opacity: 0.5; } }

	.layout { display: grid; grid-template-columns: minmax(0, 1fr) 340px; gap: var(--space-6); margin-top: var(--space-5); align-items: start; }
	.main { min-width: 0; }
	/* Sticky, so a rail taller than the window scrolls inside itself; otherwise its lower rows are reachable only at the page end. */
	.rail { display: flex; flex-direction: column; gap: var(--space-4); min-width: 0; position: sticky; top: var(--space-4); max-height: calc(100vh - 2 * var(--space-4)); overflow-y: auto; overscroll-behavior: contain; }
	.card { background: var(--card-bg); border: 1px solid var(--card-border); border-radius: var(--radius-lg); box-shadow: var(--shadow-card); }
	.card.below { margin-top: var(--space-5); max-width: 420px; }
	.rail .card { min-width: 0; }
	.rail-note { padding: var(--space-4); color: var(--text-muted); font-size: 13px; }
	.rail-skeleton { display: grid; gap: 12px; padding: var(--space-4); }
	.rail-skeleton .bar.head { margin: 0 0 2px; width: 90px; }
	.surface-switch { margin-top: var(--space-2); }
	.quiet-note { color: var(--text-secondary); font-size: 13px; margin: var(--space-3) 0; }

	.notice { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-2) var(--space-3); margin-top: var(--space-4); padding: 10px var(--space-4); border: 1px solid color-mix(in srgb, var(--accent-amber) 45%, transparent); border-radius: var(--radius); background: color-mix(in srgb, var(--accent-amber) 10%, transparent); font-size: 13px; }
	.notice.info { border-color: var(--border); background: var(--bg-secondary); }
	.spacer { flex: 1; }

	.state { padding: var(--space-10) var(--space-6); text-align: center; color: var(--text-secondary); }
	.state h3 { margin: 0 0 4px; font-size: 15px; color: var(--text-primary); }
	.state p { margin: 0; }
	.acts { display: flex; flex-wrap: wrap; justify-content: center; gap: var(--space-2); margin-top: var(--space-4); }

	.skeleton { margin-bottom: var(--space-5); }
	.bar, .dot { display: block; height: 10px; border-radius: 5px; background: var(--bg-tertiary); }
	.bar.head { width: 120px; height: 12px; margin: 0 0 12px 4px; }
	.dot { width: 16px; height: 16px; border-radius: 50%; }
	.sk { display: grid; grid-template-columns: 64px 20px 1fr 90px; align-items: center; column-gap: var(--space-3); padding: 14px var(--space-4); }
	.sk + .sk { border-top: 1px solid var(--border-subtle); }
	.sk .bar:first-child { width: 36px; }

	.dragbar { position: sticky; bottom: var(--space-4); z-index: 2; display: flex; align-items: center; gap: var(--space-3); margin-top: var(--space-4); padding: 10px var(--space-4); border: 1px solid var(--accent-primary); border-radius: var(--radius); background: var(--bg-raised); box-shadow: var(--shadow-lg); font-size: 13px; }
	.dragbar .what { flex: 1; min-width: 0; }
	.dragbar b { font-weight: 600; }
	.dragbar .review { color: var(--accent-red); font-weight: 600; }
	.dragbar .esc { color: var(--text-muted); white-space: nowrap; }
	kbd { font-family: var(--font-mono); font-size: 11px; padding: 1px 5px; border: 1px solid var(--border); border-radius: 4px; }

	@media (max-width: 1099px) {
		.layout { grid-template-columns: minmax(0, 1fr); gap: var(--space-4); }
		.rail { position: static; max-height: none; overflow: visible; display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); align-items: start; }
		.rail.single { grid-template-columns: minmax(0, 1fr); max-width: 420px; }
	}
	@media (max-width: 768px) {
		.layout { margin-top: var(--space-4); }
		.rail, .rail.single { grid-template-columns: minmax(0, 1fr); max-width: none; }
		.card.below { max-width: none; }
		.sk { grid-template-columns: 18px 1fr 60px; }
		.sk .bar:first-child { display: none; }
	}
</style>
