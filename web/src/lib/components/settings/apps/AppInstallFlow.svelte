<script lang="ts">
	import { api, PadApiError } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import Chip from '$lib/components/common/Chip.svelte';
	import AppInstallCodePanel from './AppInstallCodePanel.svelte';
	import type { AppInstallCode, AppInstallPreview } from '$lib/types';

	/**
	 * Installing an app (SPEC-6 U9a, TASK-3413; DOC-3371 §2 steps 1-8): the
	 * owner enters its URL, reviews what Pad fetched (raw AND as it will be
	 * stored, with every change the importer made called out), installs, and
	 * hands the one-time install code to the app.
	 */
	interface Props {
		wsSlug: string;
		/** An install landed: the parent re-reads its list. */
		oninstalled: () => void;
		onclose: () => void;
	}
	let { wsSlug, oninstalled, onclose }: Props = $props();

	let baseUrl = $state('');
	let busy = $state(false);
	let error = $state('');
	let errorPath = $state('');
	let preview = $state<AppInstallPreview | null>(null);
	let code = $state<AppInstallCode | null>(null);
	let installedName = $state('');

	function fail(e: unknown) {
		if (e instanceof PadApiError) {
			error = e.message;
			const path = (e.details as { path?: unknown } | undefined)?.path;
			errorPath = typeof path === 'string' ? path : '';
		} else {
			error = e instanceof Error ? e.message : 'Something went wrong';
			errorPath = '';
		}
	}

	async function fetchPreview(e?: SubmitEvent) {
		e?.preventDefault();
		const asked = authStore.identityEpoch;
		const ws = wsSlug;
		busy = true;
		error = '';
		errorPath = '';
		try {
			const p = await api.apps.preview(ws, baseUrl.trim());
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			preview = p;
		} catch (err) {
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			fail(err);
		} finally {
			if (authStore.identityEpoch === asked) busy = false;
		}
	}

	async function install() {
		if (!preview) return;
		const asked = authStore.identityEpoch;
		const ws = wsSlug;
		const reviewed = preview;
		busy = true;
		error = '';
		errorPath = '';
		try {
			// The hash the owner REVIEWED: the server refuses a different manifest.
			const r = await api.apps.confirm(ws, reviewed.pending_id, reviewed.manifest_sha256);
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			installedName = reviewed.title;
			code = { install_id: r.install_id, install_code: r.install_code, expires_at: r.expires_at, notice: r.notice };
			preview = null;
			oninstalled();
		} catch (err) {
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			fail(err);
			if (err instanceof PadApiError && (err.code === 'install_review_mismatch' || err.code === 'install_conflict' || err.code === 'not_found')) {
				// The reviewed preview no longer stands: review again.
				preview = null;
			}
		} finally {
			if (authStore.identityEpoch === asked) busy = false;
		}
	}

	async function cancel() {
		const p = preview;
		preview = null;
		if (p) {
			// Discard the staged install rather than leave it holding a pending
			// slot for an hour. A failure here is harmless: it expires.
			try {
				await api.apps.discardPending(wsSlug, p.pending_id);
			} catch {
				/* expires on its own */
			}
		}
		onclose();
	}

	function accessLabel(a: string): string {
		if (a === 'write') return 'read and write';
		if (a === 'read') return 'read only';
		if (!a || a === 'none') return 'none';
		return a;
	}
</script>

<div class="flow">
	{#if code}
		<AppInstallCodePanel {code} appName={installedName} ondone={onclose} />
	{:else if !preview}
		<form class="url-form" onsubmit={fetchPreview}>
			<label for="app-base-url">App URL</label>
			<p class="hint">The address the app is published at. Pad fetches its manifest and shows you everything before anything is installed.</p>
			<div class="row">
				<input
					id="app-base-url"
					type="url"
					placeholder="https://portal.example.com"
					bind:value={baseUrl}
					required
					disabled={busy}
					autocomplete="off"
				/>
				<Button type="submit" variant="primary" size="sm" disabled={busy || !baseUrl.trim()}>
					{busy ? 'Fetching…' : 'Review'}
				</Button>
				<Button type="button" size="sm" disabled={busy} onclick={onclose}>Cancel</Button>
			</div>
		</form>
	{:else}
		<section class="review" aria-label="Review {preview.title}">
			<header>
				<h3>{preview.title} <span class="version">v{preview.version}</span></h3>
				<p class="by">by {preview.publisher} &middot; <span class="mono">{preview.origin}</span></p>
				{#if preview.description}<p>{preview.description}</p>{/if}
				{#if preview.homepage}
					<a href={preview.homepage} target="_blank" rel="noopener noreferrer">{preview.homepage}</a>
				{/if}
			</header>

			<p class="not-reviewed" role="note" data-testid="app-not-reviewed">{preview.notice}</p>

			<h4>Access</h4>
			<ul>
				<li>The app itself: <strong>{accessLabel(preview.service_access)}</strong></li>
				<li>
					People who sign in through it: at most <strong>{accessLabel(preview.delegated_access)}</strong>, and
					never more than their own role allows. Each person chooses, and read only is the default.
				</li>
				<li data-testid="app-reads-system">{preview.reads_system_collections}</li>
			</ul>

			<h4>Collections it adds</h4>
			{#if preview.collections.length === 0}
				<p class="hint">None.</p>
			{:else}
				<ul>
					{#each preview.collections as c (c.key)}
						<li>
							{c.name} <span class="mono">/{c.slug}</span>
							{#if c.adopt}<Chip size="sm" color="var(--accent-blue)">reuses the existing collection</Chip>{/if}
						</li>
					{/each}
				</ul>
			{/if}

			<h4>Events, webhook and actions</h4>
			<ul>
				{#if preview.webhook_url}
					<li>Sends events to <span class="mono">{preview.webhook_url}</span></li>
				{/if}
				{#each preview.events as ev (ev.name)}
					<li>Event <span class="mono">{ev.name}</span> on {ev.collections.join(', ')}</li>
				{/each}
				{#each preview.item_actions as act (act.key)}
					<li>Item action "{act.label}" on {act.collections.join(', ')}</li>
				{/each}
				{#if !preview.webhook_url && preview.events.length === 0 && preview.item_actions.length === 0}
					<li class="hint">None.</li>
				{/if}
			</ul>
			{#if preview.deferred_notice}<p class="hint">{preview.deferred_notice}</p>{/if}

			<h4>Playbooks and conventions it adds ({preview.artifacts.length})</h4>
			<p class="hint">Each lands as a draft. Nothing runs until you activate it.</p>
			{#each preview.artifacts as a (a.key)}
				<article class="artifact" data-testid="app-artifact">
					<div class="artifact-head">
						<strong>{a.normalized.title}</strong>
						<span class="hint">{a.kind} &rarr; /{a.destination_collection}</span>
					</div>
					{#if a.changes.length > 0}
						<ul class="changes" aria-label="Changes Pad makes to {a.normalized.title}">
							{#each a.changes as ch (ch)}
								<li>{ch}</li>
							{/each}
						</ul>
					{:else}
						<p class="hint">Stored exactly as published.</p>
					{/if}
					<details>
						<summary>Compare what was fetched with what will be stored</summary>
						<div class="compare">
							<div>
								<h5>Fetched</h5>
								<pre>{a.raw}</pre>
							</div>
							<div>
								<h5>Stored</h5>
								<pre>{a.normalized.content}</pre>
								{#if Object.keys(a.normalized.fields).length > 0}
									<pre class="fields">{JSON.stringify(a.normalized.fields, null, 2)}</pre>
								{/if}
							</div>
						</div>
					</details>
				</article>
			{/each}

			<p class="hint">This review expires at {new Date(preview.expires_at).toLocaleTimeString()}.</p>
			<div class="row">
				<Button variant="primary" size="sm" disabled={busy} onclick={install}>
					{busy ? 'Installing…' : `Install ${preview.title}`}
				</Button>
				<Button size="sm" disabled={busy} onclick={cancel}>Cancel</Button>
			</div>
		</section>
	{/if}

	{#if error}
		<p class="error" role="alert">
			{error}{#if errorPath}<span class="mono"> ({errorPath})</span>{/if}
		</p>
	{/if}
</div>

<style>
	.flow {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.url-form {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	label {
		font-weight: 600;
		font-size: 0.9em;
	}
	input {
		flex: 1 1 16rem;
		min-width: 0;
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-primary);
		color: var(--text-primary);
	}
	.row {
		display: flex;
		gap: var(--space-2);
		flex-wrap: wrap;
		align-items: center;
	}
	.hint {
		margin: 0;
		font-size: 0.85em;
		color: var(--text-secondary);
	}
	.review {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.review h3 {
		margin: 0;
		font-size: 1.1em;
	}
	.review h4 {
		margin: var(--space-3) 0 0;
		font-size: 0.95em;
	}
	.review ul {
		margin: 0;
		padding-left: var(--space-5);
		font-size: 0.9em;
	}
	.version,
	.by {
		color: var(--text-secondary);
		font-weight: normal;
		font-size: 0.85em;
	}
	.by {
		margin: var(--space-1) 0;
	}
	.not-reviewed {
		margin: 0;
		padding: var(--space-2) var(--space-3);
		border-left: 3px solid var(--accent-amber);
		background: var(--bg-secondary);
		font-size: 0.9em;
	}
	.mono {
		font-family: var(--font-mono, monospace);
		font-size: 0.9em;
		overflow-wrap: anywhere;
	}
	.artifact {
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-3);
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.artifact-head {
		display: flex;
		gap: var(--space-2);
		align-items: baseline;
		flex-wrap: wrap;
	}
	.changes li {
		color: var(--accent-orange);
	}
	.compare {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr));
		gap: var(--space-3);
		margin-top: var(--space-2);
	}
	h5 {
		margin: 0 0 var(--space-1);
		font-size: 0.8em;
		color: var(--text-secondary);
	}
	pre {
		margin: 0;
		max-height: 20rem;
		overflow: auto;
		padding: var(--space-2);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		font-size: 0.8em;
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}
	.fields {
		margin-top: var(--space-2);
	}
	.error {
		margin: 0;
		color: var(--accent-red);
		font-size: 0.9em;
	}
</style>
