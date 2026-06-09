<script lang="ts">
	// 2D directional dependency-graph renderer for a single item's neighborhood
	// (PLAN-1780 / TASK-1783).
	//
	// Given a workspace + a focused item ref, fetches the dependency neighborhood
	// from the API and lays it out top-to-bottom with dagre (parents above,
	// children below), rendering to SVG. Single-clicking a node selects it and
	// opens a detail panel (status + open/re-root actions); double-clicking zooms
	// to it. The legend toggles collection visibility. The SVG is pannable
	// (pointer-drag) and zoomable (wheel).
	//
	// Styling mirrors the 3D graph's control/detail surfaces (color-mix
	// translucency, backdrop-blur) so it reads as the same UI inside a drawer.

	import { onMount, untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import type { GraphEdge, GraphNode, GraphResponse } from '$lib/types';
	import { createCollectionColorMap } from '$lib/graph/palette';
	import { sseService, type ItemEvent } from '$lib/services/sse.svelte';

	let {
		workspace,
		focusRef,
		depth: initialDepth = 2,
		itemHref
	}: {
		workspace: string;
		focusRef: string;
		depth?: number;
		/** Builds the href for an item — opens are rendered as real <a> links so
		 *  cmd/ctrl-click opens the item in a new tab. Collection is passed so the
		 *  caller can build /{user}/{ws}/{collection}/{ref} without a lookup. */
		itemHref: (ref: string, collection?: string) => string;
	} = $props();

	// ── Fixed layout geometry ────────────────────────────────────────────────────
	const NODE_W = 212;
	const NODE_H = 60;

	// ── Re-rooting + controls state ───────────────────────────────────────────────
	// The graph re-roots on `currentFocus`, NOT the prop directly — clicking a node
	// navigates without the parent component touching the prop. A separate prop-sync
	// effect (CONVE-606) reconciles `currentFocus` when the prop itself changes.
	// Seed local state from the props ONCE (untrack so reading the prop here doesn't
	// register a reactive dependency — subsequent prop changes flow through the
	// prop-sync effect below, CONVE-606). currentFocus, not the prop, drives the
	// graph so click-to-reroot can navigate without the parent touching the prop.
	let currentFocus = $state(untrack(() => focusRef));
	let depth = $state(untrack(() => clampDepth(initialDepth)));
	let includeTerminal = $state(false);

	function clampDepth(d: number): number {
		return Math.min(5, Math.max(1, Math.round(d)));
	}

	// Track the last prop value we synced from, so the prop-sync effect only fires on
	// an actual external change (and never fights the click-to-reroot writes).
	let lastSyncedProp = $state(untrack(() => focusRef));
	$effect(() => {
		if (focusRef !== lastSyncedProp) {
			lastSyncedProp = focusRef;
			currentFocus = focusRef;
		}
	});

	// ── Rendered model ────────────────────────────────────────────────────────────
	interface RenderNode {
		id: string; // item UUID — bridges SSE item events (item_id) to nodes
		ref: string;
		title: string;
		collection: string;
		status?: string;
		isTerminal: boolean;
		childCount: number;
		x: number; // center
		y: number; // center
		color: string;
	}
	interface RenderEdge {
		source: string;
		target: string;
		type: GraphEdge['type'];
		x1: number;
		y1: number;
		x2: number;
		y2: number;
	}

	type LoadState = 'idle' | 'loading' | 'ready' | 'error';

	let loadState = $state<LoadState>('idle');
	let errorMessage = $state('');
	let renderNodes = $state<RenderNode[]>([]);
	let renderEdges = $state<RenderEdge[]>([]);
	let legend = $state<{ slug: string; color: string }[]>([]);
	let truncated = $state(false);

	// Collections toggled off via the legend — their nodes (and any edge touching
	// them) are hidden without recomputing the layout, so positions stay stable.
	let hiddenCollections = $state(new Set<string>());
	// The node whose detail panel is open (single-click selection).
	let selectedRef = $state<string | null>(null);

	const visibleNodes = $derived(renderNodes.filter((n) => !hiddenCollections.has(n.collection)));
	const visibleRefs = $derived(new Set(visibleNodes.map((n) => n.ref)));
	const visibleEdges = $derived(
		renderEdges.filter((e) => visibleRefs.has(e.source) && visibleRefs.has(e.target))
	);
	const selectedNode = $derived(
		selectedRef ? (renderNodes.find((n) => n.ref === selectedRef) ?? null) : null
	);

	// Bumped by retry() to force the load effect to re-run after an error,
	// even when none of the real inputs changed.
	let retryNonce = $state(0);

	// Monotonic load token (CONVE-1688): the load effect reads inputs and writes
	// $state, but must not read+write the SAME rune in one pass. We bump a local
	// token at the start of each load and only commit results when the token still
	// matches — discarding stale in-flight responses without re-reading committed
	// $state inside the effect's reactive frame.
	let loadToken = 0;
	// True while any load() is awaiting. Used to COALESCE background refetches:
	// starting a new one mid-flight would bump loadToken and invalidate the
	// in-flight response, so a steady event stream could starve updates forever.
	// Instead we defer until the current load settles, then run exactly one.
	let loadInFlight = false;

	async function runLayout(payload: GraphResponse): Promise<{
		nodes: RenderNode[];
		edges: RenderEdge[];
		legend: { slug: string; color: string }[];
	}> {
		const dagre = await import('@dagrejs/dagre');
		const g = new dagre.graphlib.Graph();
		g.setGraph({ rankdir: 'TB', nodesep: 40, ranksep: 70, marginx: 20, marginy: 20 });
		g.setDefaultEdgeLabel(() => ({}));

		// Fresh color map per payload so legend + nodes agree (palette contract).
		const palette = createCollectionColorMap();

		const byRef = new Map<string, GraphNode>();
		for (const n of payload.nodes) {
			byRef.set(n.ref, n);
			g.setNode(n.ref, { width: NODE_W, height: NODE_H });
		}

		// Only wire edges whose BOTH endpoints are present as nodes — dagre throws
		// otherwise, and a truncated neighborhood can reference pruned refs.
		//
		// Edge direction here controls VERTICAL RANKING only (rankdir TB places an
		// edge's source above its target). The API models 'parent'/'implements' as
		// child → parent, so we reverse those for layout to get parents above
		// children. The rendered edges (keptEdges → RenderEdge below) keep the true
		// semantic source → target, so 'blocks' arrowheads still point the right way.
		const keptEdges: GraphEdge[] = [];
		for (const e of payload.edges) {
			if (byRef.has(e.source) && byRef.has(e.target)) {
				if (e.type === 'parent' || e.type === 'implements') {
					g.setEdge(e.target, e.source); // parent above child
				} else {
					g.setEdge(e.source, e.target);
				}
				keptEdges.push(e);
			}
		}

		dagre.layout(g);

		const nodes: RenderNode[] = [];
		for (const n of payload.nodes) {
			const pos = g.node(n.ref);
			if (!pos) continue;
			const color = palette.colorForCollection(n.collection);
			nodes.push({
				id: n.id,
				ref: n.ref,
				title: n.title,
				collection: n.collection,
				status: n.status,
				isTerminal: n.is_terminal,
				childCount: n.child_count,
				x: pos.x,
				y: pos.y,
				color
			});
		}

		const posByRef = new Map(nodes.map((n) => [n.ref, n]));
		const edges: RenderEdge[] = [];
		for (const e of keptEdges) {
			const s = posByRef.get(e.source);
			const t = posByRef.get(e.target);
			if (!s || !t) continue;
			edges.push({
				source: e.source,
				target: e.target,
				type: e.type,
				x1: s.x,
				y1: s.y,
				x2: t.x,
				y2: t.y
			});
		}

		// Legend in palette-assignment order.
		const legendEntries = Object.entries(palette.colors).map(([slug, color]) => ({
			slug,
			color
		}));

		return { nodes, edges, legend: legendEntries };
	}

	let refreshing = $state(false);

	// Fetch + lay out the current (workspace, currentFocus, depth, includeTerminal).
	// `background` mode (SSE-driven refetch) keeps the existing graph visible — no
	// loading spinner, no view-refit, and errors are swallowed so an ambient blip
	// never replaces a good graph with an error card. The loadToken guard discards
	// stale in-flight responses (CONVE-1688: never read+write the same committed
	// $state inside one reactive pass — the token is a plain local, not a rune).
	async function load(background: boolean) {
		const ws = workspace;
		const focus = currentFocus;
		const d = depth;
		const term = includeTerminal;

		const token = ++loadToken;
		loadInFlight = true;
		if (background) {
			refreshing = true;
		} else {
			// A foreground load (initial, reroot, depth/term change) supersedes any
			// armed background refetch — cancel its timer so the delayed callback
			// can't fire later and bump loadToken, cancelling THIS load and
			// (on a background failure) leaving the view stuck on the spinner.
			if (refetchTimer) {
				clearTimeout(refetchTimer);
				refetchTimer = null;
			}
			loadState = 'loading';
			errorMessage = '';
		}

		try {
			const payload = await api.graph.getFocused(ws, focus, { depth: d, includeTerminal: term });
			if (token !== loadToken) return;
			const laid = await runLayout(payload);
			if (token !== loadToken) return;
			renderNodes = laid.nodes;
			renderEdges = laid.edges;
			legend = laid.legend;
			truncated = payload.truncated ?? false;
			// Drop a selection that no longer exists in the new neighborhood.
			if (selectedRef && !laid.nodes.some((n) => n.ref === selectedRef)) {
				selectedRef = null;
			}
			loadState = 'ready';
			if (!background) queueFit(); // don't yank the view on ambient updates
		} catch (err) {
			if (token !== loadToken) return;
			if (!background) {
				errorMessage = err instanceof Error ? err.message : 'Failed to load the graph.';
				loadState = 'error';
			}
			// background failures: keep the last good graph, try again next event
		} finally {
			// Only the latest load owns the shared flags — a stale (superseded)
			// load must not clear them out from under the newer one.
			if (token === loadToken) {
				refreshing = false;
				loadInFlight = false;
				// Flush a coalesced/deferred refetch now that we're idle. runRefetch
				// re-decides foreground (error recovery) vs background (ready) — so a
				// mid-load mutation reconciles, and a failed load can still recover
				// on the next event instead of getting stuck.
				if (pendingRefetch) {
					pendingRefetch = false;
					scheduleRefetch();
				}
			}
		}
	}

	// ── Data-loading effect ───────────────────────────────────────────────────────
	// Separate from the prop-sync effect (CONVE-606). load() reads workspace,
	// currentFocus, depth, includeTerminal synchronously (before its first await),
	// so they register as this effect's dependencies; retryNonce is read here to
	// force a re-run after an error without changing a real input.
	$effect(() => {
		void retryNonce;
		void load(false);
	});

	function retry() {
		retryNonce++;
	}

	// ── Live updates (SSE) ──────────────────────────────────────────────────────────
	// The workspace layout owns sseService.connect(); the open drawer only
	// subscribes. Events are workspace-scoped — we correlate to the visible
	// neighborhood by item UUID (node.id); events for items not in view no-op.
	const GLOW_MS = 2500;
	const REFETCH_DEBOUNCE_MS = 400;
	let touchedRefs = $state(new Set<string>());
	let refetchTimer: ReturnType<typeof setTimeout> | null = null;
	// Pending glow-clear timers, keyed by ref so a re-touch RESETS that ref's
	// window (a burst keeps it lit; the fade starts GLOW_MS after the LAST
	// event, not the first). Tracked so teardown can cancel them — otherwise a
	// timeout could fire and write touchedRefs on a destroyed component.
	const glowTimers = new Map<string, ReturnType<typeof setTimeout>>();
	// Set when a refetch is requested before the first graph is ready; flushed
	// once load() commits a ready graph, so a mutation that lands mid-initial-load
	// (and may not be reflected in that first response) still gets reconciled.
	let pendingRefetch = false;

	function refForUuid(uuid: string): string | undefined {
		return renderNodes.find((n) => n.id === uuid)?.ref;
	}

	function touch(uuid: string) {
		const ref = refForUuid(uuid);
		if (!ref) return;
		const existing = glowTimers.get(ref);
		if (existing) clearTimeout(existing); // reset this ref's fade window
		if (!touchedRefs.has(ref)) {
			const next = new Set(touchedRefs);
			next.add(ref);
			touchedRefs = next;
		}
		const handle = setTimeout(() => {
			glowTimers.delete(ref);
			const after = new Set(touchedRefs);
			after.delete(ref);
			touchedRefs = after;
		}, GLOW_MS);
		glowTimers.set(ref, handle);
	}

	function scheduleRefetch() {
		if (refetchTimer) clearTimeout(refetchTimer);
		refetchTimer = setTimeout(runRefetch, REFETCH_DEBOUNCE_MS);
	}

	// Fire a refetch, choosing the right mode for the current state. Deferring
	// (pendingRefetch) is flushed from load()'s finally once it settles, so no
	// signal is lost and no second load runs concurrently.
	function runRefetch() {
		refetchTimer = null;
		if (loadInFlight) {
			// Coalesce — a load is already awaiting; run one after it settles.
			pendingRefetch = true;
			return;
		}
		if (loadState === 'ready') {
			void load(true); // background: keep the current graph visible
		} else if (loadState === 'error') {
			void load(false); // recover from a transient failure on the next event
		} else {
			// 'loading'/'idle' with nothing in flight shouldn't happen, but defer
			// to be safe rather than start an unguarded load.
			pendingRefetch = true;
		}
	}

	function handleItemEvent(event: ItemEvent) {
		switch (event.type) {
			case 'item_updated':
			case 'item_created':
			case 'item_archived':
			case 'item_restored':
				// Glow only applies to nodes already on screen — touch() no-ops for
				// off-view items (nothing to light up).
				touch(event.item_id);
				// Refetch regardless of whether the item is currently in view.
				// Structural changes that add/remove a neighbor (a new child under a
				// visible parent, or a reparent) are published by the server on the
				// CHILD — which may be off-view — and the event carries no link info
				// to distinguish a reparent from a plain field edit. So we can't
				// gate on in-view without missing newly-attached neighbors. The cost
				// is bounded: the 400ms debounce collapses bursts into one small
				// getFocused call and the drawer is only open transiently.
				scheduleRefetch();
				break;
			case 'comment_created':
				touch(event.item_id); // ambient liveness, glow only
				break;
			// Ignore comment_updated/deleted, reaction_*, workspace_updated, etc.
		}
	}

	onMount(() => {
		const unsubEvent = sseService.onItemEvent(handleItemEvent);
		// Bulk updates / replay-gap backfills route through onSyncRequired, not
		// onItemEvent — fold them into the same debounced refetch.
		const unsubSync = sseService.onSyncRequired(() => scheduleRefetch());
		return () => {
			unsubEvent();
			unsubSync();
			if (refetchTimer) clearTimeout(refetchTimer);
			for (const h of glowTimers.values()) clearTimeout(h);
			glowTimers.clear();
			// Invalidate any in-flight load so it can't commit $state (or queue a
			// fitView) after the drawer closes / component unmounts.
			loadToken++;
		};
	});

	// ── Pan / zoom ────────────────────────────────────────────────────────────────
	let viewport = $state<HTMLDivElement | null>(null);
	let tx = $state(0);
	let ty = $state(0);
	let scale = $state(1);

	const MIN_SCALE = 0.25;
	const MAX_SCALE = 2.5;

	// Drag is engaged only AFTER the pointer moves past a small threshold — a
	// plain press (a click/double-click on a node) must not capture the pointer,
	// because pointer capture on pointerdown suppresses the node's click/dblclick
	// events (which drive selection + zoom).
	const DRAG_THRESHOLD = 4;
	let maybeDrag = false;
	let dragging = false;
	let suppressClick = false;
	let capturedPointerId: number | null = null;
	let dragStartX = 0;
	let dragStartY = 0;
	let dragOriginTx = 0;
	let dragOriginTy = 0;

	function onWheel(e: WheelEvent) {
		e.preventDefault();
		const rect = viewport?.getBoundingClientRect();
		if (!rect) return;
		const cx = e.clientX - rect.left;
		const cy = e.clientY - rect.top;
		const factor = e.deltaY < 0 ? 1.1 : 1 / 1.1;
		const next = Math.min(MAX_SCALE, Math.max(MIN_SCALE, scale * factor));
		const applied = next / scale;
		// Keep the cursor anchored over the same content point while zooming.
		tx = cx - (cx - tx) * applied;
		ty = cy - (cy - ty) * applied;
		scale = next;
	}

	function onPointerDown(e: PointerEvent) {
		if (e.button !== 0) return;
		// Don't start a pan when the press lands on an interactive overlay
		// (legend toggles, detail-card actions, error retry) — they sit inside the
		// viewport, so without this a click on them would also begin a drag.
		if ((e.target as Element).closest?.('.legend, .detail-card, .state-overlay')) return;
		maybeDrag = true;
		dragging = false;
		suppressClick = false;
		dragStartX = e.clientX;
		dragStartY = e.clientY;
		dragOriginTx = tx;
		dragOriginTy = ty;
		// NOTE: no setPointerCapture here — capturing on pointerdown would swallow
		// the node's click/dblclick. We capture only once a real drag starts.
	}
	function onPointerMove(e: PointerEvent) {
		if (!maybeDrag) return;
		const dx = e.clientX - dragStartX;
		const dy = e.clientY - dragStartY;
		if (!dragging) {
			if (Math.hypot(dx, dy) < DRAG_THRESHOLD) return;
			dragging = true;
			suppressClick = true; // this gesture is a pan, not a click
			capturedPointerId = e.pointerId;
			try {
				(e.currentTarget as Element).setPointerCapture(e.pointerId);
			} catch {
				// capture unsupported/failed — panning still works while over the viewport
			}
		}
		tx = dragOriginTx + dx;
		ty = dragOriginTy + dy;
	}
	function onPointerUp(e: PointerEvent) {
		maybeDrag = false;
		if (dragging) {
			dragging = false;
			if (capturedPointerId !== null) {
				try {
					(e.currentTarget as Element).releasePointerCapture(capturedPointerId);
				} catch {
					// pointer may already be released — ignore.
				}
				capturedPointerId = null;
			}
		}
	}

	// Bounds of the currently VISIBLE nodes (respecting legend toggles), or null
	// when nothing is visible. Computed live so Fit frames what's actually on
	// screen after collections are hidden.
	function currentBounds(): { x: number; y: number; w: number; h: number } | null {
		const ns = visibleNodes;
		if (!ns.length) return null;
		let minX = Infinity;
		let minY = Infinity;
		let maxX = -Infinity;
		let maxY = -Infinity;
		for (const n of ns) {
			minX = Math.min(minX, n.x - NODE_W / 2);
			minY = Math.min(minY, n.y - NODE_H / 2);
			maxX = Math.max(maxX, n.x + NODE_W / 2);
			maxY = Math.max(maxY, n.y + NODE_H / 2);
		}
		return { x: minX, y: minY, w: Math.max(1, maxX - minX), h: Math.max(1, maxY - minY) };
	}

	// Fit the visible content into the current viewport with a margin. No-ops when
	// nothing is visible (e.g. every collection hidden) rather than framing the
	// hidden graph.
	function fitView() {
		const rect = viewport?.getBoundingClientRect();
		const b = currentBounds();
		if (!rect || !b || b.w <= 0 || b.h <= 0) return;
		const margin = 40;
		const availW = Math.max(1, rect.width - margin * 2);
		const availH = Math.max(1, rect.height - margin * 2);
		const next = Math.min(MAX_SCALE, Math.max(MIN_SCALE, Math.min(availW / b.w, availH / b.h)));
		scale = next;
		// Center the content within the viewport.
		const contentCx = b.x + b.w / 2;
		const contentCy = b.y + b.h / 2;
		tx = rect.width / 2 - contentCx * next;
		ty = rect.height / 2 - contentCy * next;
	}

	// Defer the fit until after the DOM reflects new content + the viewport exists.
	function queueFit() {
		requestAnimationFrame(() => fitView());
	}

	// ── Interactions ──────────────────────────────────────────────────────────────
	function collectionFor(ref: string): string | undefined {
		return renderNodes.find((n) => n.ref === ref)?.collection;
	}

	// Single click selects a node and opens its detail panel. Re-rooting and
	// opening the item are explicit actions in that panel (so a stray click can't
	// navigate away). Double click zooms to the node.
	function onNodeClick(ref: string) {
		if (suppressClick) {
			suppressClick = false; // this click concluded a pan gesture — ignore it
			return;
		}
		selectedRef = ref;
	}

	function onNodeDblClick(ref: string) {
		selectedRef = ref;
		zoomToNode(ref);
	}

	function focusHere(ref: string) {
		currentFocus = ref; // re-root the neighborhood on this node
	}

	function backToOrigin() {
		currentFocus = focusRef;
	}

	function changeDepth(value: string) {
		depth = clampDepth(Number(value));
	}

	// Legend toggle (#3): hide/show a collection's nodes without recomputing the
	// layout. Reassign the Set so the derived filters re-run.
	function toggleCollection(slug: string) {
		const next = new Set(hiddenCollections);
		if (next.has(slug)) next.delete(slug);
		else next.add(slug);
		hiddenCollections = next;
		// Close the detail panel if its node just got hidden.
		if (selectedNode && hiddenCollections.has(selectedNode.collection)) {
			selectedRef = null;
		}
	}

	// Center the viewport on a node and zoom in (double-click, #4).
	function zoomToNode(ref: string) {
		const n = renderNodes.find((x) => x.ref === ref);
		const rect = viewport?.getBoundingClientRect();
		if (!n || !rect) return;
		const targetScale = Math.min(MAX_SCALE, Math.max(scale, 1.6));
		scale = targetScale;
		tx = rect.width / 2 - n.x * targetScale;
		ty = rect.height / 2 - n.y * targetScale;
	}

	// ── Edge styling helpers ──────────────────────────────────────────────────────
	type EdgeClass = 'hierarchy' | 'blocks' | 'wiki' | 'muted';
	function edgeClass(type: GraphEdge['type']): EdgeClass {
		if (type === 'parent' || type === 'implements') return 'hierarchy';
		if (type === 'blocks') return 'blocks';
		if (type === 'wiki-link') return 'wiki';
		return 'muted';
	}

	// Cubic path between two node centers for a softer hierarchy tether.
	function edgePath(e: RenderEdge): string {
		const midY = (e.y1 + e.y2) / 2;
		return `M ${e.x1} ${e.y1} C ${e.x1} ${midY}, ${e.x2} ${midY}, ${e.x2} ${e.y2}`;
	}

	// Wrap a title into up to maxLines lines of ~maxPerLine chars for the node
	// card, breaking on spaces where possible and ellipsizing any overflow.
	function titleLines(title: string, maxPerLine = 32, maxLines = 2): string[] {
		const trimmed = title.trim();
		if (trimmed.length <= maxPerLine) return [trimmed];
		const lines: string[] = [];
		let rest = trimmed;
		while (rest.length && lines.length < maxLines) {
			if (rest.length <= maxPerLine) {
				lines.push(rest);
				rest = '';
				break;
			}
			let cut = rest.lastIndexOf(' ', maxPerLine);
			if (cut < maxPerLine * 0.55) cut = maxPerLine; // no good space → hard cut
			lines.push(rest.slice(0, cut).trim());
			rest = rest.slice(cut).trim();
		}
		if (rest.length) {
			// Overflow beyond maxLines — ellipsize the last line.
			const last = lines[lines.length - 1];
			lines[lines.length - 1] =
				(last.length > maxPerLine - 1 ? last.slice(0, maxPerLine - 1) : last).replace(/\s+$/, '') +
				'…';
		}
		return lines;
	}

	const isEmptyNeighborhood = $derived(loadState === 'ready' && renderNodes.length <= 1);
	const atOrigin = $derived(currentFocus === focusRef);
</script>

<div class="item-graph">
	<!-- Controls bar -->
	<div class="controls">
		<div class="control-group">
			<label class="ctrl-label" for="ig-depth">Depth</label>
			<select
				id="ig-depth"
				class="select"
				value={String(depth)}
				onchange={(e) => changeDepth(e.currentTarget.value)}
			>
				{#each [1, 2, 3, 4, 5] as d (d)}
					<option value={String(d)}>{d}</option>
				{/each}
			</select>
		</div>

		<label class="toggle">
			<input type="checkbox" bind:checked={includeTerminal} />
			<span>Include done</span>
		</label>

		{#if !atOrigin}
			<button type="button" class="ghost-btn" onclick={backToOrigin} title="Return to the original item">
				← {focusRef}
			</button>
		{/if}

		<div class="spacer"></div>

		<button type="button" class="ghost-btn" onclick={fitView} title="Fit graph to view">Fit</button>
		<a class="open-btn" href={itemHref(currentFocus, collectionFor(currentFocus))}>Open ↗</a>
	</div>

	<!-- Canvas -->
	<div
		class="viewport"
		bind:this={viewport}
		onwheel={onWheel}
		onpointerdown={onPointerDown}
		onpointermove={onPointerMove}
		onpointerup={onPointerUp}
		onpointercancel={onPointerUp}
		role="application"
		aria-label="Dependency graph canvas"
	>
		{#if loadState === 'loading'}
			<div class="state-overlay" aria-live="polite">
				<div class="spinner" aria-hidden="true"></div>
				<p>Loading graph…</p>
			</div>
		{:else if loadState === 'error'}
			<div class="state-overlay" role="alert">
				<p class="error-msg">{errorMessage}</p>
				<button type="button" class="open-btn" onclick={retry}>Retry</button>
			</div>
		{/if}

		{#if loadState === 'ready' || renderNodes.length > 0}
			<svg class="canvas" width="100%" height="100%" aria-hidden={loadState !== 'ready'}>
				<defs>
					<marker
						id="ig-arrow-blocks"
						viewBox="0 0 10 10"
						refX="9"
						refY="5"
						markerWidth="7"
						markerHeight="7"
						orient="auto-start-reverse"
					>
						<path d="M 0 0 L 10 5 L 0 10 z" fill="#f43f5e" />
					</marker>
				</defs>

				<g transform="translate({tx} {ty}) scale({scale})">
					<!-- Edges first so nodes paint over them. -->
					{#each visibleEdges as e (e.source + '->' + e.target + ':' + e.type)}
						{@const cls = edgeClass(e.type)}
						<path
							class="edge {cls}"
							d={edgePath(e)}
							marker-end={cls === 'blocks' ? 'url(#ig-arrow-blocks)' : undefined}
						/>
					{/each}

					<!-- Nodes. -->
					{#each visibleNodes as n (n.ref)}
						{@const isFocus = n.ref === currentFocus}
						{@const lines = titleLines(n.title)}
						<g
							class="node"
							class:focus={isFocus}
							class:terminal={n.isTerminal}
							class:touched={touchedRefs.has(n.ref)}
							class:selected={n.ref === selectedRef}
							transform="translate({n.x - NODE_W / 2} {n.y - NODE_H / 2})"
							role="button"
							tabindex="0"
							aria-label={n.ref + ': ' + n.title}
							onclick={() => onNodeClick(n.ref)}
							ondblclick={() => onNodeDblClick(n.ref)}
							onkeydown={(e) => {
								if (e.key === 'Enter' || e.key === ' ') {
									e.preventDefault();
									onNodeClick(n.ref);
								}
							}}
						>
							<rect
								class="node-bg"
								width={NODE_W}
								height={NODE_H}
								rx="10"
								ry="10"
								style:fill="color-mix(in srgb, {n.color} 15%, transparent)"
								style:stroke={isFocus ? n.color : `color-mix(in srgb, ${n.color} 55%, transparent)`}
							/>
							<rect
								class="node-accent"
								width="4"
								height={NODE_H}
								rx="2"
								style:fill={n.color}
							/>
							<text class="node-ref" x="14" y="20">{n.ref}</text>
							{#each lines as line, i (i)}
								<text class="node-title" x="14" y={36 + i * 13}>{line}</text>
							{/each}
						</g>
					{/each}
				</g>
			</svg>
		{/if}

		{#if isEmptyNeighborhood && loadState === 'ready'}
			<div class="empty-hint">No linked items — this item stands alone.</div>
		{/if}

		<!-- Legend overlay — click an entry to hide/show that collection. -->
		{#if legend.length > 0 && loadState === 'ready'}
			<div class="legend" aria-label="Collection legend — click to toggle">
				{#each legend as entry (entry.slug)}
					{@const off = hiddenCollections.has(entry.slug)}
					<button
						type="button"
						class="legend-item"
						class:off
						aria-pressed={!off}
						title={off ? `Show ${entry.slug}` : `Hide ${entry.slug}`}
						onclick={() => toggleCollection(entry.slug)}
					>
						<span class="legend-dot" style:background-color={entry.color} aria-hidden="true"></span>
						{entry.slug}
					</button>
				{/each}
			</div>
		{/if}

		<!-- Node detail panel (single-click selection). -->
		{#if selectedNode}
			{@const sel = selectedNode}
			<div class="detail-card" role="group" aria-label="Item details">
				<div class="detail-head">
					<span class="detail-ref">{sel.ref}</span>
					<button
						type="button"
						class="detail-close"
						aria-label="Close details"
						onclick={() => (selectedRef = null)}>✕</button
					>
				</div>
				<p class="detail-title">{sel.title}</p>
				<div class="detail-meta">
					<span class="detail-chip" style:background-color="color-mix(in srgb, {sel.color} 22%, transparent)" style:border-color={sel.color}>{sel.collection}</span>
					{#if sel.status}<span class="detail-stat">{sel.status}</span>{/if}
					{#if sel.isTerminal}<span class="detail-stat done">✓ done</span>{/if}
					{#if sel.childCount > 0}
						<span class="detail-stat">{sel.childCount} child{sel.childCount === 1 ? '' : 'ren'}</span>
					{/if}
				</div>
				<div class="detail-actions">
					<a class="open-btn" href={itemHref(sel.ref, sel.collection)}>Open item ↗</a>
					{#if sel.ref !== currentFocus}
						<button type="button" class="ghost-btn" onclick={() => focusHere(sel.ref)}>Focus here</button>
					{/if}
				</div>
			</div>
		{/if}

		<!-- Truncation indicator -->
		{#if truncated && loadState === 'ready'}
			<div class="truncated-note">
				Showing the closest {renderNodes.length} items — click a node to explore further.
			</div>
		{/if}

		<!-- Live-refresh indicator (SSE-driven background reload). -->
		{#if refreshing && loadState === 'ready'}
			<div class="live-note" aria-live="polite">
				<span class="live-dot" aria-hidden="true"></span>
				updating…
			</div>
		{/if}
	</div>
</div>

<style>
	.item-graph {
		position: relative;
		display: flex;
		flex-direction: column;
		height: 100%;
		min-height: 0;
		background: var(--bg-primary, #0a0a1a);
		color: var(--text-primary);
	}

	/* ── Controls ─────────────────────────────────────────────────────────────── */
	.controls {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-wrap: wrap;
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--bg-secondary) 88%, transparent);
		border-bottom: 1px solid var(--border);
		backdrop-filter: blur(6px);
	}
	.control-group {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
	}
	.ctrl-label {
		font-size: 0.72em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-muted);
	}
	.select {
		padding: var(--space-1) var(--space-2);
		font-size: 0.8em;
		color: var(--text-primary);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		cursor: pointer;
	}
	.toggle {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.8em;
		font-weight: 600;
		color: var(--text-secondary);
		cursor: pointer;
		user-select: none;
	}
	.toggle input {
		cursor: pointer;
	}
	.spacer {
		flex: 1 1 auto;
	}
	.ghost-btn {
		padding: var(--space-1) var(--space-3);
		font-size: 0.78em;
		font-weight: 600;
		color: var(--text-secondary);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: 999px;
		cursor: pointer;
		transition: border-color 0.15s, color 0.15s;
	}
	.ghost-btn:hover {
		border-color: var(--text-muted);
		color: var(--text-primary);
	}
	.open-btn {
		display: inline-block;
		padding: var(--space-1) var(--space-3);
		font-size: 0.8em;
		font-weight: 600;
		color: var(--btn-primary-text, #fff);
		background: var(--accent, #6366f1);
		border: none;
		border-radius: var(--radius);
		cursor: pointer;
		text-decoration: none;
		text-align: center;
	}
	.open-btn:hover {
		filter: brightness(1.08);
	}

	/* ── Viewport / canvas ────────────────────────────────────────────────────── */
	.viewport {
		position: relative;
		flex: 1 1 auto;
		min-height: 0;
		overflow: hidden;
		cursor: grab;
		touch-action: none;
	}
	.viewport:active {
		cursor: grabbing;
	}
	.canvas {
		display: block;
		width: 100%;
		height: 100%;
	}

	/* ── Edges ────────────────────────────────────────────────────────────────── */
	.edge {
		fill: none;
		stroke-width: 1.5;
	}
	.edge.hierarchy {
		stroke: color-mix(in srgb, var(--text-muted) 55%, transparent);
	}
	.edge.blocks {
		stroke: #f43f5e;
		stroke-width: 2;
	}
	.edge.wiki {
		stroke: color-mix(in srgb, var(--text-muted) 35%, transparent);
		stroke-width: 1;
		stroke-dasharray: 2 4;
	}
	.edge.muted {
		stroke: color-mix(in srgb, var(--text-muted) 45%, transparent);
		stroke-dasharray: 5 4;
	}

	/* ── Nodes ────────────────────────────────────────────────────────────────── */
	.node {
		cursor: pointer;
	}
	.node-bg {
		stroke-width: 1.5;
		transition: stroke-width 0.12s, filter 0.5s ease-out;
	}
	.node.focus .node-bg {
		stroke-width: 3;
		filter: drop-shadow(0 0 6px color-mix(in srgb, var(--accent, #6366f1) 60%, transparent));
	}
	.node.terminal {
		opacity: 0.5;
	}
	.node.terminal .node-bg {
		stroke-dasharray: 4 3;
	}
	.node:hover .node-bg {
		stroke-width: 2.5;
	}
	.node:focus-visible .node-bg {
		stroke-width: 3;
	}
	/* Selected node (single-click) — distinct ring from the focus/root node. */
	.node.selected .node-bg {
		stroke-width: 3;
		filter: drop-shadow(0 0 7px color-mix(in srgb, var(--text-primary) 45%, transparent));
	}
	/* Transient glow when an item changes live (SSE). Transition-based (not a
	   one-shot keyframe) so a burst of events keeps the node lit and it fades
	   out via the .node-bg filter transition once the ref leaves touchedRefs. */
	.node.touched .node-bg {
		filter: drop-shadow(0 0 9px color-mix(in srgb, #fff 80%, transparent));
		stroke-width: 3;
	}
	.node-ref {
		font-family: var(--font-mono, ui-monospace, monospace);
		font-size: 11px;
		font-weight: 700;
		fill: var(--text-primary);
	}
	.node-title {
		font-size: 10px;
		fill: var(--text-secondary);
	}

	/* ── Overlays ─────────────────────────────────────────────────────────────── */
	.state-overlay {
		position: absolute;
		inset: 0;
		z-index: 5;
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: var(--space-3);
		font-size: 0.85em;
		color: var(--text-muted);
		background: color-mix(in srgb, var(--bg-primary, #0a0a1a) 70%, transparent);
		backdrop-filter: blur(2px);
	}
	.error-msg {
		max-width: 24rem;
		text-align: center;
		color: var(--blocks-red, #f43f5e);
	}
	.spinner {
		width: 1.6rem;
		height: 1.6rem;
		border: 2px solid color-mix(in srgb, var(--text-muted) 30%, transparent);
		border-top-color: var(--accent, #6366f1);
		border-radius: 50%;
		animation: spin 0.8s linear infinite;
	}
	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}

	.empty-hint {
		position: absolute;
		bottom: var(--space-4);
		left: 50%;
		transform: translateX(-50%);
		padding: var(--space-1) var(--space-3);
		font-size: 0.78em;
		color: var(--text-muted);
		background: color-mix(in srgb, var(--bg-secondary) 90%, transparent);
		border: 1px solid var(--border);
		border-radius: 999px;
		backdrop-filter: blur(6px);
	}

	.legend {
		position: absolute;
		top: var(--space-3);
		left: var(--space-3);
		z-index: 4;
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		max-width: calc(100% - var(--space-6));
		padding: var(--space-1) var(--space-3);
		background: color-mix(in srgb, var(--bg-secondary) 86%, transparent);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		backdrop-filter: blur(6px);
	}
	.legend-item {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		font-size: 0.72em;
		text-transform: capitalize;
		color: var(--text-secondary);
		background: none;
		border: none;
		padding: 2px 4px;
		border-radius: var(--radius);
		cursor: pointer;
	}
	.legend-item:hover {
		color: var(--text-primary);
		background: color-mix(in srgb, var(--text-muted) 12%, transparent);
	}
	/* Toggled-off collection — dimmed + struck through, dot hollowed. */
	.legend-item.off {
		opacity: 0.45;
		text-decoration: line-through;
	}
	.legend-item.off .legend-dot {
		background: transparent !important;
		box-shadow: inset 0 0 0 1.5px currentColor;
	}
	.legend-dot {
		width: 0.6rem;
		height: 0.6rem;
		border-radius: 50%;
		flex: 0 0 auto;
	}

	.truncated-note {
		position: absolute;
		bottom: var(--space-3);
		left: var(--space-3);
		right: var(--space-3);
		z-index: 4;
		padding: var(--space-1) var(--space-3);
		font-size: 0.74em;
		text-align: center;
		color: var(--text-muted);
		background: color-mix(in srgb, var(--bg-secondary) 90%, transparent);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		backdrop-filter: blur(6px);
	}

	.live-note {
		position: absolute;
		top: var(--space-3);
		right: var(--space-3);
		z-index: 4;
		display: flex;
		align-items: center;
		gap: 6px;
		padding: 2px var(--space-2);
		font-size: 0.72em;
		color: var(--text-muted);
		background: color-mix(in srgb, var(--bg-secondary) 90%, transparent);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		backdrop-filter: blur(6px);
	}
	.live-dot {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		background: #10b981;
		animation: live-blink 1s ease-in-out infinite;
	}
	@keyframes live-blink {
		0%,
		100% {
			opacity: 1;
		}
		50% {
			opacity: 0.3;
		}
	}

	/* ── Node detail panel ────────────────────────────────────────────────────── */
	.detail-card {
		position: absolute;
		right: var(--space-3);
		bottom: var(--space-3);
		z-index: 6;
		width: min(320px, calc(100% - var(--space-6)));
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-3);
		background: color-mix(in srgb, var(--bg-secondary) 94%, transparent);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 8px 28px rgba(0, 0, 0, 0.35);
		backdrop-filter: blur(8px);
	}
	.detail-head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-2);
	}
	.detail-ref {
		font-family: var(--font-mono, ui-monospace, monospace);
		font-size: 0.8em;
		font-weight: 700;
		color: var(--text-primary);
	}
	.detail-close {
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		font-size: 0.85em;
		line-height: 1;
		padding: 2px;
	}
	.detail-close:hover {
		color: var(--text-primary);
	}
	.detail-title {
		margin: 0;
		font-size: 0.9em;
		font-weight: 600;
		color: var(--text-primary);
		line-height: 1.3;
	}
	.detail-meta {
		display: flex;
		flex-wrap: wrap;
		gap: 0.35rem;
		align-items: center;
	}
	.detail-chip {
		font-size: 0.7em;
		text-transform: capitalize;
		color: var(--text-primary);
		padding: 1px 8px;
		border: 1px solid;
		border-radius: 999px;
	}
	.detail-stat {
		font-size: 0.7em;
		color: var(--text-secondary);
		background: color-mix(in srgb, var(--text-muted) 14%, transparent);
		padding: 1px 8px;
		border-radius: 999px;
		text-transform: capitalize;
	}
	.detail-stat.done {
		color: #10b981;
	}
	.detail-actions {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		margin-top: 2px;
	}
</style>
