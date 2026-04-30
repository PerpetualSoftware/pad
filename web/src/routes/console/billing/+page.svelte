<script lang="ts">
	import { authStore } from '$lib/stores/auth.svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { onMount, onDestroy } from 'svelte';

	interface PlanLimits {
		workspaces: number;
		items_per_workspace: number;
		members_per_workspace: number;
		api_tokens: number;
		storage_bytes: number;
		webhooks: number;
		automated_backups: number;
	}

	// Static metadata for the comparison table. Each row maps a label to
	// the PlanLimits field + a display kind so the renderer picks the
	// right formatter (count vs storage-bytes). Kept outside the
	// template to keep the table JSX legible and to avoid rebuilding
	// the array on every re-render.
	type LimitKey = keyof PlanLimits;
	const COMPARE_ROWS: { label: string; key: LimitKey; kind: 'count' | 'storage' }[] = [
		{ label: 'Workspaces', key: 'workspaces', kind: 'count' },
		{ label: 'Items per workspace', key: 'items_per_workspace', kind: 'count' },
		{ label: 'Members per workspace', key: 'members_per_workspace', kind: 'count' },
		{ label: 'API tokens', key: 'api_tokens', kind: 'count' },
		{ label: 'Storage', key: 'storage_bytes', kind: 'storage' },
		{ label: 'Webhooks', key: 'webhooks', kind: 'count' },
		{ label: 'Automated backups', key: 'automated_backups', kind: 'count' }
	];

	let plan = $derived(authStore.user?.plan ?? 'free');
	let isPro = $derived(plan === 'pro');
	let limits = $state<{ free: PlanLimits; pro: PlanLimits } | null>(null);

	// Gate for Stripe-backed upgrade affordances. Stripe isn't wired up on
	// this deployment yet, so the Free → Pro CTAs would dead-end at a 404
	// from /billing/checkout. Flip this to `true` (or thread it through a
	// server flag like `authStore.session?.billing_enabled`) once the
	// pad-cloud sidecar's Stripe integration is live so the "Upgrade to Pro"
	// buttons return. The "Confirming your upgrade…" / "Upgrade confirmed"
	// banners below stay wired — they only fire when ?checkout=success is
	// present in the URL, which can only happen after a real Stripe redirect.
	const STRIPE_AVAILABLE = false;

	// Upgrade-confirmation state. After Stripe Checkout redirects back with
	// ?checkout=success, pad-cloud's webhook handler needs a moment to land
	// at pad's /admin/plan endpoint before the user's plan flips to "pro".
	// Poll authStore.load() on a short interval until the plan updates or we
	// hit the timeout — a proxy for "something is wrong, please refresh".
	let upgradeStatus = $state<'idle' | 'checking' | 'confirmed' | 'timeout'>('idle');
	let pollTimer: ReturnType<typeof setInterval> | null = null;
	let pollStartedAt = 0;
	// Guards every post-await state write: an authStore.load() or plan-limits
	// fetch in flight when the user navigates away would otherwise continue
	// on, mutate state, or even install a fresh interval after this component
	// has been destroyed. Checked after every await.
	let destroyed = false;
	const POLL_INTERVAL_MS = 2000;
	const POLL_TIMEOUT_MS = 30000;

	function formatLimit(value: number | undefined): string {
		if (value === undefined) return '...';
		if (value === -1) return 'Unlimited';
		return value.toLocaleString();
	}

	// formatBytes renders a byte count in its most natural unit. Picks the
	// unit so the displayed value is < 1024 — bump thresholds are nudged
	// down half the previous unit so a value like 1,048,575 bytes (0.999
	// MB, 1023.999 KB) reads as "1.0 MB" rather than the misleading
	// "1024 KB" you'd get from a straight Math.round at the KB tier.
	function formatBytes(bytes: number): string {
		if (bytes < 0) return `${bytes} B`;
		const KB = 1024;
		const MB = KB * 1024;
		const GB = MB * 1024;
		const bumpGB = GB - MB / 2;
		const bumpMB = MB - KB / 2;
		if (bytes >= bumpGB) return formatUnit(bytes / GB, 'GB');
		if (bytes >= bumpMB) return formatUnit(bytes / MB, 'MB');
		if (bytes >= KB) return formatUnit(bytes / KB, 'KB');
		return `${bytes} B`;
	}

	function formatUnit(value: number, unit: string): string {
		if (value >= 10) return `${Math.round(value)} ${unit}`;
		return `${value.toFixed(1)} ${unit}`;
	}

	// formatCompareCell picks the right display for the comparison table:
	//  * undefined → "…" while limits are loading
	//  * any negative (the server treats anything < 0 as unlimited in
	//    internal/store/limits.go — match that here so a stored -2 does
	//    not render as "-2 B")
	//  * storage → byte-formatted
	//  * anything else → locale-formatted integer
	//
	// Zero is NOT specially rendered — admin-configured zero quotas
	// (storage_bytes = 0, workspaces = 0) are legitimate values and
	// should display literally as "0" rather than a "—" placeholder.
	function formatCompareCell(value: number | undefined, kind: 'count' | 'storage'): string {
		if (value === undefined) return '…';
		if (value < 0) return 'Unlimited';
		if (kind === 'storage') return formatBytes(value);
		return value.toLocaleString();
	}

	function stopPolling() {
		if (pollTimer !== null) {
			clearInterval(pollTimer);
			pollTimer = null;
		}
	}

	// Remove ?checkout=success so a page reload after the banner is dismissed
	// does not re-enter the polling branch. replaceState keeps the back button
	// pointing at the pre-checkout page rather than an artifact URL.
	function clearCheckoutQuery() {
		if (typeof window === 'undefined') return;
		const url = new URL(window.location.href);
		if (url.searchParams.has('checkout')) {
			url.searchParams.delete('checkout');
			history.replaceState(history.state, '', url.toString());
		}
	}

	async function pollForUpgrade() {
		try {
			await authStore.load();
		} catch {
			// Session fetch failed this tick; another poll will retry. No state change.
		}
		if (destroyed) {
			stopPolling();
			return;
		}
		if (authStore.user?.plan === 'pro') {
			upgradeStatus = 'confirmed';
			stopPolling();
			clearCheckoutQuery();
			return;
		}
		if (Date.now() - pollStartedAt >= POLL_TIMEOUT_MS) {
			upgradeStatus = 'timeout';
			stopPolling();
		}
	}

	// Bridge the async window between Stripe redirect and pad-cloud webhook.
	// Kicked off from onMount in parallel with plan-limits fetch so a slow
	// /plan-limits request does not delay the user's confirmation banner.
	async function startUpgradeConfirmation() {
		try {
			await authStore.load();
		} catch {
			/* polling branch below retries */
		}
		if (destroyed) return;
		if (authStore.user?.plan === 'pro') {
			upgradeStatus = 'confirmed';
			clearCheckoutQuery();
			return;
		}
		upgradeStatus = 'checking';
		pollStartedAt = Date.now();
		pollTimer = setInterval(pollForUpgrade, POLL_INTERVAL_MS);
	}

	async function loadPlanLimits() {
		try {
			const resp = await fetch('/api/v1/plan-limits', { credentials: 'same-origin' });
			if (destroyed) return;
			if (resp.ok) {
				const body = await resp.json();
				if (destroyed) return;
				limits = body;
			}
		} catch {
			/* use fallback rendering */
		}
	}

	onMount(() => {
		if (!authStore.cloudMode) {
			goto('/console', { replaceState: true });
			return;
		}

		// Kick off the two independent async tasks in parallel. Neither is
		// awaited here — onMount returns immediately so Svelte can run the
		// onDestroy cleanup path synchronously if the user navigates away
		// before either task resolves (the `destroyed` guard handles that).
		if (page.url.searchParams.get('checkout') === 'success') {
			startUpgradeConfirmation();
		}
		loadPlanLimits();
	});

	onDestroy(() => {
		destroyed = true;
		stopPolling();
	});
</script>

<svelte:head>
	<title>Billing - Pad</title>
</svelte:head>

<div class="billing-page">
	<h1 class="page-title">Billing</h1>

	{#if upgradeStatus === 'checking'}
		<div class="upgrade-banner checking" role="status" aria-live="polite">
			<span class="spinner" aria-hidden="true"></span>
			<span>Confirming your upgrade…</span>
		</div>
	{:else if upgradeStatus === 'confirmed' && isPro}
		<div class="upgrade-banner success" role="status" aria-live="polite">
			<span class="check-icon" aria-hidden="true">✓</span>
			<span>Upgrade confirmed — welcome to Pro!</span>
		</div>
	{:else if upgradeStatus === 'timeout'}
		<div class="upgrade-banner warning" role="status" aria-live="polite">
			<span>Your payment went through but we haven't confirmed your upgrade yet. Try refreshing in a moment; if your plan still shows Free, contact <a href="mailto:support@getpad.dev">support@getpad.dev</a>.</span>
		</div>
	{/if}

	<section class="card">
		<h2 class="card-title">Current Plan</h2>
		<div class="card-body">
			<div class="plan-row">
				<div class="plan-info">
					<span class="plan-name">
						{isPro ? 'Pro' : 'Free'}
					</span>
					<span class="plan-badge" class:pro={isPro} class:free={!isPro}>
						{isPro ? 'Active' : 'Current'}
					</span>
				</div>
				<p class="plan-desc">
					{#if isPro}
						You have full access to all Pad features.
					{:else}
						The free plan includes basic workspace management with limited features.
					{/if}
				</p>
			</div>

			{#if isPro}
				<a href="/billing/portal" class="secondary-btn">Manage Billing</a>
			{:else if STRIPE_AVAILABLE}
				<a href="/billing/checkout" class="primary-btn">Upgrade to Pro</a>
			{/if}
		</div>
	</section>

	<section class="card">
		<h2 class="card-title">Compare plans</h2>
		<div class="card-body compare">
			<table class="compare-table">
				<thead>
					<tr>
						<th scope="col" class="feature-col">Feature</th>
						<th scope="col" class="plan-col" class:current={!isPro}>
							<div class="plan-col-inner">
								<span class="plan-col-name">Free</span>
								{#if !isPro}<span class="current-tag">Current</span>{/if}
							</div>
						</th>
						<th scope="col" class="plan-col" class:current={isPro}>
							<div class="plan-col-inner">
								<span class="plan-col-name">Pro</span>
								{#if isPro}<span class="current-tag">Current</span>{/if}
							</div>
						</th>
					</tr>
				</thead>
				<tbody>
					{#each COMPARE_ROWS as row (row.key)}
						<tr>
							<th scope="row" class="feature-col">{row.label}</th>
							<td class="plan-col" class:current={!isPro}>{formatCompareCell(limits?.free?.[row.key], row.kind)}</td>
							<td class="plan-col" class:current={isPro}>{formatCompareCell(limits?.pro?.[row.key], row.kind)}</td>
						</tr>
					{/each}
				</tbody>
			</table>
			{#if !isPro}
				{#if STRIPE_AVAILABLE}
					<div class="compare-cta">
						<a href="/billing/checkout" class="primary-btn">Upgrade to Pro</a>
					</div>
				{:else}
					<div class="compare-cta coming-soon">
						<span class="coming-soon-label">Pro — coming soon</span>
						<span class="coming-soon-note">
							Interested? Email <a href="mailto:info@getpad.dev?subject=Pad%20Pro%20interest">info@getpad.dev</a> and we'll let you know when it's ready.
						</span>
					</div>
				{/if}
			{/if}
		</div>
	</section>
</div>

<style>
	.billing-page {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
		max-width: 600px;
	}

	.page-title {
		font-size: 1.4rem;
		font-weight: 700;
		color: var(--text-primary);
	}

	.card {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		overflow: hidden;
	}

	.card-title {
		font-size: 0.95rem;
		font-weight: 600;
		color: var(--text-primary);
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--border);
	}

	.card-body {
		padding: var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.plan-row {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.plan-info {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	.plan-name {
		font-size: 1.1rem;
		font-weight: 700;
		color: var(--text-primary);
	}

	.plan-badge {
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-weight: 500;
	}

	.plan-badge.pro {
		background: color-mix(in srgb, var(--accent-green) 15%, transparent);
		color: var(--accent-green);
	}

	.plan-badge.free {
		background: color-mix(in srgb, var(--accent-gray) 15%, transparent);
		color: var(--accent-gray);
	}

	.plan-desc {
		color: var(--text-secondary);
		font-size: 0.85rem;
		line-height: 1.4;
	}

	.primary-btn {
		display: inline-block;
		align-self: flex-start;
		padding: var(--space-2) var(--space-5);
		background: var(--accent-blue);
		color: #fff;
		border-radius: var(--radius);
		font-size: 0.9rem;
		font-weight: 500;
		text-decoration: none;
		transition: opacity 0.15s;
	}

	.primary-btn:hover {
		opacity: 0.9;
		text-decoration: none;
	}

	.secondary-btn {
		display: inline-block;
		align-self: flex-start;
		padding: var(--space-2) var(--space-5);
		background: transparent;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.9rem;
		font-weight: 500;
		text-decoration: none;
		transition: color 0.15s, border-color 0.15s;
	}

	.secondary-btn:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
		text-decoration: none;
	}

	.card-body.compare {
		padding: 0;
		gap: 0;
	}

	.compare-table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.85rem;
	}

	.compare-table thead th {
		padding: var(--space-3) var(--space-5);
		border-bottom: 1px solid var(--border);
		text-align: left;
		color: var(--text-secondary);
		font-weight: 500;
		font-size: 0.8rem;
		letter-spacing: 0.02em;
		text-transform: uppercase;
	}

	.compare-table tbody th,
	.compare-table tbody td {
		padding: var(--space-3) var(--space-5);
		border-bottom: 1px solid var(--border-subtle);
		vertical-align: middle;
	}

	.compare-table tbody tr:last-child th,
	.compare-table tbody tr:last-child td {
		border-bottom: none;
	}

	.compare-table .feature-col {
		color: var(--text-secondary);
		font-weight: 400;
		text-align: left;
		width: 40%;
	}

	.compare-table tbody .feature-col {
		color: var(--text-primary);
	}

	.compare-table .plan-col {
		color: var(--text-primary);
		font-weight: 500;
		text-align: right;
		width: 30%;
	}

	.compare-table .plan-col.current {
		background: color-mix(in srgb, var(--accent-blue) 6%, transparent);
	}

	.plan-col-inner {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		justify-content: flex-end;
		width: 100%;
	}

	.plan-col-name {
		text-transform: none;
		letter-spacing: 0;
		font-weight: 600;
		color: var(--text-primary);
		font-size: 0.85rem;
	}

	.current-tag {
		padding: 1px var(--space-2);
		border-radius: var(--radius-sm);
		background: var(--accent-blue);
		color: #fff;
		font-size: 0.7rem;
		font-weight: 600;
		text-transform: none;
		letter-spacing: 0;
	}

	.compare-cta {
		padding: var(--space-4) var(--space-5);
		border-top: 1px solid var(--border);
		display: flex;
		justify-content: flex-end;
	}

	.compare-cta.coming-soon {
		flex-direction: column;
		align-items: flex-start;
		justify-content: flex-start;
		gap: var(--space-1);
	}

	.coming-soon-label {
		font-size: 0.85rem;
		font-weight: 600;
		color: var(--text-primary);
	}

	.coming-soon-note {
		font-size: 0.8rem;
		color: var(--text-secondary);
		line-height: 1.4;
	}

	.coming-soon-note a {
		color: var(--accent-blue);
		text-decoration: underline;
	}

	@media (max-width: 480px) {
		.compare-table thead th,
		.compare-table tbody th,
		.compare-table tbody td {
			padding: var(--space-3);
		}
		.compare-table .feature-col {
			width: 50%;
		}
		.compare-table .plan-col {
			width: 25%;
		}
	}

	.upgrade-banner {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		border-radius: var(--radius);
		font-size: 0.9rem;
		line-height: 1.4;
		border: 1px solid transparent;
	}

	.upgrade-banner.checking {
		background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
		border-color: color-mix(in srgb, var(--accent-blue) 30%, transparent);
		color: var(--text-primary);
	}

	.upgrade-banner.success {
		background: color-mix(in srgb, var(--accent-green) 12%, transparent);
		border-color: color-mix(in srgb, var(--accent-green) 35%, transparent);
		color: var(--text-primary);
	}

	.upgrade-banner.warning {
		background: color-mix(in srgb, var(--accent-yellow, #eab308) 12%, transparent);
		border-color: color-mix(in srgb, var(--accent-yellow, #eab308) 35%, transparent);
		color: var(--text-primary);
	}

	.upgrade-banner a {
		color: var(--accent-blue);
		text-decoration: underline;
	}

	.spinner {
		width: 14px;
		height: 14px;
		border: 2px solid color-mix(in srgb, var(--accent-blue) 30%, transparent);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.8s linear infinite;
		flex-shrink: 0;
	}

	.check-icon {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 18px;
		height: 18px;
		border-radius: 50%;
		background: var(--accent-green);
		color: #fff;
		font-size: 0.7rem;
		font-weight: 700;
		flex-shrink: 0;
	}

	@keyframes spin {
		to { transform: rotate(360deg); }
	}

	@media (prefers-reduced-motion: reduce) {
		.spinner {
			animation: none;
			border-top-color: transparent;
		}
	}
</style>
