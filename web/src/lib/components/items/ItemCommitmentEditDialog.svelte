<!--
	ItemCommitmentEditDialog: Edit on an item's commitment (TASK-4610).

	Changes the series: the start (date and optional time), Repeats and the hard
	deadline. Earliest and occupied end are shown read-only; their stored offsets
	are never sent, so they move with the start. The controls start from the
	stored values (view.role_offsets), and only members whose controls changed
	are sent. A native row's deadline changes through renegotiate alone (preview,
	review, apply); everything else goes in one PATCH.
-->
<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import DialogShell from '$lib/components/common/DialogShell.svelte';
	import { buildCommitmentEdit, COMMITMENT_RECURRENCES, describeRecurrence, editStateFromView, offsetWall, wallOffset, type CommitmentEditPlan, type CommitmentEditState, type CommitmentRecurrence } from '$lib/commitment';
	import { roleText } from '$lib/calendar/presentation';
	import type { CommitmentMoveResult, CommitmentRenegotiationResult, ItemCommitmentView } from '$lib/types';
	import { permissionCopy } from './itemCommitmentActions.svelte';

	let { open, view, wsSlug, itemSlug, itemRef = '', itemTitle = '', zone, mobile, titles = {}, onloadtitles, onclose, onsaved, onreload }: {
		open: boolean; view: ItemCommitmentView | null; wsSlug: string; itemSlug: string; itemRef?: string; itemTitle?: string; zone: string; mobile: boolean;
		titles?: Record<string, string>; onloadtitles?: (result: CommitmentMoveResult) => void;
		onclose: () => void; onsaved: () => void; onreload: () => void;
	} = $props();
	const id = $props.id();
	type Behavior = 'lapses' | 'stays_owed';
	type Renegotiate = Extract<CommitmentEditPlan, { kind: 'renegotiate' }>['body'];
	let st = $state.raw<CommitmentEditState | null>(null);
	let opened = $state.raw<ItemCommitmentView | null>(null);
	let date = $state('');
	let time = $state('');
	let rule = $state<CommitmentRecurrence | null>('once');
	let deadline = $state.raw<{ days: number; secs: number; behavior: Behavior } | null>(null);
	let reason = $state('');
	let review = $state.raw<{ body: Renegotiate; result: CommitmentRenegotiationResult } | null>(null);
	let pending = $state(false);
	let error = $state('');
	let errorField = $state('');
	let notice = $state('');
	let stale = $state(false);
	// Plain ticket: a new opening or a close drops an answer still in flight.
	let ticket = 0;
	const scheduleZone = $derived(opened?.floating ? zone : opened?.tz ?? zone);
	const oneOff = $derived(rule === 'once');
	const wall = $derived(deadline ? offsetWall(date || st?.initial.date || '', time, deadline.days, deadline.secs) : null);
	const ruleLabel = (rrule: string | undefined) => rrule ? describeRecurrence({ rrule, rdates: [], dtstart: date || '2000-01-03', anchor: 'calendar' }) : 'Once';

	$effect(() => {
		const visible = open;
		untrack(() => {
			ticket++;
			if (!visible) return;
			opened = view; st = view ? editStateFromView(view) : null;
			const init = st?.initial;
			date = init?.date ?? ''; time = init?.time ?? ''; rule = init?.rule ?? null; deadline = init?.deadline ?? null;
			reason = ''; review = null; pending = false; error = ''; errorField = ''; notice = ''; stale = false;
		});
	});

	function setWall(toDate: string, toTime: string) {
		// An empty target date has no civil day to measure from, and an empty deadline
		// time on a timed target is not midnight; either way keep the stored offset.
		if (!deadline || !toDate || !date || (time && !toTime)) return;
		deadline = { ...deadline, ...wallOffset(date, time, toDate, time ? toTime : '') };
	}
	function setDaysAfter(raw: string) {
		if (!deadline || !wall || !date || !/^-?\d+$/.test(raw)) return;
		deadline = { ...deadline, ...wallOffset(date, time, offsetWall(date, '', +raw, 0).date, wall.time) };
	}
	function failure(e: unknown): string {
		const err = e as { status?: number; code?: string; message?: unknown; details?: { field?: unknown } };
		const message = typeof err.message === 'string' ? err.message.trim() : '';
		if (err.code === 'forbidden' || err.code === 'permission_denied') return permissionCopy;
		if (['deadline_owner_required', 'schedule_linked', 'hard_deadline'].includes(err.code ?? '') && message) return message;
		if (err.status === 400 && message) { if (typeof err.details?.field === 'string') errorField = err.details.field; return message; }
		if (err.status === 403) return permissionCopy;
		return 'Could not update the date. Try again.';
	}
	async function run(work: (live: () => boolean) => Promise<void>) {
		const mine = ++ticket;
		const sameIdentity = authStore.identityFence();
		const live = () => mine === ticket && sameIdentity();
		pending = true;
		try { await work(live); }
		catch (e) { if (live()) error = failure(e); }
		finally { if (mine === ticket) pending = false; }
	}
	async function preview(body: Renegotiate, live: () => boolean) {
		const result = await api.renegotiateCommitment(wsSlug, itemSlug, { ...body, reason: reason.trim(), preview: true }) as CommitmentRenegotiationResult;
		if (!live()) return;
		review = { body, result };
		if (result.cascade) onloadtitles?.(result.cascade);
	}
	function submit(event: SubmitEvent) {
		event.preventDefault();
		if (pending || !st || !opened) return;
		error = ''; errorField = ''; notice = '';
		if (review) return void apply(review);
		const plan = buildCommitmentEdit(st, { date, time, rule, deadline }, { zone: scheduleZone, now: new Date(Date.now()) });
		if ('field' in plan) { error = plan.message; errorField = plan.field; return; }
		if (plan.kind === 'none') { notice = 'No changes.'; return; }
		if (plan.kind === 'renegotiate' && !reason.trim()) { error = 'Give a reason for changing the deadline.'; errorField = 'reason'; return; }
		const was = opened;
		void run(async live => {
			// Fence: the stored values this form started from must still be the stored values.
			const fresh = await api.getCommitment(wsSlug, itemSlug);
			if (!live()) return;
			if (fresh.schedule_id !== was.schedule_id || fresh.updated_at !== was.updated_at) { stale = true; error = 'This date changed. Reload to edit the current values.'; return; }
			if (plan.kind === 'patch') {
				await api.updateCommitment(wsSlug, itemSlug, plan.body);
				if (live()) onsaved();
				return;
			}
			await preview(plan.body, live);
		});
	}
	function apply(r: NonNullable<typeof review>) {
		void run(async live => {
			try {
				const violations = !!r.result.cascade?.violations?.length;
				await api.renegotiateCommitment(wsSlug, itemSlug, { ...r.body, reason: reason.trim(), revision: r.result.revision, ...(violations ? { acknowledge_violations: true } : {}) });
				if (live()) onsaved();
			} catch (e) {
				if (!live() || (e as { code?: string })?.code !== 'commitment_changed') throw e;
				review = null;
				await preview(r.body, live);
				if (live()) notice = 'The date changed meanwhile. Review the new preview and confirm again.';
			}
		});
	}
	const invalid = (field: string) => errorField === field || (field === 'date' && errorField === 'dtstart') || (field === 'rule' && errorField === 'rrule');
	const show = (value: string | null | undefined) => value ? roleText(value, zone) : 'None';
	const title = (change: { item_id: string; root?: boolean }) => change.root ? itemTitle || 'This item' : titles[change.item_id] ?? 'Linked item';
	// An occurrence key is `instant|wall`; the instant is the one to show.
	const appliesFrom = (key: string | null | undefined) => (key ?? '').split('|')[0].trim();
	const roleName = (role: string) => role === 'occupied_until' ? 'occupied end' : role;
</script>

<DialogShell {open} {mobile} {pending} title="Edit date" subject={itemTitle} subjectRef={itemRef || itemSlug} {onclose}>
	<form id={`${id}-form`} onsubmit={submit} novalidate>
		{#if st && opened}
			{#if review}
				{@const p = review.result.preview}
				<section class="review" aria-label="Review the deadline change">
					<p class="row"><span class="k">Deadline now</span> <span>{show(p.current_deadline)}{#if p.current_deadline_behavior}{' · '}{p.current_deadline_behavior === 'lapses' ? 'Lapses' : 'Still owed'}{/if}</span></p>
					<p class="row"><span class="k">New deadline</span> <span>{show(p.new_deadline)}{#if p.new_deadline_behavior}{' · '}{p.new_deadline_behavior === 'lapses' ? 'Lapses' : 'Still owed'}{/if}</span></p>
					{#if appliesFrom(p.applies_from)}<p class="row"><span class="k">Applies from</span> <span>{roleText(appliesFrom(p.applies_from), zone)}</span></p>{/if}
					{#if p.miss_recorded_first}<p>The deadline already passed: its miss is recorded first.</p>{/if}
					{#if p.keeps_deadline_override}<p>This occurrence keeps its own moved deadline.</p>{/if}
					{#each (review.result.cascade?.changes ?? []).filter(c => !c.root) as change}<p>{title(change)}: {roleName(change.role)} moves to {roleText(change.to, zone)}</p>{/each}
					{#each review.result.cascade?.violations ?? [] as change}<p class="warn">{title(change)}: {roleName(change.role)} lands outside its own bounds ({roleText(change.to, zone)})</p>{/each}
				</section>
			{:else}
				<div class="two">
					<div class="field">
						<label for={`${id}-date`}>Target date</label>
						<input id={`${id}-date`} type="date" required value={date} oninput={e => { date = e.currentTarget.value; }} disabled={pending || !!st.timingLock} aria-invalid={invalid('date')} />
					</div>
					<div class="field">
						<label for={`${id}-time`}>Time <span class="optional">(optional)</span></label>
						<input id={`${id}-time`} type="time" value={time} oninput={e => { time = e.currentTarget.value; }} disabled={pending || !!st.timingLock || (st.timeLocked && !st.initial.time)} aria-invalid={invalid('time')} />
					</div>
				</div>
				<p class="hint">Time zone: {opened.floating ? 'Floating' : opened.tz ?? zone}</p>
				<div class="field">
					<label for={`${id}-rule`}>Repeats</label>
					{#if rule === null}
						<input id={`${id}-rule`} value={opened.rrule ?? 'Specific dates'} readonly disabled />
					{:else}
						<select id={`${id}-rule`} value={rule} onchange={e => { rule = e.currentTarget.value as CommitmentRecurrence; }} disabled={pending || !!st.timingLock} aria-invalid={invalid('rule')}>
							{#each COMMITMENT_RECURRENCES as [value, rrule] (value)}<option {value}>{ruleLabel(rrule)}</option>{/each}
						</select>
					{/if}
				</div>
				{#if st.timingLock}<p class="hint">{st.timingLock}</p>
				{:else if st.replanDrops}<p class="hint">Changing the start or Repeats re-plans from now: this occurrence’s move or snooze is dropped.</p>{/if}
				<label class="check"><input type="checkbox" checked={!!deadline} onchange={e => { deadline = e.currentTarget.checked ? (st?.initial.deadline ?? { days: 0, secs: 0, behavior: 'lapses' }) : null; }} disabled={pending || !!st.deadlineLock} /> Hard deadline</label>
				{#if deadline && wall}
					<div class="two">
						{#if oneOff}
							<div class="field">
								<label for={`${id}-deadline`}>Deadline date</label>
								<input id={`${id}-deadline`} type="date" value={wall.date} oninput={e => setWall(e.currentTarget.value, wall.time)} disabled={pending || !!st.deadlineLock} aria-invalid={invalid('deadline')} />
							</div>
						{:else}
							<div class="field">
								<label for={`${id}-deadline-days`}>Days after the target</label>
								<input id={`${id}-deadline-days`} type="number" min="0" step="1" value={wallOffset(date, '', wall.date, '').days} oninput={e => setDaysAfter(e.currentTarget.value)} disabled={pending || !!st.deadlineLock} aria-invalid={invalid('deadline')} />
							</div>
						{/if}
						{#if time}
							<div class="field">
								<label for={`${id}-deadline-time`}>Deadline time</label>
								<input id={`${id}-deadline-time`} type="time" value={wall.time} oninput={e => setWall(wall.date, e.currentTarget.value)} disabled={pending || !!st.deadlineLock} aria-invalid={invalid('deadline')} />
							</div>
						{/if}
					</div>
					<fieldset disabled={pending || !!st.deadlineLock}>
						<legend>When the deadline passes</legend>
						<label class="check"><input type="radio" name={`${id}-behavior`} value="lapses" checked={deadline.behavior === 'lapses'} onchange={() => { deadline = deadline && { ...deadline, behavior: 'lapses' }; }} /> Lapses</label>
						<label class="check"><input type="radio" name={`${id}-behavior`} value="stays_owed" checked={deadline.behavior === 'stays_owed'} onchange={() => { deadline = deadline && { ...deadline, behavior: 'stays_owed' }; }} /> Still owed</label>
					</fieldset>
				{/if}
				{#if st.deadlineLock}<p class="hint">{st.deadlineLock}</p>{/if}
				{#if opened.roles.earliest}<p class="hint">Earliest: {roleText(opened.roles.earliest, zone)} (moves with the target)</p>{/if}
				{#if opened.roles.occupied_until}<p class="hint">Occupied end: {roleText(opened.roles.occupied_until, zone)} (moves with the target)</p>{/if}
			{/if}
			{#if st.nativeDeadline}
				<div class="field">
					<label for={`${id}-reason`}>Reason for the deadline change</label>
					<input id={`${id}-reason`} value={reason} oninput={e => { reason = e.currentTarget.value; }} disabled={pending || !!review} aria-invalid={invalid('reason')} />
				</div>
			{/if}
		{/if}
		{#if notice}<p role="status">{notice}</p>{/if}
		{#if error}<p role="alert">{error}{#if stale} <Button onclick={() => onreload()}>Reload</Button>{/if}</p>{/if}
	</form>
	{#snippet footer()}
		<Button disabled={pending} onclick={() => { if (pending) return; if (review) { review = null; notice = ''; } else onclose(); }}>{review ? 'Back' : 'Cancel'}</Button>
		<Button variant="primary" type="submit" form={`${id}-form`} disabled={pending || stale}>{review ? 'Change deadline' : 'Save'}</Button>
	{/snippet}
</DialogShell>

<style>
	form { display: flex; flex-direction: column; gap: var(--space-4); min-width: 0; }
	p { margin: 0; }
	.hint { color: var(--text-muted); font-size: 12px; overflow-wrap: anywhere; }
	.review { display: flex; flex-direction: column; gap: var(--space-2); font-size: 14px; overflow-wrap: anywhere; }
	.row { display: grid; grid-template-columns: 110px minmax(0, 1fr); column-gap: var(--space-3); align-items: baseline; }
	.k { color: var(--text-secondary); font-weight: 600; font-size: 12px; }
	.warn { color: var(--color-warning, var(--text-primary)); }
	.two { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-3); }
	.field { min-width: 0; }
	label, legend { display: block; margin-bottom: 6px; font-size: 12px; font-weight: 600; color: var(--text-secondary); }
	.optional { font-weight: 400; color: var(--text-muted); }
	fieldset { border: 0; margin: 0; padding: 0; min-width: 0; }
	.check { display: flex; align-items: center; gap: var(--space-2); min-height: 36px; font-size: 14px; font-weight: 400; color: var(--text-primary); }
	input:not([type='checkbox']):not([type='radio']), select { box-sizing: border-box; width: 100%; min-width: 0; min-height: 36px; padding: 6px 10px; color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); font: inherit; font-size: 14px; }
	input:not([type='checkbox']):not([type='radio']):disabled, select:disabled { color: var(--text-muted); background: var(--disabled-bg, var(--bg-tertiary)); border-color: var(--disabled-border, var(--border-subtle)); cursor: not-allowed; opacity: 1; -webkit-text-fill-color: var(--text-muted); }
	fieldset:disabled .check, .check:has(input:disabled) { color: var(--text-muted); cursor: not-allowed; }
	/* Light theme: tertiary is barely off the enabled fill, so disabled fields take the active tone. */
	:global([data-theme='light']) form { --disabled-bg: var(--bg-active); --disabled-border: var(--border); }
	@media (prefers-color-scheme: light) { :global(:root:not([data-theme='dark'])) form { --disabled-bg: var(--bg-active); --disabled-border: var(--border); } }
	@media (max-width: 768px) { .two { grid-template-columns: 1fr; } input:not([type='checkbox']):not([type='radio']), select, .check { min-height: 44px; } }
</style>
