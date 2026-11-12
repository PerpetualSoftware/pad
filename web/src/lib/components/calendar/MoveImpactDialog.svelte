<script lang="ts">
	import { untrack } from 'svelte';
	import DialogShell from '$lib/components/common/DialogShell.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import { roleDay, shortDay, shortRoleDate, todayIn } from '$lib/calendar/presentation';
	import type { AgendaEntry, CommitmentMoveChange, CommitmentMoveResult } from '$lib/types';
	let { entry, preview, zone, titles, mobile, initialReason = '', pending, error, onclose, onsubmit }: {
		entry: AgendaEntry; preview: CommitmentMoveResult; zone: string; titles: Record<string, string>; mobile: boolean;
		initialReason?: string; pending: boolean; error: string; onclose: () => void;
		onsubmit: (reason?: string, acknowledge?: boolean) => void;
	} = $props();
	const id = $props.id();
	let reason = $state('');
	let accepted = $state(false);
	let reasonError = $state('');
	let boundsError = $state('');
	const changes = $derived([...preview.changes.filter(change => change.root), ...preview.changes.filter(change => !change.root)]);
	const violations = $derived(preview.violations?.length ?? 0);
	const roleNames = { target: 'Target', deadline: 'Deadline', earliest: 'Available from', occupied_until: 'Until' };
	const title = (change: CommitmentMoveChange) => change.root ? entry.item_title : titles[change.item_id] ?? 'Linked item';
	const role = (change: CommitmentMoveChange) => `${change.root ? '' : 'Linked · '}${roleNames[change.role]}`;
	// An all-day target moves by the day; everything else keeps its time.
	const when = (change: CommitmentMoveChange, value: string) => change.root && entry.all_day ? shortDay(roleDay(value, zone), todayIn(zone)) : shortRoleDate(value, zone);
	const outside = (change: CommitmentMoveChange) => !!preview.violations?.some(violation => violation.item_id === change.item_id && violation.role === change.role);
	$effect(() => {
		const initial = initialReason;
		untrack(() => { reason = initial; });
	});
	function submit(event: SubmitEvent) {
		event.preventDefault();
		if (pending) return;
		reasonError = preview.renegotiates_deadline && !reason.trim() ? 'Enter a reason.' : '';
		boundsError = violations && !accepted ? 'Accept dates outside their bounds to move.' : '';
		if (reasonError || boundsError) return;
		onsubmit(reason.trim() || undefined, violations ? true : undefined);
	}
</script>

{#snippet move(change: CommitmentMoveChange, kind: 'change' | 'left')}
	<li class="change" data-change={kind === 'change' ? '' : undefined} data-left={kind === 'left' ? '' : undefined}>
		<div class="what">
			<div class="t">{title(change)}</div>
			<div class="r">{role(change)}</div>
		</div>
		<div class="mv">{#if change.from}{when(change, change.from)}{' '}{/if}→ <b>{when(change, change.to)}</b></div>
		{#if kind === 'change' && outside(change)}<span class="outside">Outside bound</span>{/if}
	</li>
{/snippet}

<DialogShell open {mobile} {pending} title="Review move" subject={[entry.item_title, entry.workspace_name].filter(Boolean).join(' · ')} subjectRef={entry.item_ref} maxWidth="560px" {onclose}>
	<form id={`${id}-form`} onsubmit={submit} novalidate>
		{#if preview.renegotiates_deadline || violations}
			<div class="callout">
				<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 4l9 16H3L12 4z" /><path d="M12 10v4M12 17v.5" /></svg>
				<span>
					{#if preview.renegotiates_deadline}<b>This move changes the hard deadline.</b>{/if}
					{#if violations}{violations} linked item{violations === 1 ? ' would move outside its bounds.' : 's would move outside their bounds.'}{/if}
				</span>
			</div>
		{/if}
		<div class="field">
			<h3>What moves</h3>
			<ul class="list">{#each changes as change, index (index)}{@render move(change, 'change')}{/each}</ul>
		</div>
		{#if preview.left_in_place?.length}
			<div class="field">
				<h3>Not moved (finished)</h3>
				<ul class="list">{#each preview.left_in_place as change, index (index)}{@render move(change, 'left')}{/each}</ul>
			</div>
		{/if}
		{#if preview.renegotiates_deadline}
			<div class="field">
				<label class="lab" for={`${id}-reason`}>Reason</label>
				<textarea id={`${id}-reason`} bind:value={reason} required disabled={pending} placeholder="Why is the deadline moving?" aria-invalid={!!reasonError} aria-describedby={reasonError ? `${id}-reason-error` : undefined}></textarea>
				{#if reasonError}<p id={`${id}-reason-error`} role="alert">{reasonError}</p>{/if}
			</div>
		{/if}
		{#if violations}
			<div class="field">
				<label class="accept"><input type="checkbox" bind:checked={accepted} disabled={pending} /> Accept dates outside their bounds</label>
				{#if boundsError}<p role="alert">{boundsError}</p>{/if}
			</div>
		{/if}
		{#if error}<p role="alert">{error}</p>{/if}
	</form>
	{#snippet footer()}
		<Button disabled={pending} onclick={() => { if (!pending) onclose(); }}>Cancel</Button>
		<Button variant="primary" type="submit" form={`${id}-form`} disabled={pending}>{preview.renegotiates_deadline ? 'Move and change deadline' : 'Move target'}</Button>
	{/snippet}
</DialogShell>

<style>
	form { display: flex; flex-direction: column; gap: var(--space-4); min-width: 0; }
	p { margin: 6px 0 0; font-size: 13px; }
	.callout { display: flex; gap: var(--space-3); padding: 10px 12px; border: 1px solid color-mix(in srgb, var(--accent-red) 35%, transparent); border-radius: var(--radius); background: color-mix(in srgb, var(--accent-red) 10%, transparent); font-size: 13px; }
	.callout svg { flex: 0 0 auto; margin-top: 2px; color: var(--accent-red); }
	h3, .lab { display: block; margin: 0 0 6px; font-size: 12px; font-weight: 600; color: var(--text-secondary); }
	.list { list-style: none; margin: 0; padding: 0; border: 1px solid var(--border); border-radius: var(--radius); overflow: hidden; }
	.change { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 2px var(--space-3); padding: 10px 12px; overflow-wrap: anywhere; }
	.change + .change { border-top: 1px solid var(--border-subtle); }
	.t { font-weight: 600; }
	.r { font-size: 12px; color: var(--text-muted); }
	.mv { font-size: 12.5px; color: var(--text-secondary); font-variant-numeric: tabular-nums; text-align: right; }
	.mv b { color: var(--text-primary); }
	.outside { grid-column: 2; justify-self: end; padding: 1px 8px; border-radius: 999px; background: color-mix(in srgb, var(--accent-red) 14%, transparent); color: var(--accent-red); font-size: 11.5px; font-weight: 600; }
	textarea { display: block; box-sizing: border-box; width: 100%; min-height: 72px; padding: 6px 10px; font: inherit; font-size: 14px; color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); resize: vertical; }
	[aria-invalid="true"] { border-color: var(--accent-red); }
	.accept { display: flex; align-items: center; gap: 10px; font-size: 14px; }
	@media (pointer: coarse) { .accept { min-height: 44px; } }
	@media (max-width: 768px) { .change { grid-template-columns: minmax(0, 1fr); } .mv { text-align: left; } .outside { grid-column: 1; justify-self: start; } }
</style>
