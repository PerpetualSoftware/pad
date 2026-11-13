<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import ChatState from '$lib/components/chat/ChatState.svelte';
	import { chatSetting } from '$lib/stores/chatSetting.svelte';

	// On the /messages route this is the page (h1); in the Chat host it sits under the
	// header's "Chat" h1, so it is a section (h2).
	let { level = 1 }: { level?: 1 | 2 } = $props();

	// Shown in place of /messages while the user's Chat setting is off (TASK-4315).
	// The page is not a 404: a link an agent or the connect instructions sent the
	// user to still explains itself. Turning Chat on flips the setting and the
	// layout then renders the page that was asked for, at the same URL.
	let saving = $state(false);
	let failed = $state(false);

	async function turnOn() {
		if (saving) return;
		saving = true;
		failed = false;
		try {
			await chatSetting.set(true);
		} catch {
			failed = true;
		} finally {
			saving = false;
		}
	}
</script>

<div class="chat-off">
	<ChatState heading="Chat is off" {level}>
		<p>
			Chat is for talking with agent sessions. It is hidden until you turn it on. Messages sent
			while it is off are kept, and show as unread once it is on.
		</p>
		{#snippet actions()}
			<Button variant="primary" touch disabled={saving} onclick={() => void turnOn()}>Turn on Chat</Button>
			<p class="status" role="status">{failed ? "Couldn't turn Chat on. Try again." : ''}</p>
			<a class="settings" href="/console/settings">Account settings</a>
		{/snippet}
	</ChatState>
</div>

<style>
	.chat-off {
		flex: 1;
		min-height: 0;
		display: flex;
		overflow-y: auto;
	}
	p.status {
		margin: 0;
		min-height: 1.2em;
		font-size: 13px;
		color: var(--text-secondary);
	}
	p.status:empty {
		min-height: 0;
	}
	.settings {
		color: var(--text-secondary);
		font-size: 13px;
	}
</style>
