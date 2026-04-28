<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/stores';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import type { User } from '$lib/types';

	let status = $state<'loading' | 'pending' | 'approved' | 'expired' | 'error' | 'success' | 'already_approved'>('loading');
	let error = $state('');
	let approving = $state(false);
	let switchingAccount = $state(false);
	let currentUser = $state<User | null>(null);

	onMount(async () => {
		const code = $page.params.code;
		if (!code) {
			status = 'error';
			error = 'Missing CLI session code.';
			return;
		}

		try {
			const session = await api.auth.cli.getSession(code);

			if (session.status === 'expired') {
				status = 'expired';
				return;
			}

			if (session.status === 'approved') {
				status = 'already_approved';
				return;
			}

			// Session is pending — check if user is logged in
			const authSession = await api.auth.session();
			if (!authSession.authenticated) {
				goto(`/login?redirect=/auth/cli/${code}`, { replaceState: true });
				return;
			}

			// Best-effort load of the current user so we can show which account
			// the CLI session is about to be linked to. If this fails, the chip
			// just won't render but the Approve flow still works.
			try {
				currentUser = await api.auth.me();
			} catch {
				currentUser = null;
			}

			status = 'pending';
		} catch {
			status = 'error';
			error = 'This link is invalid or has expired. Run `pad auth login` again.';
		}
	});

	async function handleApprove() {
		const code = $page.params.code;
		if (!code) {
			error = 'Missing CLI session code.';
			return;
		}
		approving = true;
		error = '';

		try {
			await api.auth.cli.approveSession(code);
			status = 'success';
		} catch (err: unknown) {
			if (err instanceof Error) {
				error = err.message || 'Failed to approve session.';
			} else {
				error = 'Failed to approve session.';
			}
		} finally {
			approving = false;
		}
	}

	async function handleSwitchAccount() {
		const code = $page.params.code;
		switchingAccount = true;
		try {
			await api.auth.logout();
		} catch {
			// Drop the session client-side regardless of server response.
		}
		goto(`/login?redirect=/auth/cli/${code}`, { replaceState: true });
	}
</script>

<div class="login-page">
	<div class="login-card">
		<h1 class="logo">Pad</h1>

		{#if status === 'loading'}
			<p class="subtitle">Verifying session...</p>
		{:else if status === 'error' || status === 'expired'}
			<p class="subtitle">Session unavailable</p>
			<p class="error-message">
				{status === 'expired'
					? 'This link is invalid or has expired. Run `pad auth login` again.'
					: error}
			</p>
		{:else if status === 'already_approved'}
			<p class="subtitle">Already approved</p>
			<p class="success-message">This session has already been approved.</p>
		{:else if status === 'pending'}
			<p class="subtitle">Authorize CLI session</p>
			<p class="description">
				A CLI session is requesting access to your account. Approve this request to sign in from the terminal.
			</p>

			{#if currentUser}
				<div class="account-chip">
					{#if currentUser.avatar_url}
						<img
							class="avatar"
							src={currentUser.avatar_url}
							alt=""
							width="32"
							height="32"
						/>
					{/if}
					<div class="account-info">
						<div class="account-name">{currentUser.name || currentUser.username}</div>
						<div class="account-email">{currentUser.email}</div>
					</div>
				</div>
				<button
					type="button"
					class="link-button"
					onclick={handleSwitchAccount}
					disabled={switchingAccount || approving}
				>
					{#if switchingAccount}
						Switching...
					{:else}
						I'm not {currentUser.name || currentUser.username} — switch accounts
					{/if}
				</button>
			{/if}

			{#if error}
				<p class="error">{error}</p>
			{/if}

			<button onclick={handleApprove} disabled={approving || switchingAccount}>
				{#if approving}
					Approving...
				{:else}
					Approve
				{/if}
			</button>
		{:else if status === 'success'}
			<p class="subtitle">Authorized</p>
			<p class="success-message">CLI session authorized. You can close this tab.</p>
		{/if}
	</div>
</div>

<style>
	.login-page {
		display: flex;
		align-items: center;
		justify-content: center;
		min-height: 100vh;
		background: var(--bg-primary);
		padding: var(--space-4);
	}

	.login-card {
		width: 100%;
		max-width: 360px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		padding: var(--space-10) var(--space-8);
		text-align: center;
	}

	.logo {
		font-size: 2rem;
		font-weight: 700;
		color: var(--text-primary);
		letter-spacing: -0.02em;
		margin-bottom: var(--space-2);
	}

	.subtitle {
		color: var(--text-muted);
		font-size: 0.9rem;
		margin-bottom: var(--space-6);
	}

	.description {
		color: var(--text-secondary);
		font-size: 0.85rem;
		line-height: 1.5;
		margin-bottom: var(--space-6);
		text-align: left;
	}

	.error-message {
		color: #ef4444;
		font-size: 0.85rem;
		line-height: 1.5;
	}

	.error {
		color: #ef4444;
		font-size: 0.85rem;
		text-align: left;
		margin-bottom: var(--space-4);
	}

	.success-message {
		color: var(--text-secondary);
		font-size: 0.9rem;
		line-height: 1.5;
	}

	button {
		width: 100%;
		padding: var(--space-3) var(--space-4);
		background: var(--accent-blue);
		color: #fff;
		border: none;
		border-radius: var(--radius);
		font-size: 0.95rem;
		font-weight: 500;
		font-family: var(--font-ui);
		cursor: pointer;
		transition: opacity 0.15s;
	}

	button:hover:not(:disabled) {
		opacity: 0.9;
	}

	button:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.account-chip {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-3);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		margin-bottom: var(--space-3);
		text-align: left;
	}

	.avatar {
		width: 32px;
		height: 32px;
		border-radius: 50%;
		object-fit: cover;
		flex-shrink: 0;
	}

	.account-info {
		display: flex;
		flex-direction: column;
		min-width: 0;
		flex: 1;
	}

	.account-name {
		color: var(--text-primary);
		font-size: 0.9rem;
		font-weight: 500;
		line-height: 1.3;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.account-email {
		color: var(--text-muted);
		font-size: 0.8rem;
		line-height: 1.3;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	button.link-button {
		width: 100%;
		padding: 0;
		background: none;
		color: var(--text-muted);
		border: none;
		border-radius: 0;
		font-size: 0.8rem;
		font-weight: 400;
		font-family: var(--font-ui);
		text-align: center;
		cursor: pointer;
		margin-bottom: var(--space-5);
		transition: color 0.15s;
	}

	button.link-button:hover:not(:disabled) {
		color: var(--text-primary);
		opacity: 1;
		text-decoration: underline;
	}

	button.link-button:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}
</style>
