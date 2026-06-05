<script lang="ts">
	// 3D workspace graph (PLAN-1730 / TASK-1733).
	//
	// Full-viewport force-directed view of the whole workspace: every item is a
	// node, every typed link an edge. The 3d-force-graph renderer pulls in Three.js,
	// so it's loaded ONLY via dynamic import inside onMount — that keeps WebGL out of
	// the main SPA bundle and out of any build-time SSR pass.
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { onMount, onDestroy } from 'svelte';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { titleStore } from '$lib/stores/title.svelte';
	import type { NodeObject, LinkObject } from '3d-force-graph';
	import type { GraphResponse } from '$lib/types';

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');

	// ── Data / UI state ─────────────────────────────────────────────────────────
	let graphData = $state<GraphResponse | null>(null);
	let loading = $state(true);
	let error = $state('');
	// Toggle: by default the API returns active items only; flip to pull terminal
	// (completed/closed) items too. Refetches and updates graphData in place —
	// the renderer instance is never recreated.
	let showCompleted = $state(false);

	// The workspace the loaded graph belongs to. SvelteKit reuses this route
	// component across workspace param changes; track it so a switch refetches.
	let graphWsSlug = '';
	// Monotonic request counter. Plain `let` (non-reactive) so it only gates which
	// in-flight load commits — discards stale/out-of-order responses.
	let reqSeq = 0;

	// ── Renderer handles (all plain `let`, never $state) ─────────────────────────
	// The graph instance is imperative, not template-reactive. Per CONVE-1688 we
	// never write a $state that an $effect also reads — these are read/written from
	// effects and handlers, so they stay non-reactive.
	let containerEl: HTMLDivElement | null = null;
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	let graph: any = null;
	let resizeObserver: ResizeObserver | null = null;
	// Latches once the renderer is constructed; the data-sync effect waits on it.
	let rendererReady = $state(false);

	// Node-count / edge-count readout for the toolbar.
	const nodeCount = $derived(graphData?.nodes.length ?? 0);
	const edgeCount = $derived(graphData?.edges.length ?? 0);
	const isEmpty = $derived(graphData !== null && graphData.nodes.length === 0);

	// ── Color palette ────────────────────────────────────────────────────────────
	// The chart PALETTE in $lib/components/charts/theme.ts is CSS-var-based, which
	// WebGL can't resolve — so we keep a local hex palette and assign colors to
	// collection slugs in first-seen order (stable within a single graph payload).
	const PALETTE = [
		'#6366f1', // indigo
		'#06b6d4', // cyan
		'#f59e0b', // amber
		'#10b981', // emerald
		'#f43f5e', // rose
		'#8b5cf6', // violet
		'#84cc16', // lime
		'#0ea5e9' // sky
	];
	// Built fresh on each graphData change. Plain `let` (rebuilt imperatively).
	let collectionColors: Record<string, string> = {};

	function colorForCollection(slug: string): string {
		if (!collectionColors[slug]) {
			const idx = Object.keys(collectionColors).length % PALETTE.length;
			collectionColors[slug] = PALETTE[idx];
		}
		return collectionColors[slug];
	}

	// The renderer's builder methods type their accessor params as the library's
	// own NodeObject / LinkObject (an open record), not our concrete shapes. We
	// map our GraphNode/GraphEdge fields onto each node/link, so cast at the
	// accessor boundary to read them with our local interfaces.
	const asNode = (n: NodeObject) => n as unknown as GraphNode3D;
	const asLink = (l: LinkObject<NodeObject>) => l as unknown as GraphLink3D;

	// nodeLabel renders raw HTML into a tooltip div, so escape user content.
	function escapeHtml(s: string): string {
		return s
			.replace(/&/g, '&amp;')
			.replace(/</g, '&lt;')
			.replace(/>/g, '&gt;')
			.replace(/"/g, '&quot;')
			.replace(/'/g, '&#39;');
	}

	// ── Title (kept separate from data-sync effects per CONVE-606) ───────────────
	onMount(() => {
		workspaceStore.setCurrent(wsSlug);
	});

	$effect(() => {
		page.url.pathname;
		titleStore.setPageTitle({ section: 'Graph', item: null });
	});

	// ── Renderer construction (onMount only — dynamic import keeps Three.js lazy) ──
	onMount(() => {
		let cancelled = false;

		(async () => {
			if (!containerEl) return;
			// CRITICAL: dynamic import so Three.js lands in its own chunk, not the
			// entry bundle. The default export is a factory class; v1.80 supports
			// `new ForceGraph3D(el)`.
			const ForceGraph3D = (await import('3d-force-graph')).default;
			if (cancelled || !containerEl) return;

			const instance = new ForceGraph3D(containerEl)
				.backgroundColor('rgba(0,0,0,0)')
				.nodeRelSize(4)
				// Subtree-weighted node size: parents/plans with children read bigger.
				.nodeVal((n: NodeObject) => 1 + (asNode(n).child_count ?? 0) * 2)
				.nodeColor((n: NodeObject) => colorForCollection(asNode(n).collection))
				.nodeLabel((n: NodeObject) => `${escapeHtml(asNode(n).ref)} — ${escapeHtml(asNode(n).name)}`)
				.nodeOpacity(0.95)
				// 'blocks' edges read red with a directional arrow; structural links
				// (parent/implements) brighter than soft links (wiki-link/related).
				.linkColor((l: LinkObject<NodeObject>) => linkColor(asLink(l)))
				.linkOpacity(0.5)
				.linkWidth((l: LinkObject<NodeObject>) => (asLink(l).type === 'blocks' ? 1.5 : 0.5))
				.linkDirectionalArrowLength((l: LinkObject<NodeObject>) =>
					asLink(l).type === 'blocks' ? 3 : 0
				)
				.linkDirectionalArrowRelPos(1)
				.onNodeClick((n: NodeObject) => {
					const node = asNode(n);
					// Item pages live at [collection]/[slug]; the server's ResolveItem
					// resolves a PREFIX-NUMBER ref in the slug param (same path the
					// insights "What shipped" links use), so the ref works directly.
					void goto(`/${username}/${wsSlug}/${node.collection}/${node.ref}`);
				});

			graph = instance;
			rendererReady = true;

			// Size to the container now and on every resize.
			syncSize();
			resizeObserver = new ResizeObserver(syncSize);
			resizeObserver.observe(containerEl);
		})();

		return () => {
			cancelled = true;
		};
	});

	function syncSize() {
		if (!graph || !containerEl) return;
		const w = containerEl.clientWidth;
		const h = containerEl.clientHeight;
		if (w > 0 && h > 0) {
			graph.width(w).height(h);
		}
	}

	// 'blocks' → red-ish; structural (parent/implements/supersedes/split-from) →
	// bright slate; soft (wiki-link/related) → dim slate. Alpha carries emphasis.
	function linkColor(l: GraphLink3D): string {
		if (l.type === 'blocks') return 'rgba(244, 63, 94, 0.85)';
		if (l.type === 'parent' || l.type === 'implements' || l.type === 'supersedes' || l.type === 'split-from') {
			return 'rgba(148, 163, 184, 0.85)';
		}
		return 'rgba(148, 163, 184, 0.35)';
	}

	// ── Data load + sync ─────────────────────────────────────────────────────────
	// Fetch whenever the workspace or the "show completed" toggle changes, with a
	// request token so a stale response can't clobber a newer one.
	$effect(() => {
		const slug = wsSlug;
		const withTerminal = showCompleted;
		if (slug !== graphWsSlug) {
			graphWsSlug = slug;
			// Drop the previous workspace's graph so it doesn't linger under the new
			// URL while the fetch is in flight — `loading` covers the gap.
			graphData = null;
		}
		if (slug) {
			void loadGraph(slug, withTerminal);
		}
	});

	// Push freshly-loaded data into the renderer once both are ready. Reads
	// graphData (reactive) + rendererReady (reactive); writes only the imperative
	// `graph` handle and the plain `collectionColors` map, never a tracked $state.
	$effect(() => {
		const data = graphData;
		if (!rendererReady || !graph || !data) return;
		// Reset color assignment so collection→color stays stable per payload.
		collectionColors = {};
		graph.graphData({
			nodes: data.nodes.map((n) => ({ ...n, id: n.ref, name: n.title })),
			links: data.edges.map((e) => ({ source: e.source, target: e.target, type: e.type }))
		});
	});

	async function loadGraph(slug: string, withTerminal: boolean) {
		const seq = ++reqSeq;
		loading = true;
		error = '';
		try {
			const data = await api.graph.get(slug, withTerminal);
			if (seq !== reqSeq) return;
			graphData = data;
		} catch (e) {
			if (seq !== reqSeq) return;
			error = e instanceof Error ? e.message : 'Failed to load graph.';
			graphData = null;
		} finally {
			if (seq === reqSeq) loading = false;
		}
	}

	// ── Teardown ─────────────────────────────────────────────────────────────────
	onDestroy(() => {
		resizeObserver?.disconnect();
		resizeObserver = null;
		// 3d-force-graph's teardown: stops the render loop and frees WebGL context.
		graph?._destructor?.();
		graph = null;
	});

	// Local renderer-facing node/link shapes (post-mapping). GraphNode fields are
	// spread onto the node, plus the id/name aliases the renderer keys on.
	interface GraphNode3D {
		id: string;
		name: string;
		ref: string;
		title: string;
		collection: string;
		status?: string;
		is_terminal: boolean;
		child_count: number;
		updated_at: string;
	}
	interface GraphLink3D {
		source: string;
		target: string;
		type: string;
	}
</script>

<div class="graph-page">
	<!-- Controls overlay (top-left) -->
	<div class="toolbar">
		<label class="toggle">
			<input type="checkbox" bind:checked={showCompleted} />
			<span>Show completed</span>
		</label>
		<span class="counts">
			<span class="count">{nodeCount} node{nodeCount === 1 ? '' : 's'}</span>
			<span class="count-sep">·</span>
			<span class="count">{edgeCount} edge{edgeCount === 1 ? '' : 's'}</span>
		</span>
	</div>

	<!-- The renderer mounts here; it owns its own canvas. -->
	<div class="canvas" bind:this={containerEl}></div>

	<!-- Overlay states (the canvas stays mounted underneath so the renderer keeps
	     its WebGL context across reloads). -->
	{#if error}
		<div class="overlay">
			<div class="state-card">
				<p class="state-title">Couldn't load the graph</p>
				<p class="state-desc">{error}</p>
			</div>
		</div>
	{:else if loading && !graphData}
		<div class="overlay">
			<div class="state-card">
				<p class="state-desc">Loading graph&hellip;</p>
			</div>
		</div>
	{:else if isEmpty}
		<div class="overlay">
			<div class="state-card">
				<p class="state-title">No active items to map</p>
				<p class="state-desc">
					{showCompleted
						? 'This workspace has no items yet.'
						: 'Turn on “Show completed” to include finished items.'}
				</p>
			</div>
		</div>
	{/if}
</div>

<style>
	/* Fill the layout's .main-content area (flex:1; overflow-y:auto). height:100%
	   on a block child fills it; overflow:hidden stops the canvas from scrolling. */
	.graph-page {
		position: relative;
		height: 100%;
		width: 100%;
		overflow: hidden;
		/* Dark space-like backdrop; falls back if the theme var is absent. */
		background: var(--bg-primary, #0a0a1a);
	}

	.canvas {
		position: absolute;
		inset: 0;
	}

	/* ── Toolbar ──────────────────────────────────────────────────────────────── */
	.toolbar {
		position: absolute;
		top: var(--space-4);
		left: var(--space-4);
		z-index: 10;
		display: flex;
		align-items: center;
		gap: var(--space-4);
		padding: var(--space-2) var(--space-4);
		background: color-mix(in srgb, var(--bg-secondary) 88%, transparent);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 2px 8px rgba(0, 0, 0, 0.25);
		backdrop-filter: blur(6px);
	}
	.toggle {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.82em;
		font-weight: 600;
		color: var(--text-secondary);
		cursor: pointer;
		user-select: none;
	}
	.toggle input {
		cursor: pointer;
	}
	.counts {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.78em;
		color: var(--text-muted);
		font-variant-numeric: tabular-nums;
	}
	.count-sep {
		opacity: 0.5;
	}

	/* ── State overlays ───────────────────────────────────────────────────────── */
	.overlay {
		position: absolute;
		inset: 0;
		z-index: 5;
		display: flex;
		align-items: center;
		justify-content: center;
		pointer-events: none;
	}
	.state-card {
		pointer-events: auto;
		max-width: 22rem;
		padding: var(--space-6);
		text-align: center;
		background: color-mix(in srgb, var(--bg-secondary) 92%, transparent);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 4px 16px rgba(0, 0, 0, 0.3);
		backdrop-filter: blur(6px);
	}
	.state-title {
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: var(--space-2);
	}
	.state-desc {
		font-size: 0.9em;
		color: var(--text-muted);
	}
</style>
