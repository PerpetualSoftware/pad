<script lang="ts">
	import { onDestroy, onMount, tick, untrack } from 'svelte';
	import Button from '$lib/components/common/Button.svelte';
	import ChatHeader from '$lib/components/chat/ChatHeader.svelte';
	import ChatState from '$lib/components/chat/ChatState.svelte';
	import Composer from '$lib/components/chat/Composer.svelte';
	import MessageBubble from '$lib/components/chat/MessageBubble.svelte';
	import PendingBubble from '$lib/components/chat/PendingBubble.svelte';
	import PresenceDot from '$lib/components/chat/PresenceDot.svelte';
	import UnreadDivider from '$lib/components/chat/UnreadDivider.svelte';
	import { chatInbox } from '$lib/stores/chatInbox.svelte';
	import { ChatThread } from '$lib/stores/chatThread.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { canDeleteMessage } from '$lib/chat/deletePolicy';
	import { confirmDialog } from '$lib/stores/confirmDialog.svelte';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';
	import type { ChatMessage } from '$lib/types';

	// The thread (TASK-4044 PR 3a and 3b, decisions 4cf4d4b9 and 2de240d9). The
	// store owns the window; this view owns the scroll container, the bottom
	// sentinel that says what the reader has seen, and the reply target. The
	// caller says which conversation: null is a number that cannot name one.
	// `variant` says where it renders: the /messages route, or the Chat host (dock
	// or drawer) in the shell, which has its own Close. `active` is false while the
	// host is covered by another overlay: a thread nobody can see is not read.
	let {
		n,
		variant = 'route',
		active = true
	}: { n: number | null; variant?: 'route' | 'host'; active?: boolean } = $props();

	const thread = new ChatThread();
	// Bumped by "Try again" so the load effect below re-runs (TASK-4044 2b-i, decision 90709f40).
	let retry = $state(0);

	let scroller = $state<HTMLElement | null>(null);
	let sentinel = $state<HTMLElement | null>(null);
	let positioned = false;
	let lastNewest = 0;
	let replyTo = $state<ChatMessage | null>(null);
	let composer = $state<Composer | null>(null);
	let lastOwnSend = 0;
	// Loading newer messages while a send is in flight would race the send's own
	// confirmed row into the window (TASK-4044, decision 2a03a442).
	let sending = $derived(thread.items.some((it) => it.kind === 'pending' && it.pending.status === 'sending'));

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
		untrack(() => {
			replyTo = null;
			void thread.open(target);
		});
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

	// An own send, shown or confirmed, scrolls to the end even from further up
	// (decision d87482de); a row from elsewhere only follows a reader at the end.
	$effect(() => {
		const sendTick = thread.ownSendTick;
		if (!positioned || !scroller || sendTick === lastOwnSend) return;
		lastOwnSend = sendTick;
		void tick().then(() => {
			if (scroller) scroller.scrollTop = scroller.scrollHeight;
		});
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

	$effect(() => {
		thread.active = active;
	});

	// Mark read once the end is in view on a visible tab; also retried on a refresh.
	$effect(() => {
		void thread.atBottom;
		void thread.active;
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

	// The pill is for a divider the reader cannot see. The observer writes this
	// flag and nothing it reads; with no IntersectionObserver the pill stays.
	let dividerInView = $state(false);
	// A factory over the root, so the attachment re-runs once the scroller binds.
	const watchDivider = (root: HTMLElement | null) => (el: HTMLElement) => {
		if (!root || typeof IntersectionObserver === 'undefined') return;
		const io = new IntersectionObserver(
			(entries) => {
				const last = entries[entries.length - 1];
				if (last) dividerInView = last.isIntersecting;
			},
			{ root }
		);
		io.observe(el);
		return () => {
			io.disconnect();
			dividerInView = false;
		};
	};

	function jumpToUnread() {
		scroller?.querySelector('#unread-divider')?.scrollIntoView?.({ block: 'start' });
	}

	// Reply puts the reader into the composer; the bar is there after the tick.
	function startReply(m: ChatMessage) {
		replyTo = m;
		void tick().then(() => composer?.focus());
	}

	// Retry and Discard unmount with the status flip, so focus would fall to body.
	function refocusComposer() {
		void tick().then(() => composer?.focus());
	}

	// The reply bar goes only once the delete went through, and only if it still quotes this row.
	async function deleteMessage(m: ChatMessage) {
		const target = { userId: authStore.userId, n, seq: m.seq, conversationId: m.conversation_id };
		const stillCurrent = () => authStore.userId === target.userId && n === target.n &&
			thread.conversation?.id === target.conversationId;
		if (!stillCurrent()) return;
		const confirmed = await confirmDialog.request({
			title: 'Delete this message?',
			message: 'Delete this message? It will be removed from the conversation. This cannot undo anything an agent has already received or done.',
			confirmLabel: 'Delete',
			danger: true,
			focusCancel: true
		});
		if (!confirmed || !stillCurrent()) return;
		const result = await thread.remove(target.seq);
		if (!stillCurrent()) return;
		if (result.ok) {
			if (replyTo?.seq === target.seq) replyTo = null;
		} else if (thread.status === 'ready') {
			// A not-found thread already shows its own page; a toast saying "Try again" would mislead.
			toastStore.show(result.message, 'error');
		}
	}

	function jumpToQuote(seq: number) {
		scroller?.querySelector(`#msg-${seq}`)?.scrollIntoView?.({ block: 'center' });
	}

	let conversation = $derived(thread.conversation);
	let dividerAt = $derived(thread.dividerIndex);
</script>

{#snippet presence()}
	{#if conversation}
		<PresenceDot
			state={chatInbox.presenceFor(conversation.agent_key)}
			checkedAt={chatInbox.checkedAt}
			detail={chatInbox.armedCount(conversation.agent_key) > 1
				? `${chatInbox.armedCount(conversation.agent_key)} armed sessions`
				: ''}
			showLabel
		/>
	{/if}
{/snippet}

<!-- One header row (TASK-4515): back, the name over its presence, and Close in the host. -->
<ChatHeader
	back
	title={thread.status === 'notFound' || thread.status === 'failed'
		? ''
		: conversation
			? conversation.title || conversation.agent_key
			: 'Chat'}
	onClose={variant === 'host' ? () => chatSurface.close() : undefined}
	sub={conversation && thread.status !== 'notFound' && thread.status !== 'failed' ? presence : undefined}
/>

{#if thread.status === 'notFound'}
	<div class="pane"><ChatState heading="Conversation not found" level={1} /></div>
{:else if thread.status === 'failed'}
	<div class="pane">
		<ChatState heading="Could not load this conversation" level={1}>
			{#snippet actions()}
				<Button variant="primary" touch class="retry" onclick={() => retry++}>Try again</Button>
			{/snippet}
		</ChatState>
	</div>
{:else if thread.status === 'ready'}
	{#if thread.refreshFailed}
		<p class="notice" role="status">
			<span>Couldn't refresh this conversation.</span>
			<Button size="sm" touch class="retry" onclick={() => void thread.refresh()}>Try again</Button>
		</p>
	{/if}
	<div class="scroller" class:empty={thread.items.length === 0 && thread.detached.length === 0} bind:this={scroller}>
		{#if thread.hasOlder}
			<div class="edge">
				<Button
					variant="ghost"
					size="sm"
					touch
					class="older"
					disabled={thread.loadingOlder}
					onclick={() => void loadOlder()}>Load earlier</Button
				>
				{#if thread.olderFailed}
					<span class="banner" role="status">Couldn't load earlier messages. Try again.</span>
				{/if}
			</div>
		{/if}
		{#if dividerAt >= 0 && !dividerInView}
			<!-- A zero-height sticky strip: the pill floats over the rows and takes no line. It
			     sits below Load earlier so it never covers that button, and shows only while
			     the divider is out of view, so it never covers the divider's label. -->
			<div class="jump">
				<Button size="sm" class="jump-pill" onclick={jumpToUnread}>Jump to unread</Button>
			</div>
		{/if}
		<div class="messages">
			{#each thread.items as item, i (item.kind === 'message' ? `m${item.message.seq}` : `p${item.pending.op_key}`)}
				{#if item.kind === 'message'}
					{#if i === dividerAt}
						<UnreadDivider count={thread.unreadCount} more={thread.hasNewer} {@attach watchDivider(scroller)} />
					{/if}
					<MessageBubble
						message={item.message}
						canDelete={canDeleteMessage({ message: item.message, conversation, emailVerified: authStore.emailVerified })}
						refs={thread.refs}
						onJumpToQuote={jumpToQuote}
						onReply={startReply}
						onDelete={deleteMessage}
					/>
				{:else}
					<PendingBubble
						pending={item.pending}
						onRetry={(k) => {
							const body = item.pending.body;
							// A refused Retry discards its bubble: the text comes back with its notice, as for a refused send.
							void thread.retry(k).then((outcome) => {
								if (outcome !== 'sent' && outcome !== 'failed' && outcome !== 'stale') composer?.restore(body, outcome);
							});
							refocusComposer();
						}}
						onDiscard={(k) => {
							thread.discard(k);
							refocusComposer();
						}}
					/>
				{/if}
			{:else}
				{#if thread.detached.length === 0}
					<ChatState heading="No messages yet">
						<p>
							{conversation?.agent_key
								? `Send ${conversation.agent_key} a message to start the conversation.`
								: 'Send a message to start the conversation.'}
						</p>
					</ChatState>
				{/if}
			{/each}
		</div>
		{#if thread.hasNewer}
			<div class="edge">
				<Button
					variant="ghost"
					size="sm"
					touch
					class="newer"
					disabled={thread.loadingNewer || sending}
					aria-busy={sending ? 'true' : undefined}
					onclick={() => void thread.loadNewer()}>Show newer messages</Button
				>
				{#if thread.newerFailed}
					<span class="banner" role="status">Couldn't load newer messages. Try again.</span>
				{/if}
			</div>
		{/if}
		<!-- Detached confirmed sends use the same full-width rows. -->
		<div class="detached">
			{#each thread.detached as m (m.seq)}
				<MessageBubble
					message={m}
					canDelete={canDeleteMessage({ message: m, conversation, emailVerified: authStore.emailVerified })}
					refs={thread.refs}
					onJumpToQuote={jumpToQuote}
					onReply={startReply}
					onDelete={deleteMessage}
				/>
			{/each}
		</div>
		<div class="sentinel" bind:this={sentinel} aria-hidden="true"></div>
	</div>
	{#if n !== null}
		{#key n}
			<Composer
				bind:this={composer}
				{n}
				agentKey={conversation?.agent_key ?? ''}
				title={conversation?.title ?? ''}
				presence={conversation ? chatInbox.presenceFor(conversation.agent_key) : 'unknown'}
				{replyTo}
				messages={thread.messages}
				onClearReply={() => (replyTo = null)}
				onSend={(body, replyToSeq) => thread.send(body, replyToSeq)}
			/>
		{/key}
	{/if}
{/if}

<style>
	.pane {
		flex: 1;
		min-height: 0;
		display: flex;
		overflow-y: auto;
	}
	.scroller {
		position: relative;
		flex: 1;
		min-height: 0;
		overflow-y: auto;
		padding: var(--space-3) var(--space-4) var(--space-2);
	}
	.scroller.empty {
		display: flex;
		flex-direction: column;
	}
	.scroller.empty .messages {
		flex: 1;
	}
	.messages,
	.detached {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.edge {
		display: flex;
		gap: var(--space-3);
		align-items: center;
		justify-content: center;
		padding-bottom: var(--space-2);
	}
	.banner {
		color: var(--text-secondary);
		font-size: 12px;
	}
	.notice {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin: var(--space-2) var(--space-4) 0;
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		border: 1px solid color-mix(in srgb, var(--accent-red) 40%, transparent);
		background: color-mix(in srgb, var(--accent-red) 10%, transparent);
		font-size: 13px;
		color: var(--text-primary);
	}
	.notice span {
		flex: 1;
	}
	.jump {
		position: sticky;
		top: 0;
		z-index: 2;
		display: flex;
		justify-content: center;
		height: 0;
	}
	.jump :global(.jump-pill) {
		height: 28px;
		border-radius: 999px;
		background: var(--bg-raised);
		box-shadow: var(--shadow-md);
	}
	.sentinel {
		height: 1px;
	}
</style>
