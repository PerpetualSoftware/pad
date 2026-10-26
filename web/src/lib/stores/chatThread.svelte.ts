// One open chat thread (TASK-4044 PR 3a, rulings 4cf4d4b9 and 2de240d9): a
// per-mount store, not a singleton. It holds the window of messages the page
// shows and never decides what the server's unread count is.
//
// Open: GET the conversation, then one page. With nothing unread that is the
// newest page; with unread it is the page AFTER read_seq (up to the server
// max of 200), so the "N new" divider is always inside the window. Seqs are
// contiguous per conversation (a tombstone keeps its row), so "older rows
// exist" is oldest_seq > 1 and "newer rows exist" is newest < last_seq.
//
// Realtime: the thread reloads from chatStore.version, which every hint,
// resync and focus shares through one coalesced unread fetch, so it never
// subscribes to hints and never reads a notice field (carry-forward 2.7).
//
// REFRESH (R1, ruling 4cf4d4b9). A refresh must never miss a row newer than the
// window's newest confirmed seq, whatever the window's size, so the new rows are
// fetched ANCHORED AT THE NEWEST end:
//   1. after_seq = newest, ASC, paging while has_more, at most
//      REFRESH_MAX_PAGES pages (1,000 rows). If the bound is hit the rest stays
//      on the server, `hasNewer` is true and the page offers "Show newer
//      messages"; nothing scrolls.
//   2. tombstones: one DESC newest-200 page, merged only for seqs inside the
//      window's own [oldest, newest] range, so it can neither open a gap nor
//      prepend.
// When the window was opened with unread > 200 (`hasNewer` already true) a
// refresh does NOT append past the gap: step 1 is skipped, and step 2 becomes an
// ASC page from the window's oldest row, merged only up to the window's newest.
// The reader reaches the newer rows through "Show newer messages", which pages
// ASC from the window's newest.
// RESIDUAL, disclosed: a retraction of a row older than the newest 200 (or, in
// the gap case, further than 200 rows from the window's oldest) shows on reopen,
// not live. A tombstone is the only change an older row can undergo.
//
// Identity: every answer is fenced by a seq and authStore.identityFence().
import { untrack } from 'svelte';
import { api, isNotFoundError } from '$lib/api/client';
import { ChatWindow, unreadDividerIndex } from '$lib/chat/model';
import { authStore } from '$lib/stores/auth.svelte';
import { chatStore } from '$lib/stores/chat.svelte';
import type { ChatConversationSummary, ChatMessage } from '$lib/types';

/** Rows in the first page when nothing is unread, and in a load-earlier page. */
export const THREAD_PAGE = 50;
/** The server's page maximum (store/conversations.go maxConversationPage). */
export const THREAD_MAX_PAGE = 200;
/** Pages one refresh will follow from the newest end before it sets hasNewer. */
export const REFRESH_MAX_PAGES = 5;

export type ThreadStatus = 'loading' | 'ready' | 'notFound' | 'failed';

export class ChatThread {
	conversation = $state<ChatConversationSummary | null>(null);
	/** The confirmed window, ascending by seq; REPLACED on change. */
	messages = $state.raw<ChatMessage[]>([]);
	/** read_seq as of open, frozen for the mount: the divider never moves. */
	openReadSeq = $state(0);
	status = $state<ThreadStatus>('loading');
	refreshFailed = $state(false);
	olderFailed = $state(false);
	newerFailed = $state(false);
	loadingOlder = $state(false);
	loadingNewer = $state(false);
	hasOlder = $state(false);
	hasNewer = $state(false);
	/** Set by the page from its bottom sentinel. */
	atBottom = $state(false);

	#win = new ChatWindow();
	#n: number | null = null;
	#seq = 0;
	#lastSeq = 0;
	#readSeq = 0;
	#refreshing = false;
	#dirty = false;
	#marking = false;
	#stopRoot: () => void;
	#stopIdentity: () => void;

	constructor() {
		let first = true;
		this.#stopRoot = $effect.root(() => {
			$effect(() => {
				void chatStore.version;
				if (first) {
					first = false;
					return;
				}
				untrack(() => void this.refresh());
			});
		});
		this.#stopIdentity = authStore.onIdentityChange(() => {
			this.#seq++;
			this.#reset();
		});
	}

	/** The newest confirmed seq in the window, 0 when empty. */
	get newestSeq(): number {
		return this.messages.length ? this.messages[this.messages.length - 1].seq : 0;
	}

	/** Rows the "N new" divider counts: the unreadDividerIndex predicate. */
	get unreadCount(): number {
		return this.messages.filter((m) => m.seq > this.openReadSeq && !m.mine && !m.deleted).length;
	}

	get dividerIndex(): number {
		return unreadDividerIndex(this.messages, this.openReadSeq);
	}

	// Also closes the thread: with no #n and status 'loading', a version bump after
	// an identity change fetches nothing until the page opens it again (TASK-4044
	// r1 H1, ruling on triage ba47338c). open() sets both straight after.
	#reset() {
		this.#n = null;
		this.status = 'loading';
		this.conversation = null;
		this.messages = [];
		this.#win = new ChatWindow();
		this.openReadSeq = 0;
		this.refreshFailed = this.olderFailed = this.newerFailed = false;
		this.loadingOlder = this.loadingNewer = false;
		this.hasOlder = this.hasNewer = false;
		this.atBottom = false;
		this.#lastSeq = 0;
		this.#readSeq = 0;
		this.#dirty = false;
		this.#refreshing = false;
		this.#marking = false;
	}

	// `beyond` is the server's last_seq when the answer left rows beyond the
	// window, or null when it reached the end: the window's own newest then stands
	// for last_seq. last_seq is read apart from the page rows, so a message landing
	// between the two would otherwise read as a gap and make the next refresh skip
	// its append.
	#publish(beyond: number | null) {
		this.messages = this.#win.messages;
		this.#lastSeq = beyond ?? this.newestSeq;
		const oldest = this.messages.length ? this.messages[0].seq : 0;
		this.hasOlder = oldest > 1;
		this.hasNewer = this.newestSeq < this.#lastSeq;
	}

	/** A guard for one async step: false once the thread was reopened, disposed or re-fenced. */
	#guard(): () => boolean {
		const mine = this.#seq;
		const same = authStore.identityFence();
		return () => mine === this.#seq && same();
	}

	async open(n: number | null): Promise<void> {
		this.#seq++;
		this.#reset();
		this.#n = n;
		if (n === null) {
			this.status = 'notFound';
			return;
		}
		this.status = 'loading';
		const current = this.#guard();
		try {
			const conv = await api.chat.getConversation(n);
			if (!current()) return;
			this.conversation = conv;
			const page =
				conv.unread > 0
					? await api.chat.listMessages(n, { after_seq: conv.read_seq, limit: THREAD_MAX_PAGE })
					: await api.chat.listMessages(n, { limit: THREAD_PAGE });
			if (!current()) return;
			this.openReadSeq = page.read_seq;
			this.#readSeq = page.read_seq;
			this.#win.merge(page.messages);
			this.#publish(conv.unread > 0 && page.has_more ? page.last_seq : null);
			this.status = 'ready';
		} catch (err) {
			if (!current()) return;
			this.status = isNotFoundError(err) ? 'notFound' : 'failed';
			return;
		}
		if (this.#dirty) {
			this.#dirty = false;
			void this.refresh();
		}
	}

	/** Fetch the page before the window's oldest row. A failure keeps the window. */
	async loadOlder(): Promise<void> {
		const n = this.#n;
		if (n === null || this.status !== 'ready' || this.loadingOlder || !this.hasOlder) return;
		const current = this.#guard();
		this.loadingOlder = true;
		this.olderFailed = false;
		try {
			const page = await api.chat.listMessages(n, {
				before_seq: this.messages[0].seq,
				limit: THREAD_PAGE
			});
			if (!current()) return;
			this.#win.merge(page.messages);
			this.#publish(this.#lastSeq);
		} catch {
			if (current()) this.olderFailed = true;
		} finally {
			if (current()) this.loadingOlder = false;
		}
	}

	/** Fetch one page after the window's newest row (the gap left by a long unread run). */
	async loadNewer(): Promise<void> {
		const n = this.#n;
		if (n === null || this.status !== 'ready' || this.loadingNewer || !this.hasNewer) return;
		const current = this.#guard();
		this.loadingNewer = true;
		this.newerFailed = false;
		try {
			const page = await api.chat.listMessages(n, {
				after_seq: this.newestSeq,
				limit: THREAD_MAX_PAGE
			});
			if (!current()) return;
			this.#win.merge(page.messages);
			this.#publish(page.has_more ? page.last_seq : null);
		} catch {
			if (current()) this.newerFailed = true;
		} finally {
			if (current()) this.loadingNewer = false;
		}
	}

	/**
	 * Bring new rows and tombstones into the window (see the REFRESH note on top).
	 * A refresh while one is in flight, or while the open is still loading, sets a
	 * dirty flag and runs ONCE more after.
	 */
	async refresh(): Promise<void> {
		const n = this.#n;
		if (n === null) return;
		if (this.status === 'loading' || this.#refreshing) {
			this.#dirty = true;
			return;
		}
		if (this.status !== 'ready') return;
		const current = this.#guard();
		this.#refreshing = true;
		this.refreshFailed = false;
		try {
			const gap = this.hasNewer;
			if (!gap) {
				let cursor = this.newestSeq;
				for (let pages = 0; pages < REFRESH_MAX_PAGES; pages++) {
					const page = await api.chat.listMessages(n, { after_seq: cursor, limit: THREAD_MAX_PAGE });
					if (!current()) return;
					this.#win.merge(page.messages);
					this.#publish(page.has_more ? page.last_seq : null);
					if (!page.has_more || page.messages.length === 0) break;
					cursor = page.newest_seq;
				}
			}
			if (this.messages.length > 0) {
				const lo = this.messages[0].seq;
				const hi = this.newestSeq;
				const page = gap
					? await api.chat.listMessages(n, { after_seq: lo - 1, limit: THREAD_MAX_PAGE })
					: await api.chat.listMessages(n, { limit: THREAD_MAX_PAGE });
				if (!current()) return;
				this.#win.merge(page.messages.filter((m) => m.seq >= lo && m.seq <= hi));
				this.#publish(this.#lastSeq);
			}
			this.tryMarkRead();
		} catch {
			if (current()) this.refreshFailed = true;
		} finally {
			if (current()) {
				this.#refreshing = false;
				if (this.#dirty) {
					this.#dirty = false;
					void this.refresh();
				}
			}
		}
	}

	/**
	 * Tell the server what was DISPLAYED: the newest confirmed seq in the window,
	 * and only while the bottom sentinel is in view on a visible tab. Never marks
	 * locally on a failure; the next refresh or visibility change retries.
	 */
	tryMarkRead(): void {
		const n = this.#n;
		if (n === null || this.status !== 'ready' || !this.atBottom || this.#marking) return;
		if (typeof document !== 'undefined' && document.visibilityState !== 'visible') return;
		const seq = this.newestSeq;
		if (seq <= this.#readSeq) return;
		const current = this.#guard();
		this.#marking = true;
		let marked = false;
		api.chat
			.markRead(n, seq)
			.then(() => {
				if (!current()) return;
				marked = true;
				this.#readSeq = Math.max(this.#readSeq, seq);
				// POST /read publishes no notice, so the badge is stale until this.
				chatStore.resync();
			})
			.catch(() => {})
			.finally(() => {
				if (!current()) return;
				this.#marking = false;
				// Rows that landed while this was in flight. Only after a success, so a
				// failing mark cannot loop, and only when the window grew past `seq`
				// (TASK-4044 r2, ruling on the r2 triage).
				if (marked && this.newestSeq > seq) this.tryMarkRead();
			});
	}

	dispose(): void {
		this.#seq++;
		this.#stopRoot();
		this.#stopIdentity();
	}
}
