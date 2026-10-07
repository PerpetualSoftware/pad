<script lang="ts">
	import { characterShortcuts } from '$lib/a11y/characterShortcuts.svelte';

	// The WCAG 2.1.4 off switch for every single-character shortcut (BUG-3465).
	// Shown in the shortcuts modal and on the account settings page; the
	// settings page is the way back in once `?` itself is off.
	const id = `character-shortcuts-${Math.random().toString(36).slice(2, 8)}`;
</script>

<div class="toggle">
	<input
		{id}
		type="checkbox"
		checked={characterShortcuts.enabled}
		onchange={(e) => characterShortcuts.set(e.currentTarget.checked)}
		aria-describedby="{id}-desc"
	/>
	<div>
		<label for={id}>Single-key shortcuts</label>
		<p id="{id}-desc" class="desc">
			C, ?, j / k / h / l, and + − 0 in the image viewer. Turn off if stray key presses or speech
			input trigger them. Saved on this device.
		</p>
	</div>
</div>

<style>
	.toggle {
		display: flex;
		align-items: flex-start;
		gap: var(--space-2);
	}

	input {
		margin-top: 3px;
	}

	label {
		font-weight: 500;
		color: var(--text-primary);
		cursor: pointer;
	}

	.desc {
		margin-top: var(--space-1);
		font-size: 0.85em;
		color: var(--text-muted);
	}
</style>
