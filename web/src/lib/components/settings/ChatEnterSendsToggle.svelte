<script lang="ts">
	import { inputMode } from '$lib/chat/inputMode.svelte';

	const id = `chat-enter-sends-${Math.random().toString(36).slice(2, 8)}`;
</script>

{#if inputMode.offered}
	<div class="toggle">
		<input
			{id}
			type="checkbox"
			checked={inputMode.stored}
			onchange={(e) => inputMode.setHardware(e.currentTarget.checked)}
			aria-describedby="{id}-desc"
		/>
		<div>
			<label for={id}>Chat: Enter sends (for a hardware keyboard)</label>
			<p id="{id}-desc" class="desc">
				For a tablet or phone with a keyboard attached. While the on-screen keyboard
				is up, Enter adds a line. Saved on this device.
			</p>
		</div>
	</div>
{/if}

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
