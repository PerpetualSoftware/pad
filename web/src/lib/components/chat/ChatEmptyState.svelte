<script lang="ts">
	import { chatInbox } from '$lib/stores/chatInbox.svelte';
	import { chatEmptyKind } from '$lib/chat/emptyState';

	const kind = $derived(
		chatEmptyKind({
			loaded: chatInbox.loaded,
			failed: chatInbox.failed,
			conversations: chatInbox.conversations,
			sessions: chatInbox.sessions,
			rosterLength: chatInbox.roster.length
		})
	);

	const snippets = [
		{ label: 'Claude Code', cmd: '/pad:connect' },
		{ label: 'Other agents', cmd: 'PAD_AGENT=<name> pad watch --stream --for-session' }
	];

	let copied = $state<string | null>(null);

	async function copy(cmd: string) {
		try {
			await navigator.clipboard.writeText(cmd);
			copied = cmd;
			setTimeout(() => (copied = null), 1500);
		} catch {
			// Clipboard API not available
		}
	}
</script>

{#if kind === 'start'}
	<h2 class="heading">Start a conversation</h2>
{:else if kind === 'no-agents'}
	<section class="empty" aria-label="No agents yet">
		<h2 class="heading">No one to message yet</h2>
		<p>
			Chats are with your agents. An agent appears here once it's connected to this account with a
			name — then you can start a conversation.
		</p>
		<ul class="snippets">
			{#each snippets as s, i (i)}
				<li>
					<span class="label">{s.label}</span>
					<code>{s.cmd}</code>
					<button type="button" class="copy-btn" onclick={() => void copy(s.cmd)}>
						{copied === s.cmd ? 'Copied' : 'Copy'}
					</button>
				</li>
			{/each}
		</ul>
		<p class="note">Name an agent with PAD_AGENT or agent_name in .pad.toml.</p>
	</section>
{/if}

<style>
	.heading {
		margin: 0 0 var(--space-3);
		font-size: 1.1rem;
	}
	.empty p {
		color: var(--text-secondary);
	}
	.snippets {
		list-style: none;
		margin: 0 0 var(--space-3);
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.snippets li {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
	}
	.label {
		color: var(--text-muted);
		min-width: 6rem;
	}
	code {
		overflow-wrap: anywhere;
	}
	.copy-btn {
		font-size: 0.85rem;
	}
	.note {
		color: var(--text-muted);
		font-size: 0.9rem;
	}
</style>
