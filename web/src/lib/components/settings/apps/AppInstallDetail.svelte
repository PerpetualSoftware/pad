<script lang="ts">
	import { api, PadApiError } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import Chip from '$lib/components/common/Chip.svelte';
	import AppInstallCodePanel from './AppInstallCodePanel.svelte';
	import { appStateLabel, appStateColor } from './appState';
	import type { AppInstallCode, AppInstallSummary } from '$lib/types';

	/**
	 * One install, as its owner manages it (SPEC-6 U9a, TASK-3413): its state,
	 * its webhook, deliveries dropped undelivered, a new install code, and the
	 * lifecycle doors with what each one does (DOC-3371 §2, §8).
	 */
	interface Props {
		wsSlug: string;
		install: AppInstallSummary;
		/** The install's state changed: the parent re-reads its list. */
		onchanged: () => void;
		onback: () => void;
	}
	let { wsSlug, install, onchanged, onback }: Props = $props();

	type Action = 'disable' | 'enable' | 'rotate' | 'uninstall' | 'code';

	const consequences: Record<Action, { title: string; body: string; confirm: string; danger: boolean }> = {
		disable: {
			title: 'Disable this app',
			body: 'The app stops at once: every token it holds is revoked, its webhook and item actions pause, and people signed in through it are signed out. Its collections and everything it wrote stay. You can re-enable it later; the app then gets new tokens and people sign in again.',
			confirm: 'Disable',
			danger: true
		},
		enable: {
			title: 'Re-enable this app',
			body: 'The app can connect again and resumes its webhook and item actions. Tokens issued before it was disabled stay revoked: the app obtains new ones, and people who used it sign in again.',
			confirm: 'Re-enable',
			danger: false
		},
		rotate: {
			title: 'Rotate credentials',
			body: "Replaces the app's credentials. Every token it holds is revoked and you get a new install code to give it. Until the app redeems that code, it cannot reach this workspace and its webhook waits.",
			confirm: 'Rotate',
			danger: true
		},
		uninstall: {
			title: 'Uninstall this app',
			body: "Removes the app: its credentials, webhook and item actions are deleted and its bot leaves the workspace. Its collections, items, comments and attachments stay as workspace data, still labelled with the app's name. This cannot be undone; to use the app again, install it again.",
			confirm: 'Uninstall',
			danger: true
		},
		code: {
			title: 'Issue a new install code',
			body: 'Use this when the app lost its install code. The new code works once, for 10 minutes. Issuing it also lets a webhook that is waiting for one activate when the app redeems it.',
			confirm: 'Issue code',
			danger: false
		}
	};

	let installState = $derived(install.state);
	let pending = $state<Action | null>(null);
	let busy = $state(false);
	let error = $state('');
	let code = $state<AppInstallCode | null>(null);

	// What each state offers. A state between the two phases ("disabling",
	// "uninstalling") is finished by repeating its own call.
	let actions = $derived.by((): Action[] => {
		switch (installState) {
			case 'active':
				return ['code', 'rotate', 'disable', 'uninstall'];
			case 'inactive':
				return ['enable', 'uninstall'];
			case 'disabling':
				return ['disable', 'uninstall'];
			case 'uninstalling':
				return ['uninstall'];
			default:
				return [];
		}
	});

	function label(a: Action): string {
		if (a === 'disable' && installState === 'disabling') return 'Finish disabling';
		if (a === 'uninstall' && installState === 'uninstalling') return 'Finish uninstalling';
		return consequences[a].confirm;
	}

	async function run(a: Action) {
		const asked = authStore.identityEpoch;
		const ws = wsSlug;
		busy = true;
		error = '';
		try {
			if (a === 'code') {
				const c = await api.apps.issueCode(ws, install.install_id);
				if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
				code = c;
			} else {
				const r = await api.apps.lifecycle(ws, install.install_id, a);
				if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
				if (r.install_code && r.expires_at) {
					code = { install_id: r.install_id, install_code: r.install_code, expires_at: r.expires_at, notice: r.notice ?? '' };
				}
			}
			pending = null;
			onchanged();
		} catch (e) {
			if (authStore.identityEpoch !== asked || ws !== wsSlug) return;
			if (e instanceof PadApiError && e.code === 'deliveries_in_flight') {
				error = 'The app still has webhook deliveries in flight. Wait a few seconds and press the button again to finish.';
				onchanged();
			} else if (e instanceof PadApiError && e.code === 'install_state') {
				error = 'The app changed state in the meantime; the page now shows its current state.';
				pending = null;
				onchanged();
			} else {
				error = e instanceof Error ? e.message : 'Something went wrong';
			}
		} finally {
			if (authStore.identityEpoch === asked) busy = false;
		}
	}
</script>

<div class="detail">
	<Button variant="ghost" size="sm" onclick={onback}>&larr; All apps</Button>

	<header class="head">
		<h3>{install.app_name}</h3>
		<Chip size="sm" color={appStateColor(installState)}>{appStateLabel(installState)}</Chip>
	</header>
	<dl class="facts">
		<dt>Origin</dt>
		<dd class="mono">{install.origin}</dd>
		{#if install.version}
			<dt>Version</dt>
			<dd>{install.version}</dd>
		{/if}
		<dt>Installed</dt>
		<dd>{new Date(install.created_at).toLocaleString()}</dd>
		{#if install.webhook}
			<dt>Webhook</dt>
			<dd>
				<span class="mono">{install.webhook.url}</span>
				<span class="hook-status">
					{install.webhook.status === 'awaiting_secret' ? 'waiting for an install code' : install.webhook.status}
				</span>
				{#if install.webhook.notice}
					<p class="hint">{install.webhook.notice}</p>
				{/if}
			</dd>
			<dt>Dropped</dt>
			<dd data-testid="app-undelivered-dropped">
				{install.webhook.undelivered_dropped}
				{install.webhook.undelivered_dropped === 1 ? 'delivery' : 'deliveries'} dropped undelivered after 24 hours
			</dd>
		{/if}
	</dl>

	{#if code}
		<AppInstallCodePanel {code} appName={install.app_name} ondone={() => (code = null)} />
	{/if}

	{#if error}
		<p class="error" role="alert">{error}</p>
	{/if}

	{#if pending}
		{@const c = consequences[pending]}
		<div class="confirm" class:danger={c.danger} role="group" aria-label={c.title}>
			<strong>{c.title}</strong>
			<p>{c.body}</p>
			<div class="row">
				<Button variant={c.danger ? 'danger-solid' : 'primary'} size="sm" disabled={busy} onclick={() => pending && run(pending)}>
					{busy ? 'Working…' : label(pending)}
				</Button>
				<Button size="sm" disabled={busy} onclick={() => (pending = null)}>Cancel</Button>
			</div>
		</div>
	{:else if actions.length > 0}
		<div class="row actions">
			{#each actions as a (a)}
				<Button size="sm" variant={consequences[a].danger ? 'danger' : 'secondary'} onclick={() => ((pending = a), (error = ''))}>
					{label(a)}
				</Button>
			{/each}
		</div>
	{:else}
		<p class="hint">This app was uninstalled. What it wrote stays in the workspace, labelled with its name.</p>
	{/if}
</div>

<style>
	.detail {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
		align-items: flex-start;
	}
	.head {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	h3 {
		margin: 0;
		font-size: 1.05em;
	}
	.facts {
		display: grid;
		grid-template-columns: max-content minmax(0, 1fr);
		gap: var(--space-2) var(--space-4);
		margin: 0;
		font-size: 0.9em;
		max-width: 100%;
	}
	dt {
		color: var(--text-secondary);
	}
	dd {
		margin: 0;
		overflow-wrap: anywhere;
	}
	.mono {
		font-family: var(--font-mono, monospace);
		font-size: 0.9em;
	}
	.hook-status {
		margin-left: var(--space-2);
		color: var(--text-secondary);
	}
	.hint {
		margin: var(--space-1) 0 0;
		font-size: 0.85em;
		color: var(--text-secondary);
	}
	.error {
		margin: 0;
		color: var(--accent-red);
		font-size: 0.9em;
	}
	.row {
		display: flex;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	.confirm {
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-4);
		background: var(--bg-secondary);
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		max-width: 40rem;
	}
	.confirm.danger {
		border-color: var(--accent-red);
	}
	.confirm p {
		margin: 0;
		font-size: 0.9em;
		color: var(--text-secondary);
	}
</style>
