<script lang="ts">
	// TASK-2199: a reload discarded this tab's offline edits (the item changed
	// on the server while it was offline); hand them back to copy. It never
	// writes them anywhere: the user decides what to do with their version.
	import { copyToClipboard } from '$lib/utils/clipboard';
	import { authStore } from '$lib/stores/auth.svelte';

	interface Props {
		text: string;
		/** This text is on the clipboard: the caller drops the notice, if it still
		 * holds this text (codex r1: a copy can resolve after an item switch). */
		oncopied: (copied: string) => void;
		/** The user chose to discard it. */
		ondismiss: () => void;
		/** Reports a copy that failed; the notice stays. */
		oncopyfailed?: () => void;
	}

	let { text, oncopied, ondismiss, oncopyfailed }: Props = $props();

	async function copy() {
		// The clipboard write is awaited; the result is reported only to the
		// identity that asked for it (BUG-3095's rule for a child's await).
		const isSameIdentity = authStore.identityFence();
		const copied = text;
		const ok = await copyToClipboard(copied);
		if (!isSameIdentity()) return;
		if (ok) oncopied(copied);
		else oncopyfailed?.();
	}
</script>

<div class="offline-recovery" role="alert">
	<p class="offline-recovery-msg">
		Your offline edits couldn't be merged: this item changed on the server while you were offline.
		The editor now shows the server's version. Copy yours to keep it.
	</p>
	<details class="offline-recovery-text">
		<summary>Show your version</summary>
		<textarea readonly rows="8" aria-label="Your offline version">{text}</textarea>
	</details>
	<div class="offline-recovery-actions">
		<button type="button" class="offline-recovery-copy" onclick={copy}>Copy your version</button>
		<button type="button" class="offline-recovery-dismiss" onclick={ondismiss}>Dismiss</button>
	</div>
</div>

<style>
	.offline-recovery {
		margin: var(--space-2) 0;
		padding: var(--space-3) var(--space-4);
		border: 1px solid var(--accent-amber);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		font-size: 0.875em;
	}

	.offline-recovery-msg {
		margin: 0 0 var(--space-2);
	}

	.offline-recovery-text textarea {
		width: 100%;
		margin-top: var(--space-2);
		font-family: var(--font-mono, monospace);
		font-size: 0.9em;
	}

	.offline-recovery-actions {
		display: flex;
		gap: var(--space-2);
		margin-top: var(--space-2);
	}

	.offline-recovery-copy,
	.offline-recovery-dismiss {
		padding: var(--space-1) var(--space-3);
		border-radius: var(--radius-sm);
		border: 1px solid var(--border);
		background: var(--bg-primary);
		color: var(--text-primary);
		cursor: pointer;
	}

	.offline-recovery-copy {
		background: var(--accent-primary);
		border-color: var(--accent-primary);
		color: #fff;
	}
</style>
