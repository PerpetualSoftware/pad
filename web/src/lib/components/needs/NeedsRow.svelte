<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import Chip from '$lib/components/common/Chip.svelte';
	import DeferMenu from './DeferMenu.svelte';
	import type { NeedsRow, NeedsView } from '$lib/types';

	interface Props {
		row: NeedsRow;
		view: NeedsView;
		busy?: boolean;
		/** Whether this row's defer menu is open; the page keeps one open at a time. */
		menuOpen?: boolean;
		onmenu?: (open: boolean) => void;
		ondismiss: (row: NeedsRow) => void;
		ondefer: (row: NeedsRow, deferUntil: string) => void;
		onrestore: (row: NeedsRow) => void;
	}

	let { row, view, busy = false, menuOpen = false, onmenu = () => {}, ondismiss, ondefer, onrestore }: Props = $props();

	function ago(iso: string): string {
		const ms = Date.now() - new Date(iso).getTime();
		if (!Number.isFinite(ms) || ms < 0) return 'just now';
		const mins = Math.floor(ms / 60000);
		if (mins < 60) return `${Math.max(mins, 1)}m`;
		const hours = Math.floor(mins / 60);
		if (hours < 48) return `${hours}h`;
		return `${Math.floor(hours / 24)}d`;
	}

	let hints = $derived(
		[row.importance?.hint, row.simplicity?.hint].filter((h): h is string => !!h)
	);

	let snoozeNote = $derived.by(() => {
		const s = row.snooze;
		if (!s) return '';
		if (s.kind === 'defer' && s.defer_until) {
			return `Deferred until ${new Date(s.defer_until).toLocaleString()}`;
		}
		return 'Dismissed';
	});
</script>

<li class="needs-row" data-testid="needs-row" data-ref={row.item.ref}>
	<div class="main">
		<a class="title" href={row.item.path}>{row.item.title}</a>
		<div class="meta">
			<span class="ref">{row.item.ref}</span>
			<span>{row.item.collection}</span>
			{#if row.item.status}<Chip size="sm">{row.item.status}</Chip>{/if}
			<span class="wait" title={row.waiting_since}>waiting {ago(row.waiting_since)}</span>
			{#each hints as hint, i (i)}
				<span data-testid="needs-hint"><Chip size="sm" color="var(--accent-amber)">{hint}</Chip></span>
			{/each}
		</div>
		<p class="why">{row.why.text}{#if row.why.by} <span class="by">({row.why.by})</span>{/if}</p>
		{#if view === 'hidden' && snoozeNote}<p class="snooze">{snoozeNote}</p>{/if}
	</div>
	<div class="actions">
		{#if view === 'hidden'}
			<Button size="sm" disabled={busy} onclick={() => onrestore(row)}>Restore</Button>
		{:else}
			<Button size="sm" variant="ghost" disabled={busy} onclick={() => ondismiss(row)}>Dismiss</Button>
			<div class="defer-wrap">
				<Button
					size="sm"
					variant="ghost"
					disabled={busy}
					aria-haspopup="true"
					aria-expanded={menuOpen}
					onclick={() => onmenu(!menuOpen)}
				>
					Defer
				</Button>
				{#if menuOpen}
					<div class="defer-pop">
						<DeferMenu
							onclose={() => onmenu(false)}
							onpick={(iso) => {
								onmenu(false);
								ondefer(row, iso);
							}}
						/>
					</div>
				{/if}
			</div>
		{/if}
	</div>
</li>

<style>
	.needs-row {
		display: flex;
		justify-content: space-between;
		gap: var(--space-4);
		padding: var(--space-3) var(--space-4);
		border-bottom: 1px solid var(--border);
		list-style: none;
	}
	.main {
		min-width: 0;
		flex: 1;
	}
	.title {
		font-weight: 600;
		color: var(--text-primary);
		text-decoration: none;
	}
	.title:hover {
		text-decoration: underline;
	}
	.meta {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
		margin-top: var(--space-1);
		font-size: 0.8rem;
		color: var(--text-muted);
	}
	.why,
	.snooze {
		margin: var(--space-1) 0 0;
		font-size: 0.85rem;
		color: var(--text-secondary);
	}
	.by {
		color: var(--text-muted);
	}
	.actions {
		display: flex;
		align-items: flex-start;
		gap: var(--space-1);
		flex-shrink: 0;
	}
	.defer-wrap {
		position: relative;
	}
	.defer-pop {
		position: absolute;
		right: 0;
		top: calc(100% + 4px);
		z-index: 20;
	}
</style>
