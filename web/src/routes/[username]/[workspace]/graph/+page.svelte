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
	import { sseService, type ItemEvent } from '$lib/services/sse.svelte';
	import type { NodeObject, LinkObject } from '3d-force-graph';
	import type { GraphResponse, Item } from '$lib/types';
	import DetailCard from './DetailCard.svelte';
	import GraphToolbar from './GraphToolbar.svelte';

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

	// ── Live layer (SSE) state (PLAN-1730 / TASK-1736) ───────────────────────────
	// The graph reacts to workspace mutations in real time: touched items glow and
	// fade; created/archived/restored items appear/disappear via a debounced
	// refetch. All of this state is plain `let` (CONVE-1688) — it's mutated from the
	// SSE callback + an interval and read by the renderer accessors, never by an
	// $effect. Re-evaluation is forced explicitly via graph.refresh().

	// How long a touched node glows before it has fully decayed back to its base
	// collection color (~45s of ambient afterglow).
	const PULSE_MS = 45_000;
	// Trailing-debounce window for structural refetches: coalesce a burst of
	// create/archive/restore/update events into a single loadGraph call.
	const REFETCH_DEBOUNCE_MS = 1500;
	// Prune/refresh cadence while any node is glowing — drives the fade animation by
	// re-running nodeColor on a steady tick.
	const PRUNE_INTERVAL_MS = 2000;

	// uuid → ref map, rebuilt from graphData on every payload commit. SSE events
	// carry the item UUID (item_id); the renderer keys on ref. This is the bridge.
	// IMPORTANT (per the renderer mapping `{ ...n, id: n.ref }`): we can't read the
	// uuid back off a renderer node, so we maintain it from the raw GraphResponse.
	let uuidToRef = new Map<string, string>();
	// ref → timestamp(ms) of last touch. A node touched < PULSE_MS ago glows.
	let touchedAt = new Map<string, number>();
	// Refs of items created while this page is open — marked glowing once the
	// refetch that brings them in lands (their uuid enters uuidToRef then).
	let pendingNewUuids = new Set<string>();
	// Plain `let` handles (CONVE-1688) cleared in teardown.
	let pruneInterval: ReturnType<typeof setInterval> | null = null;
	let refetchTimer: ReturnType<typeof setTimeout> | null = null;
	let unsubscribeSSE: (() => void) | null = null;

	// ── Focus / selection state (PLAN-1730 / TASK-1734) ──────────────────────────
	// The dim-everything-else highlight is driven by two plain `let` Sets that the
	// renderer accessor closures read. Per CONVE-1688 these stay non-reactive — they
	// are mutated imperatively in the click handler, never tracked by an $effect.
	// Re-evaluation is triggered explicitly by calling `graph.refresh()` after each
	// change (3d-force-graph README: `refresh()` "Redraws all the nodes/links",
	// re-running every color/opacity accessor).
	let selectedRef: string | null = null;
	let neighborRefs = new Set<string>();

	// The selected node, surfaced to the detail-card template. A separate $state from
	// the plain Sets above: this one is READ in markup, so it must be reactive — but
	// no $effect both reads and writes it (it's only written from event handlers).
	let selectedNode = $state<GraphNode3D | null>(null);
	// Richer item detail, fetched lazily on select (priority / assignee live here).
	let selectedItem = $state<Item | null>(null);
	let selectedItemLoading = $state(false);
	// Stale-select token — same shape as reqSeq; gates which in-flight item fetch
	// commits so a fast re-select can't be clobbered by an older response.
	let selectSeq = 0;

	// ── Filters (PLAN-1730 / TASK-1735) ──────────────────────────────────────────
	// Client-side filters over the loaded payload. All three are reactive $state so
	// the renderer-sync effect (which reads `filteredData`, derived from these) re-
	// runs on any change. Empty collection/status selection === no filter on that
	// axis (matches the insights page semantics); a null role === no role filter.
	// Per CONVE-1688: these are written only from event handlers (the toolbar
	// callbacks) and a deselect-on-change effect that writes deselect()'s state but
	// never reads graphData — never read+written by the same effect.
	let filterCollections = $state<string[]>([]);
	let filterStatuses = $state<string[]>([]);
	let filterRole = $state<string | null>(null);

	// Distinct option lists for the toolbar controls, in first-seen order so the
	// chips stay stable within a payload.
	const distinctCollections = $derived.by<string[]>(() => {
		const seen = new Set<string>();
		const out: string[] = [];
		for (const n of graphData?.nodes ?? []) {
			if (!seen.has(n.collection)) {
				seen.add(n.collection);
				out.push(n.collection);
			}
		}
		return out;
	});
	const distinctStatuses = $derived.by<string[]>(() => {
		const seen = new Set<string>();
		const out: string[] = [];
		for (const n of graphData?.nodes ?? []) {
			if (n.status && !seen.has(n.status)) {
				seen.add(n.status);
				out.push(n.status);
			}
		}
		return out;
	});
	const distinctRoles = $derived.by<string[]>(() => {
		const seen = new Set<string>();
		const out: string[] = [];
		for (const n of graphData?.nodes ?? []) {
			if (n.role && !seen.has(n.role)) {
				seen.add(n.role);
				out.push(n.role);
			}
		}
		return out;
	});

	// Any filter active? Drives the "X of Y" vs "X" count readout.
	const filtersActive = $derived(
		filterCollections.length > 0 || filterStatuses.length > 0 || filterRole !== null
	);

	// The filtered subset fed to the renderer: nodes matching every active axis, and
	// edges where BOTH endpoints survive. The force layout re-settles on change —
	// expected and fine. When no filter is active this is graphData verbatim.
	const filteredData = $derived.by<GraphResponse | null>(() => {
		if (!graphData) return null;
		if (!filtersActive) return graphData;
		const collSet = new Set(filterCollections);
		const statusSet = new Set(filterStatuses);
		const nodes = graphData.nodes.filter((n) => {
			if (collSet.size > 0 && !collSet.has(n.collection)) return false;
			if (statusSet.size > 0 && (!n.status || !statusSet.has(n.status))) return false;
			if (filterRole !== null && n.role !== filterRole) return false;
			return true;
		});
		const surviving = new Set(nodes.map((n) => n.ref));
		const edges = graphData.edges.filter(
			(e) => surviving.has(e.source) && surviving.has(e.target)
		);
		return { nodes, edges };
	});

	// Node/edge counts: FILTERED for the live readout, total for the "X of Y" form.
	const nodeCount = $derived(filteredData?.nodes.length ?? 0);
	const edgeCount = $derived(filteredData?.edges.length ?? 0);
	const totalNodeCount = $derived(graphData?.nodes.length ?? 0);
	const totalEdgeCount = $derived(graphData?.edges.length ?? 0);
	// Empty state keys off the unfiltered payload — a filter that hides everything is
	// the user's doing, not an empty workspace.
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
				// Selection-aware color: in focus mode, anything outside the selected
				// node's neighborhood is dimmed to a low-alpha version of its collection
				// color (the accessor reads the plain `let` Sets above).
				.nodeColor((n: NodeObject) => nodeColor(asNode(n)))
				.nodeLabel((n: NodeObject) => `${escapeHtml(asNode(n).ref)} — ${escapeHtml(asNode(n).name)}`)
				.nodeOpacity(0.95)
				// 'blocks' edges read red with a directional arrow; structural links
				// (parent/implements) brighter than soft links (wiki-link/related).
				// In focus mode, adjacent links brighten and the rest fade out.
				.linkColor((l: LinkObject<NodeObject>) => linkColor(asLink(l)))
				.linkOpacity(0.5)
				.linkWidth((l: LinkObject<NodeObject>) => (asLink(l).type === 'blocks' ? 1.5 : 0.5))
				.linkDirectionalArrowLength((l: LinkObject<NodeObject>) =>
					asLink(l).type === 'blocks' ? 3 : 0
				)
				.linkDirectionalArrowRelPos(1)
				.onNodeClick((n: NodeObject) => selectNode(asNode(n)))
				// Click empty space → exit focus mode (camera is left where it is).
				.onBackgroundClick(() => deselect());

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

	// ── Selection-aware accessors ────────────────────────────────────────────────
	// All three read the plain `let` selection Sets directly (CONVE-1688: no $state
	// in the imperative path). `graph.refresh()` re-runs them after each change.

	// Node color: collection hex normally; dimmed (low-alpha) when a selection is
	// active and this node isn't in the neighborhood.
	function nodeColor(n: GraphNode3D): string {
		const base = colorForCollection(n.collection);
		// Live-layer glow: a recently-touched node mixes toward white, brightest on a
		// fresh touch and decaying back to base over PULSE_MS (the prune interval's
		// graph.refresh ticks animate the fade). When nothing's touched, t=0 → base.
		const t = pulseFactor(n.ref);
		const lit = t > 0 ? mixHex(base, '#ffffff', t) : base;
		if (selectedRef === null) return lit;
		// Focus-mode composition (pulse-vs-dim): apply the pulse FIRST (on the lit
		// hex), THEN the dim alpha for out-of-neighborhood nodes — so a touched node
		// outside the selection still pulses, just subtly (it reads as a faint flash
		// in the dimmed crowd rather than vanishing). In-neighborhood nodes pulse at
		// full strength. This keeps the ambient-liveness signal visible without
		// fighting the focus highlight.
		return neighborRefs.has(n.ref) ? lit : hexToRgba(lit, 0.15);
	}

	// 'blocks' → red-ish; structural (parent/implements/supersedes/split-from) →
	// bright slate; soft (wiki-link/related) → dim slate. Alpha carries emphasis.
	// In focus mode: links touching the selected node brighten; the rest fade hard.
	function linkColor(l: GraphLink3D): string {
		if (selectedRef !== null) {
			// Compare against the preserved raw refs — the force layout mutates
			// source/target into node objects after ingest.
			const adjacent = l.sourceRef === selectedRef || l.targetRef === selectedRef;
			if (!adjacent) return 'rgba(148, 163, 184, 0.06)';
			if (l.type === 'blocks') return 'rgba(244, 63, 94, 0.95)';
			return 'rgba(148, 163, 184, 0.95)';
		}
		if (l.type === 'blocks') return 'rgba(244, 63, 94, 0.85)';
		if (l.type === 'parent' || l.type === 'implements' || l.type === 'supersedes' || l.type === 'split-from') {
			return 'rgba(148, 163, 184, 0.85)';
		}
		return 'rgba(148, 163, 184, 0.35)';
	}

	// Hex (#rrggbb) → rgba() with the given alpha. The dim treatment for out-of-
	// neighborhood nodes; mixing toward transparent reads as receding into the
	// dark backdrop without losing the collection hue entirely.
	function hexToRgba(hex: string, alpha: number): string {
		const r = parseInt(hex.slice(1, 3), 16);
		const g = parseInt(hex.slice(3, 5), 16);
		const b = parseInt(hex.slice(5, 7), 16);
		return `rgba(${r}, ${g}, ${b}, ${alpha})`;
	}

	// Linear-interpolate two #rrggbb hex colors by t∈[0,1] (0 → a, 1 → b),
	// returning #rrggbb. Drives the glow: mix the collection color toward white
	// proportionally to a touched node's freshness.
	function mixHex(a: string, b: string, t: number): string {
		const c = Math.max(0, Math.min(1, t));
		const ar = parseInt(a.slice(1, 3), 16);
		const ag = parseInt(a.slice(3, 5), 16);
		const ab = parseInt(a.slice(5, 7), 16);
		const br = parseInt(b.slice(1, 3), 16);
		const bg = parseInt(b.slice(3, 5), 16);
		const bb = parseInt(b.slice(5, 7), 16);
		const r = Math.round(ar + (br - ar) * c);
		const g = Math.round(ag + (bg - ag) * c);
		const bl = Math.round(ab + (bb - ab) * c);
		const hex = (v: number) => v.toString(16).padStart(2, '0');
		return `#${hex(r)}${hex(g)}${hex(bl)}`;
	}

	// Glow factor for a ref: 0 when not touched (or fully decayed), else a value
	// that starts at PULSE_PEAK on a fresh touch and decays linearly to 0 over
	// PULSE_MS. Used by nodeColor to mix the base toward white.
	const PULSE_PEAK = 0.85;
	function pulseFactor(ref: string): number {
		const at = touchedAt.get(ref);
		if (at === undefined) return 0;
		const age = Date.now() - at;
		if (age >= PULSE_MS) return 0;
		return PULSE_PEAK * (1 - age / PULSE_MS);
	}

	// ── Selection / focus mode ───────────────────────────────────────────────────
	// After the linked-list edges resolve to node objects the renderer mutates
	// `source`/`target` from refs into the node instances; but our GraphLink3D still
	// carries the original ref strings via the raw payload. We compute neighborhoods
	// from `graphData.edges` (the source of truth, untouched by the renderer).
	function computeNeighbors(ref: string): Set<string> {
		const set = new Set<string>([ref]);
		// Use the FILTERED edges so the neighborhood matches what's actually rendered —
		// a filtered-out neighbor isn't on screen to highlight anyway.
		const edges = filteredData?.edges ?? [];
		for (const e of edges) {
			if (e.source === ref) set.add(e.target);
			else if (e.target === ref) set.add(e.source);
		}
		return set;
	}

	function selectNode(node: GraphNode3D) {
		// Plain-`let` selection state (consulted by the accessors).
		selectedRef = node.ref;
		neighborRefs = computeNeighbors(node.ref);
		// Reactive copy for the detail card.
		selectedNode = node;

		// Camera fly-to: position the camera a comfortable distance out along the
		// node's position vector, looking at the node. Standard 3d-force-graph
		// pattern; guard the at-origin case where the vector has zero length.
		const dist = 60;
		const hyp = Math.hypot(node.x ?? 0, node.y ?? 0, node.z ?? 0);
		const ratio = hyp > 0 ? 1 + dist / hyp : 1;
		graph?.cameraPosition(
			{ x: (node.x ?? 0) * ratio, y: (node.y ?? 0) * ratio, z: (node.z ?? 0) * ratio },
			{ x: node.x ?? 0, y: node.y ?? 0, z: node.z ?? 0 },
			800
		);

		// Re-run every node/link accessor so the dim/highlight takes effect.
		graph?.refresh();

		// Fetch richer detail (priority / assignee) for the card. Stale-gated so a
		// rapid re-select can't be overwritten by an older response.
		void loadSelectedItem(node.ref);
	}

	// ── Filter toggles (TASK-1735) ───────────────────────────────────────────────
	// Event-handler writes to the filter $state (CONVE-1688: no effect reads+writes
	// these). The deselect-on-filter-change effect handles clearing a now-hidden
	// selection; the filteredData derived + sync effect handle the re-render.
	function toggleCollectionFilter(slug: string) {
		filterCollections = filterCollections.includes(slug)
			? filterCollections.filter((s) => s !== slug)
			: [...filterCollections, slug];
	}
	function toggleStatusFilter(status: string) {
		filterStatuses = filterStatuses.includes(status)
			? filterStatuses.filter((s) => s !== status)
			: [...filterStatuses, status];
	}
	function selectRoleFilter(role: string | null) {
		filterRole = role;
	}

	// Search fly-to (TASK-1735). The toolbar emits the chosen ref; resolve it to the
	// LIVE renderer node — `graph.graphData().nodes` carry the current x/y/z the
	// camera math in selectNode() needs (our static GraphResponse nodes don't). If
	// the node isn't found (shouldn't happen — search lists POST-filter nodes that
	// are by definition in the renderer), no-op gracefully.
	function flyToRef(ref: string) {
		if (!graph) return;
		const nodes = (graph.graphData()?.nodes ?? []) as NodeObject[];
		const match = nodes.find((n) => asNode(n).ref === ref);
		if (!match) return;
		selectNode(asNode(match));
	}

	async function loadSelectedItem(ref: string) {
		const seq = ++selectSeq;
		selectedItem = null;
		selectedItemLoading = true;
		try {
			// Refs resolve server-side (same path the node click used to navigate to).
			const item = await api.items.get(wsSlug, ref);
			if (seq !== selectSeq) return;
			selectedItem = item;
		} catch {
			// Card degrades gracefully — it just won't show priority/assignee.
			if (seq !== selectSeq) return;
			selectedItem = null;
		} finally {
			if (seq === selectSeq) selectedItemLoading = false;
		}
	}

	// Clear focus mode: un-dim everything, close the card. Does NOT move the camera
	// back (kept simple per TASK-1734). Bumps selectSeq so any in-flight item fetch
	// is discarded.
	function deselect() {
		if (selectedRef === null) return;
		selectedRef = null;
		neighborRefs = new Set<string>();
		selectedNode = null;
		selectedItem = null;
		selectedItemLoading = false;
		selectSeq++;
		graph?.refresh();
	}

	// Open the selected item's page — this is where the old direct-click navigation
	// moved to. Item pages live at [collection]/[slug]; the server's ResolveItem
	// resolves a PREFIX-NUMBER ref in the slug param, so the ref works directly.
	function openSelected() {
		if (!selectedNode) return;
		void goto(`/${username}/${wsSlug}/${selectedNode.collection}/${selectedNode.ref}`);
	}

	// Escape exits focus mode — but only when a node is selected, so it doesn't
	// swallow the key from other handlers (spirit of CONVE-639).
	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape' && selectedRef !== null) {
			e.preventDefault();
			deselect();
		}
	}

	// ── Data load + sync ─────────────────────────────────────────────────────────
	// Fetch whenever the workspace or the "show completed" toggle changes, with a
	// request token so a stale response can't clobber a newer one.
	$effect(() => {
		const slug = wsSlug;
		const withTerminal = showCompleted;
		// Either trigger (workspace switch or show-completed toggle) yields a fresh
		// payload in which the selected node may no longer exist — clear focus mode
		// so the dim/highlight + detail card don't reference a vanished node. This
		// effect reads graphData-independent state only via deselect() (which never
		// reads graphData), so it stays CONVE-1688-clean.
		deselect();
		if (slug !== graphWsSlug) {
			graphWsSlug = slug;
			// Drop the previous workspace's graph so it doesn't linger under the new
			// URL while the fetch is in flight — `loading` covers the gap.
			graphData = null;
			// A workspace switch brings a different collection/status/role universe, so
			// any prior selection would over-filter (none of B's slugs match A's). Reset
			// to no-filter. NOT done on a show-completed toggle: that's the SAME
			// workspace with a superset payload, where keeping filters is the right call.
			// These writes are filter $state this effect doesn't read — and the
			// filteredData/sync effects don't write them — so it stays CONVE-1688-clean.
			filterCollections = [];
			filterStatuses = [];
			filterRole = null;
			// Drop live-layer state carried over from the previous workspace: stale
			// glows, a stale uuid→ref bridge, pending-new uuids, and any in-flight
			// debounced refetch (which would otherwise fire loadGraph for the NEW slug
			// — harmless, but the explicit loadGraph below already covers it). The
			// prune interval self-stops once touchedAt empties on its next tick. Plain
			// `let` writes (CONVE-1688) this effect doesn't read.
			touchedAt.clear();
			uuidToRef.clear();
			pendingNewUuids.clear();
			if (refetchTimer) {
				clearTimeout(refetchTimer);
				refetchTimer = null;
			}
		}
		if (slug) {
			void loadGraph(slug, withTerminal);
		}
	});

	// Deselect whenever the filters change: the selected node may have just been
	// filtered out, in which case the dim/highlight + detail card would reference a
	// node that's no longer rendered. Reads the three filter $states (reactive) and
	// calls deselect() (which writes selection $state but never reads graphData or
	// the filter states) — so this effect never read+writes the same $state, keeping
	// it CONVE-1688-clean. Separate from the renderer-sync effect on purpose: that
	// one must NOT call deselect (it reads filteredData, and deselect writes selection
	// state — mixing them risks a read/write cycle on shared state).
	$effect(() => {
		// Track the filter axes.
		filterCollections;
		filterStatuses;
		filterRole;
		deselect();
	});

	// Push freshly-loaded data into the renderer once both are ready. Reads
	// graphData (reactive) + rendererReady (reactive); writes only the imperative
	// `graph` handle and the plain `collectionColors` map, never a tracked $state.
	// A null graphData (workspace switch in flight, or load error) clears the
	// canvas too — otherwise the previous workspace's nodes linger behind the
	// loading overlay (Codex round-1 finding #1).
	$effect(() => {
		// Feed the FILTERED subset (TASK-1735) — not the raw payload. filteredData is
		// graphData verbatim when no filter is active, so the unfiltered path is
		// unchanged. Reads filteredData (reactive, derived from graphData + the filter
		// $states); writes only the imperative `graph` handle + the plain
		// collectionColors map, never a tracked $state — CONVE-1688-clean.
		const data = filteredData;
		// Rebuild the uuid→ref bridge from the FULL payload (not the filtered subset)
		// so SSE correlation works for items the current filter happens to hide — a
		// touch on a filtered-out item still records correctly, and a refetch that
		// surfaces it shows it already glowing. Reads graphData; writes only the plain
		// uuidToRef map + glow bookkeeping, never a tracked $state — CONVE-1688-clean.
		uuidToRef = new Map<string, string>();
		for (const n of graphData?.nodes ?? []) uuidToRef.set(n.id, n.ref);
		// Items created since this page opened: now that the refetch has landed and
		// their uuid is in the bridge, mark their ref touched so they arrive glowing.
		if (pendingNewUuids.size > 0) {
			for (const uuid of pendingNewUuids) {
				const ref = uuidToRef.get(uuid);
				if (ref) {
					touchedAt.set(ref, Date.now());
					pendingNewUuids.delete(uuid);
				}
			}
			ensurePruneInterval();
		}
		// A refetch (same filter, e.g. an item archived) may have dropped the selected
		// node. The deselect-on-filter-change effect doesn't cover a same-filter
		// refetch, so check here: if the selection's ref is gone from the FULL payload,
		// clear focus mode. (uuidToRef keys are uuids; the selection is keyed by ref —
		// the renderer mapping overwrites node.id with the ref — so test the ref set.)
		// deselect() writes selection $state but never reads graphData/filteredData, so
		// this stays CONVE-1688-clean. Test against the full payload, not filteredData,
		// so a filter-driven hide is left to the filter-change effect; here we only
		// react to the item genuinely leaving the graph (archived).
		if (selectedRef !== null) {
			const stillPresent = (graphData?.nodes ?? []).some((n) => n.ref === selectedRef);
			if (!stillPresent) deselect();
		}
		if (!rendererReady || !graph) return;
		// Reset color assignment so collection→color stays stable per payload. Built
		// from the FULL node list (not the filtered subset) so a collection keeps its
		// hue whether or not the current filter happens to include it — the filter
		// chips and the rendered nodes must agree on color.
		collectionColors = {};
		for (const n of graphData?.nodes ?? []) colorForCollection(n.collection);
		graph.graphData({
			nodes: data ? data.nodes.map((n) => ({ ...n, id: n.ref, name: n.title })) : [],
			links: data
				? // sourceRef/targetRef preserve the raw ref strings: after ingest the
					// force layout mutates source/target into node OBJECTS, so any
					// accessor comparing endpoints against a ref (linkColor's focus-mode
					// adjacency check) must read these instead (Codex PR #702 round 1).
					data.edges.map((e) => ({
						source: e.source,
						target: e.target,
						sourceRef: e.source,
						targetRef: e.target,
						type: e.type
					}))
				: []
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

	// ── Live layer: SSE wiring (TASK-1736) ───────────────────────────────────────
	// The workspace +layout already calls sseService.connect(wsSlug) (and owns the
	// connection lifecycle across workspace switches), so the page only subscribes —
	// it never connects/disconnects. The SSE connection is workspace-scoped (the
	// EventSource URL pins ?workspace=slug), so events arriving here already belong
	// to the current workspace; the workspace_id guard below is belt-and-suspenders
	// for the cross-tab BroadcastChannel fan-out edge.

	onMount(() => {
		unsubscribeSSE = sseService.onItemEvent(handleItemEvent);
		// Bulk mutations (items_bulk_updated) and replay-buffer gaps don't fan
		// out through onItemEvent — the SSE service routes them to
		// onSyncRequired. Either way the graph may be stale, so fold them into
		// the same debounced refetch (Codex PR #704 round 1).
		const unsubscribeSync = sseService.onSyncRequired(() => scheduleRefetch());
		return () => {
			unsubscribeSSE?.();
			unsubscribeSSE = null;
			unsubscribeSync();
		};
	});

	function handleItemEvent(event: ItemEvent) {
		// Defensive workspace guard. The connection is already workspace-scoped, but
		// the store may carry the current workspace UUID — drop anything that doesn't
		// match. When the UUID isn't loaded yet, trust the connection scope.
		const wsId = workspaceStore.current?.id;
		if (wsId && event.workspace_id && event.workspace_id !== wsId) return;

		switch (event.type) {
			case 'item_updated': {
				// Glow immediately off the event (snappy), AND fold into the debounced
				// refetch — an update can change status/title/role, the fields the
				// card/filters read. Tradeoff: one refetch per burst (trailing-debounced),
				// and the graph payload is active-only and small, so the extra fetch is
				// cheap and keeps the rendered data true without per-field SSE patching.
				touchItem(event.item_id);
				scheduleRefetch();
				break;
			}
			case 'item_created':
			case 'item_archived':
			case 'item_restored': {
				// Structural changes: a node needs to appear/disappear. A created item's
				// uuid isn't in the bridge yet, so stash it — the renderer-sync effect
				// marks it glowing once the refetch lands and its uuid resolves to a ref.
				if (event.type === 'item_created') pendingNewUuids.add(event.item_id);
				else touchItem(event.item_id); // restore/archive: glow the surviving/leaving node
				scheduleRefetch();
				break;
			}
			case 'comment_created': {
				// A discussion lighting up its item is exactly the ambient liveness this
				// feature wants — treat it as a touch (glow only, no structural refetch).
				touchItem(event.item_id);
				break;
			}
			// Ignore everything else (comment_updated/deleted, reaction_*,
			// workspace_updated, items_bulk_updated, etc.).
		}
	}

	// Resolve an event's item UUID to a ref via the bridge and mark it touched. A
	// uuid not in the bridge (e.g. an item the current payload doesn't contain —
	// filtered out server-side as terminal, or a just-created item) simply no-ops
	// here; created items glow via the pendingNewUuids path after the refetch.
	function touchItem(uuid: string) {
		const ref = uuidToRef.get(uuid);
		if (!ref) return;
		touchedAt.set(ref, Date.now());
		ensurePruneInterval();
		graph?.refresh();
	}

	// Trailing-debounced refetch of the CURRENT (wsSlug, showCompleted) graph through
	// the existing loadGraph path — the filteredData pipeline + renderer-sync effect
	// then do the rest. Reuses loadGraph's stale-token (reqSeq) so an in-flight
	// manual load can't be clobbered. Plain `let` timer, cleared in teardown.
	function scheduleRefetch() {
		if (refetchTimer) clearTimeout(refetchTimer);
		refetchTimer = setTimeout(() => {
			refetchTimer = null;
			if (wsSlug) void loadGraph(wsSlug, showCompleted);
		}, REFETCH_DEBOUNCE_MS);
	}

	// The prune/fade ticker. Started lazily when the first node is touched and
	// stopped once touchedAt empties, so an idle graph runs no timer (the fade is
	// only interesting while something is glowing). Each tick drops fully-decayed
	// entries and calls graph.refresh() so nodeColor re-runs and the fade animates.
	function ensurePruneInterval() {
		if (pruneInterval) return;
		pruneInterval = setInterval(() => {
			const now = Date.now();
			for (const [ref, at] of touchedAt) {
				if (now - at >= PULSE_MS) touchedAt.delete(ref);
			}
			graph?.refresh();
			if (touchedAt.size === 0 && pruneInterval) {
				clearInterval(pruneInterval);
				pruneInterval = null;
			}
		}, PRUNE_INTERVAL_MS);
	}

	// ── Teardown ─────────────────────────────────────────────────────────────────
	onDestroy(() => {
		// Live-layer handles (plain `let`, CONVE-1688). The page never owns the SSE
		// connection (the +layout does), so we only drop our subscription + timers.
		unsubscribeSSE?.();
		unsubscribeSSE = null;
		if (refetchTimer) {
			clearTimeout(refetchTimer);
			refetchTimer = null;
		}
		if (pruneInterval) {
			clearInterval(pruneInterval);
			pruneInterval = null;
		}
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
		role?: string;
		is_terminal: boolean;
		child_count: number;
		updated_at: string;
		// Position coords the renderer assigns as the force simulation runs. Present
		// on any node that's been laid out (always true by the time it's clicked).
		x?: number;
		y?: number;
		z?: number;
	}
	interface GraphLink3D {
		// source/target start as ref strings but are mutated into node objects by
		// the force layout after ingest — compare sourceRef/targetRef instead.
		source: string | NodeObject;
		target: string | NodeObject;
		sourceRef: string;
		targetRef: string;
		type: string;
	}
</script>

<svelte:window onkeydown={onKeydown} />

<div class="graph-page">
	<!-- Controls overlay (top-left): toggle + filters + search fly-to. -->
	<GraphToolbar
		bind:showCompleted
		{nodeCount}
		{edgeCount}
		{totalNodeCount}
		{totalEdgeCount}
		filtered={filtersActive}
		collections={distinctCollections}
		statuses={distinctStatuses}
		roles={distinctRoles}
		selectedCollections={filterCollections}
		selectedStatuses={filterStatuses}
		selectedRole={filterRole}
		searchNodes={filteredData?.nodes ?? []}
		{colorForCollection}
		ontogglecollection={toggleCollectionFilter}
		ontogglestatus={toggleStatusFilter}
		onselectrole={selectRoleFilter}
		onsearchpick={flyToRef}
	/>

	<!-- The renderer mounts here; it owns its own canvas. -->
	<div class="canvas" bind:this={containerEl}></div>

	<!-- Focus-mode detail card (slides in from the right when a node is selected). -->
	{#if selectedNode}
		<DetailCard
			node={selectedNode}
			color={colorForCollection(selectedNode.collection)}
			item={selectedItem}
			itemLoading={selectedItemLoading}
			onopen={openSelected}
			onclose={deselect}
		/>
	{/if}

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
