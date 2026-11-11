<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { browserZone } from '$lib/timezone';
	import { canResolve, dayLabel, dayWindow, entryKey, filterEntries, groupEntries, localDate, shiftDay, todayIn } from '$lib/calendar/presentation';
	import PageHeader from '$lib/components/common/PageHeader.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import ContentError from '$lib/components/common/ContentError.svelte';
	import AgendaDayList from '$lib/components/calendar/AgendaDayList.svelte';
	import ResolveOccurrenceDialog from '$lib/components/calendar/ResolveOccurrenceDialog.svelte';
	import type { AgendaEntry, AgendaResponse, ScheduleResolveState } from '$lib/types';

	const staleCopy = 'This occurrence changed. Review the updated entry and try again.';
	const permissionCopy = 'You can no longer change this item.';
	const genericCopy = 'Could not update this occurrence. Try again.';
	const preferencePrefix = 'pad-agenda-show-events:';
	const zone = $derived(authStore.user?.timezone || browserZone() || 'UTC');
	let selectedDay = $state('');
	const day = $derived(selectedDay || todayIn(zone));
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
	let dialogEntry = $state<AgendaEntry | null>(null);
	let dialogMode = $state<'snooze' | 'move'>('snooze');
	let ticket = 0;
	let loadedUser: string | undefined;
	let preferenceUser: string | undefined;
	const entries = $derived(filterEntries(data?.entries ?? [], showEvents));
	const groups = $derived(groupEntries(entries));
	const incomplete = $derived(!!(data?.window_complete === false || data?.truncated || data?.unavailable?.length || data?.unexpanded?.length));
	const status = $derived(loading ? 'Loading agenda…' : updating ? 'Updating agenda…' : announcement);

	$effect(() => {
		const userId = authStore.user?.id;
		untrack(() => {
			preferenceUser = userId;
			let value = true;
			if (userId) {
				try { value = localStorage.getItem(`${preferencePrefix}${userId}`) !== '0'; } catch { /* Storage is optional. */ }
			}
			showEvents = value;
		});
	});

	function setShowEvents(value: boolean) {
		showEvents = value;
		const userId = authStore.user?.id;
		if (!userId || userId !== preferenceUser) return;
		try { localStorage.setItem(`${preferencePrefix}${userId}`, value ? '1' : '0'); } catch { /* Keep the in-memory choice. */ }
	}

	async function load(date: string, timeZone: string, quiet = false): Promise<boolean | undefined> {
		const mine = ++ticket;
		const sameIdentity = authStore.identityFence();
		const userId = authStore.user?.id;
		const current = () => mine === ticket && sameIdentity();
		await Promise.resolve();
		if (!current()) return;
		if (loadedUser !== userId) {
			data = null; pending = {}; saved = {}; dialogEntry = null; actionError = ''; announcement = '';
			loadedUser = userId;
		}
		if (!userId) { data = null; loading = false; updating = false; return; }
		loading = !quiet;
		updating = quiet;
		loadError = false;
		if (!quiet) { data = null; savedRefreshError = false; announcement = ''; }
		try {
			const { from, to } = dayWindow(date, timeZone);
			const response = await api.getMyAgenda(from, to, timeZone);
			if (!current()) return;
			data = response;
			saved = {};
			savedRefreshError = false;
			return true;
		} catch {
			if (!current()) return;
			loadError = true;
			return false;
		} finally {
			if (current()) { loading = false; updating = false; }
		}
	}

	$effect(() => {
		const date = day;
		const timeZone = zone;
		authStore.user?.id;
		authStore.identityEpoch;
		untrack(() => { void load(date, timeZone); });
	});
	$effect(() => () => { ticket++; });

	function today() {
		const next = todayIn(zone);
		if (day === next) void load(day, zone);
		else selectedDay = next;
	}

	function action(entry: AgendaEntry, state: ScheduleResolveState) {
		if (pending[entryKey(entry)] || saved[entryKey(entry)] || !canResolve(entry)) return;
		actionError = '';
		if (state === 'move' || state === 'snooze') {
			dialogMode = state;
			dialogEntry = entry;
		} else void resolve(entry, state);
	}

	async function resolve(entry: AgendaEntry, state: ScheduleResolveState, until?: string, comment?: string) {
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
			const result = await api.resolveCommitment(entry.workspace!, entry.item_ref, {
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
				announcement = result?.resolution?.warning === 'target_after_deadline'
					? 'Occurrence moved. The new date is after its deadline.'
					: { done: 'Occurrence marked done.', skip: 'Occurrence skipped.', snooze: 'Occurrence snoozed.', move: 'Occurrence moved.' }[state];
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
</script>

<svelte:head><title>Agenda</title></svelte:head>

<PageHeader title="Agenda" description="Dates and schedules across your workspaces.">
	<div class="controls">
		<div class="period">
			<Button size="sm" aria-label="Previous day" onclick={() => selectedDay = shiftDay(day, -1)}>&lt;</Button>
			<Button size="sm" aria-label="Next day" onclick={() => selectedDay = shiftDay(day, 1)}>&gt;</Button>
			<Button size="sm" onclick={today}>Today</Button>
			<strong>{dayLabel(day)}</strong>
		</div>
		<a href="/console/settings">{zone}</a>
		<label><input type="checkbox" checked={showEvents} onchange={event => setShowEvents(event.currentTarget.checked)} /> Show created and completed</label>
	</div>
</PageHeader>

<div role="status" aria-live="polite" aria-atomic="true">{status}</div>
{#if actionError && !dialogEntry}<p role="alert">{actionError}</p>{/if}
{#if savedRefreshError}
	<p role="alert">Saved. Could not refresh the view.</p>
	<Button onclick={() => load(day, zone, true)}>Try again</Button>
{:else if loadError}
	<ContentError title="Could not load agenda." />
	<Button onclick={() => load(day, zone, !!data)}>Try again</Button>
{/if}
{#if incomplete}
	<p>Agenda is incomplete.</p>
	{#if data?.next_from}<p>more after {localDate(data.next_from, zone)}</p>{/if}
{/if}
{#each groups as group (group.day)}
	<AgendaDayList day={group.day} entries={group.entries} {zone} {pending} {saved} onaction={action} />
{/each}
{#if !loading && !loadError && !savedRefreshError && !incomplete && entries.length === 0 && data}
	{#if !showEvents && data.entries.length > 0}
		<p>Created and completed entries are hidden.</p>
		<Button onclick={() => setShowEvents(true)}>Show created and completed</Button>
	{:else}<p>No entries on this day.</p>{/if}
{/if}
<ResolveOccurrenceDialog open={!!dialogEntry} mode={dialogMode} {zone}
	pending={dialogEntry ? !!pending[entryKey(dialogEntry)] : false} error={actionError}
	onclose={() => dialogEntry = null}
	onsubmit={(until, comment) => { if (dialogEntry) void resolve(dialogEntry, dialogMode, until, comment); }} />

<style>
	.controls { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-3); margin-top: var(--space-4); }
	.period { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-2); }
	.controls a { color: var(--text-secondary); overflow-wrap: anywhere; }
	label { display: flex; align-items: center; gap: var(--space-2); color: var(--text-primary); }
	input { flex-shrink: 0; }
	[role="status"] { color: var(--text-secondary); }
	@media (max-width: 767px) { .controls { align-items: flex-start; flex-direction: column; } }
</style>
