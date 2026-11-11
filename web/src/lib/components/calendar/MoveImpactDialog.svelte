<script lang="ts">
	import { tick, untrack } from 'svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import { dateTime } from '$lib/calendar/presentation';
	import type { AgendaEntry, CommitmentMoveChange, CommitmentMoveResult } from '$lib/types';
	let { entry, preview, zone, titles, initialReason = '', pending, error, onclose, onsubmit }: {
		entry: AgendaEntry; preview: CommitmentMoveResult; zone: string; titles: Record<string, string>;
		initialReason?: string; pending: boolean; error: string; onclose: () => void;
		onsubmit: (reason?: string, acknowledge?: boolean) => void;
	} = $props();
	const id = $props.id();
	let reason = $state('');
	let accepted = $state(false);
	let reasonError = $state('');
	let boundsError = $state('');
	let heading: HTMLHeadingElement | undefined;
	const changes = $derived([...preview.changes.filter(change => change.root), ...preview.changes.filter(change => !change.root)]);
	const roleNames = { target: '', deadline: 'Deadline', earliest: 'Available from', occupied_until: 'Until' };
	const title = (change: CommitmentMoveChange) => change.root ? entry.item_title : `${titles[change.item_id] ?? 'Linked item'} (linked)`;
	$effect(() => {
		const initial = initialReason;
		untrack(() => { reason = initial; void tick().then(() => heading?.focus()); });
	});
	function submit(event: SubmitEvent) {
		event.preventDefault();
		if (pending) return;
		reasonError = preview.renegotiates_deadline && !reason.trim() ? 'Enter a reason.' : '';
		boundsError = preview.violations?.length && !accepted ? 'Accept dates outside their bounds to move.' : '';
		if (reasonError || boundsError) return;
		onsubmit(reason.trim() || undefined, preview.violations?.length ? true : undefined);
	}
</script>

<Modal open onclose={() => { if (!pending) onclose(); }} labelledby={`${id}-heading`}>
	<form onsubmit={submit} novalidate>
		<h2 id={`${id}-heading`} tabindex="-1" bind:this={heading}>{preview.renegotiates_deadline ? 'Review deadline change' : 'Review move'}</h2>
		{#if preview.renegotiates_deadline}<p>This move changes a hard deadline.</p>{/if}
		<ul>
			{#each changes as change, index (index)}
				<li>{title(change)}{roleNames[change.role] ? ` · ${roleNames[change.role]}` : ''} → {dateTime(change.to, zone, change.root && entry.all_day)}
					{#if preview.violations?.some(violation => violation.item_id === change.item_id && violation.role === change.role)}<strong>Outside its bound</strong>{/if}
				</li>
			{/each}
		</ul>
		{#if preview.left_in_place?.length}
			<h3>Not moved (finished)</h3>
			<ul>{#each preview.left_in_place as change, index (index)}<li>{title(change)}{roleNames[change.role] ? ` · ${roleNames[change.role]}` : ''} → {dateTime(change.to, zone)}</li>{/each}</ul>
		{/if}
		{#if preview.renegotiates_deadline}
			<label for={`${id}-reason`}>Reason</label>
			<textarea id={`${id}-reason`} bind:value={reason} required disabled={pending} aria-invalid={!!reasonError} aria-describedby={reasonError ? `${id}-reason-error` : undefined}></textarea>
			{#if reasonError}<p id={`${id}-reason-error`} role="alert">{reasonError}</p>{/if}
		{/if}
		{#if preview.violations?.length}
			<label><input type="checkbox" bind:checked={accepted} disabled={pending} /> Accept dates outside their bounds</label>
			{#if boundsError}<p role="alert">{boundsError}</p>{/if}
		{/if}
		{#if error}<p role="alert">{error}</p>{/if}
		<div class="footer"><Button disabled={pending} onclick={onclose}>Cancel</Button><Button type="submit" disabled={pending}>Move</Button></div>
	</form>
</Modal>

<style>
	form { padding: var(--space-5); overflow-y: auto; }
	h2 { font-size: 1.2rem; margin-top: 0; }
	li { margin-bottom: var(--space-2); overflow-wrap: anywhere; }
	li strong { display: block; }
	label { display: block; margin: var(--space-3) 0 var(--space-2); }
	textarea { box-sizing: border-box; width: 100%; min-height: 6rem; font: inherit; color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); }
	.footer { display: flex; justify-content: flex-end; gap: var(--space-2); margin-top: var(--space-4); }
	[role="alert"] { color: var(--accent-red); }
</style>
