<!--
	The plan-limit prompt (TASK-3601). Stays until dismissed. With `scope`, it
	is the inline host for that flow (invite, import, create workspace, new
	item) and shows only that flow's refusals; without, it is the global host
	in the root layout and shows every other one.

	Upgrade remembers where the user was (upgradeResume), so the billing page
	can offer "Continue: <what was blocked>" after checkout.
-->
<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { planLimitPrompt } from '$lib/billing/planLimitPrompt.svelte';
	import { BILLING_PATH } from '$lib/billing/planLimitPrompt';
	import { saveUpgradeResume } from '$lib/billing/upgradeResume';

	let { scope = null }: { scope?: string | null } = $props();

	onMount(() => (scope ? planLimitPrompt.registerInlineHost(scope) : undefined));

	let p = $derived(planLimitPrompt.current && planLimitPrompt.current.scope === scope ? planLimitPrompt.current : null);
	let copied = $state(false);

	function upgrade() {
		if (!p) return;
		saveUpgradeResume(window.location.pathname + window.location.search, p.blocked);
		planLimitPrompt.dismiss();
		goto(BILLING_PATH);
	}

	async function copyNote() {
		if (!p) return;
		try {
			await navigator.clipboard.writeText(p.note);
			copied = true;
		} catch {
			copied = false;
		}
	}
</script>

{#if p}
	{#key p.id}
		<div class="plan-limit-prompt" class:inline={!!scope} role="alert">
			<p class="plan-limit-message">{p.message}</p>
			{#if p.hint}<p class="plan-limit-hint">{p.hint}</p>{/if}
			<div class="plan-limit-actions">
				{#if p.action === 'upgrade'}
					<button type="button" class="plan-limit-primary" onclick={upgrade}>Upgrade</button>
				{:else if p.action === 'ask'}
					<button type="button" class="plan-limit-secondary" onclick={copyNote}>{copied ? 'Copied' : 'Copy note'}</button>
				{/if}
				<button type="button" class="plan-limit-dismiss" onclick={() => planLimitPrompt.dismiss()}>Dismiss</button>
			</div>
		</div>
	{/key}
{/if}

<style>
	.plan-limit-prompt {
		border: 1px solid var(--accent-amber, var(--border));
		background: var(--bg-secondary);
		border-radius: var(--radius);
		padding: var(--space-3) var(--space-4);
		font-size: 0.9em;
	}
	.plan-limit-prompt:not(.inline) {
		position: fixed;
		left: 50%;
		bottom: var(--space-6);
		transform: translateX(-50%);
		width: min(32rem, calc(100vw - 2 * var(--space-4)));
		z-index: 1000;
		box-shadow: var(--shadow-lg, 0 8px 24px rgba(0, 0, 0, 0.2));
	}
	.plan-limit-prompt.inline {
		margin: var(--space-3) 0;
	}
	.plan-limit-message {
		margin: 0;
		color: var(--text-primary);
		font-weight: 600;
	}
	.plan-limit-hint {
		margin: var(--space-1) 0 0;
		color: var(--text-secondary);
	}
	.plan-limit-actions {
		display: flex;
		gap: var(--space-2);
		margin-top: var(--space-3);
		justify-content: flex-end;
	}
	.plan-limit-actions button {
		padding: var(--space-1) var(--space-3);
		border-radius: var(--radius-sm);
		border: 1px solid var(--border);
		background: var(--bg-primary);
		color: var(--text-primary);
		font: inherit;
		cursor: pointer;
	}
	.plan-limit-actions .plan-limit-primary {
		background: var(--accent-blue);
		border-color: var(--accent-blue);
		color: #fff;
	}
</style>
