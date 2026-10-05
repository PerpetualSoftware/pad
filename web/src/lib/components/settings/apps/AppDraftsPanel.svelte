<script lang="ts">
	import { onMount, tick } from 'svelte';
	import { page } from '$app/state';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import Chip from '$lib/components/common/Chip.svelte';
	import type { AppInstallArtifact } from '$lib/types';

	/**
	 * The playbooks and conventions an app's pack provisioned (SPEC-6 U9b,
	 * TASK-3413; DOC-3371 §2 step 7). They arrive as drafts, and activating
	 * each is a separate owner action: never the app's, never automatic. The
	 * parent remounts this panel (via its key) after an upgrade.
	 */
	interface Props {
		wsSlug: string;
		installId: string;
	}
	let { wsSlug, installId }: Props = $props();

	let artifacts = $state<AppInstallArtifact[] | null>(null);
	let loadError = $state('');
	let activating = $state<string | null>(null);
	let rowError = $state<{ id: string; message: string } | null>(null);
	/** Announced after an activation (the button that had focus is gone). */
	let announcement = $state('');
	let listEl: HTMLUListElement | undefined = $state();

	let username = $derived(page.params.username ?? '');

	async function load() {
		const asked = authStore.identityEpoch;
		const ws = wsSlug;
		loadError = '';
		try {
			const r = await api.apps.get(ws, installId);
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			artifacts = r.artifacts ?? [];
		} catch (e) {
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			loadError = e instanceof Error ? e.message : 'Could not load the app’s playbooks and conventions';
		}
	}

	onMount(load);

	async function activate(a: AppInstallArtifact) {
		const asked = authStore.identityEpoch;
		const ws = wsSlug;
		activating = a.item_id;
		rowError = null;
		try {
			// By id, never slug: a slug can be reused by another item, and the
			// item routes resolve an id first (codex U9b r1).
			await api.items.update(ws, a.item_id, { fields_patch: { status: 'active' } });
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			await load();
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			announcement = `${a.title} is now active.`;
			// Its Activate button is gone: land on the same row's link.
			await tick();
			listEl?.querySelector<HTMLAnchorElement>(`[data-item="${a.item_id}"]`)?.focus();
		} catch (e) {
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			rowError = { id: a.item_id, message: e instanceof Error ? e.message : 'Could not activate it' };
		} finally {
			if (authStore.identityEpoch === asked) activating = null;
		}
	}
</script>

<section class="drafts" aria-label="Playbooks and conventions from this app">
	<h4>Playbooks and conventions</h4>
	<p class="hint">They arrive as drafts. Nothing runs until you activate it, and the app cannot activate them itself.</p>
	<p class="sr-only" role="status">{announcement}</p>
	{#if loadError}
		<p class="error" role="alert">{loadError}</p>
		<Button size="sm" onclick={load}>Try again</Button>
	{:else if artifacts === null}
		<p class="hint">Loading…</p>
	{:else if artifacts.length === 0}
		<p class="hint">This app added none.</p>
	{:else}
		<ul bind:this={listEl}>
			{#each artifacts as a (a.item_id)}
				<li data-testid="app-artifact-row">
					<a data-item={a.item_id} href="/{username}/{wsSlug}/{a.collection_slug}/{a.ref ?? a.slug}">{a.title}</a>
					{#if a.ref}<span class="mono">{a.ref}</span>{/if}
					<Chip size="sm" color={a.status === 'draft' ? 'var(--accent-amber)' : 'var(--accent-green)'}>{a.status ?? 'unknown'}</Chip>
					<span class="hint">from v{a.version}</span>
					{#if a.status === 'draft'}
						<Button size="sm" disabled={activating !== null} onclick={() => activate(a)}>
							{activating === a.item_id ? 'Activating…' : `Activate ${a.title}`}
						</Button>
					{/if}
					{#if rowError?.id === a.item_id}
						<p class="error" role="alert">{rowError.message}</p>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	.drafts {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		width: 100%;
	}
	h4 {
		margin: 0;
		font-size: 0.95em;
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	li {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		align-items: center;
		font-size: 0.9em;
	}
	.hint {
		margin: 0;
		font-size: 0.85em;
		color: var(--text-secondary);
	}
	.mono {
		font-family: var(--font-mono, monospace);
		font-size: 0.85em;
		color: var(--text-secondary);
	}
	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		overflow: hidden;
		clip: rect(0 0 0 0);
		white-space: nowrap;
	}
	.error {
		margin: 0;
		width: 100%;
		color: var(--accent-red);
		font-size: 0.85em;
	}
</style>
