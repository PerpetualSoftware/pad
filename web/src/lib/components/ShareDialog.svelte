<script lang="ts">
	import { api } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { CollectionGrant, ItemGrant, ShareLink } from '$lib/types';

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

	// Share links state
	let shareLinks = $state<ShareLink[]>([]);
	let loadingLinks = $state(false);
	let linksError = $state('');
	let creatingLink = $state(false);
	let newlyCreatedLinkId = $state<string | null>(null);
	let deletingLinkId = $state<string | null>(null);

	// Track previous open state to detect open transitions
	let prevOpen = $state(false);

	$effect.pre(() => {
		if (open && !prevOpen) {
			// Reset form state on open
			email = '';
			permission = 'view';
			shareError = '';
			newlyCreatedLinkId = null;
			loadGrants();
			loadShareLinks();
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

	async function loadShareLinks() {
		loadingLinks = true;
		linksError = '';
		try {
			if (type === 'collection') {
				shareLinks = await api.shareLinks.listCollectionShareLinks(wsSlug, targetSlug);
			} else {
				shareLinks = await api.shareLinks.listItemShareLinks(wsSlug, targetSlug);
			}
		} catch (e: any) {
			linksError = e.message ?? 'Failed to load share links';
			shareLinks = [];
		} finally {
			loadingLinks = false;
		}
	}

	async function handleCreateShareLink() {
		if (creatingLink) return;
		creatingLink = true;
		try {
			let newLink: ShareLink;
			if (type === 'collection') {
				newLink = await api.shareLinks.createCollectionShareLink(wsSlug, targetSlug);
			} else {
				newLink = await api.shareLinks.createItemShareLink(wsSlug, targetSlug);
			}
			shareLinks = [newLink, ...shareLinks];
			newlyCreatedLinkId = newLink.id;
			toastStore.show('Share link created', 'success');
		} catch (e: any) {
			toastStore.show(e.message ?? 'Failed to create share link', 'error');
		} finally {
			creatingLink = false;
		}
	}

	async function handleDeleteShareLink(linkId: string) {
		if (deletingLinkId) return;
		deletingLinkId = linkId;
		try {
			await api.shareLinks.deleteShareLink(wsSlug, linkId);
			shareLinks = shareLinks.filter((l) => l.id !== linkId);
			if (newlyCreatedLinkId === linkId) {
				newlyCreatedLinkId = null;
			}
			toastStore.show('Share link revoked', 'success');
		} catch (e: any) {
			toastStore.show(e.message ?? 'Failed to revoke share link', 'error');
		} finally {
			deletingLinkId = null;
		}
	}

	function getShareLinkUrl(link: ShareLink): string {
		if (link.url) return link.url;
		if (link.token) {
			const origin = typeof window !== 'undefined' ? window.location.origin : '';
			return `${origin}/s/${link.token}`;
		}
		return '';
	}

	async function copyToClipboard(text: string) {
		try {
			await navigator.clipboard.writeText(text);
			toastStore.show('Link copied to clipboard', 'success');
		} catch {
			toastStore.show('Failed to copy link', 'error');
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

	function formatDate(dateStr: string): string {
		return new Date(dateStr).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
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

				<!-- Share links section -->
				<div class="share-links-section">
					<div class="share-links-header">
						<span class="section-label">Share links</span>
						<button
							class="create-link-btn"
							type="button"
							onclick={handleCreateShareLink}
							disabled={creatingLink}
						>
							{creatingLink ? 'Creating...' : '+ Create link'}
						</button>
					</div>

					{#if loadingLinks}
						<div class="grants-loading">Loading...</div>
					{:else if linksError}
						<div class="error-msg">{linksError}</div>
					{:else if shareLinks.length === 0}
						<div class="grants-empty">No share links yet. Create one to share via URL.</div>
					{:else}
						<div class="share-links-list">
							{#each shareLinks as link (link.id)}
								{@const linkUrl = getShareLinkUrl(link)}
								<div class="share-link-row" class:newly-created={link.id === newlyCreatedLinkId}>
									{#if link.id === newlyCreatedLinkId && linkUrl}
										<div class="new-link-highlight">
											<span class="new-link-label">Link created -- copy it now, the token is only shown once:</span>
											<div class="new-link-url-row">
												<input
													class="new-link-url"
													type="text"
													readonly
													value={linkUrl}
													onclick={(e) => (e.target as HTMLInputElement).select()}
												/>
												<button
													class="copy-btn"
													type="button"
													onclick={() => copyToClipboard(linkUrl)}
													title="Copy link"
												>
													Copy
												</button>
											</div>
										</div>
									{:else}
										<div class="link-info">
											<div class="link-url-display">
												{#if linkUrl}
													<span class="link-url-text">{linkUrl}</span>
													<button
														class="copy-btn-small"
														type="button"
														onclick={() => copyToClipboard(linkUrl)}
														title="Copy link"
													>
														<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
															<rect x="9" y="9" width="13" height="13" rx="2" ry="2"/>
															<path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>
														</svg>
													</button>
												{:else}
													<span class="link-url-text link-url-hidden">Link token hidden</span>
												{/if}
											</div>
											<div class="link-meta">
												<span>{link.view_count} view{link.view_count !== 1 ? 's' : ''}</span>
												<span class="link-meta-sep">&middot;</span>
												<span>Created {formatDate(link.created_at)}</span>
												{#if link.require_auth}
													<span class="link-meta-sep">&middot;</span>
													<span class="link-auth-badge">Auth required</span>
												{/if}
											</div>
										</div>
									{/if}
									<button
										class="revoke-btn"
										type="button"
										title="Revoke share link"
										onclick={() => handleDeleteShareLink(link.id)}
										disabled={deletingLinkId === link.id}
									>
										{deletingLinkId === link.id ? '...' : '\u00D7'}
									</button>
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

	/* Share links section */
	.share-links-section {
		border-top: 1px solid var(--border);
		padding-top: var(--space-4);
	}

	.share-links-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		margin-bottom: var(--space-2);
	}

	.share-links-header .section-label {
		margin-bottom: 0;
	}

	.create-link-btn {
		padding: var(--space-1) var(--space-3);
		background: none;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.8em;
		font-weight: 500;
		cursor: pointer;
		white-space: nowrap;
	}

	.create-link-btn:hover:not(:disabled) {
		border-color: var(--accent-blue);
		color: var(--accent-blue);
	}

	.create-link-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.share-links-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.share-link-row {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: var(--space-2);
		padding: var(--space-3);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		border: 1px solid transparent;
	}

	.share-link-row.newly-created {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 6%, var(--bg-tertiary));
	}

	/* Newly created link highlight */
	.new-link-highlight {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.new-link-label {
		font-size: 0.78em;
		font-weight: 600;
		color: var(--accent-blue);
	}

	.new-link-url-row {
		display: flex;
		gap: var(--space-2);
		align-items: center;
	}

	.new-link-url {
		flex: 1;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		font-size: 0.82em;
		font-family: var(--font-mono);
		color: var(--text-primary);
		min-width: 0;
	}

	.new-link-url:focus {
		outline: none;
		border-color: var(--accent-blue);
	}

	.copy-btn {
		padding: var(--space-2) var(--space-3);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.82em;
		font-weight: 500;
		cursor: pointer;
		white-space: nowrap;
		flex-shrink: 0;
	}

	.copy-btn:hover {
		filter: brightness(1.1);
	}

	/* Existing link info */
	.link-info {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: 3px;
	}

	.link-url-display {
		display: flex;
		align-items: center;
		gap: var(--space-1);
	}

	.link-url-text {
		font-size: 0.82em;
		font-family: var(--font-mono);
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		min-width: 0;
	}

	.link-url-hidden {
		color: var(--text-muted);
		font-family: var(--font-ui);
		font-style: italic;
	}

	.copy-btn-small {
		display: flex;
		align-items: center;
		justify-content: center;
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		padding: 2px;
		border-radius: var(--radius-sm);
		flex-shrink: 0;
	}

	.copy-btn-small:hover {
		color: var(--accent-blue);
		background: var(--bg-hover);
	}

	.link-meta {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		font-size: 0.75em;
		color: var(--text-muted);
		flex-wrap: wrap;
	}

	.link-meta-sep {
		color: var(--border);
	}

	.link-auth-badge {
		color: var(--accent-amber);
	}

	@media (max-width: 480px) {
		.add-row {
			flex-wrap: wrap;
		}

		.email-input {
			flex: 1 1 100%;
		}

		.new-link-url-row {
			flex-wrap: wrap;
		}

		.new-link-url {
			flex: 1 1 100%;
		}
	}
</style>
