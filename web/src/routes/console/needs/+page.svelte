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

	// Every list request takes a ticket; only the newest ticket may write state,
	// so a slow response for an old sort or view cannot overwrite a newer one.
	let ticket = 0;

	async function load(s: NeedsSort, v: NeedsView) {
		const mine = ++ticket;
		loading = true;
		loadingMore = false;
		loadError = '';
		actionError = '';
		try {
			const res = await api.needs.list({ sort: s, view: v });
			if (mine !== ticket) return;
			rows = res.rows ?? [];
			total = res.total ?? 0;
			hidden = res.hidden ?? 0;
			workspaces = res.workspaces ?? [];
			degraded = res.degraded ?? [];
			nextCursor = res.next_cursor ?? '';
		} catch (e) {
			if (mine !== ticket) return;
			rows = [];
			nextCursor = '';
			loadError = e instanceof Error ? e.message : 'Failed to load';
		} finally {
			if (mine === ticket) loading = false;
		}
	}

	async function loadMore() {
		if (!nextCursor || loadingMore) return;
		const mine = ticket;
		loadingMore = true;
		try {
			const res = await api.needs.list({ sort, view, cursor: nextCursor });
			if (mine !== ticket) return;
			const have = new Set(rows.map((r) => `${r.workspace.slug}/${r.item.ref}`));
			rows = [...rows, ...(res.rows ?? []).filter((r) => !have.has(`${r.workspace.slug}/${r.item.ref}`))];
			nextCursor = res.next_cursor ?? '';
		} catch (e) {
			if (mine !== ticket) return;
			actionError = e instanceof Error ? e.message : 'Failed to load more';
		} finally {
			if (mine === ticket) loadingMore = false;
		}
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

	async function act(row: NeedsRow, call: () => Promise<unknown>, hiddenDelta: number) {
		const k = keyOf(row);
		if (busy[k]) return;
		busy = { ...busy, [k]: true };
		actionError = '';
		const mine = ticket;
		try {
			await call();
			if (mine === ticket) settle(row, hiddenDelta);
		} catch (e) {
			actionError = e instanceof Error ? e.message : 'That did not go through';
		} finally {
			const { [k]: _gone, ...rest } = busy;
			busy = rest;
		}
	}

	const dismiss = (row: NeedsRow) =>
		act(row, () => api.needs.snooze(row.workspace.slug, row.item.ref, { kind: 'dismiss', marker: row.marker }), 1);
	const defer = (row: NeedsRow, deferUntil: string) =>
		act(
			row,
			() =>
				api.needs.snooze(row.workspace.slug, row.item.ref, {
					kind: 'defer',
					defer_until: deferUntil,
					marker: row.marker
				}),
			1
		);
	const restore = (row: NeedsRow) =>
		act(row, () => api.needs.restore(row.workspace.slug, row.item.ref), -1);

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
		? 'Items you dismissed or deferred. They return on their own when something new happens.'
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
