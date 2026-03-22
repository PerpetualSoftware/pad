<script lang="ts">
	interface Props {
		selected?: string;
		onselect: (emoji: string) => void;
	}

	const EMOJI_GROUPS = [
		{ label: 'Objects', emojis: ['📋', '📝', '📄', '📁', '📂', '📌', '📎', '🔖', '📊', '📈', '📉', '📦'] },
		{ label: 'Symbols', emojis: ['✓', '✗', '⚡', '🔥', '⭐', '💡', '🎯', '🏷️', '🔔', '💬', '🔗', '🔒'] },
		{ label: 'People', emojis: ['👤', '👥', '🧑‍💻', '🤝', '💪', '🎉', '👀', '🙌', '🧠', '❤️', '👍', '🚀'] },
		{ label: 'Nature', emojis: ['🌱', '🌿', '🌍', '☀️', '🌙', '⚙️', '🔧', '🛠️', '🧪', '🔬', '💎', '🏗️'] },
	];

	let { selected, onselect }: Props = $props();
</script>

<div class="emoji-picker">
	{#each EMOJI_GROUPS as group (group.label)}
		<div class="category-label">{group.label}</div>
		<div class="emoji-grid">
			{#each group.emojis as emoji (emoji)}
				<button
					class="emoji-btn"
					class:selected={selected === emoji}
					onclick={() => onselect(emoji)}
					type="button"
				>
					{emoji}
				</button>
			{/each}
		</div>
	{/each}
</div>

<style>
	.emoji-picker {
		width: 240px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-2);
	}

	.category-label {
		font-size: 0.7em;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		padding: var(--space-1) var(--space-1) 2px;
	}

	.emoji-grid {
		display: grid;
		grid-template-columns: repeat(6, 1fr);
		gap: 2px;
	}

	.emoji-btn {
		width: 36px;
		height: 36px;
		display: flex;
		align-items: center;
		justify-content: center;
		border: none;
		background: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
		font-size: 1.1em;
		line-height: 1;
		padding: 0;
	}

	.emoji-btn:hover {
		background: var(--bg-hover);
	}

	.emoji-btn.selected {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
	}
</style>
