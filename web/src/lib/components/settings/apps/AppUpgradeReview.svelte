<script lang="ts">
	import { onMount } from 'svelte';
	import { api, PadApiError } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import AppArtifactReview from './AppArtifactReview.svelte';
	import type { AppInstallPreview, AppInstallSummary, AppUpgradeDiffEntry, AppUpgradeConfirmResult } from '$lib/types';

	/**
	 * Upgrading an install (SPEC-6 U9b, TASK-3413; DOC-3371 §2 "Upgrade",
	 * U8b2): Pad fetches the app's current manifest and shows what would
	 * change, split into what needs the owner's review and what applies on its
	 * own. Every upgrade, removals-only included, needs this confirm.
	 */
	interface Props {
		wsSlug: string;
		install: AppInstallSummary;
		/** The upgrade landed: the parent re-reads the install. */
		onupgraded: () => void;
		onclose: () => void;
	}
	let { wsSlug, install, onupgraded, onclose }: Props = $props();

	let preview = $state<AppInstallPreview | null>(null);
	let busy = $state(true);
	let error = $state('');
	let errorPath = $state('');
	/** The review no longer stands (stale, moved, mismatched): offer a fresh one. */
	let canRetry = $state(false);
	let result = $state<AppUpgradeConfirmResult | null>(null);
	let heading: HTMLHeadingElement | undefined = $state();

	const kindLabel: Record<string, string> = {
		app: 'App details',
		access: 'Access',
		url: 'Address',
		event: 'Event',
		item_action: 'Item action',
		collection: 'Collection',
		field: 'Field',
		artifact: 'Playbook or convention',
		config: 'Settings'
	};
	const changeLabel: Record<string, string> = {
		added: 'added',
		removed: 'removed',
		changed: 'changed',
		widened: 'widened: more access',
		narrowed: 'narrowed',
		released: 'released to the workspace (data kept)'
	};

	let entries = $derived(preview?.upgrade?.diff ?? []);
	let toReview = $derived(entries.filter((e) => e.class === 'review'));
	let automatic = $derived(entries.filter((e) => e.class !== 'review'));
	// Artifacts the diff names as added or changed: they arrive as new drafts.
	let newDrafts = $derived.by(() => {
		const keys = new Set(
			entries.filter((e) => e.kind === 'artifact' && (e.change === 'added' || e.change === 'changed')).map((e) => e.key)
		);
		return (preview?.artifacts ?? []).filter((a) => keys.has(a.key));
	});

	function fail(e: unknown) {
		error = e instanceof Error ? e.message : 'Something went wrong';
		const path = e instanceof PadApiError ? (e.details as { path?: unknown } | undefined)?.path : undefined;
		errorPath = typeof path === 'string' ? path : '';
	}

	async function load() {
		const asked = authStore.identityEpoch;
		const ws = wsSlug;
		busy = true;
		error = '';
		errorPath = '';
		canRetry = false;
		preview = null;
		try {
			const p = await api.apps.upgradePreview(ws, install.install_id);
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			preview = p;
		} catch (e) {
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			fail(e);
		} finally {
			if (authStore.identityEpoch === asked) busy = false;
		}
		heading?.focus();
	}

	onMount(load);

	async function confirm() {
		if (!preview) return;
		const asked = authStore.identityEpoch;
		const ws = wsSlug;
		const reviewed = preview;
		busy = true;
		error = '';
		errorPath = '';
		try {
			const r = await api.apps.upgradeConfirm(ws, install.install_id, reviewed.pending_id, reviewed.manifest_sha256);
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			result = r;
			preview = null;
			onupgraded();
		} catch (e) {
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			fail(e);
			if (
				e instanceof PadApiError &&
				['install_review_stale', 'install_moved', 'install_review_mismatch', 'install_conflict', 'not_found'].includes(e.code)
			) {
				// Nothing was written; this review no longer describes what
				// would happen. Preview again.
				preview = null;
				canRetry = true;
			}
		} finally {
			if (authStore.identityEpoch === asked) busy = false;
		}
		heading?.focus();
	}

	async function cancel() {
		const p = preview;
		preview = null;
		if (p) {
			try {
				await api.apps.discardPending(wsSlug, p.pending_id);
			} catch {
				/* expires on its own */
			}
		}
		onclose();
	}

	function entryText(e: AppUpgradeDiffEntry): string {
		const what = kindLabel[e.kind] ?? e.kind;
		const how = changeLabel[e.change] ?? e.change;
		return `${what} ${e.key ? `“${e.key}” ` : ''}${how}`;
	}
</script>

<section class="upgrade" aria-label="Upgrade {install.app_name}">
	<h3 tabindex="-1" bind:this={heading}>
		{#if result}
			Upgraded {install.app_name} to v{result.version}
		{:else if preview}
			Upgrade {install.app_name}: v{preview.upgrade?.from_version ?? install.version} &rarr; v{preview.version}
		{:else}
			Upgrade {install.app_name}
		{/if}
	</h3>

	{#if busy && !preview && !result}
		<p class="hint" role="status">Fetching the app's current version…</p>
	{/if}

	{#if result}
		{#if result.items.length > 0}
			<p>These arrived as new drafts. Nothing you had activated was changed; activate them below when you are ready.</p>
			<ul>
				{#each result.items as it (it.key)}
					<li class="mono">{it.ref}</li>
				{/each}
			</ul>
		{:else}
			<p>No new drafts.</p>
		{/if}
		<Button size="sm" onclick={onclose}>Done</Button>
	{:else if preview}
		<p class="not-reviewed" role="note">{preview.notice}</p>
		{#if preview.upgrade?.notice}<p class="hint">{preview.upgrade.notice}</p>{/if}

		{#if entries.length === 0}
			<p data-testid="app-upgrade-nothing">Nothing changes: this install already matches the app's published version.</p>
		{:else}
			{#if toReview.length > 0}
				<h4>Needs your review</h4>
				<ul data-testid="app-upgrade-review">
					{#each toReview as e (e.kind + e.key + e.change)}
						<li>{entryText(e)}{#if e.detail}<span class="hint"> — {e.detail}</span>{/if}</li>
					{/each}
				</ul>
			{/if}
			{#if automatic.length > 0}
				<h4>Applies without review</h4>
				<ul data-testid="app-upgrade-auto">
					{#each automatic as e (e.kind + e.key + e.change)}
						<li>{entryText(e)}{#if e.detail}<span class="hint"> — {e.detail}</span>{/if}</li>
					{/each}
				</ul>
			{/if}
			{#each newDrafts as a (a.key)}
				<AppArtifactReview artifact={a} badge="arrives as a new draft" />
			{/each}
		{/if}

		<div class="row">
			{#if entries.length > 0}
				<Button variant="primary" size="sm" disabled={busy} onclick={confirm}>
					{busy ? 'Upgrading…' : `Upgrade to v${preview.version}`}
				</Button>
			{/if}
			<Button size="sm" disabled={busy} onclick={cancel}>{entries.length > 0 ? 'Cancel' : 'Close'}</Button>
		</div>
	{/if}

	{#if error}
		<p class="error" role="alert">
			{error}{#if errorPath}<span class="mono"> ({errorPath})</span>{/if}
		</p>
		<div class="row">
			{#if canRetry}<Button size="sm" disabled={busy} onclick={load}>Preview again</Button>{/if}
			{#if !preview}<Button size="sm" disabled={busy} onclick={onclose}>Back</Button>{/if}
		</div>
	{/if}
</section>

<style>
	.upgrade {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		align-items: stretch;
		width: 100%;
	}
	h3 {
		margin: 0;
		font-size: 1.05em;
	}
	h4 {
		margin: var(--space-3) 0 0;
		font-size: 0.95em;
	}
	ul {
		margin: 0;
		padding-left: var(--space-5);
		font-size: 0.9em;
	}
	p {
		margin: 0;
		font-size: 0.9em;
	}
	.hint {
		color: var(--text-secondary);
		font-size: 0.85em;
	}
	.not-reviewed {
		padding: var(--space-2) var(--space-3);
		border-left: 3px solid var(--accent-amber);
		background: var(--bg-secondary);
	}
	.mono {
		font-family: var(--font-mono, monospace);
		font-size: 0.9em;
		overflow-wrap: anywhere;
	}
	.row {
		display: flex;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	.error {
		color: var(--accent-red);
	}
</style>
