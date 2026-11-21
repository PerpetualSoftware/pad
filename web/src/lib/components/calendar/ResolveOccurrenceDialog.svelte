<script lang="ts">
	import { untrack } from 'svelte';
	import Button from '$lib/components/common/Button.svelte';
	import DialogShell from '$lib/components/common/DialogShell.svelte';
	import { instantPreview, wallToInstant } from '$lib/calendar/presentation';
	import type { AgendaEntry } from '$lib/types';

	// recurring: offer "This and future occurrences" in move mode. scope is 'series' only when chosen there.
	let { open, mode, entry, zone, mobile, pending = false, error = '', recurring = false, onclose, onsubmit }: {
		open: boolean; mode: 'snooze' | 'move'; entry: AgendaEntry | null; zone: string; mobile: boolean; pending?: boolean; error?: string; recurring?: boolean;
		onclose: () => void; onsubmit: (until: string, comment?: string, scope?: 'series') => void;
	} = $props();
	const id = $props.id();
	let wall = $state('');
	let reason = $state('');
	let timeError = $state('');
	let scope = $state<'occurrence' | 'series'>('occurrence');
	const scoped = $derived(mode === 'move' && recurring);
	const series = $derived(scoped && scope === 'series');
	const title = $derived(mode === 'move' ? 'Move target' : 'Snooze');
	const preview = $derived.by(() => {
		try { return wall ? instantPreview(wallToInstant(wall, zone), zone) : ''; }
		catch { return ''; }
	});

	$effect(() => {
		const visible = open;
		untrack(() => { if (visible) { wall = ''; reason = ''; timeError = ''; scope = 'occurrence'; } });
	});

	function submit(event: SubmitEvent) {
		event.preventDefault();
		if (pending) return;
		let until = '';
		try { until = wallToInstant(wall, zone); } catch { /* Invalid or empty input has no instant. */ }
		const ahead = Date.parse(until) - Date.now();
		timeError = !Number.isFinite(ahead) || ahead <= 0 ? 'Choose a time in the future.' : ahead > 366 * 86_400_000 ? 'Choose a time within a year.' : '';
		if (timeError) return;
		const comment = mode === 'move' ? reason.trim() || undefined : undefined;
		if (series) onsubmit(until, comment, 'series');
		else onsubmit(until, comment);
	}
</script>

<DialogShell {open} {mobile} {pending} {title} subject={[entry?.item_title, entry?.workspace_name].filter(Boolean).join(' · ')} subjectRef={entry?.item_ref} {onclose}>
	<form id={`${id}-form`} onsubmit={submit} novalidate>
		<div class="field">
			<label for={`${id}-time`}>{mode === 'move' ? 'New date and time' : 'Remind me again at…'}</label>
			<input id={`${id}-time`} type="datetime-local" bind:value={wall} disabled={pending} aria-invalid={!!timeError} aria-describedby={timeError ? `${id}-time-error ${id}-zone` : `${id}-zone`} />
			<p class="hint" id={`${id}-zone`}>{zone}</p>
			{#if timeError}<p class="field-error" id={`${id}-time-error`}>{timeError}</p>{/if}
			{#if preview}<p class="hint">{preview}</p>{/if}
		</div>
		{#if mode === 'move'}
			{#if scoped}
				<fieldset class="scope" disabled={pending}>
					<legend>Apply to</legend>
					<label class="choice"><input type="radio" name={`${id}-scope`} value="occurrence" bind:group={scope} />This occurrence</label>
					<label class="choice"><input type="radio" name={`${id}-scope`} value="series" bind:group={scope} />This and future occurrences</label>
				</fieldset>
			{/if}
			{#if series}
				<p class="note">Later occurrences move to the same time relative to their start; only this occurrence's reminder moves. There is no Undo: move the series back to undo it.</p>
			{:else}
				<p class="note">The reminder follows the target.</p>
			{/if}
			<div class="field">
				<label for={`${id}-reason`}>Reason <span class="optional">(optional)</span></label>
				<textarea id={`${id}-reason`} bind:value={reason} disabled={pending}></textarea>
			</div>
		{/if}
		{#if error}<p role="alert">{error}</p>{/if}
	</form>
	{#snippet footer()}
		<Button disabled={pending} onclick={() => { if (!pending) onclose(); }}>Cancel</Button>
		<Button variant="primary" type="submit" form={`${id}-form`} disabled={pending}>{title}</Button>
	{/snippet}
</DialogShell>

<style>
	form { display: flex; flex-direction: column; gap: var(--space-4); min-width: 0; }
	p { margin: 0; }
	.field { min-width: 0; }
	label { display: block; margin-bottom: 6px; font-size: 12px; font-weight: 600; color: var(--text-secondary); }
	.optional { font-weight: 400; color: var(--text-muted); }
	.hint { margin-top: 6px; font-size: 12px; color: var(--text-muted); overflow-wrap: anywhere; }
	.note { font-size: 13px; color: var(--text-secondary); }
	input, textarea { box-sizing: border-box; width: 100%; min-width: 0; min-height: 36px; padding: 6px 10px; color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); font: inherit; font-size: 14px; }
	textarea { display: block; min-height: 72px; resize: vertical; }
	[aria-invalid="true"] { border-color: var(--accent-red); }
	fieldset { min-width: 0; margin: 0; padding: 0; border: 0; }
	legend { margin-bottom: 6px; padding: 0; font-size: 12px; font-weight: 600; color: var(--text-secondary); }
	.choice { display: flex; align-items: center; gap: 8px; min-height: 32px; margin: 0; font-size: 14px; font-weight: 400; color: var(--text-primary); cursor: pointer; }
	.choice input { flex: none; width: 16px; height: 16px; min-height: 0; margin: 0; padding: 0; accent-color: var(--accent-primary); }
	fieldset:disabled .choice { color: var(--text-muted); cursor: default; }
	.field-error { margin-top: 6px; font-size: 12px; color: var(--accent-red); }
	@media (max-width: 768px) { input { min-height: 44px; } .choice { min-height: 44px; } }
</style>
