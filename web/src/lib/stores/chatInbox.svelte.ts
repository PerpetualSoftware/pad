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
//
// Presence (TASK-4044 2b-i): startPresence() polls GET /sessions on its own, apart
// from start(), so a page that only wants a presence dot fires no list fetch.
import { untrack } from 'svelte';
import { api } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
import { chatStore } from '$lib/stores/chat.svelte';
import { normalizeAgentName, rollupPresence, type AgentPresence } from '$lib/chat/presence';
import type { ChatConversationSummary, LiveSession } from '$lib/types';

/** How often the sessions list is read while the tab is visible. */
export const PRESENCE_POLL_MS = 10_000;
/** How long a transient failure may keep the last good read before it reads as unknown. */
export const PRESENCE_STALE_MS = 30_000;

// 503 ('unavailable') is the only live "cannot tell" answer; request() navigates
// away on a 401 before throwing 'unauthorized', so that one is defensive.
const PRESENCE_UNAVAILABLE_CODES = new Set(['unavailable', 'unauthorized']);

class ChatInboxStore {
	conversations = $state<ChatConversationSummary[]>([]);
	loaded = $state(false);
	failed = $state(false);

	/** null: no read yet. 'unavailable': the last answer could not be trusted. */
	sessions = $state<LiveSession[] | 'unavailable' | null>(null);
	/** When the last GOOD sessions read landed (ms), or null. */
	checkedAt = $state<number | null>(null);

	/**
	 * Presence per agent key over the current conversations, for the roster that
	 * TASK-4044 2b-ii adds. It has no consumer in 2b-i.
	 */
	presences = $derived(
		rollupPresence(
			this.conversations.map((c) => c.agent_key),
			this.sessions ?? 'unavailable'
		)
	);

	#seq = 0;
	#stopRoot: (() => void) | null = null;
	#presenceSeq = 0;
	#presenceActive = false;
	#presenceInFlight = false;
	#presenceStartedAt = 0;
	#presenceTimer: ReturnType<typeof setInterval> | null = null;

	constructor() {
		authStore.onIdentityChange(() => {
			this.#presenceSeq++;
			this.#presenceInFlight = false;
			this.sessions = null;
			this.checkedAt = null;
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

	/** One agent's presence. No read yet and 'unavailable' both read as 'unknown', never 'offline'. */
	presenceFor(agentKey: string): AgentPresence {
		return (
			rollupPresence([agentKey], this.sessions ?? 'unavailable').get(
				normalizeAgentName(agentKey)
			) ?? 'unknown'
		);
	}

	/** Poll the sessions list while the tab is visible. A second call is a no-op. */
	startPresence(): void {
		if (this.#presenceActive) return;
		this.#presenceActive = true;
		this.#presenceStartedAt = Date.now();
		document.addEventListener('visibilitychange', this.#onVisibility);
		if (document.visibilityState === 'visible') this.#armPresence();
	}

	stopPresence(): void {
		if (!this.#presenceActive) return;
		this.#presenceActive = false;
		// A late answer from before the stop must not land after a later start.
		this.#presenceSeq++;
		this.#presenceInFlight = false;
		this.#disarmPresence();
		document.removeEventListener('visibilitychange', this.#onVisibility);
	}

	#onVisibility = (): void => {
		this.#disarmPresence();
		if (document.visibilityState === 'visible') this.#armPresence();
	};

	#armPresence(): void {
		void this.#pollPresence();
		this.#presenceTimer = setInterval(() => void this.#pollPresence(), PRESENCE_POLL_MS);
	}

	#disarmPresence(): void {
		if (this.#presenceTimer !== null) clearInterval(this.#presenceTimer);
		this.#presenceTimer = null;
	}

	async #pollPresence(): Promise<void> {
		if (this.#presenceInFlight || !authStore.authenticated) return;
		this.#presenceInFlight = true;
		const mine = ++this.#presenceSeq;
		const isSameIdentity = authStore.identityFence();
		try {
			const res = await api.sessions.list();
			if (mine !== this.#presenceSeq || !isSameIdentity()) return;
			this.sessions = res.sessions ?? [];
			this.checkedAt = Date.now();
		} catch (err) {
			if (mine !== this.#presenceSeq || !isSameIdentity()) return;
			const code = (err as { code?: string } | null)?.code;
			if (code !== undefined && PRESENCE_UNAVAILABLE_CODES.has(code)) {
				this.sessions = 'unavailable';
			} else if (Date.now() - (this.checkedAt ?? this.#presenceStartedAt) >= PRESENCE_STALE_MS) {
				this.sessions = 'unavailable';
			}
		} finally {
			if (mine === this.#presenceSeq) this.#presenceInFlight = false;
		}
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
