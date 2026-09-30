// The workspace-access stream (PLAN-3002 U7b, TASK-3275): ONE EventSource per
// browser and user (per tab before BUG-3318, see below) on
// GET /api/v1/events/stream?access=true, whose only job is to hear
// `workspace_access_changed` (TASK-3272) and refetch.
//
// The event is a REFETCH HINT and is never applied as state. Correctness lives
// on the read-filtered GET /me/workspace-tabs and the workspace list, so a
// duplicate or missed hint costs nothing that the next fetch does not repair.
// For the same reason a reconnect, and a `sync_required`, refetch once: this
// stream cannot vouch for what it missed, and the fetch does not need it to.
//
// Per tab it ACTS, per browser it CONNECTS (BUG-3318). TASK-3275 chose one
// stream per tab because "each tab owns its own navigation, and a lost ACTIVE
// workspace must move the tab that is showing it". That is still how every tab
// acts: each one runs refetch() for its own route on every hint. What moved is
// the connection. Over plain HTTP (HTTP/1.1) a browser allows 6 connections
// per host, and one long-lived stream per tab starved new pages: with four pad
// tabs open a fifth never got a connection for its first fetch. So ONE tab per
// browser and user, elected with navigator.locks, holds the stream and relays
// each hint over a BroadcastChannel; every tab, the leader included, then
// refetches for itself. When the leader goes (closed, crashed, discarded), the
// lock passes to a waiting tab, whose first connect counts as a reconnect: it
// resyncs every tab, so a hint fired during the handover is not lost.
// Without navigator.locks or BroadcastChannel each tab opens its own stream,
// as before.
//
// Identity: every settle is fenced by authStore.identityFence(), and an
// identity change closes the source and reopens it for whoever is signed in
// NOW, so a hint that arrives for the previous identity refetches nothing.

import { goto } from '$app/navigation';
import { api, isWorkspaceNotFoundBody } from '$lib/api/client';
import { page } from '$app/state';
import { authStore } from '$lib/stores/auth.svelte';
import { tabsStore } from '$lib/stores/tabs.svelte';
import { workspaceStore } from '$lib/stores/workspace.svelte';
import { uiStore } from '$lib/stores/ui.svelte';
import { tabLanding } from '$lib/utils/tabLanding';
import { workspaceRestoreTarget } from '$lib/utils/workspace-route';
import { probeRefusal, reconnectDelayMs } from './sseReconnect';

export const ACCESS_STREAM_URL = '/api/v1/events/stream?access=true';

const ACCESS_KIND = 'workspace_access_changed';
// Changes after which the workspace no longer reaches this user.
const GONE_CHANGES = new Set(['lost', 'deleted', 'purged']);

interface AccessHint {
	kind: string;
	workspace_id?: string;
	change?: string;
}

function createAccessStream() {
	let source: EventSource | null = null;
	let running = false;
	// Bumped by stop() and by every identity change; a timer or probe from an
	// older generation is dropped.
	let generation = 0;
	let attempt = 0;
	let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
	let offIdentity: (() => void) | null = null;
	// Election state (BUG-3318). `channel` relays hints between this user's
	// tabs; `releaseLock` ends this tab's leadership.
	let channel: BroadcastChannel | null = null;
	let releaseLock: (() => void) | null = null;
	let lifecycleBound = false;

	// True only for the workspace-scoped 404. Anything else, a success, an
	// outage, a different error, keeps the tab where it is.
	async function workspaceIsGone(slug: string): Promise<boolean> {
		try {
			await api.workspaces.get(slug);
			return false;
		} catch (err) {
			return isWorkspaceNotFoundBody({ error: err });
		}
	}

	function clearTimer() {
		if (reconnectTimer) clearTimeout(reconnectTimer);
		reconnectTimer = null;
	}

	function closeSource() {
		const s = source;
		source = null;
		s?.close();
	}

	function electionSupported(): boolean {
		return (
			typeof navigator !== 'undefined' &&
			!!navigator.locks &&
			typeof navigator.locks.request === 'function' &&
			typeof BroadcastChannel !== 'undefined'
		);
	}

	// Keyed by user: a different account signed in on another tab of this
	// browser must never hear this user's hints, or hold their stream.
	function electionName(): string {
		return `pad-access-${authStore.userId ?? ''}`;
	}

	type Relay = { kind: 'hint'; hint: AccessHint } | { kind: 'resync' };

	// Act on a hint here, and relay it to the other tabs, which act on it for
	// their own routes.
	function deliver(msg: Relay) {
		if (channel) {
			try {
				channel.postMessage(msg);
			} catch {
				// A closed channel: this tab is leaving; the next leader resyncs.
			}
		}
		receive(msg);
	}

	function receive(msg: Relay) {
		if (msg.kind === 'resync') {
			void refetch();
			return;
		}
		const hint = msg.hint;
		if (hint?.kind !== ACCESS_KIND) return;
		void refetch(hint.change && GONE_CHANGES.has(hint.change) ? hint : undefined);
	}

	function leaveElection() {
		const release = releaseLock;
		releaseLock = null;
		release?.();
		const c = channel;
		channel = null;
		c?.close();
	}

	// Join the election: listen on the channel, and queue for the lock. The
	// grant opens the stream; a grant that had to wait is a TAKEOVER, so its
	// connect resyncs every tab.
	function join() {
		if (!electionSupported()) {
			open(false);
			return;
		}
		const gen = generation;
		const name = electionName();
		const c = new BroadcastChannel(name);
		channel = c;
		c.onmessage = (ev: MessageEvent) => {
			if (gen !== generation || !running) return;
			receive(ev.data as Relay);
		};
		void (async () => {
			let heldByAnother = false;
			try {
				const q = await navigator.locks.query();
				heldByAnother = !!q.held?.some((l) => l.name === name);
			} catch {
				// Unknown: treat the grant as a first connect.
			}
			if (gen !== generation || !running) return;
			navigator.locks
				.request(name, { mode: 'exclusive' }, () => {
					if (gen !== generation || !running) return;
					open(heldByAnother);
					return new Promise<void>((resolve) => {
						releaseLock = () => {
							releaseLock = null;
							closeSource();
							resolve();
						};
					});
				})
				.catch(() => {
					// The lock manager refused (a sandboxed frame, a policy):
					// fall back to this tab's own stream, as before BUG-3318.
					if (gen !== generation || !running || source) return;
					if (channel === c) {
						channel = null;
						c.close();
					}
					open(false);
				});
		})();
	}

	// A page entering the back/forward cache must give up the stream and the
	// lock, so another tab takes over; restored, it rejoins.
	function bindLifecycle() {
		if (lifecycleBound || typeof window === 'undefined') return;
		lifecycleBound = true;
		window.addEventListener('pagehide', (ev) => {
			if (!(ev as PageTransitionEvent).persisted || !running) return;
			generation++;
			clearTimer();
			closeSource();
			leaveElection();
			parked = true;
		});
		window.addEventListener('pageshow', (ev) => {
			if (!(ev as PageTransitionEvent).persisted || !parked) return;
			parked = false;
			if (!running || !authStore.authenticated) return;
			generation++;
			attempt = 0;
			join();
		});
	}
	let parked = false;

	// Refetch both reads. When `gone` names the workspace this tab is inside,
	// ask the server whether that workspace still resolves for this caller,
	// and land per Q3 only when it does not.
	//
	// The hint is not evidence and neither is the open set: a tab closed on
	// another device is absent from it while access is held, and a hint can be
	// stale (codex round 1). The workspace-scoped 404 (BUG-3069) is the one
	// answer that means "gone for you", for a member and a guest alike, which
	// GET /workspaces cannot say because it lists memberships only.
	async function refetch(gone?: AccessHint) {
		const isSameIdentity = authStore.identityFence();
		const before = tabsStore.tabs.slice();
		const active = workspaceStore.current;
		const inGone = () =>
			!!gone?.workspace_id &&
			!!active &&
			active.id === gone.workspace_id &&
			page.params.workspace === active.slug;
		const wasInGone = inGone();
		void workspaceStore.loadAll();
		try {
			await tabsStore.load();
		} catch {
			return;
		}
		// The route is re-read after every await: the user may have moved on
		// while the refetch was in flight (codex round 1).
		if (!wasInGone || !active || !isSameIdentity() || !inGone()) return;
		if (!(await workspaceIsGone(active.slug))) return;
		if (!isSameIdentity() || !inGone()) return;
		const after = tabsStore.tabs.filter((t) => t.slug !== active.slug);
		const landing = tabLanding(before, active.slug, after);
		if (!landing) {
			await goto('/console');
			uiStore.highlightAddWorkspace();
			return;
		}
		await goto(workspaceRestoreTarget(landing));
	}

	function scheduleReconnect(refused: boolean) {
		if (reconnectTimer || !running) return;
		attempt++;
		const gen = generation;
		const arm = (retryAfterMs: number | null) => {
			if (gen !== generation || !running || source) return;
			reconnectTimer = setTimeout(() => {
				reconnectTimer = null;
				if (gen !== generation || !running || source) return;
				open(true);
			}, reconnectDelayMs(attempt, retryAfterMs));
		};
		if (!refused) {
			arm(null);
			return;
		}
		void probeRefusal(ACCESS_STREAM_URL).then((probe) => {
			if (probe.kind === 'retry') {
				arm(probe.retryAfterMs);
				return;
			}
			// 401 / 403: a dead credential. Retrying it forever is the loop
			// the workspace stream's `unauthorized` handling exists to stop.
			if (gen === generation) running = false;
		});
	}

	function open(isReconnect: boolean) {
		const s = new EventSource(ACCESS_STREAM_URL);
		source = s;
		const gen = generation;
		const current = () => source === s && gen === generation;

		s.addEventListener('connected', () => {
			if (!current()) return;
			attempt = 0;
			// A reconnect cannot know what it missed; one refetch covers it,
			// in every tab (a takeover counts as a reconnect, BUG-3318).
			if (isReconnect) deliver({ kind: 'resync' });
		});
		s.addEventListener('notification', (ev) => {
			if (!current()) return;
			let hint: AccessHint;
			try {
				hint = JSON.parse((ev as MessageEvent).data);
			} catch {
				return;
			}
			if (hint.kind !== ACCESS_KIND) return;
			deliver({ kind: 'hint', hint });
		});
		s.addEventListener('sync_required', () => {
			if (!current()) return;
			deliver({ kind: 'resync' });
		});
		s.addEventListener('unauthorized', () => {
			if (!current()) return;
			running = false;
			closeSource();
			leaveElection();
		});
		s.onerror = () => {
			if (!current()) return;
			const refused = s.readyState === EventSource.CLOSED;
			closeSource();
			scheduleReconnect(refused);
		};
	}

	return {
		/** Open the stream for the signed-in user. A second call is a no-op. */
		start() {
			if (!offIdentity) {
				offIdentity = authStore.onIdentityChange(() => {
					const wasRunning = running;
					generation++;
					clearTimer();
					closeSource();
					leaveElection();
					attempt = 0;
					// Rejoin for whoever is signed in NOW, never for the
					// identity this listener was told about.
					if (wasRunning && authStore.authenticated) join();
					else running = false;
				});
			}
			bindLifecycle();
			if (running) return;
			running = true;
			generation++;
			attempt = 0;
			join();
		},
		stop() {
			running = false;
			generation++;
			clearTimer();
			closeSource();
			leaveElection();
		},
		get connected() {
			return source !== null;
		}
	};
}

export const accessStream = createAccessStream();
