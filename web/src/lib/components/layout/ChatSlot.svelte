<!--
	ChatSlot — the Chat slot shared by the phone bottom bars (TASK-4315 PR 4):
	BottomNav inside a workspace and GlobalBottomBar outside one. A button, not a
	link: it opens the Chat sheet (a dialog above the bar) through chatSurface, lit
	while that sheet is open, and carries the unread badge.
-->
<script lang="ts">
	import UnreadBadge from '$lib/components/chat/UnreadBadge.svelte';
	import { chatStore } from '$lib/stores/chat.svelte';
	import { chatSetting } from '$lib/stores/chatSetting.svelte';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';

	// `onopen` runs first: the bar closes its own sheets so only one surface is up.
	let { onopen }: { onopen?: () => void } = $props();

	// A user who turned Chat off sees no count anywhere (TASK-4315).
	let unread = $derived(chatSetting.enabled ? chatStore.unreadTotal : 0);
</script>

<button
	class="bn-item"
	class:active={chatSurface.isOpen}
	type="button"
	data-chat-slot
	aria-haspopup="dialog"
	aria-expanded={chatSurface.isOpen}
	aria-label={unread > 0 ? `Chat, ${unread} unread` : 'Chat'}
	onclick={() => {
		onopen?.();
		void chatSurface.open('/messages');
	}}
>
	<span class="bn-icon bn-chat-icon" aria-hidden="true">
		💬
		<span class="bn-badge"><UnreadBadge count={unread} /></span>
	</span>
	<span class="bn-label" aria-hidden="true">Chat</span>
</button>

<style>
	.bn-item {
		flex: 1;
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: 3px;
		background: none;
		border: none;
		cursor: pointer;
		padding: 0;
		color: var(--text-muted);
		font-family: var(--font-ui);
		min-width: 0;
	}
	.bn-item.active {
		color: var(--text-primary);
	}
	.bn-icon {
		font-size: 1.2em;
		line-height: 1;
	}
	/* The icon anchors the unread badge (TASK-4186). */
	.bn-chat-icon {
		position: relative;
	}
	.bn-badge {
		position: absolute;
		top: -6px;
		left: 55%;
		font-size: 1rem;
		pointer-events: none;
	}
	.bn-label {
		font-size: 0.68em;
		font-weight: 500;
	}
</style>
