<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import { canMove, todayIn, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry } from '$lib/types';

	let { entry, zone, pending = false, disabled = false, onaction }: {
		entry: AgendaEntry; zone: string; pending?: boolean; disabled?: boolean;
		onaction: (entry: AgendaEntry, state: AgendaAction) => void;
	} = $props();
	let open = $state(false);
	let trigger = $state<HTMLElement>();
	// A lapsed occurrence can still be recorded as done; nothing else applies to it.
	const lapsed = $derived(entry.state === 'lapsed');
	const done = $derived(lapsed ? 'Mark done' : 'Done');
	function pick(state: AgendaAction) {
		open = false;
		trigger?.focus();
		onaction(entry, state);
	}
</script>

<div class="actions">
	<Button size="sm" class="done" aria-label={`${done} ${entry.item_ref}`} disabled={pending || disabled} onclick={() => onaction(entry, 'done')}>
		{#if pending}<span class="spinner" aria-hidden="true"></span>{/if}{done}
	</Button>
	{#if !lapsed}
		<Button size="sm" variant="ghost" class="more" aria-label={`Actions ${entry.item_ref}`} data-agenda-actions={entry.item_ref} data-workspace={entry.workspace} data-owner={entry.owner_username} aria-haspopup="menu" aria-expanded={open} disabled={pending || disabled}
			onclick={(event) => { trigger = event.currentTarget; open = !open; }}>
			<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.7" /><circle cx="12" cy="12" r="1.7" /><circle cx="19" cy="12" r="1.7" /></svg>
		</Button>
		<Menu {open} {trigger} mode="portal" sheetOnMobile sheetMenu sheetTitle={entry.item_title} ariaLabel={`Actions ${entry.item_ref}`} onclose={() => open = false}>
			<MenuItem disabled={pending || disabled} onclick={() => pick('forecast')}>{entry.forecast ? 'Forecast…' : 'Set forecast…'}</MenuItem>
			<MenuItem disabled={pending || disabled} onclick={() => pick('skip')}>Skip</MenuItem>
			<MenuItem disabled={pending || disabled} onclick={() => pick('snooze')}>Snooze…</MenuItem>
			{#if canMove(entry)}
				<MenuItem disabled={pending || disabled} onclick={() => pick('move')}>Move target…</MenuItem>
				{#if entry.day !== todayIn(zone)}<MenuItem disabled={pending || disabled} onclick={() => pick('today')}>Bring to today</MenuItem>{/if}
			{:else}
				<MenuItem disabled><span class="hard-note">Legacy hard deadlines cannot be moved.</span></MenuItem>
			{/if}
		</Menu>
	{/if}
	{#if pending}<span class="sr-only">Saving…</span>{/if}
</div>

<style>
	.actions { display: flex; align-items: center; gap: var(--space-1); flex: 0 0 auto; }
	.actions :global(.btn.more) { width: 32px; height: 32px; padding: 0; }
	.spinner { width: 10px; height: 10px; border: 2px solid var(--border-strong); border-top-color: var(--text-primary); border-radius: 50%; animation: spin 0.8s linear infinite; }
	.hard-note { white-space: normal; }
	@keyframes spin { to { transform: rotate(360deg); } }
	@media (prefers-reduced-motion: reduce) { .spinner { animation: none; } }
	@media (pointer: coarse) {
		.actions :global(.btn) { min-height: 44px; }
		.actions :global(.btn.more) { width: 44px; height: 44px; }
	}
</style>
