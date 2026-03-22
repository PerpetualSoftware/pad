import type {
	Workspace,
	WorkspaceCreate,
	Collection,
	CollectionCreate,
	CollectionUpdate,
	Item,
	ItemCreate,
	ItemUpdate,
	ItemLink,
	ItemLinkCreate,
	Comment,
	CommentCreate,
	Version,
	DashboardResponse,
	SearchResponse,
	Activity,
	ApiError,
	WorkspaceTemplate,
	ConventionLibraryResponse,
	LibraryConvention,
	PlaybookLibraryResponse,
	LibraryPlaybook
} from '$lib/types';

const BASE = '/api/v1';

class PadApiError extends Error {
	code: string;
	constructor(err: ApiError) {
		super(err.message);
		this.code = err.code;
	}
}

async function request<T>(path: string, options?: RequestInit): Promise<T> {
	const resp = await fetch(BASE + path, {
		headers: { 'Content-Type': 'application/json' },
		...options
	});
	if (!resp.ok) {
		const body = await resp.json().catch(() => null);
		if (body?.error) throw new PadApiError(body.error);
		throw new Error(`API error: ${resp.status}`);
	}
	if (resp.status === 204) return undefined as T;
	return resp.json();
}

function qs(params?: Record<string, string | number | boolean | undefined>): string {
	if (!params) return '';
	const filtered: Record<string, string> = {};
	for (const [k, v] of Object.entries(params)) {
		if (v !== undefined && v !== '') filtered[k] = String(v);
	}
	const str = new URLSearchParams(filtered).toString();
	return str ? '?' + str : '';
}

export const api = {
	// ── Templates ─────────────────────────────────────────────────────────────

	templates: {
		list: () => request<WorkspaceTemplate[]>('/templates'),
	},

	// ── Workspaces ────────────────────────────────────────────────────────────

	workspaces: {
		list: () => request<Workspace[]>('/workspaces'),

		create: (data: WorkspaceCreate) =>
			request<Workspace>('/workspaces', {
				method: 'POST',
				body: JSON.stringify(data)
			}),

		get: (slug: string) => request<Workspace>(`/workspaces/${slug}`),

		update: (slug: string, data: Partial<WorkspaceCreate>) =>
			request<Workspace>(`/workspaces/${slug}`, {
				method: 'PATCH',
				body: JSON.stringify(data)
			}),

		delete: (slug: string) =>
			request<void>(`/workspaces/${slug}`, { method: 'DELETE' })
	},

	// ── Collections ───────────────────────────────────────────────────────────

	collections: {
		list: (ws: string) =>
			request<Collection[]>(`/workspaces/${ws}/collections`),

		create: (ws: string, data: CollectionCreate) =>
			request<Collection>(`/workspaces/${ws}/collections`, {
				method: 'POST',
				body: JSON.stringify(data)
			}),

		get: (ws: string, slug: string) =>
			request<Collection>(`/workspaces/${ws}/collections/${slug}`),

		update: (ws: string, slug: string, data: CollectionUpdate) =>
			request<Collection>(`/workspaces/${ws}/collections/${slug}`, {
				method: 'PATCH',
				body: JSON.stringify(data)
			})
	},

	// ── Items ─────────────────────────────────────────────────────────────────

	items: {
		/** Cross-collection item listing with optional query params. */
		list: (
			ws: string,
			params?: Record<string, string | number | boolean | undefined>
		) => request<Item[]>(`/workspaces/${ws}/items${qs(params)}`),

		/** Items within a specific collection. */
		listByCollection: (
			ws: string,
			coll: string,
			params?: Record<string, string | number | boolean | undefined>
		) =>
			request<Item[]>(
				`/workspaces/${ws}/collections/${coll}/items${qs(params)}`
			),

		create: (ws: string, coll: string, data: ItemCreate) =>
			request<Item>(`/workspaces/${ws}/collections/${coll}/items`, {
				method: 'POST',
				body: JSON.stringify(data)
			}),

		get: (ws: string, slug: string) =>
			request<Item>(`/workspaces/${ws}/items/${slug}`),

		update: (ws: string, slug: string, data: ItemUpdate) =>
			request<Item>(`/workspaces/${ws}/items/${slug}`, {
				method: 'PATCH',
				body: JSON.stringify(data)
			}),

		delete: (ws: string, slug: string) =>
			request<void>(`/workspaces/${ws}/items/${slug}`, {
				method: 'DELETE'
			}),

		restore: (ws: string, slug: string) =>
			request<Item>(`/workspaces/${ws}/items/${slug}/restore`, {
				method: 'POST'
			}),

		/** Get tasks linked to a phase item */
		tasks: (ws: string, slug: string) =>
			request<Item[]>(`/workspaces/${ws}/items/${slug}/tasks`),

		/** Get task progress for all phases in a workspace */
		phasesProgress: (ws: string) =>
			request<{phase_id: string; total: number; done: number}[]>(`/workspaces/${ws}/phases-progress`)
	},

	// ── Versions ──────────────────────────────────────────────────────────────

	versions: {
		list: (ws: string, itemSlug: string) =>
			request<Version[]>(`/workspaces/${ws}/items/${itemSlug}/versions`),

		restore: (ws: string, itemSlug: string, versionId: string) =>
			request<Item>(`/workspaces/${ws}/items/${itemSlug}/versions/${versionId}/restore`, {
				method: 'POST'
			})
	},

	// ── Links ─────────────────────────────────────────────────────────────────

	links: {
		list: (ws: string, itemSlug: string) =>
			request<ItemLink[]>(`/workspaces/${ws}/items/${itemSlug}/links`),

		create: (ws: string, itemSlug: string, data: ItemLinkCreate) =>
			request<ItemLink>(`/workspaces/${ws}/items/${itemSlug}/links`, {
				method: 'POST',
				body: JSON.stringify(data)
			})
	},

	// ── Comments ──────────────────────────────────────────────────────────────

	comments: {
		list: (ws: string, itemSlug: string) =>
			request<Comment[]>(`/workspaces/${ws}/items/${itemSlug}/comments`),

		create: (ws: string, itemSlug: string, data: CommentCreate) =>
			request<Comment>(`/workspaces/${ws}/items/${itemSlug}/comments`, {
				method: 'POST',
				body: JSON.stringify(data)
			}),

		delete: (ws: string, commentId: string) =>
			request<void>(`/workspaces/${ws}/comments/${commentId}`, {
				method: 'DELETE'
			})
	},

	// ── Dashboard ─────────────────────────────────────────────────────────────

	dashboard: {
		get: (ws: string) =>
			request<DashboardResponse>(`/workspaces/${ws}/dashboard`)
	},

	// ── Search ────────────────────────────────────────────────────────────────

	search: (query: string, workspace?: string) => {
		const params: Record<string, string> = { q: query };
		if (workspace) params.workspace = workspace;
		return request<SearchResponse>(`/search?${new URLSearchParams(params).toString()}`);
	},

	// ── Activity ──────────────────────────────────────────────────────────────

	activity: {
		list: (
			ws: string,
			params?: Record<string, string | number | boolean | undefined>
		) => request<Activity[]>(`/workspaces/${ws}/activity${qs(params)}`)
	},

	// ── Convention Library ────────────────────────────────────────────────────

	library: {
		get: () => request<ConventionLibraryResponse>('/convention-library'),

		activate: (ws: string, convention: LibraryConvention) =>
			request<Item>(`/workspaces/${ws}/collections/conventions/items`, {
				method: 'POST',
				body: JSON.stringify({
					title: convention.title,
					content: convention.content,
					fields: JSON.stringify({
						status: 'active',
						trigger: convention.trigger,
						scope: convention.scope,
						priority: convention.priority
					})
				})
			}),

		getPlaybooks: () => request<PlaybookLibraryResponse>('/playbook-library'),

		activatePlaybook: (ws: string, playbook: LibraryPlaybook) =>
			request<Item>(`/workspaces/${ws}/collections/playbooks/items`, {
				method: 'POST',
				body: JSON.stringify({
					title: playbook.title,
					content: playbook.content,
					fields: JSON.stringify({
						status: 'active',
						trigger: playbook.trigger,
						scope: playbook.scope
					})
				})
			})
	}
};

export { PadApiError };
