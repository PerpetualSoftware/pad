// Reactive stand-ins for the stores an index-backed page reads (TASK-2231):
// the local index, the collection list and the index entry helper. Plain
// getters on a vi.mock object are not reactive, so a page that DERIVES from
// them would never see a change a test makes; these are `$state`, so a test
// can fail the index, recover it on retry, or land the collection list late.
//
// Use from a vi.mock factory, which may not close over an import:
//   vi.mock('$lib/stores/localIndex.svelte', async () => ({
//     localIndex: (await import('../test/mocks/indexedPageStores.svelte')).localIndex }));

import type { BootstrapState } from '$lib/stores/localIndex.svelte';

type Row = Record<string, unknown>;

export const fake = $state({
	indexState: 'ready' as BootstrapState,
	/** What the index state becomes on the next `enterWorkspaceIndex`. */
	indexOnEntry: null as BootstrapState | null,
	revoked: false,
	rows: [] as Row[],
	collectionsFresh: true,
	collections: [] as Row[],
	/** How many coming `ensureCollections` calls reject before one lands. */
	collectionFailures: 0,
	/** When true, `ensureCollections` waits for `releaseCollections()`. */
	holdCollections: false,
	/** Calls made to the index entry helper and to ensureCollections. */
	entries: 0,
	ensures: 0,
});

export function resetFake(): void {
	fake.indexState = 'ready';
	fake.indexOnEntry = null;
	fake.revoked = false;
	fake.rows = [];
	fake.collectionsFresh = true;
	fake.collections = [];
	fake.collectionFailures = 0;
	fake.holdCollections = false;
	releaseHeld = () => {};
	fake.entries = 0;
	fake.ensures = 0;
}

let releaseHeld: () => void = () => {};
/** Lets a held `ensureCollections` land. */
export function releaseCollections(): void {
	releaseHeld();
}

export const localIndex = {
	bootstrapStateFor: (): BootstrapState => fake.indexState,
	accessRevokedFor: (): boolean => fake.revoked,
	getAll: (): Row[] => fake.rows.map((r) => ({ ...r })),
};

export const collectionStore = {
	collectionsAreFreshFor: (): boolean => fake.collectionsFresh,
	get collections(): Row[] {
		return fake.collections;
	},
	async ensureCollections(): Promise<void> {
		fake.ensures++;
		if (fake.holdCollections) await new Promise<void>((r) => { releaseHeld = r; });
		await Promise.resolve();
		if (fake.collectionFailures > 0) {
			fake.collectionFailures--;
			throw new Error('Service unavailable');
		}
		fake.collectionsFresh = true;
	},
};

export async function enterWorkspaceIndex(): Promise<boolean> {
	fake.entries++;
	await Promise.resolve();
	if (fake.indexOnEntry) {
		fake.indexState = fake.indexOnEntry;
		fake.indexOnEntry = null;
	}
	return fake.indexState === 'ready';
}
