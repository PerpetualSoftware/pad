<script lang="ts">
	import { untrack } from 'svelte';
	import { api, isUpdateConflictError } from '$lib/api/client';
	import Button from '$lib/components/common/Button.svelte';
	import PageHeader from '$lib/components/common/PageHeader.svelte';
	import NeedsRowView from '$lib/components/needs/NeedsRow.svelte';
	import { loadGiveTargets, type GiveTarget, type GiveTargets } from '$lib/components/needs/NotMinePicker.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { Item, ItemUpdate, NeedsRow, NeedsSort, NeedsView, NeedsWorkspaceCount } from '$lib/types';

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
	// A quiet refetch is in flight. It shows "Loading…" only when there are no
	// rows to keep on screen (H-F1: the empty text never shows with a cursor).
	let refetching = $state(false);
	let loadingMore = $state(false);
	let loadError = $state('');
	let actionError = $state('');
	let busy = $state<Record<string, boolean>>({});
	// The one open row menu: a row key for its defer menu (or the compact
	// row's sheet), the key plus MORE for its ⋯ menu. Opening another closes it.
	let openMenu = $state('');
	const MORE = '#more';

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

	// AT MOST ONE refetch per completed action (TASK-3923). Three paths want
	// one: a stale response accepted, an action that completed without
	// settling the current rows (H-F2), and a settle that emptied the rows while
	// a cursor remains (H-F1). They all go through here; `refetchedFor` records
	// the actionsDone a refetch was issued for, so whichever fires first wins.
	// A refetch starts after the action completed, so it is never stale itself.
	let refetchedFor = -1;
	function requestRefetch() {
		if (refetchedFor >= actionsDone) return;
		refetchedFor = actionsDone;
		void load(sort, view, true);
	}

	// `quiet` is used ONLY by requestRefetch (TASK-3923): an automatic
	// correction must not look like a page reload. It leaves `loading`, the
	// errors and the rendered list alone until its response lands (through the
	// ticket guard, as any load), and a FAILED quiet load changes nothing on
	// screen. Mount, sort and view toggles and Try again are not quiet.
	async function load(s: NeedsSort, v: NeedsView, quiet = false) {
		const mine = ++ticket;
		const startedAt = actionsDone;
		let stale = false;
		loadingMore = false;
		if (quiet) {
			refetching = true;
		} else {
			refetching = false;
			loading = true;
			loadError = '';
			actionError = '';
		}
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
			loadError = '';
		} catch (e) {
			if (mine !== ticket) return;
			if (quiet) {
				// Keep the rows, counts, cursor and degraded list exactly as they are.
				console.warn('needs: background refresh failed');
				return;
			}
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
			if (mine === ticket) {
				loading = false;
				refetching = false;
			}
		}
		// THE CLASS FIX (TASK-3923): a response whose request started before
		// the latest completed dismiss/defer/restore was served by a state that may
		// predate it. `trusted` keeps the acted row from flickering back, but its
		// counts (and a destination view's rows) can still be wrong, so ask again:
		// that request starts after every completed action, so it is authoritative.
		// Exactly one refetch per stale response, and it is not stale itself (its
		// startedAt is the current actionsDone), so it cannot trigger another
		// unless a NEW action completes meanwhile. It goes through the ticket
		// guard like any load: a later sort or view change supersedes it.
		if (stale) {
			requestRefetch();
			// Filtered down to nothing with pages left: page 1 is the next work.
			if (rows.length === 0 && nextCursor) requestRefetch();
		}
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
		if (stale) requestRefetch();
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

	/** The row's action completed and it moved from `from` to the other view. */
	function landed(row: NeedsRow, from: NeedsView) {
		const k = keyOf(row);
		const to: NeedsView = from === 'active' ? 'hidden' : 'active';
		acted[from].set(k, ++actionsDone);
		acted[to].delete(k);
		if (openMenu === k || openMenu === k + MORE) openMenu = '';
		// Counts belong to the list that showed the row; a list loaded since
		// (a quick sort or view change) never counted it as ours to adjust.
		if (view === from && rows.some((r) => keyOf(r) === k)) {
			settle(row, from === 'active' ? 1 : -1);
			// H-F1: every loaded row is gone but the cursor says more waits. The
			// server now excludes the held rows, so page 1 is the next work.
			if (rows.length === 0 && nextCursor) requestRefetch();
		} else {
			// H-F2: the current rows were not settled (another view, or a list
			// that started and landed while this call was in flight and so was
			// accepted as fresh). Ask the server again.
			requestRefetch();
		}
	}

	const errMsg = (e: unknown, fallback = 'That did not go through') => (e instanceof Error ? e.message : fallback);

	/** Runs `fn` with the row marked busy; a second action on it meanwhile is dropped. */
	async function withBusy(row: NeedsRow, fn: () => Promise<void>) {
		const k = keyOf(row);
		if (busy[k]) return;
		busy = { ...busy, [k]: true };
		actionError = '';
		try {
			await fn();
		} finally {
			const { [k]: _gone, ...rest } = busy;
			busy = rest;
		}
	}

	/** `from` is the view the action is taken in; the row leaves it for the other. */
	function act(row: NeedsRow, call: () => Promise<unknown>, from: NeedsView, after?: () => void) {
		return withBusy(row, async () => {
			try {
				await call();
			} catch (e) {
				actionError = errMsg(e);
				return;
			}
			landed(row, from);
			after?.();
		});
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
	// A not-mine row is restored by deleting the mark, a snoozed one by
	// deleting the snooze; a row holds at most one of the two.
	const restore = (row: NeedsRow) =>
		act(
			row,
			() =>
				row.not_mine
					? api.needs.undoNotMine(row.workspace.slug, row.item.ref)
					: api.needs.restore(row.workspace.slug, row.item.ref),
			'hidden'
		);

	// ── Not mine (TASK-4643) ────────────────────────────────────────────────
	// The marker goes back exactly as the server built it for the row
	// (seen_at, collection and the caller's own assignee id included); the
	// client adds nothing, and `source` is the server's to set.

	/** What a give-to changed, so Undo can put back only those columns. */
	interface Gave {
		kind: GiveTarget['kind'];
		prev: Item;
		patched: Item;
	}

	// Roles and members per workspace, loaded once per page visit. A plain Map:
	// nothing renders from it.
	const targetCache = new Map<string, Promise<GiveTargets>>();
	function loadTargets(ws: string): Promise<GiveTargets> {
		let p = targetCache.get(ws);
		if (!p) {
			p = loadGiveTargets(ws);
			targetCache.set(ws, p);
			// A load with a failed half is not kept; the next open tries again.
			p.then(
				(t) => {
					if (t.rolesError || t.peopleError) targetCache.delete(ws);
				},
				() => targetCache.delete(ws)
			);
		}
		return p;
	}

	function offerUndo(row: NeedsRow, message: string, gave?: Gave) {
		toastStore.show(message, 'success', undefined, undefined, {
			label: 'Undo',
			onAction: () => void undoNotMine(row, gave)
		});
	}

	const notMine = (row: NeedsRow) =>
		act(row, () => api.needs.notMine(row.workspace.slug, row.item.ref, { marker: row.marker }), 'active', () =>
			offerUndo(row, `${row.item.ref} marked not mine`)
		);

	/**
	 * Give the item to someone else, then mark it not mine. The reassignment
	 * goes first and carries the row's seq, so a change made since the list was
	 * read refuses it and nothing is written. The previous assignee is read
	 * just before, for Undo: the row never names it.
	 */
	const give = (row: NeedsRow, target: GiveTarget) =>
		withBusy(row, async () => {
			const ws = row.workspace.slug;
			const ref = row.item.ref;
			let prev: Item;
			let patched: Item;
			try {
				prev = await api.items.get(ws, ref);
				const expected_seq = row.item.seq ?? prev.seq;
				const body: ItemUpdate =
					target.kind === 'role'
						? { agent_role_id: target.id, clear_assigned_user: true, expected_seq }
						: { assigned_user_id: target.id, expected_seq };
				patched = await api.items.update(ws, ref, body);
			} catch (e) {
				actionError = isUpdateConflictError(e)
					? 'The item changed since you loaded the list. Reload and try again.'
					: errMsg(e);
				return;
			}
			try {
				await api.needs.notMine(ws, ref, { marker: row.marker });
			} catch (e) {
				// The reassignment stands. Whether the row still needs the caller is
				// the server's call (an open mention keeps it), so ask it once.
				actionError = `Gave ${ref} to ${target.label}, but marking it not mine failed: ${errMsg(e)}`;
				actionsDone++;
				requestRefetch();
				return;
			}
			landed(row, 'active');
			offerUndo(row, `${ref} given to ${target.label} and marked not mine`, { kind: target.kind, prev, patched });
		});

	/**
	 * Undo: delete the mark, then put back the columns a give-to changed,
	 * guarded by the seq the give's PATCH returned. If the item changed after
	 * that, the reassignment is kept and the person is told so.
	 */
	const undoNotMine = (row: NeedsRow, gave?: Gave) =>
		withBusy(row, async () => {
			const ws = row.workspace.slug;
			const ref = row.item.ref;
			const k = keyOf(row);
			try {
				await api.needs.undoNotMine(ws, ref);
			} catch (e) {
				toastStore.show(`Could not undo ${ref}: ${errMsg(e)}`, 'error');
				return;
			}
			if (gave) {
				const body: ItemUpdate = { expected_seq: gave.patched.seq };
				if (gave.kind === 'role') {
					if (gave.prev.agent_role_id) body.agent_role_id = gave.prev.agent_role_id;
					else body.clear_agent_role = true;
				}
				if (gave.prev.assigned_user_id) body.assigned_user_id = gave.prev.assigned_user_id;
				else body.clear_assigned_user = true;
				try {
					await api.items.update(ws, ref, body);
				} catch (e) {
					if (isUpdateConflictError(e)) toastStore.show('The item changed since; the reassignment was kept.', 'info');
					else toastStore.show(`${ref} is no longer marked not mine, but the reassignment was kept: ${errMsg(e)}`, 'error');
				}
			}
			// As a Restore from the hidden view: the row is back in active, and
			// the server says where it now stands.
			acted.hidden.set(k, ++actionsDone);
			acted.active.delete(k);
			if (view === 'hidden' && rows.some((r) => keyOf(r) === k)) settle(row, -1);
			else requestRefetch();
		});

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
		? 'Dismissed items come back when something new happens on them. Deferred items come back at the time you chose, whatever happens. Items you marked not mine come back only when they come back to you: a mention, a waiting status, an assignment or a reminder.'
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

{#if loading || (refetching && rows.length === 0)}
	<p class="muted">Loading…</p>
{:else if loadError}
	<p class="error" role="alert">{loadError}</p>
	<Button onclick={() => load(sort, view)}>Try again</Button>
{:else if groups.length === 0 && !nextCursor}
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
						moreOpen={openMenu === keyOf(row) + MORE}
						onmore={(open) => (openMenu = open ? keyOf(row) + MORE : openMenu === keyOf(row) + MORE ? '' : openMenu)}
						busy={!!busy[keyOf(row)]}
						ondismiss={dismiss}
						ondefer={defer}
						onrestore={restore}
						onnotmine={notMine}
						ongive={give}
						loadtargets={loadTargets}
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
