<script lang="ts">
	import { untrack } from 'svelte';
	import Button from '$lib/components/common/Button.svelte';
	import DialogShell from '$lib/components/common/DialogShell.svelte';
	import { instantPreview, wallToInstant } from '$lib/calendar/presentation';
	import type { AgendaEntry } from '$lib/types';

	let { open, mode, entry, zone, mobile, pending = false, error = '', onclose, onsubmit }: {
		open: boolean; mode: 'snooze' | 'move'; entry: AgendaEntry | null; zone: string; mobile: boolean; pending?: boolean; error?: string;
		onclose: () => void; onsubmit: (until: string, comment?: string) => void;
	} = $props();
	const id = $props.id();
	let wall = $state('');
	let reason = $state('');
	let timeError = $state('');
	const title = $derived(mode === 'move' ? 'Move target' : 'Snooze');
	const preview = $derived.by(() => {
		try { return wall ? instantPreview(wallToInstant(wall, zone), zone) : ''; }
		catch { return ''; }
	});

	$effect(() => {
		const visible = open;
		untrack(() => { if (visible) { wall = ''; reason = ''; timeError = ''; } });
	});

	function submit(event: SubmitEvent) {
		event.preventDefault();
		if (pending) return;
		let until = '';
		try { until = wallToInstant(wall, zone); } catch { /* Invalid or empty input has no instant. */ }
		const ahead = Date.parse(until) - Date.now();
		timeError = !Number.isFinite(ahead) || ahead <= 0 ? 'Choose a time in the future.' : ahead > 366 * 86_400_000 ? 'Choose a time within a year.' : '';
		if (timeError) return;
		onsubmit(until, mode === 'move' ? reason.trim() || undefined : undefined);
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
			<p class="note">The reminder follows the target.</p>
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
	.field-error { margin-top: 6px; font-size: 12px; color: var(--accent-red); }
	@media (max-width: 768px) { input { min-height: 44px; } }
</style>
