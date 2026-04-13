<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import type { User, APIToken, APITokenWithSecret } from '$lib/types';

	// Profile
	let profile = $state<User | null>(null);
	let profileName = $state('');
	let profileUsername = $state('');
	let profileSaving = $state(false);
	let profileMsg = $state('');
	let profileError = $state('');

	// Password
	let currentPassword = $state('');
	let newPassword = $state('');
	let confirmPassword = $state('');
	let passwordSaving = $state(false);
	let passwordMsg = $state('');
	let passwordError = $state('');

	// Tokens
	let tokens = $state<APIToken[]>([]);
	let newTokenName = $state('');
	let createdToken = $state<APITokenWithSecret | null>(null);
	let tokenCreating = $state(false);
	let tokenError = $state('');

	let loading = $state(true);

	onMount(async () => {
		try {
			const [me, tokenList] = await Promise.all([
				api.auth.me(),
				api.auth.tokens.list()
			]);
			profile = me;
			profileName = me.name;
			profileUsername = me.username;
			tokens = tokenList;
		} catch {
			profileError = 'Failed to load profile';
		} finally {
			loading = false;
		}
	});

	async function saveProfile() {
		profileError = '';
		profileMsg = '';
		profileSaving = true;
		try {
			const updated = await api.auth.updateProfile({
				name: profileName.trim(),
				username: profileUsername.trim()
			});
			profile = updated;
			profileMsg = 'Profile updated.';
			await authStore.load();
		} catch (err) {
			profileError = err instanceof Error ? err.message : 'Failed to update profile';
		} finally {
			profileSaving = false;
		}
	}

	async function changePassword() {
		passwordError = '';
		passwordMsg = '';
		if (!currentPassword || !newPassword) {
			passwordError = 'Please fill in all password fields.';
			return;
		}
		if (newPassword !== confirmPassword) {
			passwordError = 'New passwords do not match.';
			return;
		}
		if (newPassword.length < 8) {
			passwordError = 'Password must be at least 8 characters.';
			return;
		}

		passwordSaving = true;
		try {
			await api.auth.updateProfile({
				current_password: currentPassword,
				new_password: newPassword
			});
			passwordMsg = 'Password changed successfully.';
			currentPassword = '';
			newPassword = '';
			confirmPassword = '';
		} catch (err) {
			passwordError = err instanceof Error ? err.message : 'Failed to change password';
		} finally {
			passwordSaving = false;
		}
	}

	async function createToken() {
		tokenError = '';
		createdToken = null;
		if (!newTokenName.trim()) {
			tokenError = 'Please enter a token name.';
			return;
		}

		tokenCreating = true;
		try {
			const token = await api.auth.tokens.create(newTokenName.trim());
			createdToken = token;
			tokens = [...tokens, token];
			newTokenName = '';
		} catch (err) {
			tokenError = err instanceof Error ? err.message : 'Failed to create token';
		} finally {
			tokenCreating = false;
		}
	}

	async function deleteToken(tokenId: string) {
		try {
			await api.auth.tokens.delete(tokenId);
			tokens = tokens.filter((t) => t.id !== tokenId);
		} catch {
			// Silent failure acceptable for delete
		}
	}

	function formatDate(dateStr: string): string {
		return new Date(dateStr).toLocaleDateString('en-US', {
			month: 'short', day: 'numeric', year: 'numeric'
		});
	}
</script>

<svelte:head>
	<title>Settings - Pad</title>
</svelte:head>

<div class="settings-page">
	<h1 class="page-title">Account Settings</h1>

	{#if loading}
		<div class="loading">Loading...</div>
	{:else}
		<!-- Profile -->
		<section class="card">
			<h2 class="card-title">Profile</h2>
			<div class="card-body">
				<div class="field">
					<label for="profile-name">Name</label>
					<input id="profile-name" type="text" bind:value={profileName} disabled={profileSaving} />
				</div>
				<div class="field">
					<label for="profile-username">Username</label>
					<input id="profile-username" type="text" bind:value={profileUsername} disabled={profileSaving} />
				</div>
				<div class="field">
					<label for="profile-email">Email</label>
					<input id="profile-email" type="email" value={profile?.email ?? ''} disabled readonly />
				</div>
				{#if profileError}
					<p class="error">{profileError}</p>
				{/if}
				{#if profileMsg}
					<p class="success">{profileMsg}</p>
				{/if}
				<button class="primary-btn" onclick={saveProfile} disabled={profileSaving}>
					{profileSaving ? 'Saving...' : 'Save Changes'}
				</button>
			</div>
		</section>

		<!-- Password -->
		<section class="card">
			<h2 class="card-title">Password</h2>
			<div class="card-body">
				<div class="field">
					<label for="current-pw">Current password</label>
					<input id="current-pw" type="password" bind:value={currentPassword} disabled={passwordSaving} autocomplete="current-password" />
				</div>
				<div class="field">
					<label for="new-pw">New password</label>
					<input id="new-pw" type="password" bind:value={newPassword} disabled={passwordSaving} autocomplete="new-password" />
				</div>
				<div class="field">
					<label for="confirm-pw">Confirm new password</label>
					<input id="confirm-pw" type="password" bind:value={confirmPassword} disabled={passwordSaving} autocomplete="new-password" />
				</div>
				{#if passwordError}
					<p class="error">{passwordError}</p>
				{/if}
				{#if passwordMsg}
					<p class="success">{passwordMsg}</p>
				{/if}
				<button class="primary-btn" onclick={changePassword} disabled={passwordSaving}>
					{passwordSaving ? 'Changing...' : 'Change Password'}
				</button>
			</div>
		</section>

		<!-- API Tokens -->
		<section class="card">
			<h2 class="card-title">API Tokens</h2>
			<div class="card-body">
				{#if createdToken}
					<div class="token-created">
						<p class="token-warning">Copy this token now. It will not be shown again.</p>
						<code class="token-value">{createdToken.token}</code>
					</div>
				{/if}

				<div class="token-create-row">
					<input
						type="text"
						placeholder="Token name"
						bind:value={newTokenName}
						disabled={tokenCreating}
					/>
					<button class="primary-btn" onclick={createToken} disabled={tokenCreating || !newTokenName.trim()}>
						{tokenCreating ? 'Creating...' : 'Create'}
					</button>
				</div>

				{#if tokenError}
					<p class="error">{tokenError}</p>
				{/if}

				{#if tokens.length > 0}
					<div class="token-list">
						{#each tokens as token (token.id)}
							<div class="token-row">
								<div class="token-info">
									<span class="token-name">{token.name}</span>
									<span class="token-meta">
										{token.prefix}... &middot; Created {formatDate(token.created_at)}
										{#if token.expires_at}
											&middot; Expires {formatDate(token.expires_at)}
										{/if}
									</span>
								</div>
								<button class="delete-btn" onclick={() => deleteToken(token.id)}>Delete</button>
							</div>
						{/each}
					</div>
				{:else}
					<p class="empty-text">No API tokens yet.</p>
				{/if}
			</div>
		</section>
	{/if}
</div>

<style>
	.settings-page {
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

	.loading {
		color: var(--text-muted);
		padding: var(--space-10) 0;
		text-align: center;
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

	.field {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	label {
		font-size: 0.8rem;
		font-weight: 500;
		color: var(--text-muted);
	}

	input {
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.9rem;
		font-family: var(--font-ui);
		outline: none;
		transition: border-color 0.15s;
	}

	input:focus {
		border-color: var(--accent-blue);
	}

	input:disabled {
		opacity: 0.6;
	}

	input[readonly] {
		color: var(--text-muted);
		cursor: not-allowed;
	}

	.error {
		color: #ef4444;
		font-size: 0.85rem;
	}

	.success {
		color: var(--accent-green);
		font-size: 0.85rem;
	}

	.primary-btn {
		align-self: flex-start;
		padding: var(--space-2) var(--space-4);
		background: var(--accent-blue);
		color: #fff;
		border: none;
		border-radius: var(--radius);
		font-size: 0.85rem;
		font-weight: 500;
		font-family: var(--font-ui);
		cursor: pointer;
		transition: opacity 0.15s;
	}

	.primary-btn:hover:not(:disabled) {
		opacity: 0.9;
	}

	.primary-btn:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.token-created {
		padding: var(--space-3) var(--space-4);
		background: color-mix(in srgb, var(--accent-green) 10%, var(--bg-tertiary));
		border: 1px solid color-mix(in srgb, var(--accent-green) 30%, transparent);
		border-radius: var(--radius);
	}

	.token-warning {
		font-size: 0.8rem;
		font-weight: 500;
		color: var(--accent-green);
		margin-bottom: var(--space-2);
	}

	.token-value {
		display: block;
		font-family: var(--font-mono);
		font-size: 0.8rem;
		color: var(--text-primary);
		word-break: break-all;
	}

	.token-create-row {
		display: flex;
		gap: var(--space-2);
	}

	.token-create-row input {
		flex: 1;
	}

	.token-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.token-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
	}

	.token-info {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}

	.token-name {
		font-weight: 500;
		font-size: 0.85rem;
		color: var(--text-primary);
	}

	.token-meta {
		font-size: 0.75rem;
		color: var(--text-muted);
	}

	.delete-btn {
		padding: var(--space-1) var(--space-3);
		background: transparent;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		font-size: 0.75rem;
		cursor: pointer;
		flex-shrink: 0;
		transition: color 0.15s, border-color 0.15s;
	}

	.delete-btn:hover {
		color: #ef4444;
		border-color: #ef4444;
	}

	.empty-text {
		color: var(--text-muted);
		font-size: 0.85rem;
	}
</style>
