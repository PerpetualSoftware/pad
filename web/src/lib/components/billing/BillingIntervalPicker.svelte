<script lang="ts">
	import { authStore } from '$lib/stores/auth.svelte';
	import type { BillingInterval } from '$lib/api/client';

	// Monthly or annual Pad Pro (TASK-3468). Native radios in a fieldset, so
	// arrow keys, labels and the group name come from the platform. One value
	// is shared by every picker on the page (bind:value), so whichever Upgrade
	// button the user presses buys the interval they see selected.
	//
	// Prices are commerce: the mobile apps must not show them (PLAN-3291
	// DR-3), so this renders nothing unless authStore.commerceAllowed.
	// noCommerceOutsideGate.test.ts lists this file as gated and checks that
	// it reads the gate.

	interface Props {
		value?: BillingInterval;
		/** Intervals the sidecar reported as not sold right now; disabled. */
		unavailable?: BillingInterval[];
		/** Distinct per picker on a page, so two radio groups don't merge. */
		name: string;
		disabled?: boolean;
	}

	let { value = $bindable('monthly'), unavailable = [], name, disabled = false }: Props = $props();

	const OPTIONS: { interval: BillingInterval; label: string; price: string }[] = [
		{ interval: 'monthly', label: 'Monthly', price: '$8 / month' },
		{ interval: 'annual', label: 'Annual', price: '$80 / year' }
	];
</script>

{#if authStore.commerceAllowed}
	<fieldset class="interval-picker" {disabled}>
		<legend class="sr-only">Billing interval</legend>
		<div class="options">
			{#each OPTIONS as opt (opt.interval)}
				{@const off = unavailable.includes(opt.interval)}
				<label class="option" class:selected={value === opt.interval} class:off>
					<input type="radio" {name} value={opt.interval} bind:group={value} disabled={off} />
					<span class="label">{opt.label}</span>
					<span class="price">{opt.price}</span>
				</label>
			{/each}
		</div>
		<p class="note">Prices in USD. Stripe may charge in your local currency at checkout.</p>
	</fieldset>
{/if}

<style>
	.interval-picker {
		border: 0;
		margin: 0 0 var(--space-3);
		padding: 0;
		min-width: 0;
	}

	.options {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}

	.option {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-tertiary);
		color: var(--text-primary);
		cursor: pointer;
	}

	.option.selected {
		border-color: var(--accent-primary);
	}

	.option.off {
		opacity: 0.55;
		cursor: not-allowed;
	}

	.price {
		color: var(--text-secondary);
		font-size: 0.9em;
	}

	.note {
		margin-top: var(--space-2);
		font-size: 0.8em;
		color: var(--text-muted);
	}

	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		padding: 0;
		margin: -1px;
		overflow: hidden;
		clip: rect(0, 0, 0, 0);
		white-space: nowrap;
		border: 0;
	}
</style>
