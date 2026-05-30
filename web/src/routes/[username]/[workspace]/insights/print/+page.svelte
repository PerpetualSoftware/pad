<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { titleStore } from '$lib/stores/title.svelte';
	import BarChart from '$lib/components/charts/BarChart.svelte';
	import type { ChartDatum } from '$lib/components/charts/theme';
	import type { Collection, ReportData, ReportWindow } from '$lib/types';

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');

	// ── Active selection, read from the URL (set by the Insights "Print
	// report" button). This is the source of truth for the report — the page
	// prints exactly what the user was viewing.
	const WINDOWS: ReportWindow[] = ['day', 'week', '2wk', 'month'];
	const selectedWindow = $derived.by<ReportWindow>(() => {
		const w = page.url.searchParams.get('window');
		return WINDOWS.includes(w as ReportWindow) ? (w as ReportWindow) : 'week';
	});
	const offset = $derived.by(() => {
		const o = Number(page.url.searchParams.get('offset'));
		return Number.isFinite(o) && o > 0 ? Math.floor(o) : 0;
	});
	const selectedCollections = $derived.by(() => {
		const c = page.url.searchParams.get('collections');
		return c ? c.split(',').filter((s) => s.length > 0) : [];
	});

	// ── Data ───────────────────────────────────────────────────────────────
	let report = $state<ReportData | null>(null);
	let collections = $state<Collection[]>([]);
	let loading = $state(true);
	let error = $state('');
	// Monotonic request counter so a late, stale response can't clobber the
	// newest one (matches the Insights page's reqSeq guard).
	let reqSeq = 0;

	// Stamp the moment the report was generated, for the header.
	const generatedAt = new Date();

	onMount(() => {
		workspaceStore.setCurrent(wsSlug);
	});

	$effect(() => {
		page.url.pathname;
		titleStore.setPageTitle({ section: 'Report', item: null });
	});

	// Load collections (for labels) once per workspace.
	$effect(() => {
		if (wsSlug) loadCollections(wsSlug);
	});

	// (Re)load the report whenever the workspace or the URL-carried selection
	// changes.
	$effect(() => {
		const slug = wsSlug;
		const win = selectedWindow;
		const colls = [...selectedCollections];
		const off = offset;
		if (slug) loadReport(slug, win, colls, off);
	});

	async function loadCollections(slug: string) {
		try {
			collections = await api.collections.list(slug);
		} catch {
			// Labels degrade to slugs; not fatal.
		}
	}

	async function loadReport(slug: string, win: ReportWindow, colls: string[], off: number) {
		const seq = ++reqSeq;
		loading = true;
		error = '';
		try {
			const data = await api.report.get(slug, {
				window: win,
				collections: colls.length > 0 ? colls : undefined,
				offset: off,
				includeItems: true
			});
			if (seq !== reqSeq) return;
			report = data;
		} catch (e) {
			if (seq !== reqSeq) return;
			error = e instanceof Error ? e.message : 'Failed to load report.';
			report = null;
		} finally {
			if (seq === reqSeq) loading = false;
		}
	}

	// ── Formatters ───────────────────────────────────────────────────────────

	/** Hours → "12.3h" under 48h, else "2.1d" (same as the Insights page). */
	function fmtHours(h: number): string {
		if (!Number.isFinite(h) || h <= 0) return '0h';
		if (h < 48) return `${h.toFixed(1)}h`;
		return `${(h / 24).toFixed(1)}d`;
	}

	function fmtDate(iso: string): string {
		const d = new Date(iso);
		if (Number.isNaN(d.getTime())) return iso;
		return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
	}

	function fmtDateTime(d: Date): string {
		return d.toLocaleString('en-US', {
			month: 'short',
			day: 'numeric',
			year: 'numeric',
			hour: 'numeric',
			minute: '2-digit'
		});
	}

	function collLabel(slug: string): string {
		const c = collections.find((x) => x.slug === slug);
		return c ? `${c.icon} ${c.name}` : slug;
	}

	// ── Derived chart data (same projections as the Insights page) ─────────────

	const throughputSeries = [
		{ key: 'created', label: 'Created', color: 'var(--chart-1, #4f46e5)' },
		{ key: 'completed', label: 'Completed', color: 'var(--chart-4, #10b981)' }
	];

	// Compact X-axis label for the throughput buckets. Full ISO dates
	// ("2026-05-23", ~70px) overlap once several buckets share a narrow chart;
	// render "M/D" (day) or "M/D Hh" (hour) instead. Falls back to the raw
	// bucket string for week/month or any unrecognised format.
	function fmtBucket(b: string): string {
		const m = /^(\d{4})-(\d{2})-(\d{2})(?:T(\d{2}))?/.exec(b);
		if (!m) return b;
		const label = `${Number(m[2])}/${Number(m[3])}`;
		return m[4] !== undefined ? `${label} ${Number(m[4])}h` : label;
	}

	const throughputData = $derived<ChartDatum[]>(
		(report?.buckets ?? []).map((b) => ({
			bucket: fmtBucket(b.bucket),
			created: b.created,
			completed: b.completed
		}))
	);

	const completedByCollectionData = $derived<ChartDatum[]>(
		(report?.completed_by_collection ?? []).map((c) => ({
			collection: collLabel(c.collection),
			count: c.count
		}))
	);

	const cycleTimeData = $derived<ChartDatum[]>(
		(report?.cycle_time.by_collection ?? []).map((c) => ({
			collection: collLabel(c.collection),
			median_hours: Number(c.median_hours.toFixed(1))
		}))
	);

	const netFlow = $derived(report?.totals.net_flow ?? 0);

	// Period label honoring offset. "Week of <start> – <end>" for the current
	// period; past periods append how far back they are.
	const periodNoun = $derived.by(() => {
		switch (selectedWindow) {
			case 'day':
				return 'Day';
			case 'week':
				return 'Week';
			case '2wk':
				return '2-Week period';
			case 'month':
				return 'Month';
		}
	});

	const periodLabel = $derived.by(() => {
		if (!report) return '';
		const range = `${fmtDate(report.range_start)} – ${fmtDate(report.range_end)}`;
		const base = `${periodNoun} of ${range}`;
		if (offset === 0) return base;
		return `${base} (${offset} period${offset === 1 ? '' : 's'} ago)`;
	});

	const noActivity = $derived(
		report !== null && report.totals.created === 0 && report.totals.completed === 0
	);

	// "What shipped" — completed items grouped by collection, preserving the
	// newest-first order the API returns within each group.
	const shippedGroups = $derived.by(() => {
		const groups: { collection: string; items: { ref: string; title: string }[] }[] = [];
		for (const it of report?.completed_items ?? []) {
			let g = groups.find((x) => x.collection === it.collection);
			if (!g) {
				g = { collection: it.collection, items: [] };
				groups.push(g);
			}
			g.items.push({ ref: it.ref, title: it.title });
		}
		return groups;
	});

	const shippedOverflow = $derived(report?.completed_items_overflow_count ?? 0);

	// Status distribution grouped by collection (compact table).
	const statusByCollection = $derived.by(() => {
		const groups: {
			collection: string;
			total: number;
			rows: { status: string; count: number }[];
		}[] = [];
		for (const row of report?.status_distribution ?? []) {
			let g = groups.find((x) => x.collection === row.collection);
			if (!g) {
				g = { collection: row.collection, total: 0, rows: [] };
				groups.push(g);
			}
			g.rows.push({ status: row.status, count: row.count });
			g.total += row.count;
		}
		return groups;
	});

	const workspaceName = $derived(workspaceStore.current?.name ?? wsSlug);
	const insightsHref = $derived(`/${username}/${wsSlug}/insights`);

	function doPrint() {
		window.print();
	}
</script>

<!-- Screen-only toolbar: back link + print button. Hidden in print output. -->
<div class="toolbar no-print">
	<a class="back-link" href={insightsHref}>&larr; Back to Insights</a>
	<button type="button" class="print-btn" onclick={doPrint}>Print / Export PDF</button>
</div>

<div class="report">
	{#if error}
		<div class="state error-state">
			<p class="state-title">Couldn't load the report</p>
			<p class="state-desc">{error}</p>
		</div>
	{:else if loading && !report}
		<div class="state">Loading&hellip;</div>
	{:else if report}

		<!-- Header -->
		<header class="report-header">
			<div class="report-title">
				<h1>{workspaceName}</h1>
				<p class="report-subtitle">Activity report</p>
			</div>
			<div class="report-meta">
				<p class="period">{periodLabel}</p>
				<p class="generated">Generated {fmtDateTime(generatedAt)}</p>
			</div>
		</header>

		<!-- Headline stats -->
		<section class="stat-row" aria-label="Headline stats">
			<div class="stat-card">
				<span class="stat-label">Completed</span>
				<span class="stat-value">{report.totals.completed}</span>
			</div>
			<div class="stat-card">
				<span class="stat-label">Created</span>
				<span class="stat-value">{report.totals.created}</span>
			</div>
			<div class="stat-card">
				<span class="stat-label">Net flow</span>
				<span class="stat-value" class:positive={netFlow >= 0} class:negative={netFlow < 0}>
					{netFlow >= 0 ? '+' : ''}{netFlow}
				</span>
			</div>
			<div class="stat-card">
				<span class="stat-label">Median cycle time</span>
				<span class="stat-value">
					{report.cycle_time.sample_size > 0 ? fmtHours(report.cycle_time.median_hours) : '—'}
				</span>
			</div>
		</section>

		<!-- Charts up front: a compact grid of half-width panels intermingled
		     with the cycle-time metrics, so the report reads like a dashboard
		     rather than a stack of full-bleed graphs. Heights are passed small
		     and identical for screen + print (no @media height override) so
		     LayerCake's cached measurement matches the printed box and the SVG
		     never overflows onto the next panel. -->
		<section class="charts-grid" aria-label="Charts">
			<!-- Throughput -->
			<div class="panel chart-panel">
				<h2 class="panel-title">Throughput</h2>
				<p class="panel-sub">Created vs completed per {report.granularity}</p>
				{#if noActivity}
					<p class="empty">No activity in this period.</p>
				{:else}
					<BarChart
						data={throughputData}
						x="bucket"
						series={throughputSeries}
						height={150}
						ariaLabel="Items created versus completed per time bucket"
					/>
				{/if}
			</div>

			<!-- Completed by collection -->
			<div class="panel chart-panel">
				<h2 class="panel-title">Completed by collection</h2>
				{#if completedByCollectionData.length === 0}
					<p class="empty">Nothing completed in this period.</p>
				{:else}
					<BarChart
						data={completedByCollectionData}
						x="collection"
						series={[{ key: 'count', label: 'Completed', color: 'var(--chart-4, #10b981)' }]}
						height={150}
						ariaLabel="Completed items grouped by collection"
					/>
				{/if}
			</div>

			<!-- Cycle time (only when there were completions) -->
			{#if report.cycle_time.sample_size > 0}
				<div class="panel chart-panel cycle-panel">
					<h2 class="panel-title">Cycle time</h2>
					<p class="panel-sub">Creation to completion</p>
					<div class="cycle-body">
						<div class="metric-row">
							<div class="metric">
								<span class="metric-label">Median</span>
								<span class="metric-value">{fmtHours(report.cycle_time.median_hours)}</span>
							</div>
							<div class="metric">
								<span class="metric-label">p90</span>
								<span class="metric-value">{fmtHours(report.cycle_time.p90_hours)}</span>
							</div>
							<div class="metric">
								<span class="metric-label">Sample</span>
								<span class="metric-value">{report.cycle_time.sample_size}</span>
							</div>
						</div>
						{#if cycleTimeData.length > 0}
							<div class="cycle-chart">
								<BarChart
									data={cycleTimeData}
									x="collection"
									series={[
										{ key: 'median_hours', label: 'Median hours', color: 'var(--chart-3, #f59e0b)' }
									]}
									height={130}
									ariaLabel="Median cycle time in hours grouped by collection"
								/>
							</div>
						{/if}
					</div>
				</div>
			{/if}
		</section>

		<!-- What shipped (centerpiece) — dense multi-column list so the long
		     completed set flows down the page instead of leaving big gaps. -->
		<section class="block">
			<h2 class="block-title">What shipped</h2>
			{#if shippedGroups.length === 0}
				<p class="empty">Nothing completed in this period.</p>
			{:else}
				<div class="shipped">
					{#each shippedGroups as group (group.collection)}
						<div class="shipped-group">
							<div class="shipped-group-head">
								<span class="shipped-group-name">{collLabel(group.collection)}</span>
								<span class="shipped-group-count">{group.items.length}</span>
							</div>
							<ul class="shipped-list">
								{#each group.items as item (item.ref)}
									<li class="shipped-item">
										<span class="shipped-ref">{item.ref}</span>
										<span class="shipped-item-title">{item.title}</span>
									</li>
								{/each}
							</ul>
						</div>
					{/each}
				</div>
				{#if shippedOverflow > 0}
					<p class="overflow-note">
						+{shippedOverflow} more completed item{shippedOverflow === 1 ? '' : 's'} not shown.
					</p>
				{/if}
			{/if}
		</section>

		<!-- Status distribution (compact table) -->
		{#if statusByCollection.length > 0}
			<section class="block">
				<h2 class="block-title">Status distribution</h2>
				<div class="status-tables">
					{#each statusByCollection as group (group.collection)}
						<table class="status-table">
							<thead>
								<tr>
									<th>{collLabel(group.collection)}</th>
									<th class="num">{group.total}</th>
								</tr>
							</thead>
							<tbody>
								{#each group.rows as row (row.status)}
									<tr>
										<td>{row.status}</td>
										<td class="num">{row.count}</td>
									</tr>
								{/each}
							</tbody>
						</table>
					{/each}
				</div>
			</section>
		{/if}

		<footer class="report-footer">
			{workspaceName} · {periodLabel} · Generated {fmtDateTime(generatedAt)}
		</footer>
	{/if}
</div>

<style>
	/* ── Screen toolbar (hidden in print) ────────────────────────────────── */
	.toolbar {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-6) var(--space-6) 0;
	}
	.back-link {
		font-size: 0.85em;
		font-weight: 600;
		color: var(--text-secondary);
		text-decoration: none;
	}
	.back-link:hover {
		color: var(--text-primary);
	}
	.print-btn {
		background: var(--accent-blue);
		color: #fff;
		border: none;
		border-radius: var(--radius);
		padding: var(--space-2) var(--space-4);
		font-size: 0.85em;
		font-weight: 600;
		cursor: pointer;
	}
	.print-btn:hover {
		filter: brightness(1.05);
	}

	/* ── Report document ─────────────────────────────────────────────────── */
	/* Constrained to paper-column width (≈ Letter/A4 content area) rather than
	   the app's 960px so the on-screen preview matches the printed sheet. This
	   is also what keeps the LayerCake charts from clipping in print: each chart
	   measures its width on screen, so a paper-width screen means that cached
	   measurement already fits the print column — no off-page overflow. */
	.report {
		max-width: 720px;
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
		color: var(--text-primary);
	}

	/* ── States ──────────────────────────────────────────────────────────── */
	.state {
		padding: var(--space-10) 0;
		text-align: center;
		color: var(--text-muted);
		font-size: 0.95em;
	}
	.error-state {
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		padding: var(--space-8);
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

	/* ── Header ──────────────────────────────────────────────────────────── */
	.report-header {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: var(--space-4);
		border-bottom: 2px solid var(--border);
		padding-bottom: var(--space-4);
	}
	.report-title h1 {
		font-size: 1.7em;
		font-weight: 700;
	}
	.report-subtitle {
		font-size: 0.85em;
		color: var(--text-muted);
		margin-top: 2px;
	}
	.report-meta {
		text-align: right;
	}
	.period {
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-primary);
	}
	.generated {
		font-size: 0.78em;
		color: var(--text-muted);
		margin-top: 2px;
	}

	/* ── Headline stats ──────────────────────────────────────────────────── */
	.stat-row {
		display: grid;
		grid-template-columns: repeat(4, 1fr);
		gap: var(--space-4);
	}
	.stat-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		padding: var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}
	.stat-label {
		font-size: 0.7em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}
	.stat-value {
		font-size: 1.6em;
		font-weight: 700;
		color: var(--text-primary);
	}
	.stat-value.positive {
		color: var(--accent-green);
	}
	.stat-value.negative {
		color: var(--accent-red, #ef4444);
	}

	/* ── Blocks ──────────────────────────────────────────────────────────── */
	.block {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.block-title {
		font-size: 1.1em;
		font-weight: 700;
		color: var(--text-primary);
		padding-bottom: var(--space-2);
		border-bottom: 1px solid var(--border);
	}
	.block-sub {
		font-size: 0.8em;
		color: var(--text-muted);
		margin-top: calc(-1 * var(--space-2));
	}
	.empty {
		padding: var(--space-6) 0;
		text-align: center;
		color: var(--text-muted);
		font-size: 0.9em;
	}

	/* ── Charts grid (dashboard band, up front) ──────────────────────────── */
	.charts-grid {
		display: grid;
		grid-template-columns: repeat(2, 1fr);
		gap: var(--space-5);
	}
	.panel {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		padding: var(--space-4);
	}
	.panel-title {
		font-size: 1em;
		font-weight: 700;
		color: var(--text-primary);
	}
	.panel-sub {
		font-size: 0.78em;
		color: var(--text-muted);
		margin-top: calc(-1 * var(--space-1));
	}
	/* Cycle time spans the full grid width as a band: metrics on the left,
	   its (short) chart on the right, so it doesn't sit as a lonely half-cell. */
	.cycle-panel {
		grid-column: 1 / -1;
	}
	.cycle-body {
		display: grid;
		grid-template-columns: auto 1fr;
		align-items: center;
		gap: var(--space-6);
	}
	.cycle-panel .metric-row {
		gap: var(--space-5);
		flex-direction: column;
		flex-wrap: nowrap;
	}
	.cycle-chart {
		min-width: 0;
	}

	/* ── What shipped ────────────────────────────────────────────────────── */
	.shipped {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}
	.shipped-group-head {
		display: flex;
		align-items: baseline;
		gap: var(--space-2);
		margin-bottom: var(--space-2);
	}
	.shipped-group-name {
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-primary);
	}
	.shipped-group-count {
		font-size: 0.75em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 1px 8px;
		border-radius: 10px;
	}
	.shipped-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
	}
	.shipped-item {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
		padding: var(--space-1) 0;
		font-size: 0.9em;
		border-top: 1px solid var(--border);
	}
	.shipped-item:first-child {
		border-top: none;
	}
	.shipped-ref {
		flex-shrink: 0;
		font-weight: 600;
		font-variant-numeric: tabular-nums;
		color: var(--accent-blue);
		min-width: 5.5em;
	}
	.shipped-item-title {
		color: var(--text-primary);
	}
	.overflow-note {
		font-size: 0.82em;
		color: var(--text-muted);
		margin-top: var(--space-2);
	}

	/* ── Metrics ─────────────────────────────────────────────────────────── */
	.metric-row {
		display: flex;
		gap: var(--space-6);
		flex-wrap: wrap;
	}
	.metric {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.metric-label {
		font-size: 0.7em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}
	.metric-value {
		font-size: 1.2em;
		font-weight: 700;
		color: var(--text-primary);
	}

	/* ── Status tables ───────────────────────────────────────────────────── */
	.status-tables {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
		gap: var(--space-5);
	}
	.status-table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.85em;
	}
	.status-table th,
	.status-table td {
		text-align: left;
		padding: var(--space-1) var(--space-2);
		border-bottom: 1px solid var(--border);
	}
	.status-table thead th {
		font-weight: 700;
		color: var(--text-primary);
		border-bottom: 2px solid var(--border);
	}
	.status-table td {
		color: var(--text-secondary);
	}
	.status-table .num {
		text-align: right;
		font-variant-numeric: tabular-nums;
	}

	/* ── Footer ──────────────────────────────────────────────────────────── */
	.report-footer {
		font-size: 0.75em;
		color: var(--text-muted);
		border-top: 1px solid var(--border);
		padding-top: var(--space-3);
	}

	/* ── Responsive (screen) ─────────────────────────────────────────────── */
	@media (max-width: 768px) {
		.stat-row {
			grid-template-columns: repeat(2, 1fr);
		}
		.report-header {
			flex-direction: column;
		}
		.report-meta {
			text-align: left;
		}
	}

	/* ── Print ───────────────────────────────────────────────────────────── */
	@media print {
		/* Suppress the app shell rendered by the root +layout.svelte so the
		   printed page is just the report. The print/+layout@.svelte reset
		   already drops the workspace ConnectBanner; these :global() rules hide
		   the sidebar / desktop topbar / expand tabs / toasts that live in the
		   root layout. The mobile TopBar (`.topbar-mobile`) is rendered as a
		   sibling BEFORE `.app-layout`, so it isn't caught by the
		   `.app-layout > :not(.app-shell)` rule and needs hiding explicitly —
		   otherwise a Save-as-PDF from a mobile viewport includes the app bar. */
		:global(.app-layout > :not(.app-shell)),
		:global(.app-shell > :not(.main-content)),
		:global(.sidebar-expand-btn),
		:global(.topbar-expand-btn),
		:global(.topbar-mobile) {
			display: none !important;
		}
		:global(.app-layout),
		:global(.app-shell),
		:global(.main-content) {
			display: block !important;
			height: auto !important;
			overflow: visible !important;
			padding: 0 !important;
		}
		:global(html),
		:global(body) {
			background: #fff !important;
		}

		/* Screen-only chrome inside the report. */
		.no-print {
			display: none !important;
		}

		/* Smaller print base so the em-based sizes cascade down to a tidy
		   report scale (body ~10pt). Tighter gap reads as a compact report
		   rather than a sprawling web page. */
		.report {
			max-width: none;
			margin: 0;
			padding: 0;
			color: #000;
			font-size: 9.5pt;
			line-height: 1.3;
			gap: 0.55rem;
		}
		.stat-card,
		.error-state {
			background: transparent;
		}

		/* Cap the big screen-scale headings to report proportions. */
		.report-title h1 {
			font-size: 16pt;
		}
		.report-subtitle {
			font-size: 8.5pt;
		}
		.period {
			font-size: 9.5pt;
		}
		.generated {
			font-size: 8pt;
		}
		.report-header {
			padding-bottom: 0.4rem;
		}
		.block-title {
			font-size: 12pt;
			padding-bottom: 0.25rem;
		}
		.block-sub {
			font-size: 8.5pt;
		}
		.block {
			gap: 0.45rem;
		}

		/* Compact stat row. */
		.stat-row {
			gap: 0.5rem;
		}
		.stat-card {
			padding: 0.4rem 0.5rem;
		}
		.stat-label {
			font-size: 8pt;
		}
		.stat-value {
			font-size: 14pt;
		}

		/* Compact "what shipped" list, flowed into two columns so a long
		   completed set fills the page width and runs continuously instead of a
		   tall single column that leaves big vertical gaps. */
		.shipped {
			gap: 0.6rem;
		}
		.shipped-list {
			column-count: 2;
			column-gap: 1.4rem;
		}
		.shipped-item {
			font-size: 8.5pt;
			padding: 0.5pt 0;
			gap: 0.5rem;
			break-inside: avoid;
		}
		.shipped-ref {
			min-width: 4.6em;
		}
		.shipped-group-name {
			font-size: 9.5pt;
		}
		.shipped-group-count {
			font-size: 7.5pt;
		}
		.metric-value {
			font-size: 11pt;
		}
		.metric-label {
			font-size: 8pt;
		}
		.status-table {
			font-size: 8.5pt;
		}

		/* Compact rows: cap cell padding + line-height so a status table is
		   physically short. Repeat the header row on every page the table
		   spills onto (thead as a table-header-group), and right-size the
		   grid so two compact tables sit side by side instead of one tall stack. */
		.status-tables {
			gap: 0.6rem 1.2rem;
			grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
		}
		.status-table th,
		.status-table td {
			padding: 0.5pt 5pt;
			line-height: 1.2;
		}
		.status-table thead {
			display: table-header-group;
		}

		/* Charts grid for print. Tighten the gap and de-emphasise the panel
		   chrome (no fill / border on paper). The chart heights are passed as
		   small props (≈130–150px) identical to screen, so we DON'T override
		   .canvas height here — that was the cause of the overlap: shrinking the
		   box after LayerCake measured the screen height left the SVG drawn at
		   the old range, spilling over the next panel. `overflow: hidden` clips
		   any residual sub-pixel spill so panels can never bleed into each
		   other. */
		.charts-grid {
			gap: 0.5rem 1.2rem;
		}
		.panel {
			padding: 0;
			border: none;
			background: transparent;
			gap: 0.2rem;
		}
		.panel-title {
			font-size: 11pt;
		}
		.panel-sub {
			font-size: 8pt;
		}
		.chart-panel :global(.canvas) {
			overflow: hidden;
		}
		.chart-panel :global(.legend) {
			font-size: 8pt;
			margin-bottom: 0.2rem;
		}

		/* Keep small logical units intact across page breaks — a stat row, a
		   stat card, a chart panel, the header. Deliberately DO NOT add
		   `.block`, `.status-table`, or `.shipped-group` here: those can be
		   taller than a page, and keeping them whole forces a jump to the next
		   page that leaves a big blank gap (the page-1 symptom). They're allowed
		   to flow across page boundaries instead; only the small atoms inside
		   them (a table `tr`, a shipped line) are kept from splitting. */
		.stat-row,
		.stat-card,
		.report-header,
		.chart-panel,
		.status-table tr,
		.chart-panel :global(.canvas) {
			break-inside: avoid;
		}
		.block-title,
		.report-title h1 {
			break-after: avoid;
		}

		@page {
			margin: 1.5cm;
		}
	}
</style>
