<script lang="ts">
	import Modal from './Modal.svelte';
	import CharacterShortcutsToggle from './CharacterShortcutsToggle.svelte';
	import { characterShortcuts } from '$lib/a11y/characterShortcuts.svelte';
	import { modKeyLabel } from '$lib/utils/platform';

	interface Props {
		visible: boolean;
		onclose: () => void;
	}

	let { visible, onclose }: Props = $props();

	interface Shortcut {
		/** Keys that always work. */
		keys: string;
		/** The single-character alias, if any; governed by the single-key switch. */
		single?: string;
		description: string;
	}

	interface ShortcutGroup {
		title: string;
		shortcuts: Shortcut[];
	}

	// Labels name the key this platform has: ⌘ on macOS, Ctrl elsewhere.
	const groups: ShortcutGroup[] = [
		{
			title: 'Global',
			shortcuts: [
				{ keys: modKeyLabel('K'), description: 'Search / Command palette' },
				// ⌘N/Ctrl+N is still bound, but browsers keep it for a new window and
				// never deliver it to the page (BUG-3465), so it is not advertised.
				{ keys: '', single: 'C', description: 'New item' },
				{ keys: modKeyLabel('\\'), description: 'Show or hide the sidebar and workspace bar' },
				{ keys: modKeyLabel('F'), description: 'Filter the list (on a collection page)' },
				{ keys: '', single: '?', description: 'Show keyboard shortcuts' }
			]
		},
		{
			title: 'Navigation',
			shortcuts: [
				{ keys: '↓', single: 'j', description: 'Move down' },
				{ keys: '↑', single: 'k', description: 'Move up' },
				{ keys: '← / →', single: 'h / l', description: 'Move between board columns' },
				{ keys: 'Enter', description: 'Open selected item' },
				{ keys: 'Tab', description: 'Move into the open item pane' },
				{ keys: 'Esc', description: 'Go back / Close' }
			]
		},
		{
			title: 'Search palette',
			shortcuts: [
				{ keys: '42 / TASK-42, Enter', description: 'Go to that item by number or ref' }
			]
		},
		{
			title: 'Item Detail',
			shortcuts: [
				{ keys: modKeyLabel('Enter'), description: 'Save' },
				{ keys: 'Esc', description: 'Cancel editing' }
			]
		}
	];

	function label(s: Shortcut): string {
		if (!s.single || !characterShortcuts.enabled) return s.keys;
		return s.keys ? `${s.single} / ${s.keys}` : s.single;
	}
</script>

<!-- The native <dialog> (via <Modal>) owns Escape, backdrop dismiss, the focus
     trap, and focus save/restore — the hand-rolled listeners this component
     used to carry are gone. placement/shadow overrides keep the original
     centered look (the pre-migration backdrop was flex-centered). -->
<Modal
	open={visible}
	{onclose}
	labelledby="keyboard-shortcuts-title"
	maxWidth="500px"
	placement="center"
	--modal-shadow="0 8px 32px rgba(0, 0, 0, 0.3)"
>
	<div class="header">
		<h2 class="title" id="keyboard-shortcuts-title">Keyboard Shortcuts</h2>
		<button class="close-btn" onclick={onclose} aria-label="Close">
			<svg width="16" height="16" viewBox="0 0 16 16" fill="none">
				<path d="M4 4L12 12M12 4L4 12" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
			</svg>
		</button>
	</div>
	<div class="body">
		{#each groups as group (group.title)}
			<div class="group">
				<h3 class="group-title">{group.title}</h3>
				<div class="shortcut-list">
					{#each group.shortcuts as shortcut (shortcut.description)}
						{@const keys = label(shortcut)}
						<div class="shortcut-row" class:off={!keys}>
							<kbd class="key">{keys || shortcut.single}</kbd>
							<span class="description">
								{shortcut.description}{#if !keys}<span class="off-note"> (single-key shortcuts are off)</span>{/if}
							</span>
						</div>
					{/each}
				</div>
			</div>
		{/each}
		<CharacterShortcutsToggle />
	</div>
</Modal>

<style>
	.header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--border);
	}

	.title {
		font-size: 1em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.close-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 28px;
		height: 28px;
		border-radius: var(--radius-sm);
		color: var(--text-secondary);
		cursor: pointer;
		transition: all 0.1s;
	}

	.close-btn:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	/* The Modal box is overflow:hidden flex-column; the body is the scroll area. */
	.body {
		padding: var(--space-4) var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
		overflow-y: auto;
	}

	.group-title {
		font-size: 0.75em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		margin-bottom: var(--space-2);
	}

	.shortcut-row.off .key,
	.shortcut-row.off .description {
		opacity: 0.55;
	}

	.off-note {
		color: var(--text-muted);
	}

	.shortcut-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.shortcut-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius-sm);
	}

	.shortcut-row:hover {
		background: var(--bg-tertiary);
	}

	.key {
		display: inline-block;
		padding: 2px var(--space-2);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.8em;
		color: var(--text-secondary);
		min-width: 28px;
		text-align: center;
		line-height: 1.6;
	}

	.description {
		color: var(--text-primary);
		font-size: 0.875em;
	}
</style>
