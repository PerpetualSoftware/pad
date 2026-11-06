<script lang="ts">
	import { chatSetting } from '$lib/stores/chatSetting.svelte';

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

<section class="chat-off">
	<h1>Chat is off</h1>
	<p>
		Chat is for talking with agent sessions. It is hidden until you turn it on. Messages sent
		while it is off are kept, and show as unread once it is on.
	</p>
	<button type="button" disabled={saving} onclick={() => void turnOn()}>Turn on Chat</button>
	<p class="status" role="status">{failed ? "Couldn't turn Chat on. Try again." : ''}</p>
	<a class="settings" href="/console/settings">Account settings</a>
</section>

<style>
	.chat-off {
		display: flex;
		flex-direction: column;
		align-items: flex-start;
		gap: var(--space-3);
	}
	h1 {
		margin: 0;
		font-size: 1.5rem;
	}
	p {
		margin: 0;
		color: var(--text-secondary);
	}
	.status {
		min-height: 1.2em;
		font-size: 0.85rem;
	}
	.settings {
		color: var(--text-secondary);
		font-size: 0.85rem;
	}
</style>
