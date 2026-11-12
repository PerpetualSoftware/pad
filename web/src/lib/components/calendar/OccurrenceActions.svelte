<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import { canMove, todayIn, type AgendaAction } from '$lib/calendar/presentation';
	import type { AgendaEntry } from '$lib/types';
	import RowMenu from './RowMenu.svelte';

	let { entry, zone, pending = false, disabled = false, onaction }: {
		entry: AgendaEntry; zone: string; pending?: boolean; disabled?: boolean;
		onaction: (entry: AgendaEntry, state: AgendaAction) => void;
	} = $props();
	// A lapsed occurrence can still be recorded as done; nothing else applies to it.
	const lapsed = $derived(entry.state === 'lapsed');
	const done = $derived(lapsed ? 'Mark done' : 'Done');
</script>

<div class="actions">
	<Button size="sm" class="done" aria-label={`${done} ${entry.item_ref}`} disabled={pending || disabled} onclick={() => onaction(entry, 'done')}>
		{#if pending}<span class="spinner" aria-hidden="true"></span>{/if}{done}
	</Button>
	{#if !lapsed}
		<RowMenu label={`Actions ${entry.item_ref}`} title={entry.item_title} disabled={pending || disabled} data-agenda-actions={entry.item_ref} data-workspace={entry.workspace} data-owner={entry.owner_username}>
			{#snippet children(pick)}
				<MenuItem disabled={pending || disabled} onclick={() => pick(() => onaction(entry, 'forecast'))}>{entry.forecast ? 'Forecast…' : 'Set forecast…'}</MenuItem>
				<MenuItem disabled={pending || disabled} onclick={() => pick(() => onaction(entry, 'skip'))}>Skip</MenuItem>
				<MenuItem disabled={pending || disabled} onclick={() => pick(() => onaction(entry, 'snooze'))}>Snooze…</MenuItem>
				{#if canMove(entry)}
					<MenuItem disabled={pending || disabled} onclick={() => pick(() => onaction(entry, 'move'))}>Move target…</MenuItem>
					{#if entry.day !== todayIn(zone)}<MenuItem disabled={pending || disabled} onclick={() => pick(() => onaction(entry, 'today'))}>Bring to today</MenuItem>{/if}
				{:else}
					<MenuItem disabled><span class="hard-note">Legacy hard deadlines cannot be moved.</span></MenuItem>
				{/if}
			{/snippet}
		</RowMenu>
	{/if}
	{#if pending}<span class="sr-only">Saving…</span>{/if}
</div>

<style>
	.actions { display: flex; align-items: center; gap: var(--space-1); flex: 0 0 auto; }
	.spinner { width: 10px; height: 10px; border: 2px solid var(--border-strong); border-top-color: var(--text-primary); border-radius: 50%; animation: spin 0.8s linear infinite; }
	.hard-note { white-space: normal; }
	@keyframes spin { to { transform: rotate(360deg); } }
	@media (prefers-reduced-motion: reduce) { .spinner { animation: none; } }
	@media (pointer: coarse) {
		.actions :global(.btn) { min-height: 44px; }
	}
</style>
