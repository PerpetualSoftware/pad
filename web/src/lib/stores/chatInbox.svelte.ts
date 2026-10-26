// The chat inbox list (TASK-4044): GET /me/conversations and its load flags.
// It holds conversations ONLY. Unread counts come from chatStore and nothing
// here computes them.
//
// Reload: start() runs one effect on chatStore.version, which chatStore bumps
// after each successful unread fetch. A hint, a resync, a focus and chatStore's
// 15s retry of a failed unread fetch all end in that fetch, so this store does not subscribe to hints as well: that
// would reload twice for one hint.
//
// Identity: every answer is fenced by authStore.identityFence() and ordered by
// a seq, and an identity change drops the list.
import { untrack } from 'svelte';
import { api } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
import { chatStore } from '$lib/stores/chat.svelte';
import type { ChatConversationSummary } from '$lib/types';

class ChatInboxStore {
	conversations = $state<ChatConversationSummary[]>([]);
	loaded = $state(false);
	failed = $state(false);

	#seq = 0;
	#stopRoot: (() => void) | null = null;

	constructor() {
		authStore.onIdentityChange(() => {
			this.#seq++;
			this.conversations = [];
			this.loaded = false;
			this.failed = false;
		});
	}

	/** Load now, and again whenever chatStore finishes an unread fetch. A second call is a no-op. */
	start(): void {
		if (this.#stopRoot) return;
		this.#stopRoot = $effect.root(() => {
			$effect(() => {
				void chatStore.version;
				untrack(() => void this.load());
			});
		});
	}

	stop(): void {
		this.#stopRoot?.();
		this.#stopRoot = null;
	}

	async load(): Promise<void> {
		if (!authStore.authenticated) return;
		const mine = ++this.#seq;
		const isSameIdentity = authStore.identityFence();
		try {
			const res = await api.chat.listConversations();
			if (mine !== this.#seq || !isSameIdentity()) return;
			this.conversations = res.conversations ?? [];
			this.failed = false;
			this.loaded = true;
		} catch {
			if (mine !== this.#seq || !isSameIdentity()) return;
			// Keep the last list: a failed reload is not "no conversations".
			this.failed = true;
		}
	}
}

export const chatInbox = new ChatInboxStore();
