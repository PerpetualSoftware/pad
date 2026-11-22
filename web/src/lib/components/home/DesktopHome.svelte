<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import { avatarColor } from '$lib/utils/avatar';
	import { onMount, onDestroy, tick } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { pendingInvitations } from '$lib/stores/pendingInvitations.svelte';
	import PendingInvitations from '$lib/components/layout/PendingInvitations.svelte';
	import TutorialSuggestion from '$lib/components/tutorials/TutorialSuggestion.svelte';
	import { showHomeTutorial } from './homeTutorial';
	import { homeOverview } from '$lib/stores/homeOverview.svelte';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import NeedsRowView from '$lib/components/needs/NeedsRow.svelte';
	import AgendaWeekStrip from '$lib/components/calendar/AgendaWeekStrip.svelte';
	import AudienceToggle from '$lib/components/calendar/AudienceToggle.svelte';
	import AgendaRow from '$lib/components/calendar/AgendaRow.svelte';
	import AgendaCompleted from '$lib/components/calendar/AgendaCompleted.svelte';
	import { audienceCounts, filterAudience, type AudienceView } from '$lib/calendar/audience';
	import { completedPreferencePrefix, filterEntries, isCompleted, isSnoozed, attentionEntries, deadlineTone, entryKey, isOpenState, dayCounts, outstandingMatches, shiftDay, dayWindow, weekLabel, workOccurrence, roleMarkers, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaResponse, AgendaEntry, AgendaWorkEntry, NeedsRow, NeedsSnoozeRequest } from '$lib/types';

	let audience = $state<AudienceView>('mine');
	// The agenda's Show completed preference, per user: hidden there means hidden here.
	const showCompleted = $derived.by(() => {
		const userId = authStore.userId;
		if (!userId) return true;
		try { return localStorage.getItem(`${completedPreferencePrefix}${userId}`) !== '0'; } catch { return true; /* Storage is optional. */ }
	});
	let workspaceError = $state(false);
	let invitationsOpen = $state(false);
	let actionError = $state('');
	let menu = $state('');
	let busy = $state<Record<string, boolean>>({});
	const zone = $derived(homeOverview.zone);
	const today = $derived(homeOverview.today);
	let weekOffset = $state(0);
	let stripAgenda = $state<AgendaResponse | null>(null);
	let stripRequest = 0;
	const firstDay = $derived(shiftDay(homeOverview.monday, 7 * weekOffset));
	const days = $derived(Array.from({ length: 7 }, (_, i) => shiftDay(firstDay, i)));
	const entries = $derived(filterEntries(homeOverview.agenda?.entries ?? [], false));
	const stripEntries = $derived(weekOffset === 0 ? entries : filterEntries(stripAgenda?.entries ?? [], false));
	// The strip follows the audience toggle, so its counts agree with the summary and Today.
	const stripAudienceEntries = $derived(filterAudience(stripEntries, audience));
	// Forecasts and deadlines landing on a day, from the answer the strip already counts, as the agenda page reads them.
	const stripResponse = $derived(weekOffset === 0 ? homeOverview.agenda : stripAgenda);
	const stripMarkers = $derived(new Map([...roleMarkers(stripEntries, zone, stripResponse?.forecasts ?? [], stripResponse?.next_from)].map(([day, roles]) =>
		[day, { forecasts: filterAudience(roles.forecasts, audience).length, deadlines: filterAudience(roles.deadlines, audience).length }])));
	const stripLoad = () => weekOffset === 0
		? homeOverview.loading ? 'none' : homeOverview.agenda?.window_complete === false ? 'partial' : 'full'
		: !stripAgenda ? 'none' : stripAgenda.window_complete === false ? 'partial' : 'full';
	// The button goes away with the offset, so focus lands on the strip's tab-stop day instead of the page.
	async function backToThisWeek() {
		thisWeek();
		await tick();
		document.querySelector<HTMLButtonElement>('.home [data-strip-day][tabindex="0"]')?.focus();
	}
	function thisWeek() {
		stripRequest++;
		weekOffset = 0;
		stripAgenda = null;
	}
	// Identity, zone and week-start changes clear already-loaded local counts and fence pending requests.
	$effect(() => { authStore.identityEpoch; zone; homeOverview.monday; thisWeek(); });
	onDestroy(() => { stripRequest++; });
	async function swipeWeek(delta: 1 | -1) {
		weekOffset += delta;
		stripAgenda = null;
		const request = ++stripRequest;
		if (weekOffset === 0) return;
		const sameIdentity = authStore.identityFence();
		const from = dayWindow(firstDay, zone).from;
		const to = dayWindow(shiftDay(firstDay, 6), zone).to;
		try {
			const response = await api.getMyAgenda(from, to, zone, undefined, ['completed'], 'omit');
			if (sameIdentity() && request === stripRequest) stripAgenda = response;
		} catch {
			if (sameIdentity() && request === stripRequest) stripAgenda = null;
		}
	}
	const todayCompleted = $derived(filterAudience((homeOverview.agenda?.entries ?? []).filter(e => isCompleted(e) && e.day === today), audience));
	const todayEntries = $derived(entries.filter(e => e.day === today && (e.type === 'schedule' || e.type === 'date')));
	const counts = $derived(audienceCounts(todayEntries));
	const owed = $derived(dayCounts(filterAudience(todayEntries, audience)).owed);
	const isPastDeadline = (entry: AgendaEntry) => !!((entry.overdue && !isSnoozed(entry)) || (entry.roles?.deadline && deadlineTone(entry.roles.deadline, zone, new Date()) === 'passed'));
	const attention = $derived.by(() => {
		const rows = attentionEntries(entries, homeOverview.agenda?.priority_list ?? []).flatMap(row => {
			const entry = row.inWindow ? row.entry as AgendaEntry : workOccurrence(row.entry as AgendaWorkEntry, zone);
			return entry ? [entry] : [];
		});
		for (const entry of entries) if (isPastDeadline(entry) && (entry.type === 'date' || isOpenState(entry.state)) && !rows.some(row => entryKey(row) === entryKey(entry))) rows.push(entry);
		return rows.sort((a, b) => Number(isPastDeadline(b)) - Number(isPastDeadline(a)));
	});
	const pastDeadline = $derived(attention.filter(isPastDeadline).length);
	const todayRows = $derived(filterAudience(todayEntries, audience).filter(e => !attention.some(a => entryKey(a) === entryKey(e))));
	const agents = $derived(filterAudience(todayEntries, 'agents'));
	const agentCounts = $derived(dayCounts(agents));
	const greeting = $derived.by(() => {
		const hour = Number(new Intl.DateTimeFormat('en-GB', { hour: 'numeric', hourCycle: 'h23', timeZone: zone }).format(new Date()));
		return `Good ${hour < 12 ? 'morning' : hour < 18 ? 'afternoon' : 'evening'}${authStore.user?.name?.trim() ? ', ' + authStore.user.name.trim().split(/\s+/)[0] : ''}`;
	});
	const date = $derived(new Intl.DateTimeFormat('en-US', { weekday: 'long', month: 'long', day: 'numeric', timeZone: zone }).format(new Date()));
	const needKey = (row: NeedsRow) => `${row.workspace.slug}:${row.item.ref}`;
	// Set only by loadWorkspaces, never by an effect: true once the list has loaded (TASK-4571).
	let workspacesLoaded = $state(false);
	const showTutorial = $derived(showHomeTutorial(workspacesLoaded, workspaceStore.workspaces, authStore.userId));
	async function loadWorkspaces() {
		workspaceError = false;
		workspacesLoaded = false;
		try { await workspaceStore.ensureAll(); workspacesLoaded = true; } catch { workspaceError = true; }
	}
	// BottomNav owns overview refreshes on phone arrivals, including /.
	// Keep the desktop loading policy unchanged (TASK-4559 is separate).
	onMount(() => {
		if (!viewport.isMobile) void homeOverview.load('/');
		else void pendingInvitations.refresh();
		void loadWorkspaces();
	});
	const needsLimit = $derived(viewport.isMobile ? 2 : 4);
	async function snooze(row: NeedsRow, body: NeedsSnoozeRequest) {
		const key = needKey(row); if (busy[key]) return;
		const sameIdentity = authStore.identityFence(); busy = { ...busy, [key]: true }; actionError = '';
		try {
			await api.needs.snooze(row.workspace.slug, row.item.ref, { ...body, ...(row.marker ? { marker: row.marker } : {}) });
			if (sameIdentity()) await homeOverview.load('/', true);
		} catch { if (sameIdentity()) actionError = 'Could not update this item. Try again.'; }
		finally { if (sameIdentity()) busy = { ...busy, [key]: false }; }
	}
	function openAgenda() {
		void goto('/agenda');
	}
	// A strip tap opens the agenda on the day tapped, in whichever week the strip shows, in the audience the strip counted.
	function openAgendaOn(day: string) {
		void goto(`/agenda?day=${day}&audience=${audience}`);
	}
	async function action(entry: AgendaEntry, kind: AgendaAction) {
		// The full agenda records a lapsed occurrence at its own instant; Home hands it over.
		if ((kind !== 'done' && kind !== 'ack' && kind !== 'skip') || entry.state === 'lapsed') { openAgenda(); return; }
		const key = entryKey(entry); if (busy[key] || !entry.workspace) return;
		const sameIdentity = authStore.identityFence(); busy = { ...busy, [key]: true }; actionError = '';
		try {
			const view = await api.getCommitment(entry.workspace, entry.item_ref);
			if (!sameIdentity()) return;
			const occurrence = view.next_occurrence;
			if (!occurrence || !outstandingMatches(entry, view)) throw new Error('This occurrence changed. Open the full agenda to review it.');
			await api.resolveCommitment(entry.workspace, entry.item_ref, { occurrence_at: occurrence.at, state: kind });
			if (sameIdentity()) await homeOverview.load('/', true);
		} catch { if (sameIdentity()) actionError = 'Could not update this occurrence. Open the full agenda to review it.'; }
		finally { if (sameIdentity()) busy = { ...busy, [key]: false }; }
	}
</script>

<svelte:head><title>Home - Pad</title></svelte:head>
<div class="home">
	<header class="greeting">
		<h1>{greeting}</h1>
		<div class="summary"><p><span class="summary-date">{`${date} · `}</span><strong>{homeOverview.needs?.total ?? '…'}</strong> need you · <strong>{owed}</strong> owed today · <strong>{pastDeadline}</strong> past deadline</p><a href="/agenda">Open the full agenda →</a></div>
	</header>
	{#if homeOverview.error}<p role="alert">{homeOverview.error} <button onclick={() => homeOverview.load('/', true)}>Retry</button></p>{/if}
	{#if actionError}<p role="alert">{actionError}</p>{/if}
	{#if weekOffset !== 0}
		<div class="strip-week"><span>{weekLabel(firstDay, viewport.isMobile)}</span><button type="button" class="invitations-toggle" onclick={backToThisWeek}>This week</button></div>
	{/if}
	<AgendaWeekStrip compact={viewport.isMobile} {days} {today} entries={stripAudienceEntries} {zone} dragging={null} hoverDay="" onselect={openAgendaOn} onswipe={swipeWeek} onenter={() => {}} ondrop={() => {}} loadState={stripLoad} markers={stripMarkers} />
	<div class="overview-columns">
		<section class="panel needs-panel" aria-label="Needs you">
			<header><h2>Needs you <span class="badge">{homeOverview.needs?.total ?? '…'}</span></h2><span class="sort">Oldest waiting</span></header>
			<ul>{#each homeOverview.needs?.rows.slice(0, needsLimit) ?? [] as row (needKey(row))}
				<NeedsRowView compact={viewport.isMobile} {row} showWorkspace view="active" busy={!!busy[needKey(row)]} menuOpen={menu === needKey(row)} onmenu={open => menu = open ? needKey(row) : ''} ondismiss={row => snooze(row, { kind: 'dismiss' })} ondefer={(row, defer_until) => snooze(row, { kind: 'defer', defer_until })} onrestore={() => {}} />
			{/each}</ul>
			{#if !homeOverview.loading && homeOverview.needs?.total === 0}<p class="empty">Nothing needs you right now.</p>{/if}
			<footer><span>{Math.max(0, (homeOverview.needs?.total ?? 0) - Math.min(needsLimit, homeOverview.needs?.rows.length ?? 0))} more<span class="workspace-count">, across {homeOverview.needs?.workspaces.length ?? 0} workspaces</span></span><a href="/needs">See all {homeOverview.needs?.total ?? 0} →</a></footer>
		</section>
		<section class="panel today-panel" aria-label="Today">
			<header><h2>Today <span class="today-date">{new Intl.DateTimeFormat('en-US', {weekday:'short', day:'numeric',timeZone:zone}).format(new Date())}</span></h2><AudienceToggle value={audience} {counts} onchange={value => audience = value} /></header>
			{#if attention.length}<h3>{pastDeadline ? 'Past deadline' : 'Needs attention'}</h3><ul>{#each attention as entry (entryKey(entry))}<AgendaRow compact={viewport.isMobile} {entry} {zone} {audience} showDay pending={!!busy[entryKey(entry)]} onaction={action} />{/each}</ul>{/if}
			<h3>Today</h3><ul>{#each todayRows as entry (entryKey(entry))}<AgendaRow compact={viewport.isMobile} {entry} {zone} {audience} pending={!!busy[entryKey(entry)]} onaction={action} />{/each}</ul>
			{#if !homeOverview.loading && !todayRows.length}<p class="empty">Nothing scheduled today.</p>{/if}
			{#if showCompleted}<ul><AgendaCompleted {audience} completed={todayCompleted} {zone} compact={viewport.isMobile} onaction={action} /></ul>{/if}
			{#if showCompleted && homeOverview.agenda?.events_truncated}<p class="empty">Some completed items today may not be shown. <a href="/agenda">Open the full agenda →</a></p>{/if}
			<footer><span>Agents: {agents.length} {agents.length === 1 ? 'reminder' : 'reminders'} today, {agentCounts.owed} owed, {agentCounts.missed} missed</span><a href="/agenda?audience=agents">Agents →</a></footer>
		</section>
	</div>
	<section class="workspaces" aria-label="Workspaces">
		<header>
			<h2>Workspaces</h2>
			<span>{workspaceStore.workspaces.length} · {#if pendingInvitations.count > 0}<button type="button" class="invitations-toggle" aria-expanded={invitationsOpen} aria-controls="home-invitations" onclick={() => (invitationsOpen = !invitationsOpen)}>{pendingInvitations.count} {pendingInvitations.count === 1 ? 'invitation' : 'invitations'}</button>{:else}0 invitations{/if}</span>
			<Button variant="primary" class={uiStore.addWorkspaceHighlighted ? 'add-highlighted' : ''} onclick={() => uiStore.openCreateWorkspace()}>Create Workspace</Button>
		</header>
		{#if invitationsOpen && pendingInvitations.count > 0}
			<div id="home-invitations" class="home-invitations">
				<PendingInvitations active={invitationsOpen} onaccepted={() => (invitationsOpen = false)} />
			</div>
		{/if}
		{#if workspaceError}<p role="alert">Couldn’t load your workspaces. <button onclick={loadWorkspaces}>Retry</button></p>{/if}
		<div class="workspace-cards">{#each workspaceStore.workspaces as ws (ws.id)}
			<a class="workspace-card" href="/{ws.owner_username}/{ws.slug}"><strong><span class="avatar" style:background={avatarColor(ws.name)}>{ws.name.charAt(0).toUpperCase()}</span>{ws.name}</strong>
				{#if ws.is_guest}<span>Shared with you · guest</span>{:else if homeOverview.needs}<span>{homeOverview.needs.workspaces.find(w => w.slug === ws.slug)?.count ?? 0} need you</span>{/if}
			</a>
		{/each}</div>
		<!-- TASK-3452 / TASK-4571: a first-time user's tutorial, once per user, for a user who owns no workspace. -->
		{#if showTutorial}<TutorialSuggestion dismissKey="tutorials.console" slug="new-project" fallbackTitle="Start a new project with Pad" />{/if}
	</section>
</div>
<style>
	/* Closing the last tab highlights the way back in (PLAN-3002 Q2). */
	.workspaces :global(.btn.add-highlighted) { box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent-blue) 35%, transparent); }
	.workspaces > header > span { flex: 1; }
	.strip-week { display: flex; align-items: center; justify-content: space-between; color: var(--text-muted); font-size: 12px; }
	.invitations-toggle { color: var(--accent-blue); background: none; border: none; font: inherit; cursor: pointer; padding: 0; }
	.home-invitations { border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--bg-secondary); overflow: hidden; margin: 0 0 var(--space-3); }
	.home :global(.strip) { margin-top: 0; }
	.home { display: flex; flex-direction: column; gap: var(--space-5); }
	h1 { margin: 0 0 var(--space-2); font-size: 1.4rem; letter-spacing: -.025em; }
	h2 { margin: 0; font-size: .95rem; } h3 { margin: 0; padding: var(--space-2) var(--space-4); font-size: .7rem; text-transform: uppercase; color: var(--text-muted); background: var(--bg-tertiary); }
	.summary, section > header, footer { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
	p { margin: 0; } .summary { font-size: .9rem; color: var(--text-secondary); } a { color: var(--accent-blue); text-decoration: none; } .summary a, footer a { font-weight: 600; white-space: nowrap; }
	.overview-columns { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: var(--space-6); align-items: start; }
	.panel { border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--bg-secondary); box-shadow: 0 1px 3px rgba(0,0,0,.12); overflow: hidden; }
	.panel > header { padding: var(--space-3) var(--space-4); min-height: 52px; border-bottom: 1px solid var(--border); }
	.badge { margin-left: var(--space-1); border-radius: 99px; padding: 2px 7px; background: var(--bg-tertiary); color: var(--text-muted); font-size: .75rem; }
	.today-date { margin-left: var(--space-1); font-size: .65rem; color: var(--bg-primary); background: var(--accent-blue); padding: 2px 6px; border-radius: 99px; }
	.sort, footer, .workspaces > header { color: var(--text-secondary); font-size: .75rem; }
	ul { margin: 0; padding: 0; list-style: none; } footer { border-top: 1px solid var(--border); padding: var(--space-3) var(--space-4); }
	.empty { padding: var(--space-4); color: var(--text-muted); font-size: .85rem; }
	.workspaces { margin-top: var(--space-1); } .workspaces > header { margin: 0 var(--space-1) var(--space-3); }
	.workspace-cards { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: var(--space-3); }
	.workspace-card { display: flex; flex-direction: column; gap: var(--space-2); padding: var(--space-4); border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--bg-secondary); color: var(--text-secondary); font-size: .75rem; }
	.workspace-card strong { display: flex; align-items: center; gap: var(--space-2); color: var(--text-primary); font-size: .9rem; }
	.avatar { display: inline-flex; align-items: center; justify-content: center; background: var(--accent-purple); color: var(--bg-primary); width: 22px; height: 22px; border-radius: var(--radius-sm); }
	@media (max-width: 1100px) { .overview-columns { grid-template-columns: 1fr; } .workspace-cards { grid-template-columns: repeat(2,minmax(0,1fr)); } }
	@media (max-width: 768px) {
		.home { gap: var(--space-3); }
		.summary { font-size: 1rem; }
		.summary-date, .summary > a, .sort, .workspace-count { display: none; }
		.overview-columns { gap: var(--space-4); }
		.panel > header { padding: var(--space-3) var(--space-4); min-height: 46px; }
		.today-panel > header { flex-wrap: wrap; }
		.today-panel h2 { white-space: nowrap; }
		.today-panel :global(.seg .count) { display: none; }
		.today-panel :global(.seg .option) { padding-inline: var(--space-3); }
		footer { padding-inline: var(--space-4); }
		.today-panel footer, .workspaces > header { flex-wrap: wrap; }
		.workspace-cards { grid-template-columns: 1fr; }
	}
</style>
