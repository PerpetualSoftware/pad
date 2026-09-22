// Test double for Sidebar.quickAdd.svelte.test.ts (BUG-3115). The real
// collectionStore keeps its list in `$state`, so every read of an element
// returns the SAME proxy. A plain array would hand the component a raw object
// that each `$state` assignment re-wraps in a new proxy, and identity checks
// in the component would then fail for a reason the app never has.
const state = $state({
	collections: [
		{
			id: 'c1',
			slug: 'tasks',
			name: 'Tasks',
			icon: '✅',
			prefix: 'TASK',
			schema: '{"fields":[]}',
			settings: '{}',
			sort_order: 0,
		},
	],
});

export const collectionStore = {
	get collections() {
		return state.collections;
	},
	loadCollections: () => Promise.resolve(),
};
