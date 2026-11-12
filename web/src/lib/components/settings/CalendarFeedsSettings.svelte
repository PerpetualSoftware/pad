<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import Button from '$lib/components/common/Button.svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { CalendarFeed, CalendarFeedCreateResponse, CalendarFeedLayer, Workspace } from '$lib/types';

	const id = $props.id();
	let feeds = $state<CalendarFeed[]>([]);
	let workspaces = $state<Workspace[]>([]);
	let loading = $state(true);
	let loadError = $state('');
	const atLimit = $derived(feeds.length >= 10);

	let showCreate = $state(false);
	let name = $state('');
	let layer = $state<CalendarFeedLayer>('mine');
	let workspaceChoice = $state<'all' | 'specific'>('all');
	let workspaceIDs = $state<string[]>([]);
	let creating = $state(false);
	let createError = $state('');
	// Retained only for the open dialog, never in a store or the list.
	let createdFeed = $state<CalendarFeedCreateResponse | null>(null);
	let copied = $state(false);
	let feedToRevoke = $state<CalendarFeed | null>(null);
	let revoking = $state(false);
	let revokeError = $state('');

	async function load() {
		loading = true;
		loadError = '';
		try {
			[feeds, workspaces] = await Promise.all([api.calendarFeeds.list(), api.workspaces.list()]);
		} catch (err) {
			loadError = err instanceof Error ? err.message : 'Could not load calendar feeds.';
		} finally {
			loading = false;
		}
	}

	function openCreate() {
		name = '';
		layer = 'mine';
		workspaceChoice = 'all';
		workspaceIDs = [];
		createdFeed = null;
		createError = '';
		copied = false;
		showCreate = true;
	}

	function closeCreate() {
		// A late create response must not retain a secret in a closed dialog.
		if (creating) return;
		createdFeed = null;
		copied = false;
		showCreate = false;
	}

	async function createFeed() {
		if (creating || atLimit || (workspaceChoice === 'specific' && workspaceIDs.length === 0)) return;
		creating = true;
		createError = '';
		try {
			const result = await api.calendarFeeds.create({
				layer,
				workspace_ids: workspaceChoice === 'all' ? [] : [...workspaceIDs],
				...(name.trim() ? { name: name.trim() } : {})
			});
			feeds = [result.feed, ...feeds];
			createdFeed = result;
		} catch (err) {
			createError = err instanceof Error ? err.message : 'Could not create calendar feed.';
		} finally {
			creating = false;
		}
	}

	async function copyCreatedFeed() {
		if (!createdFeed) return;
		copied = await copyToClipboard(createdFeed.url ?? createdFeed.token);
		if (!copied) toastStore.show('Could not copy. Select the link or token and copy it by hand.', 'error');
	}

	function askRevoke(feed: CalendarFeed) {
		feedToRevoke = feed;
		revokeError = '';
	}

	function closeRevoke() {
		if (revoking) return;
		feedToRevoke = null;
		revokeError = '';
	}

	async function revokeFeed() {
		const target = feedToRevoke;
		if (!target || revoking) return;
		revoking = true;
		revokeError = '';
		try {
			await api.calendarFeeds.revoke(target.id);
			feeds = feeds.filter((feed) => feed.id !== target.id);
			feedToRevoke = null;
		} catch (err) {
			revokeError = err instanceof Error ? err.message : 'Could not revoke calendar feed.';
		} finally {
			revoking = false;
		}
	}

	function calendarLabel(feed: CalendarFeed): string {
		return feed.layer === 'mine' ? 'My calendar' : 'Agent calendar';
	}

	function feedLabel(feed: CalendarFeed): string {
		return feed.name || `${calendarLabel(feed)} feed`;
	}

	function workspaceLabel(ids: string[]): string {
		if (ids.length === 0) return 'All workspaces';
		const names: string[] = [];
		let unavailable = 0;
		for (const workspaceID of ids) {
			const workspace = workspaces.find((ws) => ws.id === workspaceID);
			if (workspace) names.push(workspace.name);
			else unavailable++;
		}
		if (unavailable) names.push(`${unavailable} unavailable workspace${unavailable === 1 ? '' : 's'}`);
		return names.join(', ');
	}

	function formatDate(date: string): string {
		return new Date(date).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
	}

	onMount(() => { void load(); });
</script>

<p class="description">Subscribe to your dates in a calendar app.</p>
<Button variant="primary" onclick={openCreate} disabled={loading || !!loadError || atLimit} aria-describedby={atLimit ? `${id}-limit` : undefined}>New feed</Button>
{#if atLimit}
	<p id="{id}-limit" class="description">You have 10 calendar feeds, the most allowed. Revoke one to create another.</p>
{/if}
{#if loading}
	<p class="description" aria-busy="true">Loading calendar feeds...</p>
{:else if loadError}
	<p class="error" role="alert">{loadError}</p>
	<Button onclick={load}>Retry</Button>
{:else if feeds.length === 0}
	<p class="description">No calendar feeds yet.</p>
{:else}
	<ul class="feeds" aria-label="Calendar feeds">
		{#each feeds as feed (feed.id)}
			<li>
				<div class="feed-info">
					<strong>{feedLabel(feed)}</strong>
					<span>{calendarLabel(feed)} · {workspaceLabel(feed.workspace_ids)}</span>
					<span class="metadata">Created {formatDate(feed.created_at)} · Last used {feed.last_used_at ? formatDate(feed.last_used_at) : 'Never'}</span>
				</div>
				<Button variant="danger" size="sm" aria-label="Revoke {feedLabel(feed)}" onclick={() => askRevoke(feed)}>Revoke</Button>
			</li>
		{/each}
	</ul>
{/if}

<Modal open={showCreate} onclose={closeCreate} labelledby="{id}-create-title" placement="center" maxWidth="540px">
	<div class="dialog-content">
		<h3 id="{id}-create-title">{createdFeed ? 'Calendar feed created' : 'Create calendar feed'}</h3>
		{#if createdFeed}
			<div class="secret-row">
				<code>{createdFeed.url ?? createdFeed.token}</code>
				<Button onclick={copyCreatedFeed}>{copied ? 'Copied' : 'Copy'}</Button>
			</div>
			{#if !createdFeed.url}
				<p class="description">The server has no public base URL configured, so the full link can't be built. Copy the token now.</p>
			{/if}
			<p class="warning">This link is a secret. Your calendar provider (for example Google) stores it, and anyone who has it can see these titles. Revoke it if it leaks.</p>
			<p class="warning">This link is shown only once. Closing this dialog loses it.</p>
			<div class="actions"><Button variant="primary" onclick={closeCreate}>Done</Button></div>
		{:else}
			<form onsubmit={(event) => { event.preventDefault(); void createFeed(); }}>
				<label for="{id}-name">Name (optional)</label>
				<input id="{id}-name" type="text" maxlength="100" bind:value={name} disabled={creating} />
				<fieldset disabled={creating}>
					<legend>Calendar</legend>
					<label class="option"><input type="radio" name="{id}-layer" value="mine" bind:group={layer} /> My calendar</label>
					<label class="option"><input type="radio" name="{id}-layer" value="agent" bind:group={layer} /> Agent calendar</label>
					<p class="description">An agent feed is a separate calendar, so your calendar app can hide it and colour it on its own.</p>
				</fieldset>
				<fieldset disabled={creating}>
					<legend>Workspaces</legend>
					<label class="option"><input type="radio" name="{id}-workspaces" value="all" bind:group={workspaceChoice} /> All workspaces</label>
					<label class="option"><input type="radio" name="{id}-workspaces" value="specific" bind:group={workspaceChoice} /> Specific workspaces</label>
					{#if workspaceChoice === 'specific'}
						<div class="workspace-options">
							{#each workspaces as workspace (workspace.id)}
								<label class="option"><input type="checkbox" value={workspace.id} bind:group={workspaceIDs} /> {workspace.name}</label>
							{:else}
								<p class="description">No workspaces available.</p>
							{/each}
						</div>
					{/if}
				</fieldset>
				{#if createError}<p class="error" role="alert">{createError}</p>{/if}
				<div class="actions">
					<Button onclick={closeCreate} disabled={creating}>Cancel</Button>
					<Button variant="primary" type="submit" disabled={creating || atLimit || (workspaceChoice === 'specific' && workspaceIDs.length === 0)}>{creating ? 'Creating...' : 'Create feed'}</Button>
				</div>
			</form>
		{/if}
	</div>
</Modal>

<Modal open={!!feedToRevoke} onclose={closeRevoke} labelledby="{id}-revoke-title" placement="center" maxWidth="420px">
	{#if feedToRevoke}
		<div class="dialog-content">
			<h3 id="{id}-revoke-title">Revoke the feed “{feedLabel(feedToRevoke)}”?</h3>
			<p class="description">Calendar apps using this link lose access immediately.</p>
			{#if revokeError}<p class="error" role="alert">{revokeError}</p>{/if}
			<div class="actions">
				<Button onclick={closeRevoke} disabled={revoking}>Cancel</Button>
				<Button variant="danger" onclick={revokeFeed} disabled={revoking}>{revoking ? 'Revoking...' : 'Revoke feed'}</Button>
			</div>
		</div>
	{/if}
</Modal>

<style>
	.description {
		color: var(--text-secondary);
		margin: var(--space-3) 0;
	}

	.feeds {
		list-style: none;
		padding: 0;
		margin: var(--space-4) 0 0;
	}

	.feeds li {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-3) 0;
		border-top: 1px solid var(--border);
	}

	.feed-info {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		min-width: 0;
		overflow-wrap: anywhere;
	}

	.metadata {
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.dialog-content {
		padding: var(--space-5);
		overflow-y: auto;
	}

	h3 {
		margin: 0 0 var(--space-4);
		font-size: 1.1rem;
	}

	input[type='text'] {
		display: block;
		box-sizing: border-box;
		width: 100%;
		margin-top: var(--space-2);
		padding: var(--space-2);
		background: var(--bg-primary);
		color: var(--text-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}

	fieldset {
		border: 0;
		padding: 0;
		margin: var(--space-4) 0;
		min-width: 0;
	}

	legend {
		font-weight: 600;
		margin-bottom: var(--space-2);
	}

	.option {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin: var(--space-2) 0;
	}

	.workspace-options {
		max-height: 200px;
		overflow-y: auto;
		padding-left: var(--space-4);
	}

	.secret-row {
		display: flex;
		align-items: flex-start;
		gap: var(--space-2);
	}

	code {
		flex: 1;
		min-width: 0;
		overflow-wrap: anywhere;
		user-select: all;
		padding: var(--space-2);
		background: var(--bg-primary);
		border-radius: var(--radius);
	}

	.warning {
		margin-top: var(--space-3);
		color: var(--text-primary);
	}

	.error {
		color: var(--accent-red);
		margin-top: var(--space-3);
	}

	.actions {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
		margin-top: var(--space-4);
	}

</style>
