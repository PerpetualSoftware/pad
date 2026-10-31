<script lang="ts">
	import { tick } from 'svelte';
	import { chatInbox } from '$lib/stores/chatInbox.svelte';
	import { chatEmptyKind, NAME_ORDER, ARM_COMMAND } from '$lib/chat/emptyState';

	const kind = $derived(
		chatEmptyKind({
			loaded: chatInbox.loaded,
			failed: chatInbox.failed,
			conversations: chatInbox.conversations,
			sessions: chatInbox.sessions,
			rosterLength: chatInbox.roster.length
		})
	);

	// The monitor line is what an agent harness runs, not something a person
	// pastes: `pad watch --stream` is "Not meant to be run by hand", so it gets
	// no Copy button (a copy affordance invites pasting it into a shell).
	const snippets = [
		{ label: 'Claude Code', cmd: '/pad:connect', copyable: true },
		{
			label: "Other agents (your harness's Pad monitor runs)",
			cmd: 'pad watch --stream --for-session',
			copyable: false
		}
	];

	let copied = $state<Record<string, boolean>>({});
	let status = $state('');
	const timers = new Map<string, ReturnType<typeof setTimeout>>();

	async function copy(cmd: string) {
		clearTimeout(timers.get(cmd));
		try {
			await navigator.clipboard.writeText(cmd);
			copied[cmd] = true;
			// A live region announces a change; 'Copied' -> 'Copied' is none. Clear,
			// let the DOM update, then set, so every copy is announced.
			status = '';
			await tick();
			status = 'Copied';
			timers.set(
				cmd,
				setTimeout(() => {
					copied[cmd] = false;
					status = '';
				}, 1500)
			);
		} catch {
			// Rejected, or navigator.clipboard is undefined (plain http).
			copied[cmd] = false;
			status = "Couldn't copy \u2014 select the command";
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
			{#each snippets as s (s.cmd)}
				<li>
					<span class="label">{s.label}</span>
					<code>{s.cmd}</code>
					{#if s.copyable}
						<button
							type="button"
							class="copy-btn"
							aria-label={`Copy ${s.label} command`}
							onclick={() => void copy(s.cmd)}
						>
							{copied[s.cmd] ? 'Copied' : 'Copy'}
						</button>
					{/if}
				</li>
			{/each}
		</ul>
		<p class="status" role="status" aria-live="polite">{status}</p>
		<p class="note">
			Other agents need an armed session ({ARM_COMMAND}), the monitor above running from a
			directory linked to this workspace (.pad.toml), and a name: {NAME_ORDER}. Details: docs/api.md,
			"Agent chat identity and arming".
		</p>
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
	.status {
		margin: 0 0 var(--space-2);
		min-height: 1.2em;
		font-size: 0.9rem;
	}
	.note {
		color: var(--text-muted);
		font-size: 0.9rem;
	}
</style>
