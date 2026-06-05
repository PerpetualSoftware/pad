<script lang="ts">
	// Focus-mode detail card for the 3D workspace graph (PLAN-1730 / TASK-1734).
	//
	// Slides in from the right when a node is selected. The graph +page.svelte owns
	// selection state and the richer item fetch; this component is pure presentation
	// — it takes the selected node, the (optionally still-loading) full Item, and
	// emits open/close callbacks. Styling mirrors the page's .toolbar / .state-card
	// (backdrop-blur, color-mix surfaces) so the focus layer reads as the same UI.
	import { relativeTime } from '$lib/utils/markdown';
	import type { Item } from '$lib/types';

	// The selected node's renderer-facing shape (a subset of the page's GraphNode3D).
	// Kept structural so the page can pass its mapped node straight through.
	interface SelectedNode {
		ref: string;
		title: string;
		collection: string;
		status?: string;
		is_terminal: boolean;
		child_count: number;
		updated_at: string;
	}

	let {
		node,
		color,
		item,
		itemLoading,
		blockedBy,
		blocksCount,
		chainDepth,
		onjump,
		onopen,
		onclose
	}: {
		node: SelectedNode;
		/** collection color (hex) — shared with the node's renderer color. */
		color: string;
		/** full item, fetched lazily by the page; null until it arrives. */
		item: Item | null;
		itemLoading: boolean;
		/** direct blockers of this node (TASK-1737) — clickable to drill up the chain. */
		blockedBy: { ref: string; title: string }[];
		/** how many items this node blocks (outgoing blocks-edges). */
		blocksCount: number;
		/** deepest blocker hop from this node (1 = a direct blocker; >direct = chain). */
		chainDepth: number;
		/** fly to + re-select a blocker by ref (re-traces ITS chain — natural drill-up). */
		onjump: (ref: string) => void;
		onopen: () => void;
		onclose: () => void;
	} = $props();

	// Cap the rendered blocker list; the rest fold into a "+N more" line. The full
	// chain is still walkable by jumping into any listed blocker.
	const BLOCKER_LIMIT = 6;
	const shownBlockers = $derived(blockedBy.slice(0, BLOCKER_LIMIT));
	const moreBlockers = $derived(Math.max(0, blockedBy.length - BLOCKER_LIMIT));
	// "chain depth N" is only worth showing when the transitive chain runs DEEPER than
	// the direct blockers in the card — otherwise it just restates the list length.
	const showChainDepth = $derived(chainDepth > 1 && chainDepth > blockedBy.length);

	// item.fields is a JSON string on the Item type. Parse defensively — handle a
	// pre-parsed object too (some callers/snapshots hydrate it eagerly) and never
	// throw on malformed JSON.
	const fields = $derived.by<Record<string, unknown>>(() => {
		const raw = item?.fields;
		if (!raw) return {};
		if (typeof raw === 'object') return raw as Record<string, unknown>;
		try {
			const parsed = JSON.parse(raw);
			return parsed && typeof parsed === 'object' ? parsed : {};
		} catch {
			return {};
		}
	});

	const priority = $derived(
		typeof fields.priority === 'string' && fields.priority ? fields.priority : null
	);
	const assignee = $derived(item?.assigned_user_name ?? null);
</script>

<aside class="detail-card" aria-label="Selected item">
	<button class="close" onclick={onclose} aria-label="Close detail card">×</button>

	<header class="card-head">
		<span class="dot" style:background-color={color} aria-hidden="true"></span>
		<span class="ref">{node.ref}</span>
	</header>

	<h2 class="title">{node.title}</h2>

	<div class="pills">
		{#if node.status}
			<span class="pill" class:terminal={node.is_terminal}>{node.status}</span>
		{/if}
		{#if node.child_count > 0}
			<span class="meta">{node.child_count} {node.child_count === 1 ? 'child' : 'children'}</span>
		{/if}
		<span class="meta">Updated {relativeTime(node.updated_at)}</span>
	</div>

	<!-- Richer detail fetched separately; shows a shimmer line until it lands. -->
	<div class="detail-rows">
		{#if itemLoading}
			<div class="shimmer" aria-hidden="true"></div>
		{:else if item}
			{#if priority}
				<div class="row">
					<span class="row-key">Priority</span>
					<span class="row-val">{priority}</span>
				</div>
			{/if}
			{#if assignee}
				<div class="row">
					<span class="row-key">Assignee</span>
					<span class="row-val">{assignee}</span>
				</div>
			{/if}
		{/if}
	</div>

	<!-- Blocker chain (TASK-1737): direct blockers, clickable to drill up the chain.
	     Red accent — these are the reason this item is stuck. -->
	{#if blockedBy.length > 0}
		<section class="blockers" aria-label="Blocked by">
			<p class="blockers-head">Blocked by</p>
			<ul class="blocker-list">
				{#each shownBlockers as b (b.ref)}
					<li>
						<button class="blocker-row" onclick={() => onjump(b.ref)} title="Trace this blocker">
							<span class="blocker-ref">{b.ref}</span>
							<span class="blocker-title">{b.title}</span>
						</button>
					</li>
				{/each}
			</ul>
			{#if moreBlockers > 0}
				<p class="blockers-more">+{moreBlockers} more</p>
			{/if}
			{#if showChainDepth}
				<p class="chain-depth">chain depth {chainDepth}</p>
			{/if}
		</section>
	{/if}

	{#if blocksCount > 0}
		<p class="blocks-count">Blocks {blocksCount} {blocksCount === 1 ? 'item' : 'items'}</p>
	{/if}

	<button class="open-btn" onclick={onopen}>Open item</button>
</aside>

<style>
	.detail-card {
		position: absolute;
		top: 50%;
		right: var(--space-4);
		transform: translateY(-50%);
		z-index: 12;
		width: 18rem;
		max-width: calc(100% - var(--space-8));
		padding: var(--space-5);
		background: color-mix(in srgb, var(--bg-secondary) 92%, transparent);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 4px 20px rgba(0, 0, 0, 0.35);
		backdrop-filter: blur(8px);
		/* Slide in from the right. */
		animation: slide-in 180ms ease-out;
	}

	@keyframes slide-in {
		from {
			opacity: 0;
			transform: translate(12px, -50%);
		}
		to {
			opacity: 1;
			transform: translate(0, -50%);
		}
	}

	.close {
		position: absolute;
		top: var(--space-2);
		right: var(--space-2);
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 1.6rem;
		height: 1.6rem;
		padding: 0;
		font-size: 1.2em;
		line-height: 1;
		color: var(--text-muted);
		background: transparent;
		border: none;
		border-radius: var(--radius-sm, 4px);
		cursor: pointer;
	}
	.close:hover {
		color: var(--text-primary);
		background: color-mix(in srgb, var(--text-primary) 8%, transparent);
	}

	.card-head {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding-right: 1.6rem;
	}
	.dot {
		width: 0.7rem;
		height: 0.7rem;
		border-radius: 50%;
		flex: 0 0 auto;
	}
	.ref {
		font-family: var(--font-mono, ui-monospace, monospace);
		font-size: 0.75em;
		font-weight: 600;
		color: var(--text-secondary);
		letter-spacing: 0.02em;
	}

	.title {
		margin: var(--space-2) 0 var(--space-3);
		font-size: 1.02em;
		font-weight: 600;
		line-height: 1.3;
		color: var(--text-primary);
	}

	.pills {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
	}
	.pill {
		padding: 0.1rem 0.5rem;
		font-size: 0.72em;
		font-weight: 600;
		text-transform: capitalize;
		color: var(--text-secondary);
		background: color-mix(in srgb, var(--text-secondary) 12%, transparent);
		border-radius: 999px;
	}
	.pill.terminal {
		color: var(--success, #10b981);
		background: color-mix(in srgb, var(--success, #10b981) 16%, transparent);
	}
	.meta {
		font-size: 0.72em;
		color: var(--text-muted);
		font-variant-numeric: tabular-nums;
	}

	.detail-rows {
		min-height: 1.2rem;
		margin: var(--space-3) 0;
	}
	.row {
		display: flex;
		justify-content: space-between;
		gap: var(--space-3);
		padding: 0.15rem 0;
		font-size: 0.8em;
	}
	.row-key {
		color: var(--text-muted);
	}
	.row-val {
		color: var(--text-primary);
		text-transform: capitalize;
		text-align: right;
	}

	.shimmer {
		height: 0.85rem;
		width: 60%;
		border-radius: var(--radius-sm, 4px);
		background: linear-gradient(
			90deg,
			color-mix(in srgb, var(--text-muted) 10%, transparent) 25%,
			color-mix(in srgb, var(--text-muted) 22%, transparent) 50%,
			color-mix(in srgb, var(--text-muted) 10%, transparent) 75%
		);
		background-size: 200% 100%;
		animation: shimmer 1.1s ease-in-out infinite;
	}
	@keyframes shimmer {
		from {
			background-position: 200% 0;
		}
		to {
			background-position: -200% 0;
		}
	}

	/* ── Blocker chain (TASK-1737) ─────────────────────────────────────────────── */
	.blockers {
		margin: var(--space-3) 0;
		padding-top: var(--space-3);
		border-top: 1px solid color-mix(in srgb, var(--blocks-red, #f43f5e) 25%, transparent);
	}
	.blockers-head {
		margin: 0 0 var(--space-2);
		font-size: 0.7em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--blocks-red, #f43f5e);
	}
	.blocker-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.15rem;
	}
	.blocker-row {
		display: flex;
		align-items: baseline;
		gap: var(--space-2);
		width: 100%;
		padding: 0.2rem 0.35rem;
		text-align: left;
		background: transparent;
		border: none;
		border-radius: var(--radius-sm, 4px);
		cursor: pointer;
	}
	.blocker-row:hover {
		background: color-mix(in srgb, var(--blocks-red, #f43f5e) 12%, transparent);
	}
	.blocker-ref {
		flex: 0 0 auto;
		font-family: var(--font-mono, ui-monospace, monospace);
		font-size: 0.7em;
		font-weight: 600;
		color: var(--blocks-red, #f43f5e);
	}
	.blocker-title {
		flex: 1 1 auto;
		min-width: 0;
		font-size: 0.78em;
		color: var(--text-secondary);
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.blockers-more,
	.chain-depth {
		margin: var(--space-1, 0.25rem) 0 0 0.35rem;
		font-size: 0.7em;
		color: var(--text-muted);
	}
	.blocks-count {
		margin: var(--space-2) 0;
		font-size: 0.75em;
		color: var(--text-muted);
	}

	.open-btn {
		width: 100%;
		padding: var(--space-2) var(--space-3);
		font-size: 0.82em;
		font-weight: 600;
		color: var(--btn-primary-text, #fff);
		background: var(--accent, #6366f1);
		border: none;
		border-radius: var(--radius);
		cursor: pointer;
	}
	.open-btn:hover {
		filter: brightness(1.08);
	}
</style>
