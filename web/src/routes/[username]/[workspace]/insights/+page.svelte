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

	// Data
	let report = $state<ReportData | null>(null);
	let collections = $state<Collection[]>([]);
	let loading = $state(true);
	let error = $state('');

	// Filters
	let selectedWindow = $state<ReportWindow>('week');
	// Empty set === no filter (show all collections).
	let selectedCollections = $state<string[]>([]);

	const WINDOW_OPTIONS: { value: ReportWindow; label: string }[] = [
		{ value: 'day', label: 'Day' },
		{ value: 'week', label: 'Week' },
		{ value: '2wk', label: '2 Weeks' },
		{ value: 'month', label: 'Month' }
	];

	onMount(() => {
		workspaceStore.setCurrent(wsSlug);
	});

	// Reflect the page in the browser tab title. Track pathname so this
	// re-runs across SPA workspace switches (same pattern as the activity page).
	$effect(() => {
		page.url.pathname;
		titleStore.setPageTitle({ section: 'Insights', item: null });
	});

	// Load collections (for the filter) once per workspace.
	$effect(() => {
		if (wsSlug) {
			loadCollections(wsSlug);
		}
	});

	// Reload the report whenever workspace, window, or collection selection
	// changes. `selectedCollections` is a reactive array, so reading it (via
	// the snapshot below) tracks element changes.
	$effect(() => {
		const slug = wsSlug;
		const win = selectedWindow;
		const colls = [...selectedCollections];
		if (slug) {
			loadReport(slug, win, colls);
		}
	});

	async function loadCollections(slug: string) {
		try {
			collections = await api.collections.list(slug);
		} catch {
			// Filter is a progressive enhancement; allow the page to render.
		}
	}

	async function loadReport(slug: string, win: ReportWindow, colls: string[]) {
		loading = true;
		error = '';
		try {
			report = await api.report.get(slug, {
				window: win,
				collections: colls.length > 0 ? colls : undefined
			});
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load report.';
			report = null;
		} finally {
			loading = false;
		}
	}

	function toggleCollection(slug: string) {
		if (selectedCollections.includes(slug)) {
			selectedCollections = selectedCollections.filter((s) => s !== slug);
		} else {
			selectedCollections = [...selectedCollections, slug];
		}
	}

	function clearCollectionFilter() {
		selectedCollections = [];
	}

	// ── Formatters ───────────────────────────────────────────────────────────

	/** Hours → "12.3h" under 48h, else "2.1d". */
	function fmtHours(h: number): string {
		if (!Number.isFinite(h) || h <= 0) return '0h';
		if (h < 48) return `${h.toFixed(1)}h`;
		return `${(h / 24).toFixed(1)}d`;
	}

	function fmtDate(iso: string): string {
		const d = new Date(iso);
		if (Number.isNaN(d.getTime())) return iso;
		return d.toLocaleDateString('en-US', {
			month: 'short',
			day: 'numeric',
			year: 'numeric'
		});
	}

	function collLabel(slug: string): string {
		const c = collections.find((x) => x.slug === slug);
		return c ? `${c.icon} ${c.name}` : slug;
	}

	// ── Derived chart data ─────────────────────────────────────────────────────

	const throughputSeries = [
		{ key: 'created', label: 'Created', color: 'var(--chart-1, #4f46e5)' },
		{ key: 'completed', label: 'Completed', color: 'var(--chart-4, #10b981)' }
	];

	const throughputData = $derived<ChartDatum[]>(
		(report?.buckets ?? []).map((b) => ({
			bucket: b.bucket,
			created: b.created,
			completed: b.completed
		}))
	);

	const agingData = $derived<ChartDatum[]>(
		(report?.wip.aging_buckets ?? []).map((b) => ({ label: b.label, count: b.count }))
	);

	const completedByCollectionData = $derived<ChartDatum[]>(
		(report?.completed_by_collection ?? []).map((c) => ({
			collection: collLabel(c.collection),
			count: c.count
		}))
	);

	const noActivity = $derived(
		report !== null && report.totals.created === 0 && report.totals.completed === 0
	);

	const netFlow = $derived(report?.totals.net_flow ?? 0);

	// Status distribution grouped by collection.
	const statusByCollection = $derived.by(() => {
		const groups: { collection: string; total: number; rows: { status: string; count: number }[] }[] =
			[];
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
</script>

<div class="insights-page">
	<header class="page-header">
		<div class="page-header-left">
			<h1>Insights</h1>
			{#if report}
				<span class="date-range">
					{fmtDate(report.range_start)} &ndash; {fmtDate(report.range_end)}
				</span>
			{/if}
		</div>

		<div class="window-control" role="group" aria-label="Time window">
			{#each WINDOW_OPTIONS as opt (opt.value)}
				<button
					type="button"
					class="window-btn"
					class:active={selectedWindow === opt.value}
					aria-pressed={selectedWindow === opt.value}
					onclick={() => (selectedWindow = opt.value)}
				>
					{opt.label}
				</button>
			{/each}
		</div>
	</header>

	{#if collections.length > 0}
		<div class="collection-filter" role="group" aria-label="Filter by collection">
			<button
				type="button"
				class="chip"
				class:active={selectedCollections.length === 0}
				aria-pressed={selectedCollections.length === 0}
				onclick={clearCollectionFilter}
			>
				All
			</button>
			{#each collections as coll (coll.id)}
				<button
					type="button"
					class="chip"
					class:active={selectedCollections.includes(coll.slug)}
					aria-pressed={selectedCollections.includes(coll.slug)}
					onclick={() => toggleCollection(coll.slug)}
				>
					<span class="chip-icon">{coll.icon}</span>
					{coll.name}
				</button>
			{/each}
		</div>
	{/if}

	{#if error}
		<div class="error-state">
			<p class="error-title">Couldn't load insights</p>
			<p class="error-desc">{error}</p>
		</div>
	{:else if loading && !report}
		<div class="loading-state">Loading&hellip;</div>
	{:else if report}
		<div class="content" class:dimmed={loading}>
			<!-- ── Totals ──────────────────────────────────────────────── -->
			<section class="stat-row">
				<div class="stat-card">
					<span class="stat-label">Created</span>
					<span class="stat-value">{report.totals.created}</span>
				</div>
				<div class="stat-card">
					<span class="stat-label">Completed</span>
					<span class="stat-value">{report.totals.completed}</span>
				</div>
				<div class="stat-card">
					<span class="stat-label">Net flow</span>
					<span class="stat-value" class:positive={netFlow >= 0} class:negative={netFlow < 0}>
						{netFlow >= 0 ? '+' : ''}{netFlow}
					</span>
				</div>
			</section>

			<!-- ── Throughput ──────────────────────────────────────────── -->
			<section class="card">
				<div class="card-header">
					<h2>Throughput</h2>
					<span class="card-sub">Created vs completed per {report.granularity}</span>
				</div>
				{#if noActivity}
					<div class="card-empty">No activity in this window.</div>
				{:else}
					<BarChart
						data={throughputData}
						x="bucket"
						series={throughputSeries}
						height={260}
						ariaLabel="Items created versus completed per time bucket"
					/>
				{/if}
			</section>

			<div class="grid">
				<!-- ── Cycle time ──────────────────────────────────────── -->
				<section class="card">
					<div class="card-header">
						<h2>Cycle time</h2>
						<span class="card-sub">Creation to completion</span>
					</div>
					{#if report.cycle_time.sample_size === 0}
						<div class="card-empty">No completions in this window.</div>
					{:else}
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
						{#if report.cycle_time.by_collection.length > 0}
							<ul class="dist-list">
								{#each report.cycle_time.by_collection as row (row.collection)}
									<li class="dist-row">
										<span class="dist-name">{collLabel(row.collection)}</span>
										<span class="dist-meta">
											<span class="dist-count">{row.count}</span>
											<span class="dist-val">{fmtHours(row.median_hours)}</span>
										</span>
									</li>
								{/each}
							</ul>
						{/if}
					{/if}
				</section>

				<!-- ── Work in progress ────────────────────────────────── -->
				<section class="card">
					<div class="card-header">
						<h2>Work in progress</h2>
						<span class="card-sub">Open items right now</span>
					</div>
					{#if report.wip.open_count === 0}
						<div class="card-empty">No open items.</div>
					{:else}
						<div class="metric-row">
							<div class="metric">
								<span class="metric-label">Open</span>
								<span class="metric-value">{report.wip.open_count}</span>
							</div>
							<div class="metric">
								<span class="metric-label">Median age</span>
								<span class="metric-value">{fmtHours(report.wip.median_age_hours)}</span>
							</div>
						</div>
						<BarChart
							data={agingData}
							x="label"
							series={[{ key: 'count', label: 'Open items', color: 'var(--chart-3, #f59e0b)' }]}
							height={180}
							ariaLabel="Open items by age bucket"
						/>
						{#if report.wip.by_collection.length > 0}
							<ul class="dist-list">
								{#each report.wip.by_collection as row (row.collection)}
									<li class="dist-row">
										<span class="dist-name">{collLabel(row.collection)}</span>
										<span class="dist-meta">
											<span class="dist-count">{row.count}</span>
											<span class="dist-val">{fmtHours(row.median_hours)}</span>
										</span>
									</li>
								{/each}
							</ul>
						{/if}
					{/if}
				</section>
			</div>

			<!-- ── Completed by collection ─────────────────────────────── -->
			<section class="card">
				<div class="card-header">
					<h2>Completed by collection</h2>
				</div>
				{#if completedByCollectionData.length === 0}
					<div class="card-empty">Nothing completed in this window.</div>
				{:else}
					<BarChart
						data={completedByCollectionData}
						x="collection"
						series={[{ key: 'count', label: 'Completed', color: 'var(--chart-4, #10b981)' }]}
						height={220}
						ariaLabel="Completed items grouped by collection"
					/>
				{/if}
			</section>

			<!-- ── Status distribution ─────────────────────────────────── -->
			<section class="card">
				<div class="card-header">
					<h2>Status distribution</h2>
				</div>
				{#if statusByCollection.length === 0}
					<div class="card-empty">No items to show.</div>
				{:else}
					<div class="status-groups">
						{#each statusByCollection as group (group.collection)}
							<div class="status-group">
								<div class="status-group-head">
									<span class="status-group-name">{collLabel(group.collection)}</span>
									<span class="status-group-total">{group.total}</span>
								</div>
								<div class="status-bars">
									{#each group.rows as row (row.status)}
										<div class="status-bar-row">
											<span class="status-name">{row.status}</span>
											<span class="status-track">
												<span
													class="status-fill"
													style:width={`${group.total > 0 ? (row.count / group.total) * 100 : 0}%`}
												></span>
											</span>
											<span class="status-count">{row.count}</span>
										</div>
									{/each}
								</div>
							</div>
						{/each}
					</div>
				{/if}
			</section>
		</div>
	{/if}
</div>

<style>
	/* ── Page Layout ──────────────────────────────────────────────────── */
	.insights-page {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
	}

	/* ── Header ───────────────────────────────────────────────────────── */
	.page-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		margin-bottom: var(--space-4);
		flex-wrap: wrap;
	}
	.page-header-left {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
	}
	.page-header h1 {
		font-size: 1.6em;
		font-weight: 700;
	}
	.date-range {
		font-size: 0.9em;
		color: var(--text-muted);
	}

	/* ── Window segmented control ─────────────────────────────────────── */
	.window-control {
		display: inline-flex;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: 2px;
		gap: 2px;
	}
	.window-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.8em;
		font-weight: 600;
		padding: var(--space-1) var(--space-3);
		border-radius: var(--radius-sm);
		cursor: pointer;
		white-space: nowrap;
		transition: background 0.15s, color 0.15s;
	}
	.window-btn:hover {
		color: var(--text-primary);
	}
	.window-btn.active {
		background: var(--bg-primary, var(--bg));
		color: var(--text-primary);
		box-shadow: 0 1px 2px rgba(0, 0, 0, 0.08);
	}

	/* ── Collection filter chips ──────────────────────────────────────── */
	.collection-filter {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		margin-bottom: var(--space-6);
	}
	.chip {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		background: var(--bg-secondary);
		color: var(--text-secondary);
		border: 1px solid var(--border);
		border-radius: 999px;
		padding: var(--space-1) var(--space-3);
		font-size: 0.8em;
		font-weight: 500;
		cursor: pointer;
		transition: background 0.15s, border-color 0.15s, color 0.15s;
	}
	.chip:hover {
		border-color: var(--text-muted);
		color: var(--text-primary);
	}
	.chip.active {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		border-color: var(--accent-blue);
		color: var(--accent-blue);
	}
	.chip-icon {
		font-size: 1em;
	}

	/* ── States ───────────────────────────────────────────────────────── */
	.loading-state {
		padding: var(--space-10) 0;
		text-align: center;
		color: var(--text-muted);
		font-size: 0.95em;
	}
	.error-state {
		padding: var(--space-8);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		text-align: center;
	}
	.error-title {
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: var(--space-2);
	}
	.error-desc {
		font-size: 0.9em;
		color: var(--text-muted);
	}

	.content {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
		transition: opacity 0.15s;
	}
	.content.dimmed {
		opacity: 0.55;
		pointer-events: none;
	}

	/* ── Stat cards ───────────────────────────────────────────────────── */
	.stat-row {
		display: grid;
		grid-template-columns: repeat(3, 1fr);
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
		font-size: 0.72em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--text-muted);
	}
	.stat-value {
		font-size: 1.8em;
		font-weight: 700;
		color: var(--text-primary);
	}
	.stat-value.positive {
		color: var(--accent-green);
	}
	.stat-value.negative {
		color: var(--accent-red, #ef4444);
	}

	/* ── Cards ────────────────────────────────────────────────────────── */
	.card {
		padding: var(--space-5);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}
	.card-header {
		display: flex;
		align-items: baseline;
		justify-content: space-between;
		gap: var(--space-3);
		margin-bottom: var(--space-4);
		flex-wrap: wrap;
	}
	.card-header h2 {
		font-size: 1em;
		font-weight: 600;
		color: var(--text-primary);
	}
	.card-sub {
		font-size: 0.78em;
		color: var(--text-muted);
	}
	.card-empty {
		padding: var(--space-8) var(--space-4);
		text-align: center;
		color: var(--text-muted);
		font-size: 0.9em;
	}

	.grid {
		display: grid;
		grid-template-columns: repeat(2, 1fr);
		gap: var(--space-6);
	}

	/* ── Metrics ──────────────────────────────────────────────────────── */
	.metric-row {
		display: flex;
		gap: var(--space-6);
		margin-bottom: var(--space-4);
		flex-wrap: wrap;
	}
	.metric {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.metric-label {
		font-size: 0.72em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}
	.metric-value {
		font-size: 1.3em;
		font-weight: 700;
		color: var(--text-primary);
	}

	/* ── Distribution lists ───────────────────────────────────────────── */
	.dist-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.dist-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-1) 0;
		font-size: 0.85em;
		border-top: 1px solid var(--border);
	}
	.dist-row:first-child {
		border-top: none;
	}
	.dist-name {
		color: var(--text-secondary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.dist-meta {
		display: inline-flex;
		align-items: center;
		gap: var(--space-3);
		flex-shrink: 0;
	}
	.dist-count {
		font-size: 0.85em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 1px 8px;
		border-radius: 10px;
	}
	.dist-val {
		font-weight: 600;
		color: var(--text-primary);
		min-width: 3.5em;
		text-align: right;
	}

	/* ── Status distribution ──────────────────────────────────────────── */
	.status-groups {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}
	.status-group-head {
		display: flex;
		align-items: baseline;
		justify-content: space-between;
		margin-bottom: var(--space-2);
	}
	.status-group-name {
		font-size: 0.85em;
		font-weight: 600;
		color: var(--text-secondary);
	}
	.status-group-total {
		font-size: 0.78em;
		color: var(--text-muted);
	}
	.status-bars {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.status-bar-row {
		display: grid;
		grid-template-columns: 8rem 1fr 2.5rem;
		align-items: center;
		gap: var(--space-3);
		font-size: 0.82em;
	}
	.status-name {
		color: var(--text-secondary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.status-track {
		height: 8px;
		background: var(--bg-tertiary);
		border-radius: 4px;
		overflow: hidden;
	}
	.status-fill {
		display: block;
		height: 100%;
		background: var(--accent-blue);
		border-radius: 4px;
	}
	.status-count {
		text-align: right;
		color: var(--text-muted);
		font-variant-numeric: tabular-nums;
	}

	/* ── Responsive ───────────────────────────────────────────────────── */
	@media (max-width: 768px) {
		.grid {
			grid-template-columns: 1fr;
		}
		.stat-row {
			grid-template-columns: 1fr;
		}
		.status-bar-row {
			grid-template-columns: 6rem 1fr 2rem;
		}
	}
</style>
