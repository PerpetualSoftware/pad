<script lang="ts">
	// The pad_board view an MCP Apps host renders inline or fullscreen: one
	// collection as status columns. Everything reaches the server through the
	// bridge passed in as `connect`; item text is only ever rendered as escaped
	// text, item bodies are never read, and the only write is an item's status.
	import { onMount, tick, untrack } from 'svelte';
	import type { Bridge, HostContext, connectApp } from './bridge';
	import { uniqueBy } from '../lib/utils/unique';
	import { buildUpdateArgs, isItemResult, parseToolError, type ItemLike, type ToolResultLike } from './model';
	import {
		applyChanges, boardItemFrom, incompleteReshowDue, needsResync, nextDelay, parseBoard, parseChanges, shouldPoll,
		type Board, type BoardChanges, type BoardItem
	} from './boardModel';

	type Notice = { tone: 'info' | 'error'; text: string };
	type ToolError = { code: string; message: string };
	type Reload = 'ok' | 'limited' | 'failed';

	let { connect }: { connect: typeof connectApp } = $props();

	let board = $state.raw<Board | null>(null);
	/** Shown instead of the board when it could not be read, or can no longer be. */
	let failure = $state.raw<string | null>(null);
	let notice = $state.raw<Notice | null>(null);
	/** Item id to the status it is being saved to. Replaced, never mutated. */
	let pending = $state.raw<ReadonlyMap<string, string>>(new Map());
	let host = $state.raw<HostContext>({});
	let visible = $state(true);
	let refreshing = $state(false);
	let slowed = $state(false);
	let syncedAt = $state.raw<Date | null>(null);
	let root = $state<HTMLElement>();

	// Plain variables on purpose: nothing renders from them, and the polling
	// effect must not depend on what it writes.
	let args: Record<string, unknown> = {};
	let generation = 0;
	let bridge: Promise<Bridge> | undefined;
	let timer: ReturnType<typeof setTimeout> | undefined;
	let delay = 0;
	let dueAt = 0;
	// When the current snapshot was adopted; plain, read only by poll.
	let shownAt = 0;
	let polling = false;
	let reloading: Promise<Reload> | undefined;
	let destroyed = false;

	onMount(() => {
		const onVisibility = () => { visible = document.visibilityState === 'visible'; };
		onVisibility();
		document.addEventListener('visibilitychange', onVisibility);
		bridge = connect({
			onToolInput(next) { args = next; reset(); },
			onToolResult(result) { showResult(result); },
			onHostContext(ctx) { host = { ...host, ...ctx }; }
		});
		// A host that never completes the handshake leaves an empty view, not an unhandled rejection.
		bridge.catch(() => {});
		return () => {
			destroyed = true;
			generation++;
			disarm();
			document.removeEventListener('visibilitychange', onVisibility);
		};
	});

	async function call(tool: string, callArgs: Record<string, unknown>): Promise<ToolResultLike> {
		if (!bridge) throw new Error('Not connected');
		return (await bridge).call(tool, callArgs);
	}

	function stringArg(key: string): string | undefined {
		const value = args[key];
		return typeof value === 'string' && value !== '' ? value : undefined;
	}
	/** The parent filter show was given; changes and every re-show must repeat it. */
	function parentArg(): { parent?: string } {
		const parent = stringArg('parent');
		return parent === undefined ? {} : { parent };
	}

	function reset() {
		generation++;
		disarm();
		board = null; failure = null; notice = null; pending = new Map(); refreshing = false; slowed = false; syncedAt = null;
		delay = 0; dueAt = 0; reloading = undefined;
	}

	function showResult(result: ToolResultLike) {
		reset();
		// A changes call made by the model has nothing for this view to draw.
		if (args.action === 'changes') return;
		const error = parseToolError(result);
		if (error) { failure = describe(error); return; }
		const parsed = parseBoard(result.structuredContent);
		if (parsed) adopt(parsed);
	}

	// The collection may have been deleted since the board was opened.
	const describe = (error: ToolError): string => error.code === 'not_found' ? 'Collection not found' : error.message;
	// Codes: internal/mcp/errors.go (ErrRateLimited; its message names HTTP 429).
	const isRateLimit = (error: ToolError): boolean => error.code === 'rate_limited' || error.code === '429' || /\b429\b/.test(error.message);
	// Codes after which the board's rows may no longer be shown: internal/mcp/errors.go
	// (ErrNotFound, ErrUnknownWorkspace, ErrPermissionDenied, ErrAuthRequired).
	const GONE = ['not_found', 'unknown_workspace', 'permission_denied', 'auth_required'];
	const thrown = (error: unknown, fallback: string): string => error instanceof Error && error.message ? error.message : fallback;

	function adopt(next: Board) {
		board = next;
		shownAt = Date.now();
		failure = null;
		slowed = false;
		syncedAt = new Date();
		// A fresh snapshot starts the usual pace over.
		delay = nextDelay(0, false);
		dueAt = Date.now() + delay;
		if (untrack(() => live)) arm();
	}

	/** Re-runs show and replaces the board. One at a time; a second caller shares the first's answer. */
	function reload(): Promise<Reload> {
		reloading ??= runShow().finally(() => { reloading = undefined; });
		return reloading;
	}
	async function runShow(): Promise<Reload> {
		const workspace = board?.workspace ?? stringArg('workspace');
		const collection = board?.collection ?? stringArg('collection');
		if (workspace === undefined || collection === undefined) return 'failed';
		const mine = generation;
		let error: ToolError | undefined;
		try {
			const result = await call('pad_board', { action: 'show', workspace, collection, ...parentArg() });
			if (mine !== generation) return 'failed';
			error = parseToolError(result);
			const parsed = error ? null : parseBoard(result.structuredContent);
			if (parsed) { adopt(parsed); return 'ok'; }
			error ??= { code: 'unknown', message: 'The board could not be reloaded' };
		} catch (caught) {
			if (mine !== generation) return 'failed';
			error = { code: 'unknown', message: thrown(caught, 'The board could not be reloaded') };
		}
		if (isRateLimit(error)) return 'limited';
		if (board === null || GONE.includes(error.code)) {
			// Nothing to keep, or nothing this viewer may keep seeing.
			disarm();
			board = null; pending = new Map(); notice = null;
			failure = describe(error);
		} else {
			notice = { tone: 'error', text: error.message };
		}
		return 'failed';
	}

	async function refresh() {
		if (refreshing) return;
		const mine = generation;
		refreshing = true;
		const outcome = await reload();
		if (mine !== generation) return;
		refreshing = false;
		if (outcome === 'ok') notice = null;
		else if (outcome === 'limited') notice = { tone: 'error', text: 'Too many requests; try again in a moment' };
	}

	// --- live updates -------------------------------------------------------

	const live = $derived(board !== null && shouldPoll({ displayMode: host.displayMode, visible }));
	$effect(() => {
		const on = live;
		untrack(() => (on ? arm() : disarm()));
	});

	/** Waits out what is left of the current delay, so hiding and showing the view never polls early. */
	function arm() {
		if (destroyed || polling) return;
		disarm();
		timer = setTimeout(() => { timer = undefined; void poll(); }, Math.max(0, dueAt - Date.now()));
	}
	function disarm() {
		if (timer !== undefined) clearTimeout(timer);
		timer = undefined;
	}

	async function poll() {
		const at = board;
		if (!at || polling) return;
		const mine = generation;
		polling = true;
		let limited = false;
		try {
			const result = await call('pad_board', {
				action: 'changes', workspace: at.workspace, collection: at.collection, ...parentArg(),
				since: at.cursor, access_epoch: at.access_epoch
			});
			if (mine !== generation) return;
			const error = parseToolError(result);
			const changes = error ? null : parseChanges(result.structuredContent);
			if (error && isRateLimit(error)) limited = true;
			// An incomplete board cannot place a delta for an item it does not show.
			// Its totals can also drift on departures it cannot see, so it re-reads on a fixed bound.
			else if (!changes || changes.resync || (board !== null && (needsResync(board, changes) || incompleteReshowDue(board, shownAt, Date.now())))) limited = (await reload()) === 'limited';
			else if (board && board.cursor === at.cursor && board.access_epoch === at.access_epoch) board = applyChanges(board, newerOnly(board, changes));
			if (mine !== generation) return;
			if (!limited) syncedAt = new Date();
		} catch {
			// The host did not answer: keep the board and try again more slowly.
			if (mine !== generation) return;
			limited = true;
		} finally {
			polling = false;
		}
		// A successful re-show already set the pace in adopt.
		if (limited || dueAt <= Date.now()) {
			delay = nextDelay(delay, limited);
			dueAt = Date.now() + delay;
		}
		slowed = limited;
		if (untrack(() => live)) arm();
	}

	/** A delta asked for before a save landed can carry that item's older state: the higher seq wins. */
	function newerOnly(current: Board, changes: BoardChanges): BoardChanges {
		const held = new Map(current.columns.flatMap(column => column.items.map(item => [item.id, item.seq] as const)));
		return { ...changes, upserts: changes.upserts.filter(item => item.seq >= (held.get(item.id) ?? -Infinity)) };
	}

	// --- status change --------------------------------------------------------

	function setPending(id: string, to?: string) {
		const next = new Map(pending);
		if (to === undefined) next.delete(id); else next.set(id, to);
		pending = next;
	}

	async function fetchItem(ref: string, workspace: string): Promise<ItemLike | null> {
		try {
			const result = await call('pad_item', { action: 'get', ref, workspace });
			return !result.isError && isItemResult(result.structuredContent) ? result.structuredContent : null;
		} catch {
			return null;
		}
	}

	/** Puts one freshly read item where it now belongs, leaving the poll cursor alone. */
	function place(next: ItemLike, row: BoardItem) {
		if (!board) return;
		const fresh = boardItemFrom(next, row);
		// A poll may have delivered a newer state while this read was out; the
		// cursor has moved past it, so overwriting it would lose it for good.
		const held = board.columns.flatMap(column => column.items).find(item => item.id === row.id);
		if (held && held.seq > fresh.seq) return;
		const here = next.collection_slug === board.collection;
		board = applyChanges(board, {
			upserts: here ? [fresh] : [], removed_ids: here ? [] : [row.id], departed_ids: [],
			cursor: board.cursor, access_epoch: board.access_epoch, resync: false
		});
	}

	async function changeStatus(item: BoardItem, to: string) {
		const at = board;
		if (!at || pending.has(item.id) || to === item.status) return;
		const mine = generation;
		setPending(item.id, to);
		notice = null;
		let next: ItemLike | null = null;
		let outcome: Notice | null = null;
		try {
			const result = await call('pad_item', buildUpdateArgs({ workspace: at.workspace, ref: item.ref, expectedSeq: item.seq, field: { key: 'status', value: to } }));
			const error = parseToolError(result);
			const conflict = error?.code === 'update_conflict';
			if (error && !conflict) {
				outcome = { tone: 'error', text: error.message };
			} else {
				next = !error && isItemResult(result.structuredContent) ? result.structuredContent : await fetchItem(item.ref, at.workspace);
				// The item itself could not be read back: the whole board is the next best truth.
				const reloaded = next !== null || (mine === generation && (await reload()) === 'ok');
				if (conflict) outcome = reloaded ? { tone: 'info', text: 'Changed elsewhere; reloaded' } : { tone: 'error', text: 'Changed elsewhere; could not reload' };
				else if (!reloaded) outcome = { tone: 'error', text: 'Saved, but the board could not be reloaded' };
			}
		} catch (caught) {
			outcome = { tone: 'error', text: thrown(caught, 'Update failed') };
		}
		// A newer tool result replaced this board while the call was out.
		if (mine !== generation) return;
		if (next) place(next, item);
		setPending(item.id);
		if (outcome) notice = outcome;
		// The control was disabled while saving, which drops focus to the body: hand it back.
		await tick();
		if (document.activeElement && document.activeElement !== document.body) return;
		Array.from(root?.querySelectorAll<HTMLElement>('[data-control]') ?? []).find(el => el.dataset.control === item.id)?.focus();
	}

	// --- what is drawn --------------------------------------------------------

	const columns = $derived.by(() => {
		if (!board) return [];
		// A stored schema can repeat a status option, and one id must never be drawn twice.
		const out = uniqueBy(board.columns, column => column.status).map(column => ({
			status: column.status, terminal: column.terminal, abandoned: column.abandoned, total: column.total,
			source: column.items, items: [] as { item: BoardItem; status: string; saving: boolean }[]
		}));
		const byStatus = new Map(out.map(column => [column.status, column]));
		const seen = new Set<string>();
		for (const column of out) {
			for (const item of column.source) {
				if (seen.has(item.id)) continue;
				seen.add(item.id);
				const to = pending.get(item.id);
				const target = to === undefined ? undefined : byStatus.get(to);
				if (target && target !== column) {
					// Optimistic move: drawn in the target column until the save settles.
					column.total = Math.max(0, column.total - 1);
					target.total++;
					target.items.push({ item, status: target.status, saving: true });
				} else {
					column.items.push({ item, status: column.status, saving: to !== undefined });
				}
			}
		}
		// Most recently changed first, as show orders them; a move in flight leads its column.
		const time = (item: BoardItem) => Date.parse(item.updated_at) || 0;
		for (const column of out) column.items.sort((a, b) => Number(b.saving) - Number(a.saving) || time(b.item) - time(a.item));
		return out;
	});
	const statuses = $derived(columns.map(column => column.status));
	const fullscreen = $derived(host.displayMode === 'fullscreen');
	const insets = $derived(host.safeAreaInsets);
	const parent = $derived.by(() => { void board; return stringArg('parent'); });
	const syncedLabel = $derived.by(() => {
		if (!syncedAt) return '';
		try {
			return new Intl.DateTimeFormat(host.locale, { timeStyle: 'short', timeZone: host.timeZone }).format(syncedAt);
		} catch {
			return syncedAt.toLocaleTimeString();
		}
	});
	const canRetry = $derived.by(() => { void failure; return stringArg('workspace') !== undefined && stringArg('collection') !== undefined; });

	const titleCase = (slug: string): string => slug.charAt(0).toUpperCase() + slug.slice(1).replace(/[-_]/g, ' ');
	const count = (n: number): string => `${n} ${n === 1 ? 'item' : 'items'}`;
	const kind = (column: { terminal: boolean; abandoned: boolean }): string =>
		column.abandoned ? ', closed without delivering' : column.terminal ? ', closed' : '';
</script>

{#if board}
	<section
		class="board" class:fullscreen bind:this={root} aria-label="{titleCase(board.collection)} board" aria-busy={refreshing || pending.size > 0}
		style:--inset-top="{insets?.top ?? 0}px" style:--inset-right="{insets?.right ?? 0}px"
		style:--inset-bottom="{insets?.bottom ?? 0}px" style:--inset-left="{insets?.left ?? 0}px"
	>
		<header class="top">
			<div class="heading">
				<p class="eyebrow">
					<span>{board.workspace}</span>
					{#if parent}<span>under <span class="ref">{parent}</span></span>{/if}
				</p>
				<h2 class="title">{titleCase(board.collection)}</h2>
			</div>
			<div class="sync">
				<!-- Always present so the live region exists before it speaks. -->
				<span class="sync-status" role="status">
					{#if pending.size > 0}Saving…
					{:else if refreshing}Refreshing…
					{:else if live}<span class="dot" class:slowed aria-hidden="true"></span>{slowed ? 'Live, slowed' : 'Live'}
					{:else if syncedLabel}As of {syncedLabel}{/if}
				</span>
				{#if !live}
					<button type="button" class="button" disabled={refreshing} onclick={refresh}>Refresh</button>
				{/if}
			</div>
		</header>

		{#if notice}
			<p class="notice" class:error={notice.tone === 'error'} role={notice.tone === 'error' ? 'alert' : 'status'}>
				<span>{notice.text}</span>
				<button type="button" class="icon-button" aria-label="Dismiss notice" onclick={() => (notice = null)}>
					<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true" focusable="false">
						<path d="M4 4l8 8M12 4l-8 8" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
					</svg>
				</button>
			</p>
		{/if}
		{#if board.unplaced > 0}
			<p class="note">{board.unplaced === 1 ? '1 item has' : `${board.unplaced} items have`} a status that is not a column here, and is not shown.</p>
		{/if}

		<div class="columns">
			{#each columns as column (column.status)}
				<section class="column" class:terminal={column.terminal} class:abandoned={column.abandoned} aria-label="{column.status}, {count(column.total)}{kind(column)}">
					<header class="column-head">
						<h3 class="status">{column.status}</h3>
						<span class="count" aria-hidden="true">{column.total}</span>
					</header>
					{#if column.items.length < column.total}
						<p class="partial">showing {column.items.length} of {column.total}</p>
					{/if}
					{#if column.items.length > 0}
						<ul class="items" role="list">
							{#each column.items as entry (entry.item.id)}
								<li class="item" class:saving={entry.saving}>
									<p class="meta">
										<span class="ref">{entry.item.ref}</span>
										{#if entry.item.priority}<span class="priority">{entry.item.priority}</span>{/if}
									</p>
									<p class="item-title">{entry.item.title}</p>
									<div class="foot">
										<span class="who">{entry.item.assignee}</span>
										{#if entry.item.ref}
											<select
												class="select" data-control={entry.item.id} aria-label="Status of {entry.item.ref}" disabled={entry.saving}
												value={entry.status} onchange={event => changeStatus(entry.item, event.currentTarget.value)}
											>
												{#each statuses as status}<option value={status}>{status}</option>{/each}
											</select>
										{/if}
									</div>
								</li>
							{/each}
						</ul>
					{:else if column.total === 0}
						<p class="empty">No items</p>
					{/if}
				</section>
			{/each}
		</div>
	</section>
{:else if failure}
	<div class="board failed">
		<p class="notice error" role="alert"><span>{failure}</span></p>
		{#if canRetry}
			<button type="button" class="button" disabled={refreshing} onclick={refresh}>Try again</button>
		{/if}
	</div>
{/if}

<style>
	/* Host style variables (applied on :root by the bridge) win; these are the fallbacks, the card's own. */
	:global(:root) {
		color-scheme: light dark;
		--fallback-bg: #ffffff;
		--fallback-surface: #f4f3ef;
		--fallback-text: #1f1e1c;
		--fallback-muted: #66645e;
		--fallback-border: #d6d3cb;
		--fallback-ring: #2c6bd6;
		--fallback-danger: #a3271c;
		--fallback-danger-bg: #fbeceb;
		--fallback-info-bg: #eef2f8;
		--fallback-live: #2f7d4f;
	}
	:global(:root[data-theme='dark']) {
		--fallback-bg: #262624;
		--fallback-surface: #30302e;
		--fallback-text: #f2f0ea;
		--fallback-muted: #b0ada4;
		--fallback-border: #4a4945;
		--fallback-ring: #7aa7f5;
		--fallback-danger: #f1a199;
		--fallback-danger-bg: #44211e;
		--fallback-info-bg: #2b3340;
		--fallback-live: #6fc191;
	}
	@media (prefers-color-scheme: dark) {
		:global(:root:not([data-theme])) {
			--fallback-bg: #262624;
			--fallback-surface: #30302e;
			--fallback-text: #f2f0ea;
			--fallback-muted: #b0ada4;
			--fallback-border: #4a4945;
			--fallback-ring: #7aa7f5;
			--fallback-danger: #f1a199;
			--fallback-danger-bg: #44211e;
			--fallback-info-bg: #2b3340;
			--fallback-live: #6fc191;
		}
	}

	.board {
		--bg: var(--color-background-primary, var(--fallback-bg));
		--surface: var(--color-background-secondary, var(--fallback-surface));
		--text: var(--color-text-primary, var(--fallback-text));
		--muted: var(--color-text-secondary, var(--fallback-muted));
		--border: var(--color-border-primary, var(--fallback-border));
		--ring: var(--color-ring-primary, var(--fallback-ring));
		--danger: var(--color-text-danger, var(--fallback-danger));
		--danger-bg: var(--color-background-danger, var(--fallback-danger-bg));
		--info-bg: var(--color-background-info, var(--fallback-info-bg));
		--live: var(--color-text-success, var(--fallback-live));
		--radius: var(--border-radius-md, 8px);
		--small: var(--font-text-xs-size, 0.75rem);
		--body: var(--font-text-sm-size, 0.875rem);
		--pad-x: 16px;

		box-sizing: border-box;
		display: flex;
		flex-direction: column;
		gap: 12px;
		padding: calc(14px + var(--inset-top, 0px)) 0 calc(14px + var(--inset-bottom, 0px));
		background: var(--bg);
		color: var(--text);
		font-family: var(--font-sans, system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif);
		font-size: var(--body);
		line-height: 1.45;
		overflow-wrap: anywhere;
	}
	.board :global(*) { box-sizing: border-box; }
	/* Fullscreen owns the viewport: the columns scroll, the page does not. */
	.board.fullscreen { height: 100vh; height: 100dvh; }
	p, h2, h3, ul { margin: 0; }

	.top, .notice, .note, .failed > .button {
		margin-left: calc(var(--pad-x) + var(--inset-left, 0px));
		margin-right: calc(var(--pad-x) + var(--inset-right, 0px));
	}
	.top { display: flex; flex-wrap: wrap; align-items: flex-end; justify-content: space-between; gap: 6px 16px; }
	.heading { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
	.eyebrow { display: flex; flex-wrap: wrap; align-items: baseline; gap: 8px; font-size: var(--small); color: var(--muted); }
	.ref { font-family: var(--font-mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace); font-weight: var(--font-weight-semibold, 600); color: var(--text); letter-spacing: 0.01em; }
	.title { font-size: var(--font-heading-xs-size, 1.0625rem); line-height: 1.3; font-weight: var(--font-weight-semibold, 600); }

	.sync { display: flex; align-items: center; gap: 10px; min-height: 32px; margin-left: auto; font-size: var(--small); color: var(--muted); }
	.sync-status { display: inline-flex; align-items: center; gap: 6px; }
	.dot { width: 7px; height: 7px; border-radius: 50%; background: var(--live); }
	.dot.slowed { background: var(--muted); }

	.button, .select {
		min-height: 32px;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text);
		font: inherit;
		cursor: pointer;
	}
	.button { padding: 4px 12px; background: var(--surface); }
	.icon-button {
		flex: none;
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 26px;
		height: 26px;
		padding: 0;
		border: 0;
		border-radius: var(--radius);
		background: transparent;
		color: inherit;
		cursor: pointer;
	}
	.button:hover:not(:disabled), .icon-button:hover:not(:disabled) { background: var(--surface); filter: brightness(0.97); }
	.board :global(:disabled) { cursor: default; opacity: 0.6; }
	.board :global(:focus-visible) { outline: 2px solid var(--ring); outline-offset: 2px; }

	.notice {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 8px;
		max-width: 46rem;
		padding: 4px 4px 4px 10px;
		border-radius: var(--radius);
		background: var(--info-bg);
	}
	.notice.error { background: var(--danger-bg); color: var(--danger); }
	.note { font-size: var(--small); color: var(--muted); }

	.columns {
		display: grid;
		grid-auto-flow: column;
		grid-auto-columns: minmax(min(15rem, 80vw), 1fr);
		align-items: start;
		gap: 10px;
		padding: 2px calc(var(--pad-x) + var(--inset-right, 0px)) 4px calc(var(--pad-x) + var(--inset-left, 0px));
		overflow-x: auto;
		scroll-snap-type: x proximity;
		scroll-padding-inline: calc(var(--pad-x) + var(--inset-left, 0px));
		overscroll-behavior-x: contain;
	}
	.fullscreen .columns { flex: 1; min-height: 0; align-items: stretch; grid-template-rows: minmax(0, 1fr); }

	.column {
		display: flex;
		flex-direction: column;
		gap: 8px;
		min-width: 0;
		min-height: 0;
		padding: 10px 8px 8px;
		border: 1px solid transparent;
		border-radius: calc(var(--radius) + 2px);
		background: var(--surface);
		scroll-snap-align: start;
	}
	.column-head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; padding: 0 4px; }
	.status { min-width: 0; font-size: var(--body); line-height: 1.3; font-weight: var(--font-weight-semibold, 600); }
	.count { font-size: var(--small); color: var(--muted); font-variant-numeric: tabular-nums; }
	.partial, .empty { padding: 0 4px; font-size: var(--small); color: var(--muted); }
	.empty { padding-bottom: 4px; }

	.items { display: flex; flex-direction: column; gap: 6px; padding: 0; list-style: none; }
	/* Inline, a long column scrolls inside itself instead of stretching the conversation. */
	.board:not(.fullscreen) .items { max-height: 26rem; overflow-y: auto; }
	.fullscreen .items { flex: 1; min-height: 0; overflow-y: auto; }

	.item {
		display: flex;
		flex-direction: column;
		gap: 4px;
		padding: 8px 10px 6px;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg);
	}
	.meta { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; font-size: var(--small); color: var(--muted); }
	.priority { flex: none; padding: 0 6px; border: 1px solid var(--border); border-radius: 999px; }
	.item-title { display: -webkit-box; -webkit-box-orient: vertical; -webkit-line-clamp: 3; line-clamp: 3; overflow: hidden; }
	.foot { display: flex; align-items: center; justify-content: space-between; gap: 8px; min-height: 28px; font-size: var(--small); color: var(--muted); }
	.who { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.select { flex: none; max-width: 60%; min-height: 28px; padding: 2px 2px 2px 6px; border-color: transparent; background: transparent; color: var(--muted); font-size: var(--small); }
	.select:hover:not(:disabled), .select:focus-visible { border-color: var(--border); color: var(--text); }
	.item.saving { border-style: dashed; }
	.item.saving .item-title { color: var(--muted); font-style: italic; }

	/* Closed work steps back: no fill behind the column, flat rows, muted titles. */
	.column.terminal { border-color: var(--border); background: transparent; }
	.terminal .status, .terminal .item-title, .terminal .ref { color: var(--muted); }
	.terminal .status, .terminal .ref { font-weight: var(--font-weight-normal, 400); }
	.terminal .item { background: transparent; }
	/* Closed without delivering steps back further: a dashed outline and struck titles. */
	.column.abandoned { border-style: dashed; }
	.abandoned .item { border-color: transparent; border-bottom-color: var(--border); border-radius: 0; padding-left: 4px; padding-right: 4px; }
	.abandoned .item:last-child { border-bottom-color: transparent; }
	.abandoned .item-title { text-decoration: line-through; text-decoration-color: var(--border); }

	.failed { align-items: flex-start; }
	.failed .notice { padding: 6px 10px; }

	@media (pointer: coarse) {
		.button { min-height: 40px; }
		.select { min-height: 36px; }
		.icon-button { width: 36px; height: 36px; }
	}
	@media (prefers-reduced-motion: no-preference) {
		.select, .button, .icon-button { transition: background-color 120ms, border-color 120ms, color 120ms; }
	}
</style>
