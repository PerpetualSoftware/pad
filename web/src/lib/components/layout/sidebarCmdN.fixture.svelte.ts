// Test double for Sidebar.cmdN.svelte.test.ts (BUG-3258). State lives in
// $state so a change re-runs the component's deriveds, as the real store does.
export const ws = $state({
	current: { slug: 'ws-a', owner_username: 'u', is_guest: false } as {
		slug: string;
		owner_username: string;
		is_guest: boolean;
	},
	membershipKnown: false,
	editable: true,
});

export const workspaceStore = {
	get current() {
		return ws.current;
	},
	get membershipKnown() {
		return ws.membershipKnown;
	},
	// Like the real store: no membership answer yet, nothing is editable.
	canEditCollection: () => ws.membershipKnown && ws.editable,
};
