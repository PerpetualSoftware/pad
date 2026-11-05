<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import { canMove } from '$lib/calendar/presentation';
	import type { AgendaEntry, ScheduleResolveState } from '$lib/types';

	let { entry, pending = false, disabled = false, onaction }: {
		entry: AgendaEntry; pending?: boolean; disabled?: boolean;
		onaction: (entry: AgendaEntry, state: ScheduleResolveState) => void;
	} = $props();
	let open = $state(false);
	let trigger = $state<HTMLElement>();
	function pick(state: ScheduleResolveState) {
		open = false;
		trigger?.focus();
		onaction(entry, state);
	}
</script>

<div class="actions">
	<Button size="sm" aria-label={`Done ${entry.item_ref}`} disabled={pending || disabled} onclick={() => onaction(entry, 'done')}>Done</Button>
	<Button size="sm" aria-label={`Actions ${entry.item_ref}`} aria-haspopup="menu" aria-expanded={open} disabled={pending || disabled}
		onclick={(event) => { trigger = event.currentTarget; open = !open; }}>Actions</Button>
	<Menu {open} {trigger} mode="portal" ariaLabel={`Actions ${entry.item_ref}`} onclose={() => open = false}>
		<MenuItem disabled={pending || disabled} onclick={() => pick('skip')}>Skip</MenuItem>
		<MenuItem disabled={pending || disabled} onclick={() => pick('snooze')}>Snooze…</MenuItem>
		{#if canMove(entry)}
			<MenuItem disabled={pending || disabled} onclick={() => pick('move')}>Move…</MenuItem>
		{:else}
			<MenuItem disabled><span class="hard-note">Hard deadlines cannot be moved.</span></MenuItem>
		{/if}
	</Menu>
	{#if pending}<span class="saving">Saving…</span>{/if}
</div>

<style>
	.actions { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-1); }
	.saving { color: var(--text-secondary); font-size: 0.85rem; }
	.hard-note { white-space: normal; }
</style>
