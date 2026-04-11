import type {
	Workspace,
	WorkspaceCreate,
	WorkspaceUpdate,
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
	View,
	User,
	UserProfileUpdate,
	APIToken,
	APITokenWithSecret,
	Reaction,
	TimelineResponse,
	AgentRole,
	AgentRoleCreate,
	AgentRoleUpdate,
	RoleBoardLane,
	ChangesResponse,
	CollectionGrant,
	ItemGrant,
	ShareLink
} from '$lib/types';

const BASE = '/api/v1';

class PadApiError extends Error {
	code: string;
	constructor(err: ApiError) {
		super(err.message);
		this.code = err.code;
	}
}

function getCSRFToken(): string | null {
	if (typeof document === 'undefined') return null;
	const match = document.cookie.match(/(?:^|;\s*)pad_csrf=([^;]+)/);
	return match ? match[1] : null;
}

async function request<T>(path: string, options?: RequestInit): Promise<T> {
	const headers: Record<string, string> = { 'Content-Type': 'application/json' };

	// Attach CSRF token for state-changing requests
	const method = options?.method?.toUpperCase();
	if (method && method !== 'GET' && method !== 'HEAD') {
		const csrf = getCSRFToken();
		if (csrf) headers['X-CSRF-Token'] = csrf;
	}

	const resp = await fetch(BASE + path, {
		headers,
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

export interface HealthResponse {
	status: string;
	version?: string;
	commit?: string;
	build_time?: string;
}

export interface AuthSession {
	authenticated: boolean;
	setup_required: boolean;
	setup_method?: 'local_cli' | 'docker_exec' | 'cloud';
	auth_method: 'password' | 'cloud';
	user?: { id: string; email: string; username: string; name: string; role: string };
}

export interface LoginResponse {
	user?: { id: string; email: string; username: string; name: string; role: string };
	token?: string;
	requires_2fa?: boolean;
	challenge_token?: string;
}

export const api = {
	// ── Health / Version ──────────────────────────────────────────────────────

	health: () => request<HealthResponse>('/health'),

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

		update: (slug: string, data: WorkspaceUpdate) =>
			request<Workspace>(`/workspaces/${slug}`, {
				method: 'PATCH',
				body: JSON.stringify(data)
			}),

		delete: (slug: string) =>
			request<void>(`/workspaces/${slug}`, { method: 'DELETE' }),

		reorder: (updates: { slug: string; sort_order: number }[]) =>
			request<void>('/workspaces/reorder', {
				method: 'PUT',
				body: JSON.stringify(updates)
			})
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

	// ── Agent Roles ──────────────────────────────────────────────────────────

	agentRoles: {
		list: (ws: string) =>
			request<AgentRole[]>(`/workspaces/${ws}/agent-roles`),

		create: (ws: string, data: AgentRoleCreate) =>
			request<AgentRole>(`/workspaces/${ws}/agent-roles`, {
				method: 'POST',
				body: JSON.stringify(data)
			}),

		get: (ws: string, idOrSlug: string) =>
			request<AgentRole>(`/workspaces/${ws}/agent-roles/${idOrSlug}`),

		update: (ws: string, idOrSlug: string, data: AgentRoleUpdate) =>
			request<AgentRole>(`/workspaces/${ws}/agent-roles/${idOrSlug}`, {
				method: 'PATCH',
				body: JSON.stringify(data)
			}),

		delete: (ws: string, idOrSlug: string) =>
			request<void>(`/workspaces/${ws}/agent-roles/${idOrSlug}`, {
				method: 'DELETE'
			}),

		board: (ws: string, assignedUserId?: string) => {
			const params = assignedUserId ? `?assigned_user_id=${assignedUserId}` : '';
			return request<{ lanes: RoleBoardLane[] }>(`/workspaces/${ws}/roles/board${params}`);
		},

		reorder: (ws: string, updates: { item_id: string; role_sort_order: number }[]) =>
			request<void>(`/workspaces/${ws}/roles/board/reorder`, {
				method: 'PUT',
				body: JSON.stringify(updates)
			}),
		reorderLanes: (ws: string, updates: { role_id: string; sort_order: number }[]) =>
			request<void>(`/workspaces/${ws}/roles/board/lane-order`, {
				method: 'PUT',
				body: JSON.stringify(updates)
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

		/** Get child items linked to a parent item */
		children: (ws: string, slug: string) =>
			request<Item[]>(`/workspaces/${ws}/items/${slug}/children`),

		/** Get completion progress for an item's children */
		progress: (ws: string, slug: string) =>
			request<{total: number; done: number; percentage: number}>(`/workspaces/${ws}/items/${slug}/progress`),

		/** @deprecated Use children() */
		tasks: (ws: string, slug: string) =>
			request<Item[]>(`/workspaces/${ws}/items/${slug}/children`),

		/** @deprecated Use progress() per-item instead */
		plansProgress: (ws: string) =>
			request<{item_id: string; total: number; done: number}[]>(`/workspaces/${ws}/plans-progress`)
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
			}),

		delete: (ws: string, linkId: string) =>
			request<void>(`/workspaces/${ws}/links/${linkId}`, {
				method: 'DELETE'
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
			}),

		reply: (ws: string, commentId: string, data: CommentCreate) =>
			request<Comment>(`/workspaces/${ws}/comments/${commentId}/replies`, {
				method: 'POST',
				body: JSON.stringify(data)
			}),

		addReaction: (ws: string, commentId: string, emoji: string) =>
			request<Reaction>(`/workspaces/${ws}/comments/${commentId}/reactions`, {
				method: 'POST',
				body: JSON.stringify({ emoji })
			}),

		removeReaction: (ws: string, commentId: string, emoji: string) =>
			request<void>(`/workspaces/${ws}/comments/${commentId}/reactions/${encodeURIComponent(emoji)}`, {
				method: 'DELETE'
			})
	},

	// ── Timeline ──────────────────────────────────────────────────────────────

	timeline: {
		list: (ws: string, itemSlug: string, params?: { limit?: number; before?: string; before_id?: string }) => {
			const qs = new URLSearchParams();
			if (params?.limit != null) qs.set('limit', String(params.limit));
			if (params?.before) qs.set('before', params.before);
			if (params?.before_id) qs.set('before_id', params.before_id);
			const suffix = qs.toString() ? `?${qs}` : '';
			return request<TimelineResponse>(`/workspaces/${ws}/items/${itemSlug}/timeline${suffix}`);
		}
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

	// ── Incremental Sync ─────────────────────────────────────────────────────

	changes: {
		/** Fetch items modified since the given timestamp (unix ms). */
		since: (ws: string, sinceMs: number) =>
			request<ChangesResponse>(`/workspaces/${ws}/changes?since=${sinceMs}`)
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
						category: convention.category,
						trigger: convention.trigger,
						scope: convention.surfaces?.[0] ?? 'all',
						priority: convention.enforcement,
						enforcement: convention.enforcement,
						surfaces: convention.surfaces,
						commands: convention.commands ?? [],
						convention: {
							category: convention.category,
							trigger: convention.trigger,
							surfaces: convention.surfaces,
							enforcement: convention.enforcement,
							commands: convention.commands ?? []
						}
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
		remove: (ws: string, userId: string, revokeGrants: boolean = true) =>
			request<void>(`/workspaces/${ws}/members/${userId}?revoke_grants=${revokeGrants}`, { method: 'DELETE' }),
		updateRole: (ws: string, userId: string, role: string) =>
			request<{ user_id: string; role: string }>(`/workspaces/${ws}/members/${userId}`, {
				method: 'PATCH',
				body: JSON.stringify({ role })
			}),
		cancelInvitation: (ws: string, invitationId: string) =>
			request<void>(`/workspaces/${ws}/members/invitations/${invitationId}`, { method: 'DELETE' }),
		acceptInvitation: (code: string) =>
			request<{ accepted: boolean; workspace_id: string; role: string }>(`/invitations/${code}/accept`, {
				method: 'POST'
			}),
		getMemberCollectionAccess: (ws: string, userId: string) =>
			request<{ collection_access: string; collection_ids: string[] }>(`/workspaces/${ws}/members/${userId}/collection-access`),
		setMemberCollectionAccess: (ws: string, userId: string, mode: string, collectionIDs: string[]) =>
			request<{ collection_access: string; collection_ids: string[] }>(`/workspaces/${ws}/members/${userId}/collection-access`, {
				method: 'PUT',
				body: JSON.stringify({ mode, collection_ids: collectionIDs })
			})
	},

	// ── Grants ───────────────────────────────────────────────────────────────

	grants: {
		listCollectionGrants: (ws: string, collSlug: string) =>
			request<CollectionGrant[]>(`/workspaces/${ws}/collections/${collSlug}/grants`),
		createCollectionGrant: (ws: string, collSlug: string, email: string, permission: string) =>
			request<CollectionGrant>(`/workspaces/${ws}/collections/${collSlug}/grants`, {
				method: 'POST',
				body: JSON.stringify({ email, permission })
			}),
		deleteCollectionGrant: (ws: string, collSlug: string, grantId: string) =>
			request<void>(`/workspaces/${ws}/collections/${collSlug}/grants/${grantId}`, { method: 'DELETE' }),
		listItemGrants: (ws: string, itemSlug: string) =>
			request<ItemGrant[]>(`/workspaces/${ws}/items/${itemSlug}/grants`),
		createItemGrant: (ws: string, itemSlug: string, email: string, permission: string) =>
			request<ItemGrant>(`/workspaces/${ws}/items/${itemSlug}/grants`, {
				method: 'POST',
				body: JSON.stringify({ email, permission })
			}),
		deleteItemGrant: (ws: string, itemSlug: string, grantId: string) =>
			request<void>(`/workspaces/${ws}/items/${itemSlug}/grants/${grantId}`, { method: 'DELETE' }),
		listUserGrants: (ws: string, userId: string) =>
			request<{ collection_grants: CollectionGrant[]; item_grants: ItemGrant[] }>(`/workspaces/${ws}/users/${userId}/grants`),
	},

	// ── Share Links ─────────────────────────────────────────────────────────

	shareLinks: {
		listItemShareLinks: (ws: string, itemSlug: string) =>
			request<ShareLink[]>(`/workspaces/${ws}/items/${itemSlug}/share-links`),
		createItemShareLink: (ws: string, itemSlug: string) =>
			request<ShareLink>(`/workspaces/${ws}/items/${itemSlug}/share-links`, { method: 'POST' }),
		listCollectionShareLinks: (ws: string, collSlug: string) =>
			request<ShareLink[]>(`/workspaces/${ws}/collections/${collSlug}/share-links`),
		createCollectionShareLink: (ws: string, collSlug: string) =>
			request<ShareLink>(`/workspaces/${ws}/collections/${collSlug}/share-links`, { method: 'POST' }),
		deleteShareLink: (ws: string, linkId: string) =>
			request<void>(`/workspaces/${ws}/share-links/${linkId}`, { method: 'DELETE' }),
	},

	// ── Public Share (no auth) ──────────────────────────────────────────────

	share: {
		get: (token: string, password?: string) => {
			const headers: Record<string, string> = {};
			if (password) headers['X-Share-Password'] = password;
			return fetch(`${BASE}/s/${token}`, { credentials: 'same-origin', headers }).then(async (resp) => {
				if (!resp.ok) {
					const body = await resp.json().catch(() => null);
					if (body?.error) throw new PadApiError(body.error);
					throw new Error(`API error: ${resp.status}`);
				}
				return resp.json();
			});
		},
	},

	// ── Auth ──────────────────────────────────────────────────────────────────

	auth: {
		session: (): Promise<AuthSession> => fetch(BASE + '/auth/session', { credentials: 'same-origin' }).then((r) => r.json()),
		login: (email: string, password: string) =>
			request<LoginResponse>('/auth/login', {
				method: 'POST',
				body: JSON.stringify({ email, password })
			}),
		verify2FA: (challengeToken: string, code?: string, recoveryCode?: string) =>
			request<{ user: { id: string; email: string; username: string; name: string; role: string }; token: string }>('/auth/2fa/login-verify', {
				method: 'POST',
				body: JSON.stringify({ challenge_token: challengeToken, code: code || undefined, recovery_code: recoveryCode || undefined })
			}),
		register: (email: string, name: string, password: string, username?: string, invitation_code?: string) =>
			request<{ user: { id: string; email: string; username: string; name: string; role: string }; token: string }>('/auth/register', {
				method: 'POST',
				body: JSON.stringify({ email, name, password, ...(username ? { username } : {}), ...(invitation_code ? { invitation_code } : {}) })
			}),
		checkUsername: (username: string) =>
			request<{ available: boolean; reason: string | null; message: string | null }>(`/auth/check-username?username=${encodeURIComponent(username)}`),
		logout: () => request<{ ok: boolean }>('/auth/logout', { method: 'POST' }),
		forgotPassword: (email: string) =>
			request<{ ok: boolean; message: string }>('/auth/forgot-password', {
				method: 'POST',
				body: JSON.stringify({ email })
			}),
		resetPassword: (token: string, password: string) =>
			request<{ ok: boolean; user: { id: string; email: string; username: string; name: string; role: string }; token: string }>('/auth/reset-password', {
				method: 'POST',
				body: JSON.stringify({ token, password })
			}),
		me: () => request<User>('/auth/me'),
		updateProfile: (data: UserProfileUpdate) =>
			request<User>('/auth/me', {
				method: 'PATCH',
				body: JSON.stringify(data)
			}),
		tokens: {
			list: () => request<APIToken[]>('/auth/tokens'),
			create: (name: string) =>
				request<APITokenWithSecret>('/auth/tokens', {
					method: 'POST',
					body: JSON.stringify({ name })
				}),
			delete: (tokenId: string) =>
				request<void>(`/auth/tokens/${tokenId}`, { method: 'DELETE' })
		}
	},

	// ── Admin ────────────────────────────────────────────────────────────────

	admin: {
		getSettings: () => request<Record<string, string>>('/admin/settings'),
		updateSettings: (settings: Record<string, string>) =>
			request<{ ok: boolean }>('/admin/settings', {
				method: 'PATCH',
				body: JSON.stringify(settings)
			}),
		testEmail: (to?: string) =>
			request<{ ok: boolean; sent_to: string }>('/admin/test-email', {
				method: 'POST',
				body: JSON.stringify(to ? { to } : {})
			})
	}
};

export { PadApiError };
