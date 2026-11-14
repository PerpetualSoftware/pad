// A reactive workspaceStore stand-in: `current` is $state, so a component
// reading it re-renders when a test changes it (TASK-4545).
type Ws = { id: string; slug: string; name: string; owner_username: string };
let current = $state<Ws | null>(null);
export const workspaceStore = {
	get current() { return current; },
	set current(v: Ws | null) { current = v; },
	workspaces: [] as Ws[],
	loadAll: async () => {}
};
