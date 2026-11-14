<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import { avatarColor } from '$lib/utils/avatar';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { pendingInvitations } from '$lib/stores/pendingInvitations.svelte';
	import { homeOverview } from '$lib/stores/homeOverview.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import NeedsRowView from '$lib/components/needs/NeedsRow.svelte';
	import AgendaWeekStrip from '$lib/components/calendar/AgendaWeekStrip.svelte';
	import AudienceToggle from '$lib/components/calendar/AudienceToggle.svelte';
	import AgendaRow from '$lib/components/calendar/AgendaRow.svelte';
	import { audienceCounts, filterAudience, type AudienceView } from '$lib/calendar/audience';
	import { attentionEntries, deadlineTone, entryKey, isOpenState, outstandingMatches, shiftDay, workOccurrence, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry, AgendaWorkEntry, NeedsRow, NeedsSnoozeRequest } from '$lib/types';

	let audience = $state<AudienceView>('mine');
	let workspaceError = $state(false);
	let actionError = $state('');
	let menu = $state('');
	let busy = $state<Record<string, boolean>>({});
	const zone = $derived(homeOverview.zone);
	const today = $derived(homeOverview.today);
	const days = $derived(Array.from({ length: 7 }, (_, i) => shiftDay(homeOverview.monday, i)));
	const entries = $derived(homeOverview.agenda?.entries ?? []);
	const todayEntries = $derived(entries.filter(e => e.day === today && (e.type === 'schedule' || e.type === 'date')));
	const counts = $derived(audienceCounts(todayEntries));
	const owed = $derived(todayEntries.filter(e => e.type === 'schedule' && isOpenState(e.state)).length);
	const isPastDeadline = (entry: AgendaEntry) => !!(entry.overdue || (entry.roles?.deadline && deadlineTone(entry.roles.deadline, zone, new Date()) === 'passed'));
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
	const greeting = $derived.by(() => {
		const hour = Number(new Intl.DateTimeFormat('en-GB', { hour: 'numeric', hourCycle: 'h23', timeZone: zone }).format(new Date()));
		return `Good ${hour < 12 ? 'morning' : hour < 18 ? 'afternoon' : 'evening'}${authStore.user?.name?.trim() ? ', ' + authStore.user.name.trim().split(/\s+/)[0] : ''}`;
	});
	const date = $derived(new Intl.DateTimeFormat('en-US', { weekday: 'long', month: 'long', day: 'numeric', timeZone: zone }).format(new Date()));
	const needKey = (row: NeedsRow) => `${row.workspace.slug}:${row.item.ref}`;
	async function loadWorkspaces() {
		workspaceError = false;
		try { await workspaceStore.ensureAll(); } catch { workspaceError = true; }
	}
	onMount(() => { void homeOverview.load('/'); void loadWorkspaces(); });
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
		<div class="summary"><p>{date} · <strong>{homeOverview.needs?.total ?? '…'}</strong> need you · <strong>{owed}</strong> owed today · <strong>{pastDeadline}</strong> past deadline</p><a href="/agenda">Open the full agenda →</a></div>
	</header>
	{#if homeOverview.error}<p role="alert">{homeOverview.error} <button onclick={() => homeOverview.load('/', true)}>Retry</button></p>{/if}
	{#if actionError}<p role="alert">{actionError}</p>{/if}
	<AgendaWeekStrip {days} {today} {entries} {zone} dragging={null} hoverDay="" onselect={openAgenda} onenter={() => {}} ondrop={() => {}} loadState={() => homeOverview.loading ? 'none' : homeOverview.agenda?.window_complete === false ? 'partial' : 'full'} />
	<div class="overview-columns">
		<section class="panel needs-panel" aria-label="Needs you">
			<header><h2>Needs you <span class="badge">{homeOverview.needs?.total ?? '…'}</span></h2><span class="sort">Oldest waiting</span></header>
			<ul>{#each homeOverview.needs?.rows.slice(0,4) ?? [] as row (needKey(row))}
				<NeedsRowView {row} showWorkspace view="active" busy={!!busy[needKey(row)]} menuOpen={menu === needKey(row)} onmenu={open => menu = open ? needKey(row) : ''} ondismiss={row => snooze(row, { kind: 'dismiss' })} ondefer={(row, defer_until) => snooze(row, { kind: 'defer', defer_until })} onrestore={() => {}} />
			{/each}</ul>
			{#if !homeOverview.loading && homeOverview.needs?.total === 0}<p class="empty">Nothing needs you right now.</p>{/if}
			<footer><span>{Math.max(0, (homeOverview.needs?.total ?? 0) - Math.min(4, homeOverview.needs?.rows.length ?? 0))} more, across {homeOverview.needs?.workspaces.length ?? 0} workspaces</span><a href="/needs">See all {homeOverview.needs?.total ?? 0} →</a></footer>
		</section>
		<section class="panel today-panel" aria-label="Today">
			<header><h2>Today <span class="today-date">{new Intl.DateTimeFormat('en-US', {weekday:'short', day:'numeric',timeZone:zone}).format(new Date())}</span></h2><AudienceToggle value={audience} {counts} onchange={value => audience = value} /></header>
			{#if attention.length}<h3>{pastDeadline ? 'Past deadline' : 'Needs attention'}</h3><ul>{#each attention as entry (entryKey(entry))}<AgendaRow {entry} {zone} {audience} showDay pending={!!busy[entryKey(entry)]} onaction={action} />{/each}</ul>{/if}
			<h3>Today</h3><ul>{#each todayRows as entry (entryKey(entry))}<AgendaRow {entry} {zone} {audience} pending={!!busy[entryKey(entry)]} onaction={action} />{/each}</ul>
			{#if !homeOverview.loading && !todayRows.length}<p class="empty">Nothing scheduled today.</p>{/if}
			<footer><span>Agents: {agents.length} reminders today, {agents.filter(e => isOpenState(e.state)).length} owed, {agents.filter(e => e.state === 'missed' || e.state === 'lapsed').length} missed</span><a href="/agenda?audience=agents">Agents →</a></footer>
		</section>
	</div>
	<section class="workspaces" aria-label="Workspaces">
		<header>
			<h2>Workspaces</h2><span>{workspaceStore.workspaces.length} · {pendingInvitations.count} {pendingInvitations.count === 1 ? 'invitation' : 'invitations'}</span>
			<Button variant="primary" class={uiStore.addWorkspaceHighlighted ? 'add-highlighted' : ''} onclick={() => uiStore.openCreateWorkspace()}>Create Workspace</Button>
		</header>
		{#if workspaceError}<p role="alert">Couldn’t load your workspaces. <button onclick={loadWorkspaces}>Retry</button></p>{/if}
		<div class="workspace-cards">{#each workspaceStore.workspaces as ws (ws.id)}
			<a class="workspace-card" href="/{ws.owner_username}/{ws.slug}"><strong><span class="avatar" style:background={avatarColor(ws.name)}>{ws.name.charAt(0).toUpperCase()}</span>{ws.name}</strong>
				{#if ws.is_guest}<span>Shared with you · guest</span>{:else if homeOverview.needs}<span>{homeOverview.needs.workspaces.find(w => w.slug === ws.slug)?.count ?? 0} need you</span>{/if}
			</a>
		{/each}</div>
	</section>
</div>
<style>
	/* Closing the last tab highlights the way back in (PLAN-3002 Q2). */
	.workspaces :global(.btn.add-highlighted) { box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent-blue) 35%, transparent); }
	.workspaces > header > span { flex: 1; }
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
</style>
