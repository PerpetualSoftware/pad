<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import type { AdminBillingStats } from '$lib/types';

	let stats = $state<AdminBillingStats | null>(null);
	let loading = $state(true);
	let error = $state('');
	let refreshing = $state(false);

	async function loadStats() {
		error = '';
		try {
			stats = await api.admin.getBillingStats();
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load billing stats';
		}
	}

	async function initialLoad() {
		loading = true;
		await loadStats();
		loading = false;
	}

	async function refresh() {
		refreshing = true;
		await loadStats();
		refreshing = false;
	}

	// Stripe-derived cards must be greyed out when either the cloud sidecar is
	// unreachable or Stripe simply isn't configured yet (the expected pre-launch
	// state). Local cards (customers_by_plan, new_signups_30d) are always real.
	let stripeUnavailable = $derived(
		!stats || stats.cloud_unreachable || !stats.stripe_configured
	);

	let currencyCode = $derived((stats?.currency || 'usd').toUpperCase());

	let mrrFormatted = $derived(
		stats ? formatCurrency(stats.mrr_cents / 100, currencyCode) : ''
	);
	let arrFormatted = $derived(
		stats ? formatCurrency(stats.arr_cents / 100, currencyCode) : ''
	);
	let churnFormatted = $derived(
		stats ? `${(stats.churn_rate_30d * 100).toFixed(1)}%` : ''
	);
	let plansBreakdown = $derived(
		stats
			? Object.entries(stats.customers_by_plan)
					.map(([plan, count]) => `${capitalize(plan)}: ${count}`)
					.join(' · ')
			: ''
	);
	let cacheAgeText = $derived(
		stats
			? stats.cache_age_seconds < 30
				? 'just now'
				: `${Math.round(stats.cache_age_seconds / 60)} min ago`
			: ''
	);

	function formatCurrency(amount: number, currency: string): string {
		try {
			return new Intl.NumberFormat('en-US', {
				style: 'currency',
				currency
			}).format(amount);
		} catch {
			// Fallback if currency code is invalid
			return `${currency} ${amount.toFixed(2)}`;
		}
	}

	function capitalize(s: string): string {
		if (!s) return s;
		return s.charAt(0).toUpperCase() + s.slice(1);
	}

	onMount(() => {
		initialLoad();
	});
</script>

<div class="billing-page">
	{#if loading}
		<div class="loading-msg">Loading billing stats...</div>
	{:else if error}
		<div class="error-msg">
			<p>{error}</p>
			<button class="btn" onclick={initialLoad}>Retry</button>
		</div>
	{:else if stats}
		<header class="page-header">
			<h1>Billing</h1>
			<div class="header-actions">
				<button class="btn" onclick={refresh} disabled={refreshing}>
					{refreshing ? 'Refreshing...' : 'Refresh'}
				</button>
				<a
					class="btn"
					href="https://dashboard.stripe.com"
					target="_blank"
					rel="noopener noreferrer"
				>
					Open in Stripe Dashboard <span aria-hidden="true">&#8599;</span>
				</a>
			</div>
		</header>

		{#if stats.cloud_unreachable}
			<div class="banner warning" role="alert">
				Pad Cloud sidecar unreachable &mdash; showing local data only. Try refreshing.
			</div>
		{:else if !stats.stripe_configured}
			<div class="banner info" role="status" aria-live="polite">
				<span class="badge info-badge">Stripe not configured</span>
				Stripe billing is not yet configured. Stripe-derived metrics will be zero
				until <code>STRIPE_SECRET_KEY</code> is set on pad-cloud.
			</div>
		{/if}

		<div class="metrics-grid">
			<!-- 1. MRR (Stripe-derived) -->
			<div class="stat" class:disabled={stripeUnavailable}>
				<span class="stat-label">MRR</span>
				{#if stripeUnavailable}
					<span class="badge na">N/A</span>
				{:else}
					<span class="stat-value">{mrrFormatted}</span>
				{/if}
				<span class="stat-sub">Monthly recurring revenue</span>
			</div>

			<!-- 2. ARR (Stripe-derived) -->
			<div class="stat" class:disabled={stripeUnavailable}>
				<span class="stat-label">ARR</span>
				{#if stripeUnavailable}
					<span class="badge na">N/A</span>
				{:else}
					<span class="stat-value">{arrFormatted}</span>
				{/if}
				<span class="stat-sub">Annual run rate</span>
			</div>

			<!-- 3. Active Subscriptions (Stripe-derived) -->
			<div class="stat" class:disabled={stripeUnavailable}>
				<span class="stat-label">Active Subscriptions</span>
				{#if stripeUnavailable}
					<span class="badge na">N/A</span>
				{:else}
					<span class="stat-value">{stats.active_subscriptions}</span>
				{/if}
				<span class="stat-sub">Currently paying customers</span>
			</div>

			<!-- 4. Customers by Plan (LOCAL, always real) -->
			<div class="stat">
				<span class="stat-label">Customers by Plan</span>
				{#if Object.keys(stats.customers_by_plan).length === 0}
					<span class="stat-value">&mdash;</span>
				{:else}
					<span class="stat-breakdown">{plansBreakdown}</span>
				{/if}
				<span class="stat-sub">All registered users</span>
			</div>

			<!-- 5. New Signups 30d (LOCAL, always real) -->
			<div class="stat">
				<span class="stat-label">New Signups (30d)</span>
				<span class="stat-value">{stats.new_signups_30d}</span>
				<span class="stat-sub">Pro signups in last 30 days</span>
			</div>

			<!-- 6. Churn 30d (Stripe-derived) -->
			<div class="stat" class:disabled={stripeUnavailable}>
				<span class="stat-label">Churn (30d)</span>
				{#if stripeUnavailable}
					<span class="badge na">N/A</span>
				{:else}
					<span class="stat-value">{churnFormatted}</span>
				{/if}
				<span class="stat-sub">{stats.cancelled_30d} cancelled</span>
			</div>
		</div>

		<footer class="updated-footer">
			Updated {cacheAgeText}
		</footer>
	{/if}
</div>

<style>
	.billing-page {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.loading-msg {
		color: var(--text-muted);
		padding: var(--space-6) 0;
		text-align: center;
		font-size: 0.9rem;
	}
	.error-msg {
		color: #ef4444;
		padding: var(--space-6);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.error-msg p {
		margin: 0;
		font-size: 0.85rem;
	}

	.page-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		flex-wrap: wrap;
	}
	.page-header h1 {
		margin: 0;
		font-size: 1.5rem;
		font-weight: 600;
		color: var(--text-primary);
	}
	.header-actions {
		display: flex;
		gap: var(--space-2);
		align-items: center;
	}

	.btn {
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius);
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		color: var(--text-secondary);
		font-size: 0.85rem;
		font-weight: 500;
		cursor: pointer;
		text-decoration: none;
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		transition:
			border-color 0.15s,
			color 0.15s;
	}
	.btn:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
		text-decoration: none;
	}
	.btn:disabled {
		opacity: 0.5;
		cursor: default;
	}

	.banner {
		padding: var(--space-3) var(--space-4);
		border-radius: var(--radius);
		font-size: 0.85rem;
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	.banner code {
		background: rgba(0, 0, 0, 0.08);
		padding: 1px var(--space-1);
		border-radius: var(--radius-sm);
		font-size: 0.8rem;
	}
	.banner.warning {
		background: #fef3c7;
		border-left: 4px solid #f59e0b;
		color: #92400e;
	}
	.banner.info {
		background: var(--bg-secondary);
		border-left: 4px solid var(--accent-blue);
		color: var(--text-secondary);
	}

	.metrics-grid {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
		gap: var(--space-4);
	}

	.stat {
		padding: var(--space-4) var(--space-5);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.stat.disabled {
		opacity: 0.6;
	}
	.stat-value {
		font-size: 1.5rem;
		font-weight: 700;
		color: var(--text-primary);
	}
	.stat-label {
		font-size: 0.8rem;
		color: var(--text-muted);
		text-transform: capitalize;
	}
	.stat-sub {
		font-size: 0.75rem;
		color: var(--text-muted);
	}
	.stat-breakdown {
		font-size: 0.95rem;
		font-weight: 600;
		color: var(--text-primary);
		line-height: 1.4;
	}

	.badge {
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-weight: 500;
		background: color-mix(in srgb, #888 15%, transparent);
		color: var(--text-muted);
		display: inline-block;
		width: fit-content;
	}
	.badge.na {
		font-size: 1rem;
		padding: 4px var(--space-2);
	}
	.badge.info-badge {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}

	.updated-footer {
		font-size: 0.75rem;
		color: var(--text-muted);
		text-align: right;
		padding-top: var(--space-2);
	}
</style>
