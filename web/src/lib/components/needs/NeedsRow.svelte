<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import Chip from '$lib/components/common/Chip.svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import DeferMenu from './DeferMenu.svelte';
	import NotMinePicker, { type GiveTarget, type GiveTargets } from './NotMinePicker.svelte';
	import type { NeedsRow, NeedsView } from '$lib/types';

	interface Props {
		row: NeedsRow;
		showWorkspace?: boolean;
		/** Home on phones keeps actions in an overflow sheet. */
		compact?: boolean;
		view: NeedsView;
		busy?: boolean;
		/** Whether this row's defer menu is open; the page keeps one open at a time. */
		menuOpen?: boolean;
		onmenu?: (open: boolean) => void;
		ondismiss: (row: NeedsRow) => void;
		ondefer: (row: NeedsRow, deferUntil: string) => void;
		onrestore: (row: NeedsRow) => void;
		/**
		 * "Not mine" (TASK-4643). The entries render only when the parent passes
		 * these, so a parent that does not (Home) is unchanged. "Give to…" also
		 * needs `ongive` and the caller's edit right on the item.
		 */
		onnotmine?: (row: NeedsRow) => void;
		ongive?: (row: NeedsRow, target: GiveTarget) => void;
		/** The give-to picker's loader; the page passes a per-workspace cache. */
		loadtargets?: (ws: string) => Promise<GiveTargets>;
		/** Whether the desktop row's ⋯ menu is open (one open menu per page). */
		moreOpen?: boolean;
		onmore?: (open: boolean) => void;
	}

	let {
		row,
		compact = false,
		showWorkspace = false,
		view,
		busy = false,
		menuOpen = false,
		onmenu = () => {},
		ondismiss,
		ondefer,
		onrestore,
		onnotmine,
		ongive,
		loadtargets,
		moreOpen = false,
		onmore = () => {}
	}: Props = $props();

	let trigger = $state<HTMLElement>();
	let moreTrigger = $state<HTMLElement>();
	let deferring = $state(false);
	let giving = $state(false);

	let canGive = $derived(!!onnotmine && !!ongive && row.can_edit === true);

	function pick(target: GiveTarget) {
		giving = false;
		ongive?.(row, target);
	}

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
		if (row.not_mine) return `Not mine since ${new Date(row.not_mine.created_at).toLocaleDateString()}`;
		const s = row.snooze;
		if (!s) return '';
		if (s.kind === 'defer' && s.defer_until) {
			return `Deferred until ${new Date(s.defer_until).toLocaleString()}`;
		}
		return 'Dismissed';
	});
</script>

<li class="needs-row" class:compact data-testid="needs-row" data-ref={row.item.ref}>
	<div class="main">
		<!-- An empty path means the workspace owner no longer resolves (BUG-4050): no route, so no link (href="" would reload this page). -->
		{#if row.item.path}
			<a class="title" href={row.item.path}>{row.item.title}</a>
		{:else}
			<span class="title">{row.item.title}</span>
		{/if}
		<div class="meta">
			{#if showWorkspace}<span>{row.workspace.name}</span>{/if}
			<span class="ref">{row.item.ref}</span>
			<span>{row.item.collection}</span>
			{#if row.item.status}<Chip size="sm">{row.item.status}</Chip>{/if}
			<span class="wait" title={row.waiting_since}>waiting {ago(row.waiting_since)}</span>
			{#each hints as hint, i (i)}
				<span data-testid="needs-hint"><Chip size="sm" color="var(--accent-amber)">{hint}</Chip></span>
			{/each}
		</div>
		<p class="why">{row.why.text}{#if row.why.by}{' '}<span class="by">({row.why.by})</span>{/if}</p>
		{#if view === 'hidden' && snoozeNote}<p class="snooze">{snoozeNote}</p>{/if}
	</div>
	<div class="actions">
		{#if compact && view === 'active'}
			<Button size="sm" variant="ghost" aria-label={`Actions ${row.item.ref}`} aria-haspopup="dialog" aria-expanded={menuOpen} disabled={busy}
				onclick={(event: MouseEvent) => { trigger = event.currentTarget as HTMLElement; deferring = false; giving = false; onmenu(!menuOpen); }}>⋯</Button>
			<Menu open={menuOpen} {trigger} mode="portal" sheetOnMobile sheetTitle={row.item.title} ariaLabel={`Actions ${row.item.ref}`} onclose={() => onmenu(false)} focusKey={deferring ? 'defer' : giving ? 'give' : 'actions'}>
				{#if deferring}
					<DeferMenu onclose={() => onmenu(false)} onpick={(iso) => { onmenu(false); ondefer(row, iso); }} />
				{:else if giving}
					<NotMinePicker ws={row.workspace.slug} load={loadtargets} onpick={(t) => { onmenu(false); pick(t); }} />
				{:else}
					<Button size="sm" variant="ghost" disabled={busy} onclick={() => { onmenu(false); ondismiss(row); }}>Dismiss</Button>
					<Button size="sm" variant="ghost" disabled={busy} onclick={() => deferring = true}>Defer</Button>
					{#if onnotmine}
						<Button size="sm" variant="ghost" disabled={busy} onclick={() => { onmenu(false); onnotmine(row); }}>Not mine</Button>
					{/if}
					{#if canGive}
						<Button size="sm" variant="ghost" disabled={busy} onclick={() => giving = true}>Not mine, give to…</Button>
					{/if}
				{/if}
			</Menu>
		{:else if view === 'hidden'}
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
			{#if onnotmine}
				<Button
					size="sm"
					variant="ghost"
					disabled={busy}
					aria-label={`More actions ${row.item.ref}`}
					aria-haspopup="menu"
					aria-expanded={moreOpen}
					onclick={(event: MouseEvent) => {
						moreTrigger = event.currentTarget as HTMLElement;
						giving = false;
						onmore(!moreOpen);
					}}
				>
					⋯
				</Button>
				<Menu
					open={moreOpen}
					trigger={moreTrigger}
					mode="portal"
					width={240}
					sheetOnMobile
					sheetMenu
					sheetTitle={row.item.title}
					ariaLabel={`More actions ${row.item.ref}`}
					onclose={() => onmore(false)}
					focusKey={giving ? 'give' : 'more'}
				>
					{#if giving}
						<NotMinePicker ws={row.workspace.slug} load={loadtargets} onpick={(t) => { onmore(false); pick(t); }} />
					{:else}
						<MenuItem disabled={busy} onclick={() => { onmore(false); onnotmine(row); }}>Not mine</MenuItem>
						{#if canGive}
							<MenuItem disabled={busy} onclick={() => (giving = true)}>Not mine, give to…</MenuItem>
						{/if}
					{/if}
				</Menu>
			{/if}
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
	.compact { display: grid; grid-template-columns: minmax(0, 1fr) auto; padding: var(--space-3); column-gap: var(--space-2); row-gap: 0; }
	.compact:last-child { border-bottom: 0; }
	.compact .main { display: contents; }
	.compact .title, .compact .why { grid-column: 1 / -1; overflow-wrap: anywhere; }
	.compact .actions { grid-column: 2; grid-row: 2; }
	.compact .meta { grid-column: 1; }
	.main {
		min-width: 0;
		flex: 1;
	}
	.title {
		font-weight: 600;
		color: var(--text-primary);
		text-decoration: none;
	}
	a.title:hover {
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
