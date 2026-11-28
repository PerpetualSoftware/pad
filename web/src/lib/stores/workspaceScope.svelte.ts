// The workspace an item view answers permission and collection questions
// about (TASK-4902).
//
// On a workspace route that is the route's workspace, which the global
// `workspaceStore` / `collectionStore` describe. Home is outside every
// workspace route, and the root layout points those stores at a SHELL
// workspace for the chrome, so an item opened there from another workspace
// would read the shell workspace's rights: owner controls on a guest item.
// The item pane on home provides its own scope, fetched from that item's
// workspace, through context.
//
// `workspaceScope()` must be called during component initialisation. With no
// scope in context it forwards to the global stores only while the route has a
// workspace param, and is closed otherwise: no membership, every predicate
// false, no collections. A component that ends up outside the pane's context
// then shows read-only rather than the shell workspace's rights.
import { getContext, setContext } from 'svelte';
import { page } from '$app/state';
import { api } from '$lib/api/client';
import type { Collection, WorkspaceMembership } from '$lib/types';
import * as perms from '$lib/utils/permissions';
import { authStore } from './auth.svelte';
import { workspaceStore } from './workspace.svelte';
import { collectionStore } from './collections.svelte';

export interface WorkspaceScope {
	readonly slug: string;
	readonly username: string;
	readonly isOwner: boolean;
	readonly currentMembership: WorkspaceMembership | null;
	canEditItem(item: { id: string; collection_id: string }): boolean;
	canEditCollection(collectionId: string): boolean;
	readonly collections: Collection[];
	/** The workspace `collections` was loaded for, or null before a load lands. */
	readonly collectionsWorkspace: string | null;
	collectionsAreFreshFor(slug: string): boolean;
	/** Refetch the collection list; a slug naming another workspace is a no-op. */
	reloadCollections(slug?: string): Promise<void>;
}

const KEY = Symbol('workspace-scope');

const NO_COLLECTIONS: Collection[] = [];

const onWorkspaceRoute = () => !!page.params.workspace;

// The route's workspace, through the global stores, while there is one;
// closed off workspace routes.
const ROUTE_SCOPE: WorkspaceScope = {
	get slug() {
		return page.params.workspace ?? '';
	},
	get username() {
		return onWorkspaceRoute() ? (page.params.username ?? '') : '';
	},
	get isOwner() {
		return onWorkspaceRoute() && workspaceStore.isOwner;
	},
	get currentMembership() {
		return onWorkspaceRoute() ? workspaceStore.currentMembership : null;
	},
	canEditItem(item) {
		return onWorkspaceRoute() && workspaceStore.canEditItem(item);
	},
	canEditCollection(collectionId) {
		return onWorkspaceRoute() && workspaceStore.canEditCollection(collectionId);
	},
	get collections() {
		return onWorkspaceRoute() ? collectionStore.collections : NO_COLLECTIONS;
	},
	get collectionsWorkspace() {
		return onWorkspaceRoute() ? collectionStore.collectionsWorkspace : null;
	},
	collectionsAreFreshFor(slug) {
		return onWorkspaceRoute() && collectionStore.collectionsAreFreshFor(slug);
	},
	async reloadCollections(slug) {
		const ws = slug ?? page.params.workspace;
		if (!onWorkspaceRoute() || !ws) return;
		await collectionStore.loadCollections(ws);
	},
};

/** Provide `scope` to this component's subtree. Call during initialisation. */
export function setWorkspaceScope(scope: WorkspaceScope): void {
	setContext(KEY, scope);
}

/** The scope in context, else the route's. Call during initialisation. */
export function workspaceScope(): WorkspaceScope {
	return getContext<WorkspaceScope | undefined>(KEY) ?? ROUTE_SCOPE;
}

/**
 * A scope for one item's workspace, fetched from that workspace's `/me` and
 * collection list in parallel. Until both answer, and after either fails,
 * membership is null and every predicate is false. A response that lands
 * after `dispose()` or after an identity change is dropped, and an identity
 * change clears an answer already held.
 *
 * State is written only from the promise callbacks and the identity listener,
 * so the scope can be created at component initialisation and read freely
 * from effects.
 */
export function createItemWorkspaceScope(
	owner: string,
	slug: string,
): WorkspaceScope & { dispose(): void } {
	let membership = $state<WorkspaceMembership | null>(null);
	let collections = $state<Collection[]>(NO_COLLECTIONS);
	let loaded = $state(false);
	let disposed = false;
	let gen = 0;

	function clear() {
		gen++;
		membership = null;
		collections = NO_COLLECTIONS;
		loaded = false;
	}

	const stopIdentity = authStore.onIdentityChange(clear);

	function load() {
		const my = ++gen;
		const sameIdentity = authStore.identityFence();
		const current = () => !disposed && my === gen && sameIdentity();
		Promise.all([api.workspaces.me(slug), api.collections.list(slug)]).then(
			([m, list]) => {
				if (!current()) return;
				membership = m ?? null;
				collections = list ?? NO_COLLECTIONS;
				loaded = true;
			},
			() => {
				if (!current()) return;
				membership = null;
				collections = NO_COLLECTIONS;
				loaded = false;
			},
		);
	}

	load();

	return {
		get slug() {
			return slug;
		},
		get username() {
			return owner;
		},
		get isOwner() {
			return membership?.role === 'owner';
		},
		get currentMembership() {
			return membership;
		},
		canEditItem(item) {
			return perms.canEditItem(membership, item);
		},
		canEditCollection(collectionId) {
			return perms.canEditCollection(membership, collectionId);
		},
		get collections() {
			return collections;
		},
		get collectionsWorkspace() {
			return loaded ? slug : null;
		},
		collectionsAreFreshFor(ws) {
			return loaded && ws === slug;
		},
		async reloadCollections(ws) {
			if (disposed || (ws !== undefined && ws !== slug)) return;
			const my = gen;
			const sameIdentity = authStore.identityFence();
			try {
				const list = await api.collections.list(slug);
				if (disposed || my !== gen || !sameIdentity() || !loaded) return;
				collections = list ?? NO_COLLECTIONS;
			} catch {
				// The list already held stays; the next reload retries.
			}
		},
		dispose() {
			if (disposed) return;
			disposed = true;
			gen++;
			stopIdentity();
		},
	};
}
