<!--
	ScheduleEndDialog: a done or skip that would end the schedule of an item
	that is still open (409 item_still_open, TASK-4537). Nothing has been written
	yet. The choices are the ones the server's details allow:
	- close the item too (done only, when the done field has a delivering close);
	- end the schedule and keep the item open (it stays in Outstanding);
	- acknowledge the occurrence instead (fired and not yet acknowledged);
	- cancel, which writes nothing.
-->
<script lang="ts" module>
	export type ScheduleEndChoice = { kind: 'close'; value: string } | { kind: 'end' } | { kind: 'ack' };
</script>

<script lang="ts">
	import { untrack } from 'svelte';
	import Button from '$lib/components/common/Button.svelte';
	import DialogShell from '$lib/components/common/DialogShell.svelte';
	import type { AgendaEntry, ItemStillOpenDetails } from '$lib/types';

	let { open, entry, details, mobile, pending = false, error = '', onclose, onchoose }: {
		open: boolean; entry: AgendaEntry | null; details: ItemStillOpenDetails | null; mobile: boolean; pending?: boolean; error?: string;
		onclose: () => void; onchoose: (choice: ScheduleEndChoice) => void;
	} = $props();
	const id = $props.id();
	type Kind = ScheduleEndChoice['kind'];
	let kind = $state<Kind>('end');
	let value = $state('');

	const closable = $derived(details?.state === 'done' && (details?.close_options.length ?? 0) > 0);
	const options = $derived(details?.close_options ?? []);
	const title = $derived(`Last occurrence: ${entry?.item_title || 'this item'} is still open`);
	const verb = $derived(details?.state === 'skip' ? 'Skipping' : 'Marking done');
	const primary = $derived(kind === 'close' ? 'Close item' : kind === 'ack' ? 'Acknowledge' : 'End schedule');

	$effect(() => {
		const visible = open, d = details;
		untrack(() => {
			if (!visible || !d) return;
			kind = d.state === 'done' && d.close_options.length ? 'close' : 'end';
			value = d.close_options[0] ?? '';
		});
	});

	function submit(event: SubmitEvent) {
		event.preventDefault();
		if (pending || !details) return;
		if (kind === 'close') { if (closable && value) onchoose({ kind, value }); return; }
		if (kind === 'ack' && !details.ack_available) return;
		onchoose(kind === 'ack' ? { kind } : { kind: 'end' });
	}
</script>

<DialogShell {open} {mobile} {pending} {title} subject={[entry?.item_title, entry?.workspace_name].filter(Boolean).join(' · ')} subjectRef={entry?.item_ref} {onclose}>
	<form id={`${id}-form`} onsubmit={submit}>
		<p class="lead">{verb} ends this item's schedule: nothing is due after this occurrence. The item is still open{#if details?.item_status}{' '}(<span class="status">{details.item_status}</span>){/if}. Nothing has been saved yet.</p>
		<fieldset disabled={pending}>
			<legend class="sr-only">What to do</legend>
			{#if closable}
				<label class="choice" data-choice="close">
					<input type="radio" name={`${id}-choice`} value="close" bind:group={kind} />
					<span><span class="name">Close item too</span><span class="hint">End the schedule, then set the item to {options.length === 1 ? options[0] : 'the status you choose'}.</span></span>
				</label>
				{#if kind === 'close' && options.length > 1}
					<div class="field">
						<label for={`${id}-value`}>Close as</label>
						<select id={`${id}-value`} bind:value>
							{#each options as option (option)}<option value={option}>{option}</option>{/each}
						</select>
					</div>
				{/if}
			{/if}
			<label class="choice" data-choice="end">
				<input type="radio" name={`${id}-choice`} value="end" bind:group={kind} />
				<span><span class="name">End schedule, keep item open</span><span class="hint">It stays in Outstanding until it is closed or dismissed.</span></span>
			</label>
			{#if details?.ack_available}
				<label class="choice" data-choice="ack">
					<input type="radio" name={`${id}-choice`} value="ack" bind:group={kind} />
					<span><span class="name">Acknowledge instead</span><span class="hint">Silence the reminder; the occurrence stays owed.</span></span>
				</label>
			{/if}
		</fieldset>
		{#if error}<p role="alert">{error}</p>{/if}
	</form>
	{#snippet footer()}
		<Button disabled={pending} onclick={() => { if (!pending) onclose(); }}>Cancel</Button>
		<Button variant="primary" type="submit" form={`${id}-form`} disabled={pending || !details}>{primary}</Button>
	{/snippet}
</DialogShell>

<style>
	form { display: flex; flex-direction: column; gap: var(--space-4); min-width: 0; }
	p { margin: 0; }
	.lead { font-size: 13px; color: var(--text-secondary); }
	.status { font-weight: 600; color: var(--text-primary); }
	fieldset { display: flex; flex-direction: column; gap: var(--space-2); margin: 0; padding: 0; border: 0; min-width: 0; }
	.choice { display: flex; align-items: flex-start; gap: var(--space-3); padding: 10px 12px; border: 1px solid var(--border); border-radius: var(--radius); cursor: pointer; }
	.choice:has(input:checked) { border-color: var(--accent-primary); background: color-mix(in srgb, var(--accent-primary) 8%, transparent); }
	.choice input { margin: 2px 0 0; flex: 0 0 auto; }
	.name { display: block; font-size: 13px; font-weight: 600; color: var(--text-primary); }
	.hint { display: block; margin-top: 2px; font-size: 12px; color: var(--text-muted); }
	.field { min-width: 0; padding-left: calc(var(--space-3) + 24px); }
	.field label { display: block; margin-bottom: 6px; font-size: 12px; font-weight: 600; color: var(--text-secondary); }
	select { box-sizing: border-box; width: 100%; min-height: 36px; padding: 6px 10px; color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); font: inherit; font-size: 14px; }
	@media (max-width: 768px) { .choice { min-height: 44px; } select { min-height: 44px; } }
</style>
