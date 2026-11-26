// The Home Agents panel's read (DOC-4851 section 4): GET /me/agents, plus a
// longer queue for every lane the user opened with "Show all".
//
// Modelled on homeOverview: a generation counter lets only the latest load
// publish, and an identity change resets everything and fences off every
// in-flight result, so one person's lanes never render for the next.
//
// The optimistic overlay lives here, not in the component, so that "only
// handlers and load completion write the overlay" holds without an effect:
// handlers set a lane's overlay, and the next published load clears all of
// them, since the server's answer then shows what landed.

import { api } from '$lib/api/client';
import { authStore } from './auth.svelte';
import type { MeAgentsResponse, RoleQueueResponse } from '$lib/types';

export const AGENTS_QUEUE_LIMIT = 5;
export const AGENTS_SHOW_ALL_LIMIT = 50;
export const AGENTS_FINISHED_LIMIT = 5;

/** A lane's optimistic order: the queue refs, and where the line sits among them (null: no line). */
export interface LaneOverlay {
	refs: string[];
	line: number | null;
}

export const laneKey = (lane: Pick<RoleQueueResponse, 'workspace' | 'role'>) => `${lane.workspace}/${lane.role.slug}`;

class AgentsOverview {
	data = $state<MeAgentsResponse | null>(null);
	error = $state('');
	loading = $state(false);
	overlay = $state<Record<string, LaneOverlay>>({});
	showAll = $state<Record<string, true>>({});
	private gen = 0;
	constructor() {
		authStore.onIdentityChange(() => {
			this.gen++;
			this.data = null;
			this.error = '';
			this.loading = false;
			this.overlay = {};
			this.showAll = {};
		});
	}

	load(): Promise<void> {
		const gen = ++this.gen;
		const sameIdentity = authStore.identityFence();
		const current = () => sameIdentity() && gen === this.gen;
		const wanted = Object.keys(this.showAll);
		this.loading = true;
		this.error = '';
		return (async () => {
			try {
				const res = await api.getMyAgents({ finished: AGENTS_FINISHED_LIMIT, queue: AGENTS_QUEUE_LIMIT });
				if (!current()) return;
				const lanes = new Map<string, RoleQueueResponse>();
				await Promise.all(wanted.map(async key => {
					const slash = key.indexOf('/');
					try {
						lanes.set(key, await api.agentRoles.queue(key.slice(0, slash), key.slice(slash + 1), AGENTS_SHOW_ALL_LIMIT));
					} catch {
						// The short queue from /me/agents still stands.
					}
				}));
				if (!current()) return;
				const agents = (res.agents ?? []).map(agent => ({
					...agent,
					lanes: (agent.lanes ?? []).map(lane => lanes.get(laneKey(lane)) ?? lane)
				}));
				this.data = { agents, degraded: res.degraded ?? [] };
				this.overlay = {};
			} catch {
				if (current()) this.error = 'Could not load your agents.';
			} finally {
				if (current()) this.loading = false;
			}
		})();
	}

	/** Show up to AGENTS_SHOW_ALL_LIMIT queue items for a lane, from now on. */
	expandLane(key: string): Promise<void> {
		this.showAll = { ...this.showAll, [key]: true };
		return this.load();
	}

	setOverlay(key: string, overlay: LaneOverlay) {
		this.overlay = { ...this.overlay, [key]: overlay };
	}

	clearOverlay(key: string) {
		if (!(key in this.overlay)) return;
		const next = { ...this.overlay };
		delete next[key];
		this.overlay = next;
	}
}

export const agentsOverview = new AgentsOverview();
