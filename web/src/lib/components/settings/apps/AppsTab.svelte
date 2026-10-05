<script lang="ts">
	import { api } from '$lib/api/client';
	import { onMount, tick } from 'svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import Chip from '$lib/components/common/Chip.svelte';
	import AppInstallFlow from './AppInstallFlow.svelte';
	import AppInstallDetail from './AppInstallDetail.svelte';
	import { appStateLabel, appStateColor } from './appState';
	import { normalizeInstallList } from './appsShape';
	import type { AppInstallList } from '$lib/types';

	/**
	 * Settings → Apps (SPEC-6 U9a, TASK-3413): the workspace's installed
	 * apps, owner-only. The parent renders it only for an owner; the server
	 * refuses anyone else regardless.
	 */
	interface Props {
		wsSlug: string;
	}
	let { wsSlug }: Props = $props();

	let list = $state<AppInstallList | null>(null);
	let loadError = $state('');
	let installing = $state(false);
	let selectedId = $state<string | null>(null);
	let root: HTMLDivElement | undefined = $state();

	/** Leave the detail and return focus to the card the owner opened. */
	async function back() {
		const was = selectedId;
		selectedId = null;
		await tick();
		root?.querySelector<HTMLButtonElement>(`[data-install="${was}"]`)?.focus();
	}

	// Which (identity, workspace) the loaded list describes; a load that
	// answers for another is dropped.
	let loadGen = 0;

	async function load() {
		const gen = ++loadGen;
		const asked = authStore.identityEpoch;
		const ws = wsSlug;
		loadError = '';
		try {
			const r = await api.apps.list(ws);
			if (gen !== loadGen || authStore.identityEpoch !== asked) return;
			list = normalizeInstallList(r);
		} catch (e) {
			if (gen !== loadGen || authStore.identityEpoch !== asked) return;
			loadError = e instanceof Error ? e.message : 'Could not load apps';
		}
	}

	// The parent keys this component on (workspace, identity), so a switch
	// remounts it with nothing of the old view left; one load per mount.
	onMount(load);

	let live = $derived(list?.installs.filter((i) => i.state !== 'uninstalled') ?? []);
	let tombstones = $derived(list?.installs.filter((i) => i.state === 'uninstalled') ?? []);
	let selected = $derived(list?.installs.find((i) => i.install_id === selectedId) ?? null);
</script>

<div class="apps-tab" bind:this={root}>
	{#if loadError}
		<p class="error" role="alert">{loadError}</p>
		<Button size="sm" onclick={load}>Try again</Button>
	{:else if !list}
		<p class="hint">Loading…</p>
	{:else if !list.available}
		<div class="off" data-testid="apps-unavailable">
			<strong>Apps are turned off on this server</strong>
			<p>
				{#if list.cloud}
					Apps are not available here right now.
				{:else}
					A server admin has to enable apps before a workspace can install them. Ask your admin to turn them on in the
					admin settings.
				{/if}
			</p>
		</div>
	{:else if installing}
		<AppInstallFlow {wsSlug} oninstalled={load} onclose={() => (installing = false)} />
	{:else if selected}
		<AppInstallDetail {wsSlug} install={selected} onchanged={load} onback={back} />
	{:else}
		<p class="hint">
			Apps connect other tools to this workspace. Each one gets its own collections and acts as its own member, and
			you can disable or uninstall it at any time.
		</p>
		{#if live.length === 0}
			<p class="empty">No apps installed.</p>
		{:else}
			<ul class="list">
				{#each live as app (app.install_id)}
					<li>
						<button class="card" data-install={app.install_id} onclick={() => (selectedId = app.install_id)}>
							<span class="name">{app.app_name}</span>
							<Chip size="sm" color={appStateColor(app.state)}>{appStateLabel(app.state)}</Chip>
							<span class="origin">{app.origin}</span>
							{#if app.webhook?.status === 'awaiting_secret'}
								<span class="flag">Webhook waiting for an install code</span>
							{/if}
							{#if app.webhook && app.webhook.undelivered_dropped > 0}
								<span class="flag">{app.webhook.undelivered_dropped} dropped</span>
							{/if}
						</button>
					</li>
				{/each}
			</ul>
		{/if}
		<Button variant="primary" size="sm" onclick={() => (installing = true)}>Install an app</Button>
		{#if tombstones.length > 0}
			<details class="tombstones">
				<summary>Uninstalled ({tombstones.length})</summary>
				<ul class="list">
					{#each tombstones as app (app.install_id)}
						<li class="tomb">
							<span class="name">{app.app_name}</span>
							<span class="origin">{app.origin}</span>
						</li>
					{/each}
				</ul>
			</details>
		{/if}
	{/if}
</div>

<style>
	.apps-tab {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		align-items: flex-start;
	}
	.hint,
	.empty {
		margin: 0;
		font-size: 0.9em;
		color: var(--text-secondary);
	}
	.error {
		margin: 0;
		color: var(--accent-red);
	}
	.off {
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-4);
		background: var(--bg-secondary);
	}
	.off p {
		margin: var(--space-2) 0 0;
		font-size: 0.9em;
		color: var(--text-secondary);
	}
	.list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		width: 100%;
	}
	.card {
		width: 100%;
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		color: var(--text-primary);
		text-align: left;
		cursor: pointer;
	}
	.card:hover {
		background: var(--bg-hover);
	}
	.name {
		font-weight: 600;
	}
	.origin {
		font-family: var(--font-mono, monospace);
		font-size: 0.8em;
		color: var(--text-secondary);
		overflow-wrap: anywhere;
	}
	.flag {
		font-size: 0.8em;
		color: var(--accent-orange);
	}
	.tombstones {
		width: 100%;
		font-size: 0.9em;
	}
	.tomb {
		display: flex;
		gap: var(--space-2);
		flex-wrap: wrap;
		padding: var(--space-1) 0;
		color: var(--text-secondary);
	}
</style>
