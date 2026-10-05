<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import type { ItemAppAction } from '$lib/types';

	/**
	 * Item actions installed apps offer on this item (SPEC-6 U9d over U11,
	 * TASK-3413; DOC-3371 §6): a link out, never embedded UI. Pressing one
	 * mints a single-use context code server-side and opens the app at the URL
	 * it returns, in a new tab with no opener. The parent remounts this per
	 * item and identity, so nothing here outlives the item it was loaded for.
	 */
	interface Props {
		wsSlug: string;
		itemSlug: string;
	}
	let { wsSlug, itemSlug }: Props = $props();

	let actions = $state<ItemAppAction[]>([]);
	let opening = $state<string | null>(null);
	let error = $state('');
	/** The parent remounts this per item: a mint that answers after the pane
	 *  moved on must not open the old item's app (codex U9d r1). */
	let destroyed = false;
	onDestroy(() => {
		destroyed = true;
	});

	onMount(async () => {
		const asked = authStore.identityEpoch;
		try {
			const list = await api.items.appActions(wsSlug, itemSlug);
			if (authStore.identityEpoch !== asked) return;
			actions = Array.isArray(list) ? list : [];
		} catch {
			// No actions to offer is the quiet answer: an item with none, an
			// older server, apps off. The item pane is not about this button.
			actions = [];
		}
	});

	const keyOf = (a: ItemAppAction) => `${a.install_id}/${a.action_key}`;

	/** Only a web URL is followed: never javascript:, data: or the like. */
	function safeUrl(raw: string): string | null {
		try {
			const u = new URL(raw);
			return u.protocol === 'https:' || u.protocol === 'http:' ? u.href : null;
		} catch {
			return null;
		}
	}

	async function open(a: ItemAppAction) {
		const asked = authStore.identityEpoch;
		error = '';
		// Open the tab NOW, inside the click: a window opened after an await
		// is a popup the browser blocks. Cut its opener before anything loads,
		// then send it to the URL once it is minted. The navigation's referrer
		// follows Pad's own Referrer-Policy (strict-origin-when-cross-origin),
		// so the app learns Pad's origin, which it is installed on, and never
		// the item's URL.
		const tab = window.open('', '_blank');
		if (tab) tab.opener = null;
		opening = keyOf(a);
		try {
			const { url } = await api.items.mintAppAction(wsSlug, itemSlug, a.install_id, a.action_key);
			if (authStore.identityEpoch !== asked || destroyed) {
				tab?.close();
				return;
			}
			const target = safeUrl(url);
			if (!target) {
				tab?.close();
				error = `Couldn't open ${a.app_title}.`;
				return;
			}
			if (tab) {
				tab.location.replace(target);
			} else {
				// Popups blocked outright: fall back to a no-opener open.
				window.open(target, '_blank', 'noopener,noreferrer');
			}
		} catch {
			// Every refusal is the same 404 by design (U11): say only that it
			// did not open.
			tab?.close();
			if (authStore.identityEpoch === asked && !destroyed) error = `Couldn't open ${a.app_title}.`;
		} finally {
			if (authStore.identityEpoch === asked) opening = null;
		}
	}
</script>

{#if actions.length > 0}
	<span class="app-actions">
		{#each actions as a (keyOf(a))}
			<button
				class="app-action-btn"
				type="button"
				disabled={opening !== null}
				title="{a.label}: opens {a.app_title} in a new tab"
				onclick={() => open(a)}
			>
				{a.label}<span class="external" aria-hidden="true">↗</span>
				<span class="sr-only">(opens {a.app_title} in a new tab)</span>
			</button>
		{/each}
		{#if error}
			<span class="app-action-error" role="alert">{error}</span>
		{/if}
	</span>
{/if}

<style>
	.app-actions {
		display: inline-flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-1);
		min-width: 0;
	}
	/* ItemDetail's .action-btn is scoped to it: the look is repeated here. */
	.app-action-btn {
		padding: var(--space-1) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85em;
		cursor: pointer;
		max-width: 16rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.app-action-btn:hover:not(:disabled) {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}
	.app-action-btn:disabled {
		opacity: 0.6;
		cursor: default;
	}
	.external {
		margin-left: 0.25em;
		font-size: 0.85em;
	}
	.app-action-error {
		font-size: 0.8em;
		color: var(--accent-red);
	}
	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		overflow: hidden;
		clip: rect(0 0 0 0);
		white-space: nowrap;
	}
</style>
