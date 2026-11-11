<script lang="ts">
	import { tick, untrack } from 'svelte';
	import Button from '$lib/components/common/Button.svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import { instantPreview, wallToInstant } from '$lib/calendar/presentation';

	let { open, mode, zone, pending = false, error = '', onclose, onsubmit }: {
		open: boolean; mode: 'snooze' | 'move'; zone: string; pending?: boolean; error?: string;
		onclose: () => void; onsubmit: (until: string, comment?: string) => void;
	} = $props();
	const id = $props.id();
	let wall = $state('');
	let reason = $state('');
	let timeError = $state('');
	let heading: HTMLHeadingElement | undefined;
	const title = $derived(mode === 'move' ? 'Move target' : 'Snooze occurrence');
	const preview = $derived.by(() => {
		try { return wall ? instantPreview(wallToInstant(wall, zone), zone) : ''; }
		catch { return ''; }
	});

	$effect(() => {
		const visible = open;
		untrack(() => {
			if (!visible) return;
			wall = ''; reason = ''; timeError = '';
			void tick().then(() => heading?.focus());
		});
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

<Modal {open} onclose={() => { if (!pending) onclose(); }} labelledby={`${id}-heading`}>
	<form onsubmit={submit} novalidate>
		<h2 id={`${id}-heading`} tabindex="-1" bind:this={heading}>{title}</h2>
		<label for={`${id}-time`}>{mode === 'move' ? 'New date and time' : 'Remind me again at…'}</label>
		<div class="time-field">
			<input id={`${id}-time`} type="datetime-local" bind:value={wall} disabled={pending} aria-invalid={!!timeError} aria-describedby={timeError ? `${id}-time-error ${id}-zone` : `${id}-zone`} />
			<span id={`${id}-zone`}>{zone}</span>
		</div>
		{#if timeError}<p class="field-error" id={`${id}-time-error`}>{timeError}</p>{/if}
		{#if preview}<p class="preview">{preview}</p>{/if}
		{#if mode === 'move'}
			<p>The reminder follows the target.</p>
			<label for={`${id}-reason`}>Reason (optional)</label>
			<textarea id={`${id}-reason`} bind:value={reason} disabled={pending}></textarea>
		{/if}
		{#if error}<p role="alert">{error}</p>{/if}
		<div class="footer">
			<Button disabled={pending} onclick={onclose}>Cancel</Button>
			<Button type="submit" disabled={pending}>{mode === 'move' ? 'Move target' : 'Snooze'}</Button>
		</div>
	</form>
</Modal>

<style>
	form { padding: var(--space-5); overflow-y: auto; min-width: 0; }
	h2 { font-size: 1.2rem; margin: 0 0 var(--space-4); }
	label { display: block; margin: var(--space-3) 0 var(--space-2); font-weight: 600; }
	.time-field { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-2); }
	.time-field span { color: var(--text-secondary); overflow-wrap: anywhere; }
	input, textarea { box-sizing: border-box; min-width: 0; max-width: 100%; padding: var(--space-2); color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); font: inherit; }
	textarea { display: block; width: 100%; min-height: 6rem; resize: vertical; }
	[aria-invalid="true"] { border-color: var(--accent-red); }
	.field-error { color: var(--accent-red); }
	.preview { color: var(--text-secondary); overflow-wrap: anywhere; }
	.footer { display: flex; justify-content: flex-end; flex-wrap: wrap; gap: var(--space-2); margin-top: var(--space-5); }
</style>
