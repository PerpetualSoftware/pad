// The chat unread store (TASK-4044, DOC-4036). Chat is global, not under a
// workspace, so the count lives here and not in a workspace store.
//
// A `kind: 'chat'` notice from the access stream is a REFETCH HINT and is never
// applied as state: it carries ids and no body, and correctness lives on the
// access-checked GET /me/chat/unread. So a duplicate or missed hint costs
// nothing the next fetch does not repair, and a burst of hints (an agent's
// reply streams in as several sends) coalesces into one request: a trailing
// debounce, one fetch in flight at a time, and ONE follow-up when a hint
// landed during that fetch.
//
// A reconnect, a `sync_required` or a takeover cannot vouch for what the
// stream missed, so resync() fetches at once.
//
// Identity: every answer is fenced by authStore.identityFence() and ordered by
// a seq, so an answer for the previous account never lands, and an identity
// change clears the counts and cancels a pending fetch.
import { api } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
import type { ChatUnread } from '$lib/types';

/** The trailing debounce between the last hint and the fetch it causes. */
export const CHAT_HINT_DEBOUNCE_MS = 400;

/** A `kind: 'chat'` notice as the access stream relays it: ids only, no body. */
export interface ChatNotice {
	kind: 'chat';
	conversation_id: string;
	conversation: number;
	message_id?: string;
	message_seq?: number;
	author_agent_key?: string;
	author_is_human?: boolean;
	deleted?: boolean;
}

type Listener = (notice: ChatNotice) => void;

class ChatStore {
	unreadTotal = $state(0);
	unreadByNumber = $state<Record<number, number>>({});
	// Bumped after each SUCCESSFUL fetch whose answer was applied, so the
	// conversation list can reload from one signal that coalesced hints,
	// resyncs and focus all share. A failed or discarded fetch never bumps it.
	version = $state(0);

	// Orders answers: an older fetch cannot overwrite a newer one, and an
	// identity change outdates whatever is in flight.
	#seq = 0;
	#inFlight = false;
	// A hint (or resync) arrived while a fetch was in flight.
	#dirty = false;
	#timer: ReturnType<typeof setTimeout> | null = null;
	#listeners = new Set<Listener>();
	#started = false;
	#onFocus = () => this.#schedule();

	constructor() {
		// A different account's counts must never be shown. A swap reloads the
		// tab anyway; this covers sign-out, which does not.
		authStore.onIdentityChange(() => {
			this.#seq++;
			this.#dirty = false;
			this.#cancelTimer();
			this.unreadTotal = 0;
			this.unreadByNumber = {};
			if (this.#started && authStore.authenticated) void this.#run();
		});
	}

	/** Fetch the counts, and refetch on window focus. A second call is a no-op. */
	start(): void {
		if (this.#started) return;
		this.#started = true;
		if (typeof window !== 'undefined') window.addEventListener('focus', this.#onFocus);
		void this.#run();
	}

	stop(): void {
		this.#started = false;
		this.#cancelTimer();
		if (typeof window !== 'undefined') window.removeEventListener('focus', this.#onFocus);
	}

	/** A chat notice arrived: schedule a coalesced refetch and tell subscribers. */
	onHint(notice: ChatNotice): void {
		if (!authStore.authenticated) return;
		this.#schedule();
		// One subscriber throwing must not cost the others this hint (TASK-4044 r1).
		for (const fn of [...this.#listeners]) {
			try {
				fn(notice);
			} catch (err) {
				console.error('chat: hint subscriber threw', err);
			}
		}
	}

	/** The stream cannot vouch for what it missed: refetch now. */
	resync(): void {
		this.#cancelTimer();
		void this.#run();
	}

	subscribe(fn: Listener): () => void {
		this.#listeners.add(fn);
		return () => {
			this.#listeners.delete(fn);
		};
	}

	#cancelTimer() {
		if (this.#timer) clearTimeout(this.#timer);
		this.#timer = null;
	}

	#schedule() {
		this.#cancelTimer();
		this.#timer = setTimeout(() => {
			this.#timer = null;
			void this.#run();
		}, CHAT_HINT_DEBOUNCE_MS);
	}

	async #run(): Promise<void> {
		if (!authStore.authenticated) return;
		if (this.#inFlight) {
			this.#dirty = true;
			return;
		}
		this.#inFlight = true;
		const mine = ++this.#seq;
		const isSameIdentity = authStore.identityFence();
		try {
			const res = await api.chat.unread();
			if (mine === this.#seq && isSameIdentity()) {
				const rows: ChatUnread[] = res.conversations ?? [];
				const byNumber: Record<number, number> = {};
				for (const r of rows) byNumber[r.number] = r.unread;
				this.unreadByNumber = byNumber;
				this.unreadTotal = res.total ?? 0;
				this.version++;
			}
		} catch {
			// Keep the last counts: a failed refetch is not "nothing unread".
		} finally {
			this.#inFlight = false;
		}
		if (this.#dirty) {
			this.#dirty = false;
			void this.#run();
		}
	}
}

export const chatStore = new ChatStore();
