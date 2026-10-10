<script lang="ts">
	import type { SaveStatus } from '$lib/items/saveTracker.svelte';
	import type { CollabConnectionState } from '$lib/collab/wsProvider.svelte';

	/**
	 * TASK-2221 (C38): the item's save and sync state, in the sticky header so
	 * it stays visible once the meta line has scrolled away, plus a polite
	 * live region that tells a screen reader about the changes that matter.
	 *
	 * The chip shows only what needs attention or confirms a write: Saving…,
	 * ✓ Saved, Reconnecting…, Offline. The steady states (synced, idle) and the
	 * first connect show nothing. Announced: Saved, Reconnecting, Offline, and
	 * Back online after either of those. Saving is not announced (it fires on
	 * every debounced keystroke burst).
	 */
	let {
		saveStatus,
		collabState,
	}: { saveStatus: SaveStatus; collabState: CollabConnectionState | null } = $props();

	const chip = $derived.by((): { kind: string; label: string } | null => {
		if (collabState === 'offline') return { kind: 'offline', label: 'Offline' };
		if (collabState === 'reconnecting') return { kind: 'reconnecting', label: 'Reconnecting…' };
		if (saveStatus === 'saving') return { kind: 'saving', label: 'Saving…' };
		if (saveStatus === 'saved') return { kind: 'saved', label: '✓ Saved' };
		return null;
	});

	let message = $state('');
	// A repeated message (two saves in a row) must still be a CHANGE to the
	// region, or a screen reader stays silent; alternate a trailing NBSP.
	let flip = false;
	function announce(text: string) {
		flip = !flip;
		message = flip ? text : `${text} `;
	}

	let prevSave: SaveStatus = 'idle';
	let prevCollab: CollabConnectionState | null = null;
	let wasDisconnected = false;
	$effect(() => {
		const s = saveStatus;
		const c = collabState;
		if (s === 'saved' && prevSave !== 'saved') announce('Saved');
		if (c !== prevCollab) {
			if (c === 'offline') {
				wasDisconnected = true;
				announce('Offline. Your edits are kept in this tab until the connection returns.');
			} else if (c === 'reconnecting') {
				wasDisconnected = true;
				announce('Connection lost. Reconnecting.');
			} else if (c === 'synced' && wasDisconnected) {
				wasDisconnected = false;
				announce('Back online. Changes are syncing.');
			}
		}
		prevSave = s;
		prevCollab = c;
	});
</script>

<span class="save-sync">
	{#if chip}
		<span class="save-sync-chip save-sync-{chip.kind}" aria-hidden="true">{chip.label}</span>
	{/if}
	<span class="sr-only" role="status" aria-live="polite" aria-atomic="true">{message}</span>
</span>

<style>
	.save-sync {
		display: inline-flex;
		align-items: center;
		flex-shrink: 0;
		margin-left: auto;
	}
	.save-sync-chip {
		font-size: 0.75em;
		padding: 1px 8px;
		border-radius: 999px;
		white-space: nowrap;
		color: var(--text-muted);
		background: var(--bg-tertiary);
	}
	.save-sync-saved {
		color: var(--accent-green);
	}
	.save-sync-reconnecting {
		color: var(--accent-amber);
	}
	.save-sync-offline {
		color: var(--accent-red);
	}
</style>
