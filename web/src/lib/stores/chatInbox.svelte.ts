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
//
// Roster and ensure (TASK-4044 2b-ii, ruling 018faf64): roster merges presences
// with the DM numbers; ensureDM() creates-or-reuses a DM and is fenced by identity.
import { untrack } from 'svelte';
import { api } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
import { chatStore } from '$lib/stores/chat.svelte';
import { normalizeAgentName, rollupPresence, type AgentPresence } from '$lib/chat/presence';
import type { ChatConversationSummary, LiveSession } from '$lib/types';

/** One roster row: an agent, how reachable it is, and its DM number if it has one. */
export interface RosterEntry {
	key: string;
	presence: AgentPresence;
	number: number | null;
}

const ROSTER_RANK: Record<AgentPresence, number> = {
	online: 0,
	busy_uncertain: 1,
	connected_not_accepting: 2,
	unknown: 3,
	offline: 4
};

/** How often the sessions list is read while the tab is visible. */
export const PRESENCE_POLL_MS = 10_000;
/** How long a rate-limited ensure keeps its button off, at the least. */
export const ENSURE_COOLDOWN_MS = 5_000;
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
	 * Presence per agent key over the current DMs (a group conversation's key is
	 * not an agent to chat with, ruling 018faf64 Q3), plus session-only agents
	 * while the sessions read is good. It feeds the roster.
	 */
	presences = $derived(
		rollupPresence(
			this.conversations.filter((c) => c.kind === 'direct').map((c) => c.agent_key),
			this.sessions ?? 'unavailable'
		)
	);

	/**
	 * The agents to start or open a chat with. No unread here: the badge reads
	 * chatStore.unreadByNumber (Q7).
	 */
	roster = $derived.by((): RosterEntry[] => {
		const numbers = new Map<string, number>();
		for (const c of this.conversations) {
			if (c.kind !== 'direct') continue;
			const k = normalizeAgentName(c.agent_key);
			if (k !== '' && !numbers.has(k)) numbers.set(k, c.number);
		}
		const out: RosterEntry[] = [];
		for (const [key, presence] of this.presences) {
			out.push({ key, presence, number: numbers.get(key) ?? null });
		}
		return out.sort(
			(a, b) => ROSTER_RANK[a.presence] - ROSTER_RANK[b.presence] || a.key.localeCompare(b.key)
		);
	});

	/** Agent keys with an ensure POST in flight. REPLACED on each change, never mutated (K3). */
	ensuring = $state<Set<string>>(new Set());
	/** Keys whose ensure was rate limited and is cooling down. */
	rateLimited = $state<Record<string, true>>({});
	/** Keys whose last ensure failed for another reason. */
	ensureFailed = $state<Record<string, true>>({});

	#seq = 0;
	#stopRoot: (() => void) | null = null;
	#presenceSeq = 0;
	#presenceActive = false;
	#presenceOwners = 0;
	#cooldowns = new Map<string, ReturnType<typeof setTimeout>>();
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
			// An ensure answer from before this point is fenced; its state goes too (K1, K2).
			this.ensuring = new Set();
			this.rateLimited = {};
			this.ensureFailed = {};
			for (const t of this.#cooldowns.values()) clearTimeout(t);
			this.#cooldowns.clear();
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

	/**
	 * Poll the sessions list while the tab is visible. Reference counted: pages
	 * that overlap during a navigation each start and stop once, and the poll
	 * runs until the last owner stops.
	 */
	startPresence(): void {
		if (++this.#presenceOwners > 1) return;
		this.#presenceActive = true;
		this.#presenceStartedAt = Date.now();
		document.addEventListener('visibilitychange', this.#onVisibility);
		if (document.visibilityState === 'visible') this.#armPresence();
	}

	stopPresence(): void {
		if (this.#presenceOwners === 0) return;
		if (--this.#presenceOwners > 0) return;
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

	/**
	 * Create-or-reuse the DM with an agent and reload the list. Returns the
	 * conversation number, or null when nothing was started (an ensure already in
	 * flight, a failure, a rate limit, or an identity change meanwhile). A 429 is
	 * never retried: the POST has no idempotency key, so the button stays off for
	 * at least ENSURE_COOLDOWN_MS.
	 */
	async ensureDM(rawKey: string): Promise<number | null> {
		const key = normalizeAgentName(rawKey);
		if (key === '' || this.ensuring.has(key) || this.rateLimited[key]) return null;
		this.ensuring = new Set(this.ensuring).add(key);
		this.ensureFailed = withoutKey(this.ensureFailed, key);
		const isSameIdentity = authStore.identityFence();
		try {
			const res = await api.chat.ensureConversation(key);
			if (!isSameIdentity()) return null;
			await this.load();
			if (!isSameIdentity()) return null;
			return res.conversation.number;
		} catch (err) {
			if (!isSameIdentity()) return null;
			const e = err as { code?: string; retryAfterMs?: number } | null;
			if (e?.code === 'rate_limited') {
				this.rateLimited = { ...this.rateLimited, [key]: true };
				const delay = Math.max(e.retryAfterMs ?? 0, ENSURE_COOLDOWN_MS);
				this.#cooldowns.set(
					key,
					setTimeout(() => {
						this.#cooldowns.delete(key);
						this.rateLimited = withoutKey(this.rateLimited, key);
					}, delay)
				);
			} else {
				this.ensureFailed = { ...this.ensureFailed, [key]: true };
			}
			return null;
		} finally {
			if (isSameIdentity()) {
				const next = new Set(this.ensuring);
				next.delete(key);
				this.ensuring = next;
			}
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

function withoutKey(rec: Record<string, true>, key: string): Record<string, true> {
	const { [key]: _gone, ...rest } = rec;
	return rest;
}

export const chatInbox = new ChatInboxStore();
