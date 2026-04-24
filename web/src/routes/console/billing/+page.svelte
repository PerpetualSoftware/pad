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
	}

	let plan = $derived(authStore.user?.plan ?? 'free');
	let isPro = $derived(plan === 'pro');
	let limits = $state<{ free: PlanLimits; pro: PlanLimits } | null>(null);

	// Upgrade-confirmation state. After Stripe Checkout redirects back with
	// ?checkout=success, pad-cloud's webhook handler needs a moment to land
	// at pad's /admin/plan endpoint before the user's plan flips to "pro".
	// Poll authStore.load() on a short interval until the plan updates or we
	// hit the timeout — a proxy for "something is wrong, please refresh".
	let upgradeStatus = $state<'idle' | 'checking' | 'confirmed' | 'timeout'>('idle');
	let pollTimer: ReturnType<typeof setInterval> | null = null;
	let pollStartedAt = 0;
	const POLL_INTERVAL_MS = 2000;
	const POLL_TIMEOUT_MS = 30000;

	function formatLimit(value: number | undefined): string {
		if (value === undefined) return '...';
		if (value === -1) return 'Unlimited';
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

	onMount(async () => {
		if (!authStore.cloudMode) {
			goto('/console', { replaceState: true });
			return;
		}

		try {
			const resp = await fetch('/api/v1/plan-limits', { credentials: 'same-origin' });
			if (resp.ok) limits = await resp.json();
		} catch {
			/* use fallback rendering */
		}

		if (page.url.searchParams.get('checkout') === 'success') {
			// Refresh once up front — if the webhook has already landed by the
			// time the browser redirects back, we can skip polling entirely.
			try {
				await authStore.load();
			} catch {
				/* ignore — the polling branch below will retry */
			}
			if (authStore.user?.plan === 'pro') {
				upgradeStatus = 'confirmed';
				clearCheckoutQuery();
			} else {
				upgradeStatus = 'checking';
				pollStartedAt = Date.now();
				pollTimer = setInterval(pollForUpgrade, POLL_INTERVAL_MS);
			}
		}
	});

	onDestroy(() => {
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
	{:else if upgradeStatus === 'confirmed'}
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
			{:else}
				<a href="/billing/checkout" class="primary-btn">Upgrade to Pro</a>
			{/if}
		</div>
	</section>

	<section class="card">
		<h2 class="card-title">Usage</h2>
		<div class="card-body">
			<div class="usage-row">
				<span class="usage-label">Plan</span>
				<span class="usage-value">{isPro ? 'Pro' : 'Free'}</span>
			</div>
			<div class="usage-row">
				<span class="usage-label">Workspaces</span>
				<span class="usage-value">{isPro ? formatLimit(limits?.pro?.workspaces) : formatLimit(limits?.free?.workspaces)}</span>
			</div>
			<div class="usage-row">
				<span class="usage-label">Items per workspace</span>
				<span class="usage-value">{isPro ? formatLimit(limits?.pro?.items_per_workspace) : formatLimit(limits?.free?.items_per_workspace)}</span>
			</div>
			<div class="usage-row">
				<span class="usage-label">Members per workspace</span>
				<span class="usage-value">{isPro ? formatLimit(limits?.pro?.members_per_workspace) : formatLimit(limits?.free?.members_per_workspace)}</span>
			</div>
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

	.usage-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-2) 0;
		border-bottom: 1px solid var(--border-subtle);
	}

	.usage-row:last-child {
		border-bottom: none;
	}

	.usage-label {
		color: var(--text-secondary);
		font-size: 0.85rem;
	}

	.usage-value {
		color: var(--text-primary);
		font-size: 0.85rem;
		font-weight: 500;
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
