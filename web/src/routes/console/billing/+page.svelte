<script lang="ts">
	import { authStore } from '$lib/stores/auth.svelte';
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';

	let plan = $derived(authStore.user?.plan ?? 'free');
	let isPro = $derived(plan === 'pro');

	onMount(() => {
		if (!authStore.cloudMode) {
			goto('/console', { replaceState: true });
		}
	});
</script>

<svelte:head>
	<title>Billing - Pad</title>
</svelte:head>

<div class="billing-page">
	<h1 class="page-title">Billing</h1>

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
				<span class="usage-value">{isPro ? 'Unlimited' : 'Up to 5'}</span>
			</div>
			<div class="usage-row">
				<span class="usage-label">Members per workspace</span>
				<span class="usage-value">{isPro ? 'Unlimited' : 'Up to 3'}</span>
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
</style>
