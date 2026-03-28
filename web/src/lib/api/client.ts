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
	LibraryPlaybook,
	View
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
		credentials: 'same-origin',
		...options
	});
	if (resp.status === 401) {
		// Redirect to login page (avoid infinite loop)
		if (typeof window !== 'undefined' && !window.location.pathname.startsWith('/login')) {
			window.location.href = '/login';
		}
		throw new PadApiError({ code: 'unauthorized', message: 'Authentication required' });
	}
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
			}),

		delete: (ws: string, slug: string) =>
			request<void>(`/workspaces/${ws}/collections/${slug}`, {
				method: 'DELETE'
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

		move: (ws: string, slug: string, targetCollection: string, fieldOverrides?: Record<string, any>) =>
			request<Item>(`/workspaces/${ws}/items/${slug}/move`, {
				method: 'POST',
				body: JSON.stringify({
					target_collection: targetCollection,
					field_overrides: fieldOverrides,
					source: 'web'
				})
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
			}),

		/** Activity feed for a single item (all changes, not just content versions). */
		activity: (ws: string, itemSlug: string) =>
			request<Activity[]>(`/workspaces/${ws}/items/${itemSlug}/activity`)
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

	// ── Views ─────────────────────────────────────────────────────────────────

	views: {
		list: (ws: string, coll: string) =>
			request<View[]>(`/workspaces/${ws}/collections/${coll}/views`),

		create: (ws: string, coll: string, data: { name: string; view_type: string; config: string }) =>
			request<View>(`/workspaces/${ws}/collections/${coll}/views`, {
				method: 'POST',
				body: JSON.stringify(data)
			}),

		update: (ws: string, coll: string, viewId: string, data: { name?: string; view_type?: string; config?: string; sort_order?: number }) =>
			request<View>(`/workspaces/${ws}/collections/${coll}/views/${viewId}`, {
				method: 'PATCH',
				body: JSON.stringify(data)
			}),

		delete: (ws: string, coll: string, viewId: string) =>
			request<void>(`/workspaces/${ws}/collections/${coll}/views/${viewId}`, {
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
	},

	// ── Raw requests ──────────────────────────────────────────────────────────

	raw: {
		post: (path: string, data: unknown) =>
			request<any>(path, {
				method: 'POST',
				body: JSON.stringify(data)
			})
	},

	// ── Members ──────────────────────────────────────────────────────────────

	members: {
		list: (ws: string) =>
			request<{
				members: { workspace_id: string; user_id: string; role: string; created_at: string; user_name: string; user_email: string }[];
				invitations: { id: string; email: string; role: string; code: string; join_url?: string; created_at: string }[];
			}>(`/workspaces/${ws}/members`),
		invite: (ws: string, email: string, role: string) =>
			request<{ added?: boolean; invited?: boolean; code?: string; join_url?: string; email: string; role: string; name?: string; user_id?: string }>(
				`/workspaces/${ws}/members/invite`,
				{ method: 'POST', body: JSON.stringify({ email, role }) }
			),
		remove: (ws: string, userId: string) =>
			request<void>(`/workspaces/${ws}/members/${userId}`, { method: 'DELETE' }),
		updateRole: (ws: string, userId: string, role: string) =>
			request<{ user_id: string; role: string }>(`/workspaces/${ws}/members/${userId}`, {
				method: 'PATCH',
				body: JSON.stringify({ role })
			}),
		acceptInvitation: (code: string) =>
			request<{ accepted: boolean; workspace_id: string; role: string }>(`/invitations/${code}/accept`, {
				method: 'POST'
			})
	},

	// ── Auth ──────────────────────────────────────────────────────────────────

	auth: {
		session: (): Promise<{ authenticated: boolean; needs_setup?: boolean; user?: { id: string; email: string; name: string; role: string } }> =>
			fetch(BASE + '/auth/session', { credentials: 'same-origin' }).then((r) => r.json()),
		login: (email: string, password: string) =>
			request<{ user: { id: string; email: string; name: string; role: string }; token: string }>('/auth/login', {
				method: 'POST',
				body: JSON.stringify({ email, password })
			}),
		register: (email: string, name: string, password: string, invitation_code?: string) =>
			request<{ user: { id: string; email: string; name: string; role: string }; token: string }>('/auth/register', {
				method: 'POST',
				body: JSON.stringify({ email, name, password, ...(invitation_code ? { invitation_code } : {}) })
			}),
		logout: () => request<{ ok: boolean }>('/auth/logout', { method: 'POST' }),
		me: () => request<{ id: string; email: string; name: string; role: string }>('/auth/me')
	}
};

export { PadApiError };
