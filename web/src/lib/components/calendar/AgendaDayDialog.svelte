<script lang="ts">
	import type { AgendaEntry } from '$lib/types';
	import { dayLabel } from '$lib/calendar/monthGrid';
	import type { AgendaAction } from '$lib/calendar/presentation';
	import Modal from '../common/Modal.svelte';
	import BottomSheet from '../common/BottomSheet.svelte';
	import Button from '../common/Button.svelte';
	import AgendaDayList from './AgendaDayList.svelte';
	let { day, entries, zone, mobile, incomplete, error = '', pending, saved, onclose, onaction }: {
		day: string | null; entries: AgendaEntry[]; zone: string; mobile: boolean; incomplete: boolean; error?: string;
		pending: Record<string, boolean>; saved: Record<string, boolean>;
		onclose: () => void; onaction: (entry: AgendaEntry, action: AgendaAction) => void;
	} = $props();
	const uid = $props.id();
</script>

{#snippet body()}
	<div class="detail">
		{#if !mobile}<div class="heading"><h2 id={uid}>{day ? dayLabel(day, true) : ''}</h2><Button onclick={onclose}>Close</Button></div>{/if}
		<p>Time zone: {zone}</p>
		{#if error}<p role="alert">{error}</p>{/if}
		{#if incomplete}<p>Not fully loaded</p>{/if}
		{#if !incomplete && entries.length === 0}<p>No entries on this day.</p>{/if}
		{#if day}<AgendaDayList {day} {entries} {zone} {pending} {saved} {onaction} heading={false} />{/if}
	</div>
{/snippet}
{#if mobile}
	<BottomSheet open={!!day} {onclose} title={day ? dayLabel(day, true) : ''}>{@render body()}</BottomSheet>
{:else}
	<Modal open={!!day} {onclose} labelledby={uid} maxWidth="800px">{@render body()}</Modal>
{/if}

<style>
	.detail { padding: var(--space-4); overflow: auto; overscroll-behavior: contain; }
	.heading { display: flex; align-items: flex-start; justify-content: space-between; gap: var(--space-2); }
	h2 { font-size: 18px; margin: 0; }
	p { color: var(--text-secondary); }
</style>
