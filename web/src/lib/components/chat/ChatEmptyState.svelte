<script lang="ts">
	import { onDestroy, tick } from 'svelte';
	import Button from '$lib/components/common/Button.svelte';
	import ChatState from '$lib/components/chat/ChatState.svelte';
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
	// Shared across commands: with a second copyable command this must become
	// per-command, or one command's outcome would overwrite the other's (TASK-4210 L3).
	let status = $state('');

	let destroyed = false;
	onDestroy(() => {
		destroyed = true;
	});

	// Per-command attempt counter. Clipboard writes can settle out of order, so
	// only the latest attempt for a command may touch status or copied; an earlier
	// one that settles late (resolve or reject) would otherwise overwrite what the
	// newer attempt did. A click resets copied and status synchronously, so a
	// pending newer attempt never shows a previous attempt's 'Copied'. Neither
	// outcome fades: it stays until the next click on that button.
	const attempt: Record<string, number> = {};

	// A live region announces a change; 'Copied' -> 'Copied' is none. The click
	// already cleared status, and only the live attempt writes it, so no clear is
	// needed here. The tick still matters: when navigator.clipboard is undefined,
	// writeText throws synchronously, so without it the click's '' and the failure
	// text would batch into one flush and the live region would not change.
	async function announce(cmd: string, n: number, text: string) {
		await tick();
		if (destroyed || attempt[cmd] !== n) return;
		status = text;
	}

	async function copy(cmd: string) {
		const n = (attempt[cmd] = (attempt[cmd] ?? 0) + 1);
		copied[cmd] = false;
		status = '';
		const live = () => !destroyed && attempt[cmd] === n;
		try {
			await navigator.clipboard.writeText(cmd);
			if (!live()) return;
			copied[cmd] = true;
			await announce(cmd, n, 'Copied');
		} catch {
			// Rejected, or navigator.clipboard is undefined (plain http).
			if (!live()) return;
			await announce(cmd, n, "Couldn't copy \u2014 select the command");
		}
	}
</script>

{#if kind === 'start'}
	<h2 class="heading">Start a conversation</h2>
{:else if kind === 'no-agents'}
	<ChatState heading="No one to message yet" level={2} tone="accent" label="No agents yet">
		<p>
			Chats are with your agents. An agent appears here once it's connected to this account with a
			name — then you can start a conversation.
		</p>
		<ul class="snippets">
			{#each snippets as s (s.cmd)}
				<li>
					<span class="label">{s.label}</span>
					<span class="cmd">
						<code>{s.cmd}</code>
						{#if s.copyable}
							<Button
								size="sm"
								touch
								class="copy-btn"
								aria-label={`Copy ${s.label} command`}
								onclick={() => void copy(s.cmd)}
							>
								{copied[s.cmd] ? 'Copied' : 'Copy'}
							</Button>
						{/if}
					</span>
				</li>
			{/each}
		</ul>
		<p class="status" role="status" aria-live="polite">{status}</p>
		<p class="note">
			Other agents need an armed session ({ARM_COMMAND}), the monitor above running from a
			directory linked to this workspace (.pad.toml), and a name: {NAME_ORDER}. Details: docs/api.md,
			"Agent chat identity and arming".
		</p>
	</ChatState>
{/if}

<style>
	/* The inbox's section label (TASK-4515), the sidebar's "COLLECTIONS" style. */
	.heading {
		margin: 0 0 var(--space-2) var(--space-1);
		font-size: 11px;
		font-weight: 600;
		letter-spacing: 0.06em;
		text-transform: uppercase;
		color: var(--text-muted);
	}
	.snippets {
		list-style: none;
		width: 100%;
		margin: var(--space-2) 0 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		text-align: left;
	}
	.snippets li {
		display: flex;
		flex-direction: column;
		gap: 4px;
	}
	.label {
		font-size: 12px;
		color: var(--text-secondary);
	}
	.cmd {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		min-height: 40px;
		box-sizing: border-box;
		padding: 4px 4px 4px 12px;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-tertiary);
	}
	code {
		flex: 1;
		font-family: var(--font-mono);
		font-size: 13px;
		overflow-wrap: anywhere;
	}
	p.status {
		min-height: 1.2em;
		font-size: 13px;
	}
	p.status:empty {
		min-height: 0;
	}
	p.note {
		width: 100%;
		box-sizing: border-box;
		padding-top: var(--space-3);
		border-top: 1px solid var(--border-subtle);
		text-align: left;
		color: var(--text-muted);
		font-size: 12px;
	}
</style>
