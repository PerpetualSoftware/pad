<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import Button from '$lib/components/common/Button.svelte';
	import PageHeader from '$lib/components/common/PageHeader.svelte';
	import NeedsRowView from '$lib/components/needs/NeedsRow.svelte';
	import type { NeedsRow, NeedsSort, NeedsView, NeedsWorkspaceCount } from '$lib/types';

	// "Needs you" (TASK-3923, IDEA-3906 unit A6): what is waiting on this person
	// across every workspace, oldest first by default. Dismiss and Defer hide a
	// row for this person only; Restore brings it back.

	const SORTS: { value: NeedsSort; label: string }[] = [
		{ value: 'age', label: 'Oldest waiting' },
		{ value: 'importance', label: 'Importance' },
		{ value: 'simplicity', label: 'Simplicity' }
	];

	let sort = $state<NeedsSort>('age');
	let view = $state<NeedsView>('active');

	let rows = $state<NeedsRow[]>([]);
	let total = $state(0);
	let hidden = $state(0);
	let workspaces = $state<NeedsWorkspaceCount[]>([]);
	let degraded = $state<string[]>([]);
	let nextCursor = $state('');
	let loading = $state(true);
	let loadingMore = $state(false);
	let loadError = $state('');
	let actionError = $state('');
	let busy = $state<Record<string, boolean>>({});
	// The one row whose defer menu is open; opening another closes it.
	let openMenu = $state('');

	// Every list request takes a ticket; only the newest ticket may write state,
	// so a slow response for an old sort or view cannot overwrite a newer one.
	let ticket = 0;

	// Rows this person acted on, per view, with the order the action COMPLETED
	// in. A list request that started before that cannot be trusted about the
	// row (its page may predate the PUT), so its rows are filtered; a request
	// that started after is served by the committed state and passes untouched.
	let actionsDone = 0;
	const acted: Record<NeedsView, Map<string, number>> = { active: new Map(), hidden: new Map() };

	function trusted(rs: NeedsRow[], v: NeedsView, startedAt: number): NeedsRow[] {
		const m = acted[v];
		if (m.size === 0) return rs;
		return rs.filter((r) => (m.get(keyOf(r)) ?? 0) <= startedAt);
	}

	async function load(s: NeedsSort, v: NeedsView) {
		const mine = ++ticket;
		const startedAt = actionsDone;
		let stale = false;
		loading = true;
		loadingMore = false;
		loadError = '';
		actionError = '';
		try {
			const res = await api.needs.list({ sort: s, view: v });
			if (mine !== ticket) return;
			stale = startedAt < actionsDone;
			rows = trusted(res.rows ?? [], v, startedAt);
			total = res.total ?? 0;
			hidden = res.hidden ?? 0;
			workspaces = res.workspaces ?? [];
			degraded = res.degraded ?? [];
			nextCursor = res.next_cursor ?? '';
		} catch (e) {
			if (mine !== ticket) return;
			// Nothing from the previous view survives next to the error. `hidden`
			// does: it counts everything this person hid, whatever the sort or
			// view, so it is still true, and 0 would be a false "Hidden (0)".
			rows = [];
			total = 0;
			workspaces = [];
			degraded = [];
			nextCursor = '';
			loadError = e instanceof Error ? e.message : 'Failed to load';
		} finally {
			if (mine === ticket) loading = false;
		}
		// THE CLASS FIX (TASK-3923 r3): a response whose request started before
		// the latest completed dismiss/defer/restore was served by a state that may
		// predate it. `trusted` keeps the acted row from flickering back, but its
		// counts (and a destination view's rows) can still be wrong, so ask again:
		// that request starts after every completed action, so it is authoritative.
		// Exactly one refetch per stale response, and it is not stale itself (its
		// startedAt is the current actionsDone), so it cannot trigger another
		// unless a NEW action completes meanwhile. It goes through the ticket
		// guard like any load: a later sort or view change supersedes it.
		if (stale) void load(s, v);
	}

	async function loadMore() {
		if (!nextCursor || loadingMore) return;
		const mine = ticket;
		const startedAt = actionsDone;
		const v = view;
		let stale = false;
		loadingMore = true;
		try {
			const res = await api.needs.list({ sort, view: v, cursor: nextCursor });
			if (mine !== ticket) return;
			// Counts are not written from this response; only the rows are filtered.
			stale = startedAt < actionsDone;
			const have = new Set(rows.map(keyOf));
			rows = [...rows, ...trusted(res.rows ?? [], v, startedAt).filter((r) => !have.has(keyOf(r)))];
			nextCursor = res.next_cursor ?? '';
		} catch (e) {
			if (mine !== ticket) return;
			actionError = e instanceof Error ? e.message : 'Failed to load more';
		} finally {
			if (mine === ticket) loadingMore = false;
		}
		// Same class fix as `load`. A stale Load more reloads PAGE 1 of the
		// current view, which drops the extra pages already loaded and resets
		// pagination. That is acceptable: it only happens when an action completed
		// during the Load more request, which is rare, and the reload's rows and
		// counts are authoritative.
		if (stale) void load(sort, v);
	}

	// The one effect: refetch whenever the sort or the view changes.
	$effect(() => {
		const s = sort;
		const v = view;
		untrack(() => {
			void load(s, v);
		});
	});

	function keyOf(row: NeedsRow): string {
		return `${row.workspace.slug}/${row.item.ref}`;
	}

	/** The row left this view: drop it and move the counts by one, locally. */
	function settle(row: NeedsRow, hiddenDelta: number) {
		rows = rows.filter((r) => keyOf(r) !== keyOf(row));
		total = Math.max(0, total - 1);
		hidden = Math.max(0, hidden + hiddenDelta);
		workspaces = workspaces.map((w) =>
			w.slug === row.workspace.slug ? { ...w, count: Math.max(0, w.count - 1) } : w
		);
	}

	/** `from` is the view the action is taken in; the row leaves it for the other. */
	async function act(row: NeedsRow, call: () => Promise<unknown>, from: NeedsView) {
		const k = keyOf(row);
		if (busy[k]) return;
		busy = { ...busy, [k]: true };
		actionError = '';
		try {
			await call();
			const to: NeedsView = from === 'active' ? 'hidden' : 'active';
			acted[from].set(k, ++actionsDone);
			acted[to].delete(k);
			if (openMenu === k) openMenu = '';
			// Counts belong to the list that showed the row; a list loaded since
			// (a quick sort or view change) never counted it as ours to adjust.
			if (view === from && rows.some((r) => keyOf(r) === k)) settle(row, from === 'active' ? 1 : -1);
		} catch (e) {
			actionError = e instanceof Error ? e.message : 'That did not go through';
		} finally {
			const { [k]: _gone, ...rest } = busy;
			busy = rest;
		}
	}

	const dismiss = (row: NeedsRow) =>
		act(row, () => api.needs.snooze(row.workspace.slug, row.item.ref, { kind: 'dismiss', marker: row.marker }), 'active');
	const defer = (row: NeedsRow, deferUntil: string) =>
		act(
			row,
			() =>
				api.needs.snooze(row.workspace.slug, row.item.ref, {
					kind: 'defer',
					defer_until: deferUntil,
					marker: row.marker
				}),
			'active'
		);
	const restore = (row: NeedsRow) =>
		act(row, () => api.needs.restore(row.workspace.slug, row.item.ref), 'hidden');

	// Groups by first appearance, so the server's sort carries through.
	let groups = $derived.by(() => {
		const out: { slug: string; name: string; count: number; rows: NeedsRow[] }[] = [];
		const index = new Map<string, number>();
		for (const row of rows) {
			let i = index.get(row.workspace.slug);
			if (i === undefined) {
				i = out.length;
				index.set(row.workspace.slug, i);
				const wc = workspaces.find((w) => w.slug === row.workspace.slug);
				out.push({ slug: row.workspace.slug, name: row.workspace.name, count: wc?.count ?? 0, rows: [] });
			}
			out[i].rows.push(row);
		}
		for (const g of out) {
			const wc = workspaces.find((w) => w.slug === g.slug);
			g.count = wc ? wc.count : g.rows.length;
		}
		return out;
	});
</script>

<svelte:head><title>Needs you · Pad</title></svelte:head>

<PageHeader
	title={view === 'hidden' ? 'Hidden' : 'Needs you'}
	count={loading ? undefined : total}
	description={view === 'hidden'
		? 'Dismissed items come back when something new happens on them. Deferred items come back at the time you chose, whatever happens.'
		: 'What is waiting on you, across every workspace.'}
>
	{#snippet actions()}
		{#if view === 'hidden'}
			<Button variant="secondary" onclick={() => (view = 'active')}>Back to needs you</Button>
		{:else}
			<Button variant="secondary" onclick={() => (view = 'hidden')}>Hidden ({hidden})</Button>
		{/if}
	{/snippet}
</PageHeader>

<div class="sorts" role="group" aria-label="Sort by">
	{#each SORTS as s (s.value)}
		<Button
			size="sm"
			variant={sort === s.value ? 'primary' : 'ghost'}
			aria-pressed={sort === s.value}
			onclick={() => (sort = s.value)}
		>
			{s.label}
		</Button>
	{/each}
</div>

{#if degraded.length > 0}
	<p class="degraded" role="status" data-testid="needs-degraded">
		Scoring was unavailable for {degraded.join(', ')}; those rows may be out of order for importance and simplicity.
	</p>
{/if}
{#if actionError}<p class="error" role="alert" data-testid="needs-action-error">{actionError}</p>{/if}

{#if loading}
	<p class="muted">Loading…</p>
{:else if loadError}
	<p class="error" role="alert">{loadError}</p>
	<Button onclick={() => load(sort, view)}>Try again</Button>
{:else if groups.length === 0}
	<p class="muted" data-testid="needs-empty">
		{view === 'hidden' ? 'Nothing is hidden.' : 'Nothing needs you right now.'}
	</p>
{:else}
	{#each groups as g (g.slug)}
		<section class="group" aria-label={g.name} data-testid="needs-group">
			<h2>{g.name} <span class="count" data-testid="needs-group-count">{g.count}</span></h2>
			<ul>
				{#each g.rows as row (keyOf(row))}
					<NeedsRowView
						{row}
						{view}
						menuOpen={openMenu === keyOf(row)}
						onmenu={(open) => (openMenu = open ? keyOf(row) : openMenu === keyOf(row) ? '' : openMenu)}
						busy={!!busy[keyOf(row)]}
						ondismiss={dismiss}
						ondefer={defer}
						onrestore={restore}
					/>
				{/each}
			</ul>
		</section>
	{/each}
	{#if nextCursor}
		<Button disabled={loadingMore} onclick={loadMore}>{loadingMore ? 'Loading…' : 'Load more'}</Button>
	{/if}
{/if}

<style>
	.sorts {
		display: flex;
		gap: var(--space-1);
		margin: var(--space-3) 0 var(--space-4);
	}
	.group {
		margin-bottom: var(--space-6);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
	}
	.group h2 {
		margin: 0;
		padding: var(--space-3) var(--space-4);
		font-size: 0.95rem;
		border-bottom: 1px solid var(--border);
	}
	.group ul {
		margin: 0;
		padding: 0;
	}
	.count {
		margin-left: var(--space-1);
		color: var(--text-muted);
		font-weight: 400;
	}
	.muted {
		color: var(--text-muted);
	}
	.error {
		color: var(--accent-red);
	}
	.degraded {
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--accent-amber);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85rem;
	}
</style>
