<script lang="ts">
	import { api } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { CollectionGrant, ItemGrant } from '$lib/types';

	interface Props {
		wsSlug: string;
		type: 'collection' | 'item';
		targetSlug: string;
		targetName: string;
		open: boolean;
	}

	let { wsSlug, type, targetSlug, targetName, open = $bindable() }: Props = $props();

	let grants = $state<(CollectionGrant | ItemGrant)[]>([]);
	let loadingGrants = $state(false);
	let loadError = $state('');

	let email = $state('');
	let permission = $state('view');
	let sharing = $state(false);
	let shareError = $state('');

	let revokingId = $state<string | null>(null);

	// Track previous open state to detect open transitions
	let prevOpen = $state(false);

	$effect.pre(() => {
		if (open && !prevOpen) {
			// Reset form state on open
			email = '';
			permission = 'view';
			shareError = '';
			loadGrants();
		}
		prevOpen = open;
	});

	async function loadGrants() {
		loadingGrants = true;
		loadError = '';
		try {
			if (type === 'collection') {
				grants = await api.grants.listCollectionGrants(wsSlug, targetSlug);
			} else {
				grants = await api.grants.listItemGrants(wsSlug, targetSlug);
			}
		} catch (e: any) {
			loadError = e.message ?? 'Failed to load grants';
			grants = [];
		} finally {
			loadingGrants = false;
		}
	}

	async function handleShare() {
		const trimmed = email.trim();
		if (!trimmed || sharing) return;
		sharing = true;
		shareError = '';
		try {
			let newGrant: CollectionGrant | ItemGrant;
			if (type === 'collection') {
				newGrant = await api.grants.createCollectionGrant(wsSlug, targetSlug, trimmed, permission);
			} else {
				newGrant = await api.grants.createItemGrant(wsSlug, targetSlug, trimmed, permission);
			}
			grants = [...grants, newGrant];
			email = '';
			permission = 'view';
			toastStore.show(`Shared with ${trimmed}`, 'success');
		} catch (e: any) {
			shareError = e.message ?? 'Failed to share';
		} finally {
			sharing = false;
		}
	}

	async function handleRevoke(grantId: string) {
		if (revokingId) return;
		revokingId = grantId;
		try {
			if (type === 'collection') {
				await api.grants.deleteCollectionGrant(wsSlug, targetSlug, grantId);
			} else {
				await api.grants.deleteItemGrant(wsSlug, targetSlug, grantId);
			}
			grants = grants.filter((g) => g.id !== grantId);
			toastStore.show('Access revoked', 'success');
		} catch (e: any) {
			toastStore.show(e.message ?? 'Failed to revoke access', 'error');
		} finally {
			revokingId = null;
		}
	}

	function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape' && open) {
			open = false;
		}
	}

	function handleShareKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' && email.trim()) {
			e.preventDefault();
			handleShare();
		}
	}

	function formatPermission(perm: string): string {
		return perm === 'edit' ? 'Can edit' : 'Can view';
	}

	function grantDisplayName(grant: CollectionGrant | ItemGrant): string {
		if (grant.user_name) return grant.user_name;
		if (grant.user_email) return grant.user_email;
		return grant.user_id;
	}

	function grantDisplayEmail(grant: CollectionGrant | ItemGrant): string | null {
		if (grant.user_name && grant.user_email) return grant.user_email;
		return null;
	}
</script>

<svelte:window onkeydown={handleKeydown} />

{#if open}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="overlay" onclick={() => (open = false)}>
		<div class="modal" onclick={(e) => e.stopPropagation()}>
			<div class="modal-header">
				<h2>Share {targetName}</h2>
				<button class="close-btn" type="button" onclick={() => (open = false)}>&#10005;</button>
			</div>

			<div class="modal-body">
				<!-- Add people section -->
				<div class="add-section">
					<span class="section-label">Add people</span>
					<div class="add-row">
						<input
							class="email-input"
							type="email"
							placeholder="Email address"
							bind:value={email}
							onkeydown={handleShareKeydown}
							disabled={sharing}
						/>
						<select class="permission-select" bind:value={permission} disabled={sharing}>
							<option value="view">Can view</option>
							<option value="edit">Can edit</option>
						</select>
						<button
							class="share-btn"
							type="button"
							onclick={handleShare}
							disabled={!email.trim() || sharing}
						>
							{sharing ? 'Sharing...' : 'Share'}
						</button>
					</div>
					{#if shareError}
						<div class="error-msg">{shareError}</div>
					{/if}
				</div>

				<!-- Current grants -->
				<div class="grants-section">
					<span class="section-label">People with access</span>

					{#if loadingGrants}
						<div class="grants-loading">Loading...</div>
					{:else if loadError}
						<div class="error-msg">{loadError}</div>
					{:else if grants.length === 0}
						<div class="grants-empty">No one has been granted access yet.</div>
					{:else}
						<div class="grants-list">
							{#each grants as grant (grant.id)}
								<div class="grant-row">
									<div class="grant-user">
										<span class="grant-name">{grantDisplayName(grant)}</span>
										{#if grantDisplayEmail(grant)}
											<span class="grant-email">{grantDisplayEmail(grant)}</span>
										{/if}
									</div>
									<div class="grant-actions">
										<span class="permission-badge">{formatPermission(grant.permission)}</span>
										<button
											class="revoke-btn"
											type="button"
											title="Revoke access"
											onclick={() => handleRevoke(grant.id)}
											disabled={revokingId === grant.id}
										>
											{revokingId === grant.id ? '...' : '\u00D7'}
										</button>
									</div>
								</div>
							{/each}
						</div>
					{/if}
				</div>
			</div>
		</div>
	</div>
{/if}

<style>
	.overlay {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		z-index: 50;
		display: flex;
		justify-content: center;
		align-items: flex-start;
		padding-top: 10vh;
	}

	.modal {
		width: 100%;
		max-width: 480px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 20px 60px rgba(0, 0, 0, 0.5);
		overflow: hidden;
		max-height: 80vh;
		display: flex;
		flex-direction: column;
	}

	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}

	.modal-header h2 {
		margin: 0;
		font-size: 1.1em;
		font-weight: 600;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.close-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 1em;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		line-height: 1;
		flex-shrink: 0;
	}

	.close-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.modal-body {
		padding: var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
		overflow-y: auto;
	}

	.section-label {
		display: block;
		font-size: 0.75em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		margin-bottom: var(--space-2);
	}

	/* Add people */
	.add-row {
		display: flex;
		gap: var(--space-2);
		align-items: center;
	}

	.email-input {
		flex: 1;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 0.9em;
		color: var(--text-primary);
		min-width: 0;
	}

	.email-input:hover {
		border-color: var(--border);
	}

	.email-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.email-input::placeholder {
		color: var(--text-muted);
	}

	.permission-select {
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 0.85em;
		color: var(--text-primary);
		cursor: pointer;
		flex-shrink: 0;
	}

	.permission-select:hover {
		border-color: var(--border);
	}

	.permission-select:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.share-btn {
		padding: var(--space-2) var(--space-4);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.85em;
		font-weight: 500;
		cursor: pointer;
		white-space: nowrap;
		flex-shrink: 0;
	}

	.share-btn:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.share-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.error-msg {
		margin-top: var(--space-2);
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 12%, transparent);
		color: var(--accent-red, #ef4444);
		border-radius: var(--radius);
		font-size: 0.82em;
	}

	/* Grants list */
	.grants-loading,
	.grants-empty {
		font-size: 0.85em;
		color: var(--text-muted);
		padding: var(--space-3) 0;
	}

	.grants-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.grant-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
	}

	.grant-user {
		display: flex;
		flex-direction: column;
		gap: 1px;
		min-width: 0;
	}

	.grant-name {
		font-size: 0.9em;
		font-weight: 500;
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.grant-email {
		font-size: 0.78em;
		color: var(--text-muted);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.grant-actions {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-shrink: 0;
	}

	.permission-badge {
		font-size: 0.75em;
		font-weight: 500;
		color: var(--text-secondary);
		background: var(--bg-secondary);
		padding: 2px 8px;
		border-radius: 999px;
		white-space: nowrap;
	}

	.revoke-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 24px;
		height: 24px;
		background: none;
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		cursor: pointer;
		font-size: 1.1em;
		line-height: 1;
		padding: 0;
	}

	.revoke-btn:hover:not(:disabled) {
		color: var(--accent-red, #ef4444);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
		border-color: color-mix(in srgb, var(--accent-red, #ef4444) 20%, transparent);
	}

	.revoke-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	@media (max-width: 480px) {
		.add-row {
			flex-wrap: wrap;
		}

		.email-input {
			flex: 1 1 100%;
		}
	}
</style>
