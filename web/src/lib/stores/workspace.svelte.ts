import { api } from '$lib/api/client';
import type { Workspace } from '$lib/types';

let workspaces = $state<Workspace[]>([]);
let current = $state<Workspace | null>(null);
let loading = $state(false);

export const workspaceStore = {
	get workspaces() { return workspaces; },
	get current() { return current; },
	get loading() { return loading; },

	async loadAll() {
		loading = true;
		try {
			workspaces = await api.workspaces.list();
		} finally {
			loading = false;
		}
	},

	async setCurrent(ws: Workspace | string) {
		if (typeof ws === 'object') {
			current = ws;
			return;
		}
		const found = workspaces.find(w => w.slug === ws);
		if (found) {
			current = found;
			return;
		}
		try {
			current = await api.workspaces.get(ws);
		} catch {
			current = null;
		}
	},

	async create(data: { name: string; description?: string; template?: string }) {
		const ws = await api.workspaces.create(data);
		workspaces = [...workspaces, ws];
		current = ws;
		return ws;
	},
};
