<script lang="ts">
	import { onDestroy, onMount, tick, untrack } from 'svelte';
	import { page } from '$app/state';
	import ChatBackLink from '$lib/components/chat/ChatBackLink.svelte';
	import MessageBubble from '$lib/components/chat/MessageBubble.svelte';
	import PresenceDot from '$lib/components/chat/PresenceDot.svelte';
	import UnreadDivider from '$lib/components/chat/UnreadDivider.svelte';
	import { chatInbox } from '$lib/stores/chatInbox.svelte';
	import { ChatThread } from '$lib/stores/chatThread.svelte';

	// The read-only thread (TASK-4044 PR 3a, rulings 4cf4d4b9 and 2de240d9). The
	// store owns the window; this page owns the scroll container and the bottom
	// sentinel that says what the reader has seen.
	let raw = $derived(page.params.n ?? '');
	let n = $derived(/^[1-9][0-9]*$/.test(raw) ? Number(raw) : null);

	const thread = new ChatThread();
	// Bumped by "Try again" so the load effect below re-runs (TASK-4044 2b-i, ruling 90709f40).
	let retry = $state(0);

	let scroller = $state<HTMLElement | null>(null);
	let sentinel = $state<HTMLElement | null>(null);
	let positioned = false;
	let lastNewest = 0;

	// The presence dot polls only while this page is open.
	onMount(() => chatInbox.startPresence());
	onDestroy(() => {
		chatInbox.stopPresence();
		thread.dispose();
	});

	$effect(() => {
		const target = n;
		void retry;
		positioned = false;
		lastNewest = 0;
		untrack(() => void thread.open(target));
	});

	// First paint of a window: at the "N new" divider when there is one, else the end.
	$effect(() => {
		if (thread.status !== 'ready' || positioned || !scroller) return;
		positioned = true;
		lastNewest = thread.newestSeq;
		void tick().then(() => {
			const divider = scroller?.querySelector('#unread-divider');
			if (divider) divider.scrollIntoView?.({ block: 'start' });
			else if (scroller) scroller.scrollTop = scroller.scrollHeight;
		});
	});

	// A new row follows the reader only when they are at the end and nothing waits
	// beyond the window; otherwise nothing scrolls.
	$effect(() => {
		const newest = thread.newestSeq;
		if (!positioned || !scroller) return;
		const follow = newest > lastNewest && thread.atBottom && !thread.hasNewer;
		lastNewest = newest;
		if (follow) {
			void tick().then(() => {
				if (scroller) scroller.scrollTop = scroller.scrollHeight;
			});
		}
	});

	// The sentinel at the end of the window: in view means the reader has seen the end.
	$effect(() => {
		if (!sentinel || !scroller || typeof IntersectionObserver === 'undefined') return;
		const io = new IntersectionObserver(
			(entries) => {
				const last = entries[entries.length - 1];
				if (last) thread.atBottom = last.isIntersecting;
			},
			{ root: scroller }
		);
		io.observe(sentinel);
		return () => io.disconnect();
	});

	// Mark read once the end is in view on a visible tab; also retried on a refresh.
	$effect(() => {
		void thread.atBottom;
		void thread.newestSeq;
		void thread.status;
		untrack(() => thread.tryMarkRead());
	});
	onMount(() => {
		const onVisible = () => thread.tryMarkRead();
		document.addEventListener('visibilitychange', onVisible);
		return () => document.removeEventListener('visibilitychange', onVisible);
	});

	async function loadOlder() {
		const el = scroller;
		const before = el ? el.scrollHeight - el.scrollTop : 0;
		await thread.loadOlder();
		await tick();
		// Keep the row the reader was looking at where it was.
		if (el) el.scrollTop = el.scrollHeight - before;
	}

	function jumpToUnread() {
		scroller?.querySelector('#unread-divider')?.scrollIntoView?.({ block: 'start' });
	}

	function jumpToQuote(seq: number) {
		scroller?.querySelector(`#msg-${seq}`)?.scrollIntoView?.({ block: 'center' });
	}

	let conversation = $derived(thread.conversation);
	let dividerAt = $derived(thread.dividerIndex);
</script>

<svelte:head>
	<title>Chat · Pad</title>
</svelte:head>

<header class="chat-header">
	<ChatBackLink />
	<a class="all-chats" href="/messages">All conversations</a>
	{#if conversation}
		<span class="presence">
			<PresenceDot
				state={chatInbox.presenceFor(conversation.agent_key)}
				checkedAt={chatInbox.checkedAt}
			/>
			{conversation.agent_key}
		</span>
	{/if}
</header>

{#if thread.status === 'notFound'}
	<h1>Conversation not found</h1>
{:else if thread.status === 'failed'}
	<h1>Could not load this conversation</h1>
	<button type="button" class="retry" onclick={() => retry++}>Try again</button>
{:else}
	<h1>{conversation ? conversation.title || conversation.agent_key : 'Chat'}</h1>
	{#if thread.status === 'ready'}
		{#if thread.refreshFailed}
			<p class="banner" role="status">
				Couldn't refresh this conversation.
				<button type="button" class="retry" onclick={() => void thread.refresh()}>Try again</button>
			</p>
		{/if}
		{#if dividerAt >= 0}
			<button type="button" class="jump" onclick={jumpToUnread}>Jump to unread</button>
		{/if}
		<div class="scroller" bind:this={scroller}>
			{#if thread.hasOlder}
				<div class="edge">
					<button
						type="button"
						class="older"
						disabled={thread.loadingOlder}
						onclick={() => void loadOlder()}>Load earlier</button
					>
					{#if thread.olderFailed}
						<span class="banner" role="status">Couldn't load earlier messages. Try again.</span>
					{/if}
				</div>
			{/if}
			<div class="messages">
				{#each thread.messages as m, i (m.seq)}
					{#if i === dividerAt}
						<UnreadDivider count={thread.unreadCount} more={thread.hasNewer} />
					{/if}
					<MessageBubble message={m} onJumpToQuote={jumpToQuote} />
				{:else}
					<p class="empty">No messages yet.</p>
				{/each}
			</div>
			{#if thread.hasNewer}
				<div class="edge">
					<button
						type="button"
						class="newer"
						disabled={thread.loadingNewer}
						onclick={() => void thread.loadNewer()}>Show newer messages</button
					>
					{#if thread.newerFailed}
						<span class="banner" role="status">Couldn't load newer messages. Try again.</span>
					{/if}
				</div>
			{/if}
			<div class="sentinel" bind:this={sentinel} aria-hidden="true"></div>
		</div>
	{/if}
{/if}

<style>
	.chat-header {
		display: flex;
		gap: var(--space-4);
		margin-bottom: var(--space-4);
	}
	.presence {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		margin-left: auto;
		font-size: 0.85rem;
	}
	.all-chats {
		color: var(--text-secondary);
		font-size: 0.85rem;
	}
	h1 {
		margin: 0 0 var(--space-3);
		font-size: 1.5rem;
	}
	.scroller {
		flex: 1;
		min-height: 0;
		overflow-y: auto;
		padding-bottom: var(--space-3);
	}
	.messages {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.edge {
		display: flex;
		gap: var(--space-3);
		align-items: center;
		justify-content: center;
		padding: var(--space-2) 0;
	}
	.banner,
	.empty {
		color: var(--text-secondary);
		font-size: 0.85rem;
	}
	.jump {
		align-self: flex-start;
		margin-bottom: var(--space-2);
	}
	.sentinel {
		height: 1px;
	}
</style>
