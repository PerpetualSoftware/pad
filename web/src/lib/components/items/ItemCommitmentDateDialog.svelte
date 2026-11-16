<!--
	ItemCommitmentDateDialog: "+ Add date" on an item with no commitment (TASK-4600).

	One target date (the soft planning date), an optional time, a recurrence
	(once or one of the server's shorthands) and an optional hard deadline in
	the same precision as the target. It sends zone-less walls with no tz: the
	server pins the account zone, or UTC when the account has none, so that is
	the zone shown here (never the browser's).
-->
<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import DialogShell from '$lib/components/common/DialogShell.svelte';
	import { buildCommitmentCreate, COMMITMENT_RECURRENCES, describeRecurrence, type CommitmentRecurrence } from '$lib/commitment';
	import { todayIn } from '$lib/calendar/presentation';

	let { open, wsSlug, itemSlug, itemRef = '', itemTitle = '', mobile, onclose, oncreated }: {
		open: boolean; wsSlug: string; itemSlug: string; itemRef?: string; itemTitle?: string; mobile: boolean;
		onclose: () => void; oncreated: () => void;
	} = $props();
	const id = $props.id();
	const accountZone = $derived(authStore.user?.timezone || '');
	let date = $state('');
	let time = $state('');
	let rule = $state<CommitmentRecurrence>('once');
	let hasDeadline = $state(false);
	let deadlineDate = $state('');
	let deadlineTime = $state('');
	let behavior = $state<'lapses' | 'stays_owed'>('lapses');
	let pending = $state(false);
	let error = $state('');
	let errorField = $state('');
	// Plain ticket: a new opening or a close drops an answer still in flight.
	let ticket = 0;
	const seed = $derived(date || todayIn(accountZone || 'UTC'));
	const ruleLabel = (rrule: string | undefined) => rrule ? describeRecurrence({ rrule, rdates: [], dtstart: seed, anchor: 'calendar' }) : 'Once';

	$effect(() => {
		const visible = open;
		untrack(() => {
			ticket++;
			if (visible) { date = ''; time = ''; rule = 'once'; hasDeadline = false; deadlineDate = ''; deadlineTime = ''; behavior = 'lapses'; pending = false; error = ''; errorField = ''; }
		});
	});

	function failure(e: unknown): string {
		const err = e as { status?: number; code?: string; message?: unknown; details?: { field?: unknown } };
		if (err.status === 403 || err.code === 'forbidden' || err.code === 'permission_denied') return 'You can no longer change this item.';
		if (err.status === 400 && typeof err.details?.field === 'string') errorField = err.details.field;
		return err.status === 400 && typeof err.message === 'string' && err.message ? err.message : 'Could not add the date. Try again.';
	}
	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (pending) return;
		error = ''; errorField = '';
		const body = buildCommitmentCreate({ date, time, rule, ...(hasDeadline ? { deadline: { date: deadlineDate, time: time ? deadlineTime : '', behavior } } : {}) });
		if ('field' in body) { error = body.message; errorField = body.field; return; }
		const mine = ++ticket;
		const sameIdentity = authStore.identityFence();
		pending = true;
		try {
			await api.createCommitment(wsSlug, itemSlug, body);
			if (mine !== ticket || !sameIdentity()) return;
			oncreated();
		} catch (e) {
			if (mine !== ticket || !sameIdentity()) return;
			// Someone added one meanwhile: show it rather than posting again.
			if ((e as { code?: string })?.code === 'schedule_exists') { oncreated(); return; }
			error = failure(e);
		} finally {
			if (mine === ticket) pending = false;
		}
	}
	const invalid = (field: string) => errorField === field || (field === 'date' && errorField === 'dtstart');
</script>

<DialogShell {open} {mobile} {pending} title="Add date" subject={itemTitle} subjectRef={itemRef || itemSlug} {onclose}>
	<form id={`${id}-form`} onsubmit={submit} novalidate>
		<div class="two">
			<div class="field">
				<label for={`${id}-date`}>Target date</label>
				<input id={`${id}-date`} type="date" required bind:value={date} disabled={pending} aria-invalid={invalid('date')} />
			</div>
			<div class="field">
				<label for={`${id}-time`}>Time <span class="optional">(optional)</span></label>
				<input id={`${id}-time`} type="time" bind:value={time} disabled={pending} aria-invalid={invalid('time')} />
			</div>
		</div>
		<p class="hint">Time zone: {accountZone || 'UTC (no account time zone set)'}</p>
		<div class="field">
			<label for={`${id}-rule`}>Repeats</label>
			<select id={`${id}-rule`} bind:value={rule} disabled={pending} aria-invalid={invalid('rrule')}>
				{#each COMMITMENT_RECURRENCES as [value, rrule] (value)}<option {value}>{ruleLabel(rrule)}</option>{/each}
			</select>
		</div>
		<label class="check"><input type="checkbox" bind:checked={hasDeadline} disabled={pending} /> Hard deadline</label>
		{#if hasDeadline}
			<div class="two">
				<div class="field">
					<label for={`${id}-deadline`}>Deadline date</label>
					<input id={`${id}-deadline`} type="date" bind:value={deadlineDate} disabled={pending} aria-invalid={invalid('deadline')} />
				</div>
				{#if time}
					<div class="field">
						<label for={`${id}-deadline-time`}>Deadline time</label>
						<input id={`${id}-deadline-time`} type="time" bind:value={deadlineTime} disabled={pending} aria-invalid={invalid('deadline')} />
					</div>
				{/if}
			</div>
			<fieldset>
				<legend>When the deadline passes</legend>
				<label class="check"><input type="radio" name={`${id}-behavior`} value="lapses" bind:group={behavior} disabled={pending} /> Lapses</label>
				<label class="check"><input type="radio" name={`${id}-behavior`} value="stays_owed" bind:group={behavior} disabled={pending} /> Still owed</label>
			</fieldset>
		{/if}
		{#if error}<p role="alert">{error}</p>{/if}
	</form>
	{#snippet footer()}
		<Button disabled={pending} onclick={() => { if (!pending) onclose(); }}>Cancel</Button>
		<Button variant="primary" type="submit" form={`${id}-form`} disabled={pending}>Add date</Button>
	{/snippet}
</DialogShell>

<style>
	form { display: flex; flex-direction: column; gap: var(--space-4); min-width: 0; }
	p { margin: 0; }
	.hint { color: var(--text-muted); font-size: 12px; overflow-wrap: anywhere; }
	.two { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-3); }
	.field { min-width: 0; }
	label, legend { display: block; margin-bottom: 6px; font-size: 12px; font-weight: 600; color: var(--text-secondary); }
	.optional { font-weight: 400; color: var(--text-muted); }
	fieldset { border: 0; margin: 0; padding: 0; min-width: 0; }
	.check { display: flex; align-items: center; gap: var(--space-2); min-height: 36px; font-size: 14px; font-weight: 400; color: var(--text-primary); }
	input:not([type='checkbox']):not([type='radio']), select { box-sizing: border-box; width: 100%; min-width: 0; min-height: 36px; padding: 6px 10px; color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); font: inherit; font-size: 14px; }
	@media (max-width: 768px) { .two { grid-template-columns: 1fr; } input:not([type='checkbox']):not([type='radio']), select, .check { min-height: 44px; } }
</style>
