<script lang="ts">
	import { onMount } from 'svelte';
	import Button from '$lib/components/common/Button.svelte';
	import { dateAt9, localDateValue, nextMondayAt9, tomorrowAt9 } from './deferTimes';

	interface Props {
		/** Called with the chosen instant as RFC3339. */
		onpick: (deferUntil: string) => void;
		onclose: () => void;
	}

	let { onpick, onclose }: Props = $props();

	let picked = $state('');
	let error = $state('');
	let root: HTMLDivElement | undefined = $state();

	function choose(d: Date) {
		onpick(d.toISOString());
	}

	function chooseDate() {
		const now = new Date();
		const d = dateAt9(picked, now);
		if (!d) {
			error = 'Pick a date.';
			return;
		}
		if (d.getTime() <= now.getTime()) {
			error = 'Pick a date in the future.';
			return;
		}
		error = '';
		choose(d);
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			e.stopPropagation();
			onclose();
		}
	}

	// A press outside closes the menu. The row's own Defer button is outside the
	// menu's root but is the toggle, so it is left to its click handler.
	function onPointerdown(e: PointerEvent) {
		const t = e.target;
		if (!(t instanceof Node) || !root || root.contains(t)) return;
		if (t instanceof Element && t.closest('.defer-wrap')?.contains(root)) return;
		onclose();
	}

	onMount(() => {
		root?.querySelector<HTMLElement>('button')?.focus();
	});
</script>

<svelte:window onkeydown={onKeydown} onpointerdown={onPointerdown} />

<div class="defer-menu" role="group" aria-label="Defer until" bind:this={root}>
	<Button variant="ghost" size="sm" onclick={() => choose(tomorrowAt9())}>Tomorrow 09:00</Button>
	<Button variant="ghost" size="sm" onclick={() => choose(nextMondayAt9())}>Next week (Mon 09:00)</Button>
	<div class="pick">
		<input
			type="date"
			aria-label="Defer until date"
			min={localDateValue(new Date())}
			bind:value={picked}
			oninput={() => (error = '')}
		/>
		<Button size="sm" disabled={!picked} onclick={chooseDate}>Defer</Button>
	</div>
	{#if error}<p class="err" role="alert">{error}</p>{/if}
</div>

<style>
	.defer-menu {
		display: flex;
		flex-direction: column;
		align-items: stretch;
		gap: var(--space-1);
		padding: var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 8px 24px rgba(0, 0, 0, 0.3);
		min-width: 220px;
	}
	.pick {
		display: flex;
		gap: var(--space-2);
		align-items: center;
	}
	.pick input {
		flex: 1;
		min-width: 0;
		background: var(--bg-primary);
		color: var(--text-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		padding: var(--space-1) var(--space-2);
	}
	.err {
		margin: 0;
		color: var(--accent-red);
		font-size: 0.8rem;
	}
</style>
