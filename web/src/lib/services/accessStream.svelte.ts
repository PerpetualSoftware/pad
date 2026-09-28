// The workspace-access stream (PLAN-3002 U7b, TASK-3275): ONE EventSource per
// browser tab on GET /api/v1/events/stream?access=true, whose only job is to
// hear `workspace_access_changed` (TASK-3272) and refetch.
//
// The event is a REFETCH HINT and is never applied as state. Correctness lives
// on the read-filtered GET /me/workspace-tabs and the workspace list, so a
// duplicate or missed hint costs nothing that the next fetch does not repair.
// For the same reason a reconnect, and a `sync_required`, refetch once: this
// stream cannot vouch for what it missed, and the fetch does not need it to.
//
// Why per TAB and not leader-elected like the workspace stream (sse.svelte.ts):
// each tab owns its own navigation, and a lost ACTIVE workspace must move the
// tab that is showing it. The cost is one PAD_SSE_MAX_PER_USER slot per tab.
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
			// A reconnect cannot know what it missed; one refetch covers it.
			if (isReconnect) void refetch();
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
			void refetch(hint.change && GONE_CHANGES.has(hint.change) ? hint : undefined);
		});
		s.addEventListener('sync_required', () => {
			if (!current()) return;
			void refetch();
		});
		s.addEventListener('unauthorized', () => {
			if (!current()) return;
			running = false;
			closeSource();
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
					attempt = 0;
					// Reopen for whoever is signed in NOW, never for the
					// identity this listener was told about.
					if (wasRunning && authStore.authenticated) open(false);
					else running = false;
				});
			}
			if (running) return;
			running = true;
			generation++;
			attempt = 0;
			open(false);
		},
		stop() {
			running = false;
			generation++;
			clearTimer();
			closeSource();
		},
		get connected() {
			return source !== null;
		}
	};
}

export const accessStream = createAccessStream();
