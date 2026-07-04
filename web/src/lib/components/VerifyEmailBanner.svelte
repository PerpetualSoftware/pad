<script lang="ts">
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';

	// Shown ONLY for an unverified Pad Cloud account (PLAN-1933 DR-1 model b /
	// TASK-1940). authStore.emailVerified defaults TRUE, so self-hosted
	// instances, older servers (absent field), and OAuth/invited/admin-created
	// accounts never match — this is a pure cloud-mode surface.
	let visible = $derived(
		authStore.cloudMode && !!authStore.user && !authStore.emailVerified
	);

	// idle → sending → sent (terminal) ; error is retryable.
	let resendState = $state<'idle' | 'sending' | 'sent' | 'error'>('idle');

	async function resend() {
		const email = authStore.user?.email;
		if (!email || resendState === 'sending') return;
		resendState = 'sending';
		try {
			// Enumeration-safe (always 200); a resolved promise is the only
			// success signal, so treat any non-throw as "sent".
			await api.auth.resendVerification(email);
			resendState = 'sent';
		} catch {
			resendState = 'error';
		}
	}
</script>

{#if visible}
	<div class="verify-banner" role="status">
		<span class="verify-icon" aria-hidden="true">
			<svg
				width="16"
				height="16"
				viewBox="0 0 24 24"
				fill="none"
				stroke="currentColor"
				stroke-width="2"
				stroke-linecap="round"
				stroke-linejoin="round"
			>
				<path d="M4 4h16c1.1 0 2 .9 2 2v12c0 1.1-.9 2-2 2H4c-1.1 0-2-.9-2-2V6c0-1.1.9-2 2-2z" />
				<path d="m22 6-10 7L2 6" />
			</svg>
		</span>
		<span class="verify-text">
			Verify your email to create and share. Check your inbox for the confirmation link.
		</span>
		<span class="verify-actions">
			{#if resendState === 'sent'}
				<span class="verify-sent">Verification email sent</span>
			{:else}
				<button
					class="verify-resend"
					type="button"
					onclick={resend}
					disabled={resendState === 'sending'}
				>
					{#if resendState === 'sending'}
						Sending…
					{:else if resendState === 'error'}
						Retry
					{:else}
						Resend
					{/if}
				</button>
			{/if}
		</span>
	</div>
{/if}

<style>
	.verify-banner {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-2) var(--space-4);
		background: color-mix(in srgb, var(--accent-blue) 8%, var(--bg-secondary));
		border: 1px solid color-mix(in srgb, var(--accent-blue) 40%, var(--border));
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85em;
		box-sizing: border-box;
	}
	.verify-icon {
		display: flex;
		align-items: center;
		justify-content: center;
		color: var(--accent-blue);
		flex-shrink: 0;
	}
	.verify-text {
		flex: 1;
		min-width: 0;
		color: var(--text-secondary);
	}
	.verify-actions {
		display: flex;
		align-items: center;
		flex-shrink: 0;
	}
	.verify-sent {
		color: var(--accent-blue);
		font-weight: 600;
	}
	.verify-resend {
		padding: var(--space-1) var(--space-3);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius);
		color: #fff;
		font-size: inherit;
		font-family: var(--font-ui);
		font-weight: 500;
		cursor: pointer;
		transition: opacity 0.15s;
	}
	.verify-resend:hover:not(:disabled) {
		opacity: 0.9;
	}
	.verify-resend:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}
</style>
