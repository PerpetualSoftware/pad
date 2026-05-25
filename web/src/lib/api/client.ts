import type {
	Workspace,
	WorkspaceCreate,
	WorkspaceUpdate,
	Collection,
	CollectionCreate,
	CollectionUpdate,
	Backlink,
	Item,
	ItemChangeRow,
	ItemChangesResponse,
	ItemCreate,
	ItemIndexResponse,
	ItemIndexRow,
	ItemUpdate,
	ItemLink,
	ItemLinkCreate,
	Comment,
	CommentCreate,
	Version,
	DashboardResponse,
	SearchResponse,
	SearchFilters,
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
	WorkspaceMembership,
	ShareLink,
	TOTPSetupResponse,
	TOTPVerifyResponse,
	TOTPDisableResponse,
	AdminBillingStats,
	AttachmentUploadResult,
	AttachmentTransformRequest,
	AttachmentTransformResult,
	ServerCapabilities,
	WorkspaceStorageInfo,
	AttachmentListFilters,
	AttachmentListResponse,
	ConnectedApp,
	ClaimCodeResponse
} from '$lib/types';

const BASE = '/api/v1';

class PadApiError extends Error {
	code: string;
	/**
	 * Structured details for error codes that carry recovery
	 * information. Shape varies by code; call sites branching on
	 * `code` cast to the matching payload. The canonical consumer
	 * today is `open_children` (IDEA-1494 / BUG-1538), whose details
	 * carry `{ open_children, hidden_blocker_count, done_field,
	 * attempted_value }`. Undefined for codes that don't supply it.
	 *
	 * For `plan_limit_exceeded` (TASK-788) the shape is:
	 *   { feature: string, limit: number, current: number, plan: string, upgrade_url: string }
	 */
	details?: Record<string, unknown>;
	constructor(err: ApiError) {
		super(err.message);
		this.code = err.code;
		this.details = err.details;
	}
}

/**
 * Returns true when `err` is a PadApiError with code "plan_limit_exceeded".
 * Use this at every write-operation catch block to branch on plan-gating
 * rather than showing a generic "Failed to …" toast. TASK-788.
 */
function isPlanLimitError(err: unknown): err is PadApiError {
	return err instanceof PadApiError && err.code === 'plan_limit_exceeded';
}

/**
 * Returns a human-readable upgrade-signal message for a plan limit error.
 * Falls back to the server-supplied `err.message` if details are unavailable,
 * so the function is always safe to call. TASK-788.
 *
 * Example output:
 *   "You've reached the 3-member limit on the free plan. Upgrade to Pro →"
 */
function planLimitMessage(err: PadApiError): string {
	// The server already sends a good sentence in err.message (TASK-788).
	// We use it directly here so there is a single source of truth for the
	// wording; callers append the upgrade link separately in the UI.
	return err.message || 'Plan limit reached. Upgrade to Pro to continue.';
}

/**
 * `AccessRevokedScope` describes WHAT the 403 response was for so the
 * registered handler can purge the right slice of the local cache.
 * Per DOC-1342 design decision #3: the local cache is "what you could
 * see last time you synced" — a 403 mid-session means access was
 * revoked, and the offending entry should drop.
 *
 * The scope is the WHOLE WORKSPACE. Pad's server returns 403 from
 * the workspace-access middleware (see internal/server/middleware_auth.go:
 * `permission_denied`, `not a member of this workspace`), and item-
 * level visibility misses return 404. So a 403 on any workspace-scoped
 * endpoint means access to the workspace is gone — purging a single
 * item or collection would leave the rest of the cache stale.
 * Per-item granular purge was attempted in TASK-1360 round 1 and
 * round 2 (Codex P1 each) — both got the scope wrong; this is the
 * conservative fix.
 */
export type AccessRevokedScope = { kind: 'workspace'; workspace: string };

type AccessRevokedHandler = (scope: AccessRevokedScope) => void;

let accessRevokedHandler: AccessRevokedHandler | null = null;

/**
 * Register a single callback fired when the API client sees a 403
 * Forbidden response on a workspace-scoped item or collection
 * endpoint. The app calls this once at startup (typically from
 * +layout.svelte) to wire `localIndex` purges into the API error
 * path without forming a client.ts → store circular dependency.
 *
 * Calling this multiple times replaces the previous handler.
 * Handler failures are caught and logged; the 403 still propagates
 * to the caller as a `PadApiError`.
 */
export function setAccessRevokedHandler(handler: AccessRevokedHandler | null): void {
	accessRevokedHandler = handler;
}

/**
 * Parse a URL path and decide whether a 403 on it indicates that the
 * caller's READ access to the workspace's item set has been revoked.
 * Returns null for any path that doesn't qualify — auth, admin,
 * server health, OR workspace-scoped endpoints whose 403 doesn't
 * imply workspace-wide read loss (members list, storage usage, etc.
 * 403 from those for grant-only guests is expected and shouldn't
 * purge the cache — Codex P1 round 3 of TASK-1360).
 *
 * The whitelist is the set of endpoints the local-first read model
 * actually consumes for items:
 *
 *   GET /workspaces/{ws}/items
 *   GET /workspaces/{ws}/items/{slug}
 *   GET /workspaces/{ws}/items-index
 *   GET /workspaces/{ws}/items-changes
 *   GET /workspaces/{ws}/collections/{coll}/items
 *
 * A 403 on any of these means the local cache is stale-by-permission
 * (membership revoked or item-grant scope shrunk to nothing). A 403
 * on anything else stays opaque to the local index.
 */
function parseAccessRevokedScope(path: string): AccessRevokedScope | null {
	// Strip the BASE prefix and any leading slash / query string.
	let stripped = path.startsWith(BASE) ? path.slice(BASE.length) : path;
	const qIdx = stripped.indexOf('?');
	if (qIdx >= 0) stripped = stripped.slice(0, qIdx);
	if (stripped.startsWith('/')) stripped = stripped.slice(1);
	const parts = stripped.split('/');
	if (parts.length < 3 || parts[0] !== 'workspaces' || !parts[1]) {
		return null;
	}
	const ws = parts[1];
	const tail = parts[2];

	// /workspaces/{ws}/items-index   |  /workspaces/{ws}/items-changes
	if (parts.length === 3 && (tail === 'items-index' || tail === 'items-changes')) {
		return { kind: 'workspace', workspace: ws };
	}
	// /workspaces/{ws}/items                 (list)
	// /workspaces/{ws}/items/{idOrSlug}      (single read — exact, no subroute)
	if (parts.length === 3 && tail === 'items') {
		return { kind: 'workspace', workspace: ws };
	}
	if (parts.length === 4 && tail === 'items') {
		return { kind: 'workspace', workspace: ws };
	}
	// /workspaces/{ws}/collections/{coll}/items
	if (
		parts.length === 5 &&
		tail === 'collections' &&
		parts[4] === 'items'
	) {
		return { kind: 'workspace', workspace: ws };
	}
	return null;
}

/**
 * Fire the registered access-revoked handler for a 403 response. The
 * handler is called best-effort — its failures are swallowed so the
 * caller still sees a clean PadApiError. Public for testing.
 */
function notifyAccessRevoked(path: string): void {
	if (!accessRevokedHandler) return;
	const scope = parseAccessRevokedScope(path);
	if (!scope) return;
	try {
		accessRevokedHandler(scope);
	} catch (err) {
		// eslint-disable-next-line no-console
		console.warn('access-revoked handler threw', err);
	}
}

function getCSRFToken(): string | null {
	if (typeof document === 'undefined') return null;
	// Check __Host- prefixed cookie first (secure/TLS mode), fall back to unprefixed
	const hostMatch = document.cookie.match(/(?:^|;\s*)__Host-pad_csrf=([^;]+)/);
	if (hostMatch) return hostMatch[1];
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
	if (resp.status === 403) {
		// Signal the registered access-revoked handler BEFORE
		// throwing, so the local cache purges its stale entry as
		// part of the same error path (DOC-1342 decision #3, TASK-1360).
		// The handler is best-effort and never throws into the API
		// client.
		//
		// Restricted to GET / HEAD requests. A 403 on
		// POST/PATCH/DELETE usually means "you can READ but not
		// WRITE this resource" — purging on those would wipe
		// perfectly accessible cache rows for a read-only user who
		// tried (and was correctly denied) to create / update /
		// archive an item (Codex P1 round 1 of TASK-1360). Read 403
		// is the canonical "visibility revoked" signal.
		//
		// `method` is already uppercased above; undefined means GET
		// (the fetch default).
		if (method === undefined || method === 'GET' || method === 'HEAD') {
			notifyAccessRevoked(path);
		}
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
	cloud_mode?: boolean;
}

export interface AuthSession {
	authenticated: boolean;
	setup_required: boolean;
	// 'logs_token' added in TASK-1167 for the first-run logs-token bootstrap
	// flow. Self-host servers with no users + a loaded bootstrap token surface
	// this value so SetupRequiredNotice renders the "paste your bootstrap
	// token from the container logs" branch instead of the local-CLI
	// instructions. Cloud mode never advertises 'logs_token' (D10/F9).
	//
	// 'open' added for PAD_BYPASS_SETUP_TOKEN: self-host operators on
	// trusted networks who explicitly opted into open-bootstrap. The
	// /setup form works without a token, and SetupRequiredNotice points
	// directly at /setup with no copy-from-logs instructions. Cloud
	// mode also never advertises 'open'.
	setup_method?: 'local_cli' | 'docker_exec' | 'cloud' | 'logs_token' | 'open';
	auth_method: 'password' | 'cloud';
	cloud_mode?: boolean;
	// mcp_public_url is the canonical URL clients paste into their MCP-capable
	// agent (e.g. "https://mcp.getpad.dev"). Empty string when PAD_MCP_PUBLIC_URL
	// is unset on the server — UI code should use the empty string as the gate
	// for "Remote MCP not exposed by this instance, fall back to CLI flow."
	mcp_public_url: string;
	user?: { id: string; email: string; username: string; name: string; role: string; plan?: string };
}

// ImportURLResponse mirrors internal/server/handlers_import.go's
// importURLResponse. Side-effect-free: no DB writes happen on the
// server during this call; the editor decides whether to splice the
// markdown into an item.
export interface ImportURLResponse {
	markdown: string;
	detected_type: 'openapi' | 'generic';
	title?: string;
	source_url: string;
	fetched_at: string;
	content_type: string;
}

export interface LoginResponse {
	user?: { id: string; email: string; username: string; name: string; role: string; plan?: string };
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

		// me returns the current user's effective workspace context — role,
		// collection_access mode, visible collection IDs, and direct grants.
		// Used by the workspace store's permission helpers (PLAN-1100).
		me: (slug: string) =>
			request<WorkspaceMembership>(`/workspaces/${slug}/me`),

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
			}),

		// Generate a 6-digit stateless claim code for this workspace, OR
		// (when an active OAuth connection of the calling user already
		// covers it) report `suppressed: true` so the modal can render
		// the "your agent can already see this workspace" hint instead.
		// Backed by GET /api/v1/workspaces/{slug}/claim-code
		// (PLAN-1519 / TASK-1525 / IDEA-1517 §4).
		claimCode: (slug: string) =>
			request<ClaimCodeResponse>(`/workspaces/${slug}/claim-code`),

		// importBundle uploads a workspace tar.gz bundle to the bundle-import
		// endpoint. The server dispatches on Content-Type
		// (application/gzip → bundle path, anything else → legacy JSON path),
		// so we explicitly set application/gzip and POST the raw File body
		// rather than going through the JSON-encoding `request` helper.
		// Mirrors the CLI's `pad workspace import <bundle.tar.gz>` flow.
		importBundle: async (file: File, name?: string): Promise<Workspace> => {
			const headers: Record<string, string> = { 'Content-Type': 'application/gzip' };
			const csrf = getCSRFToken();
			if (csrf) headers['X-CSRF-Token'] = csrf;

			const url = name
				? `${BASE}/workspaces/import?name=${encodeURIComponent(name)}`
				: `${BASE}/workspaces/import`;
			const resp = await fetch(url, {
				method: 'POST',
				headers,
				credentials: 'same-origin',
				body: file
			});
			if (resp.status === 401) {
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
			return resp.json();
		}
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

		/**
		 * Skinny-projection cross-collection listing for the local-first
		 * read model (PLAN-1343 / TASK-1344). Returns every item in a
		 * workspace MINUS the rich-text `content` body, plus a `total`
		 * count and a real workspace-scoped `seq` cursor (TASK-1353).
		 *
		 * The response `cursor` is the decimal-encoded `MAX(seq)` across
		 * the requested scope (or the workspace's true `MAX(seq)` when
		 * the filtered set is empty, so /items-changes?since=cursor
		 * starts at the right floor). Each row carries its own `seq`
		 * field so the client can reason about ordering without parsing
		 * the cursor.
		 *
		 * Optional filters mirror the server: `collection` narrows to one
		 * collection slug, `include_archived` flips the soft-delete gate.
		 *
		 * Endpoint: `GET /api/v1/workspaces/{ws}/items-index`. The path is
		 * deliberately at workspace level (sibling to `/plans-progress`)
		 * rather than `/items/index` to avoid colliding with any item
		 * whose slug is `"index"` — see PR #486 Codex round 1.
		 *
		 * The server's `ListItemsIndex` query doesn't scan `i.content`, but
		 * the Go struct serializes the zero value (`content: ""`) over the
		 * wire because `models.Item.Content` has no `omitempty`. Strip it
		 * here so the returned shape matches `ItemIndexRow`'s
		 * `Omit<Item, 'content'>` contract — preventing downstream code
		 * from spreading a row back into the canonical item store and
		 * silently blanking the rich-text body. Per Codex round 1 [P2]
		 * on PR #487.
		 */
		listIndex: async (
			ws: string,
			opts?: { collection?: string; includeArchived?: boolean }
		): Promise<ItemIndexResponse> => {
			const raw = await request<{
				items: (ItemIndexRow & { content?: string })[];
				total: number;
				cursor: string;
			}>(
				`/workspaces/${ws}/items-index${qs({
					collection: opts?.collection,
					include_archived: opts?.includeArchived ? 'true' : undefined,
				})}`
			);
			const items: ItemIndexRow[] = raw.items.map((row) => {
				// Destructure to discard the always-empty `content` key so
				// the returned object truly has no `content` property —
				// `delete row.content` would mutate the parsed JSON in
				// place, but the explicit rest pattern survives strict
				// linting and produces a new shallow copy per row.
				const { content: _ignored, ...rest } = row;
				return rest;
			});
			return { items, total: raw.total, cursor: raw.cursor };
		},

		/**
		 * Delta-fetch sibling of `listIndex` (PLAN-1343 / TASK-1354).
		 *
		 * Endpoint: `GET /api/v1/workspaces/{ws}/items-changes?since=<cursor>`.
		 * Returns every row that has mutated since the caller's `since`
		 * cursor — including soft-deleted tombstones (`deleted: true`) so
		 * the client can remove them from its local index without a
		 * second roundtrip. Rows are sorted ascending by `seq`.
		 *
		 * Cursor contract: re-pass the response's `cursor` as `since`
		 * on the next poll for no overlap and no gap. When the response
		 * is empty the server returns the caller's `since` unchanged
		 * (position preserved). Treat the value as opaque.
		 *
		 * Limit: defaults to the server's `DefaultItemChangesLimit`
		 * (currently 5000). Clients that need a smaller page (low-RAM
		 * mobile resume) or a larger one can pass `limit`; the server
		 * clamps to `MaxItemChangesLimit` (50000). When the response is
		 * truncated, the returned cursor sits at the last row's seq so
		 * the client can resume.
		 */
		changes: async (
			ws: string,
			sinceCursor: string,
			opts?: { limit?: number }
		): Promise<ItemChangesResponse> => {
			const raw = await request<{
				changes: (ItemChangeRow & { content?: string })[];
				cursor: string;
			}>(
				`/workspaces/${ws}/items-changes${qs({
					since: sinceCursor,
					limit: opts?.limit,
				})}`
			);
			// Mirror `listIndex`'s defensive content-strip: the server's
			// skinny scan omits `i.content`, but the Go zero-value would
			// serialize as `content: ""` if any non-omitempty wrapper
			// surfaced it. Strip explicitly so callers never accidentally
			// spread a delta row into the canonical item store and blank
			// out the rich-text body.
			const changes: ItemChangeRow[] = raw.changes.map((row) => {
				const { content: _ignored, ...rest } = row;
				return rest;
			});
			return { changes, cursor: raw.cursor };
		},

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

		/**
		 * flushCollabContent PATCHes items.content with the
		 * `?source=collab-snapshot` query param so the server
		 * skips the applier-routing path. Used by the editor's
		 * 5s-idle + on-disconnect flush (TASK-1260) — the
		 * connected tab IS the canonical source of truth for
		 * Y.Doc state, and routing through the applier would
		 * loop the request back to itself.
		 *
		 * `keepalive` is passed straight to fetch so the
		 * unmount / beforeunload flush path can outlive the
		 * page lifecycle (browser holds the request open until
		 * it completes or hits the ~64KB body cap; markdown
		 * bodies are well under that for typical items).
		 */
		flushCollabContent: (
			ws: string,
			slug: string,
			content: string,
			opts?: { keepalive?: boolean; opLogCursor?: number },
		) => {
			const body: { content: string; op_log_cursor?: number } = { content };
			// `op_log_cursor` (TASK-1319) is the highest
			// item_yjs_updates.id this tab has applied. The server
			// advances `items.content_flushed_op_log_id` only when
			// the cursor matches the current MAX(op-log.id) — proving
			// the markdown captures every persisted op. Cursors below
			// MAX leave the watermark untouched (peer ops outside this
			// tab's view exist; the GC sweeper must not delete them).
			//
			// Always include the cursor when the caller passed it,
			// INCLUDING zero. The server's stale-snapshot gate
			// (round 12 [P1]) needs to see cursor=0 explicitly to
			// reject flushes from clients whose Y.Doc is populated
			// but whose cursor was never anchored (network blip
			// during the post-replay cursor write). Omitting on 0
			// would silently bypass the gate. Per Codex round 13
			// [P1] of TASK-1319.
			if (opts?.opLogCursor !== undefined) {
				body.op_log_cursor = opts.opLogCursor;
			}
			return request<Item>(`/workspaces/${ws}/items/${slug}?source=collab-snapshot`, {
				method: 'PATCH',
				body: JSON.stringify(body),
				keepalive: opts?.keepalive,
			});
		},

		delete: (ws: string, slug: string) =>
			request<void>(`/workspaces/${ws}/items/${slug}`, {
				method: 'DELETE'
			}),

		restore: (ws: string, slug: string) =>
			request<Item>(`/workspaces/${ws}/items/${slug}/restore`, {
				method: 'POST'
			}),

		/**
		 * Move an item to a different collection. The server applies
		 * the same open-children guard the PATCH path uses (IDEA-1494)
		 * — pass `force: true` to override when moving a parent whose
		 * current done-field value would land terminal in the target
		 * collection. Mirrors the CLI's `--force` and the server's
		 * `?force=true` query param on POST /move.
		 */
		move: (
			ws: string,
			slug: string,
			targetCollection: string,
			fieldOverrides?: Record<string, any>,
			opts?: { force?: boolean }
		) =>
			request<Item>(
				`/workspaces/${ws}/items/${slug}/move${opts?.force ? '?force=true' : ''}`,
				{
					method: 'POST',
					body: JSON.stringify({
						target_collection: targetCollection,
						field_overrides: fieldOverrides,
						source: 'web'
					})
				}
			),

		/**
		 * Inbound `[[...]]` references to an item — the "Mentioned in"
		 * panel data source (PLAN-1593 / TASK-1596). Returns same-
		 * workspace backlinks first, then cross-workspace backlinks
		 * (each cross-ws row carries `source_workspace_slug`). The
		 * server applies visibility filtering: a guest with only
		 * partial workspace access sees only sources they're permitted
		 * to read.
		 *
		 * Pagination: `limit` defaults to 50 server-side (max 300);
		 * `offset` enables "Load more" affordances. The panel loads
		 * the first page on mount; if `combined.length === limit`, the
		 * server probably has more rows and the UI exposes a "Show
		 * older" button.
		 */
		backlinks: (
			ws: string,
			slug: string,
			opts?: { limit?: number; offset?: number }
		) =>
			request<Backlink[]>(
				`/workspaces/${ws}/items/${slug}/backlinks${qs({
					limit: opts?.limit,
					offset: opts?.offset,
				})}`
			),

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
			request<{item_id: string; total: number; done: number}[]>(`/workspaces/${ws}/plans-progress`),

		/**
		 * Markdown-checkbox progress for items in a single collection.
		 * The server scans `- [ ]` / `- [x]` markers in each item's
		 * `content` and returns `{item_id, total, done}` for items with
		 * at least one checkbox. Pairs with `listIndex` (TASK-1349):
		 * the index endpoint omits content for bandwidth, this endpoint
		 * supplies the small derived counts the views need to render
		 * progress badges.
		 */
		collectionCheckboxProgress: (
			ws: string,
			coll: string,
			opts?: { includeArchived?: boolean }
		) =>
			request<{item_id: string; total: number; done: number}[]>(
				`/workspaces/${ws}/collections/${coll}/checkbox-progress${qs({
					include_archived: opts?.includeArchived ? 'true' : undefined,
				})}`
			),

		/** Star an item for the current user (idempotent) */
		star: (ws: string, itemSlug: string) =>
			request<void>(`/workspaces/${ws}/items/${itemSlug}/star`, {
				method: 'POST'
			}),

		/** Unstar an item for the current user */
		unstar: (ws: string, itemSlug: string) =>
			request<void>(`/workspaces/${ws}/items/${itemSlug}/star`, {
				method: 'DELETE'
			}),

		/** Check if an item is starred by the current user */
		starStatus: (ws: string, itemSlug: string) =>
			request<{starred: boolean}>(`/workspaces/${ws}/items/${itemSlug}/star`),

		/** List all starred items in a workspace for the current user */
		starred: (ws: string, params?: {include_terminal?: boolean}) =>
			request<Item[]>(`/workspaces/${ws}/starred${qs(params)}`)
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

	search: (query: string, filters?: SearchFilters) => {
		const params: Record<string, string> = { q: query };
		if (filters?.workspace) params.workspace = filters.workspace;
		if (filters?.collection) params.collection = filters.collection;
		if (filters?.status) params.status = filters.status;
		if (filters?.priority) params.priority = filters.priority;
		if (filters?.limit) params.limit = String(filters.limit);
		if (filters?.offset) params.offset = String(filters.offset);
		if (filters?.sort) params.sort = filters.sort;
		if (filters?.order) params.order = filters.order;
		if (filters?.fields) {
			for (const [key, value] of Object.entries(filters.fields)) {
				params[`field.${key}`] = value;
			}
		}
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

		activatePlaybook: (ws: string, playbook: LibraryPlaybook) => {
			// Forward invocation_slug + arguments only when set so legacy
			// library entries (without them) seed with the original
			// three-field shape. Mirrors ShipPlaybook() in
			// internal/collections/templates_startup_ship.go.
			const fields: Record<string, unknown> = {
				status: 'active',
				trigger: playbook.trigger,
				scope: playbook.scope
			};
			if (playbook.invocation_slug) {
				fields.invocation_slug = playbook.invocation_slug;
			}
			if (playbook.arguments && playbook.arguments.length > 0) {
				fields.arguments = playbook.arguments;
			}
			return request<Item>(`/workspaces/${ws}/collections/playbooks/items`, {
				method: 'POST',
				body: JSON.stringify({
					title: playbook.title,
					content: playbook.content,
					fields: JSON.stringify(fields)
				})
			});
		}
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
		// First-run bootstrap with a logs-token (TASK-1167). The token is
		// transmitted via the X-Bootstrap-Token header — never the URL or
		// query string — to keep it out of access logs, proxy logs, and
		// browser history (F6 / D9). Same response shape as register, so
		// the caller can reuse the post-registration redirect logic.
		//
		// When token === '' the header is omitted entirely. This is the
		// PAD_BYPASS_SETUP_TOKEN open-bootstrap path: the server-side
		// gate accepts the request without a token when bypass is on
		// AND the user count is still zero. Sending an empty header
		// would also work (the server treats "" as missing) but the
		// omitted-header form is more explicit + slightly less weird.
		//
		// Content-Type is set explicitly here because request() builds its
		// default headers BEFORE spreading caller options, so any caller-
		// provided `headers` object replaces the defaults entirely. The
		// /auth/bootstrap endpoint is pre-auth and CSRF-exempt, so no
		// X-CSRF-Token is needed.
		bootstrap: (email: string, name: string, password: string, token: string) =>
			request<{ user: { id: string; email: string; username: string; name: string; role: string }; token: string }>('/auth/bootstrap', {
				method: 'POST',
				headers: {
					'Content-Type': 'application/json',
					...(token ? { 'X-Bootstrap-Token': token } : {})
				},
				body: JSON.stringify({ email, name, password })
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
		unlinkProvider: (provider: string) =>
			request<{ ok: boolean; provider: string }>('/auth/oauth-unlink', {
				method: 'POST',
				body: JSON.stringify({ provider })
			}),
		totp: {
			setup: () => request<TOTPSetupResponse>('/auth/2fa/setup', { method: 'POST' }),
			verify: (code: string, secret: string) =>
				request<TOTPVerifyResponse>('/auth/2fa/verify', {
					method: 'POST',
					body: JSON.stringify({ code, secret })
				}),
			disable: (password: string) =>
				request<TOTPDisableResponse>('/auth/2fa/disable', {
					method: 'POST',
					body: JSON.stringify({ password })
				})
		},
		tokens: {
			list: () => request<APIToken[]>('/auth/tokens'),
			create: (name: string) =>
				request<APITokenWithSecret>('/auth/tokens', {
					method: 'POST',
					body: JSON.stringify({ name })
				}),
			delete: (tokenId: string) =>
				request<void>(`/auth/tokens/${tokenId}`, { method: 'DELETE' })
		},
		cli: {
			getSession: (code: string) =>
				request<{ status: string; token?: string; user?: { id: string; email: string; name: string; role: string } }>(`/auth/cli/sessions/${code}`),
			approveSession: (code: string) =>
				request<{ approved: boolean; user: { id: string; email: string; name: string; role: string } }>(`/auth/cli/sessions/${code}/approve`, {
					method: 'POST'
				})
		}
	},

	// ── Attachments ──────────────────────────────────────────────────────────
	//
	// The upload endpoint takes multipart/form-data, not JSON, so it
	// bypasses the shared `request` helper (which sets Content-Type:
	// application/json). It still uses fetch directly with cookies and
	// CSRF — same behavior every other state-changing request gets.
	//
	// downloadUrl is a pure URL builder so callers can wire it directly
	// into <img src=...>, anchor href, etc. — no fetch needed.

	attachments: {
		/**
		 * Upload a file via multipart POST. Returns the persisted
		 * attachment metadata + the canonical download URL.
		 *
		 * @param workspaceSlug  workspace slug (not ID)
		 * @param file           the File / Blob to upload
		 * @param itemId         optional parent item UUID — pass undefined
		 *                       for a free-floating upload
		 * @param onProgress     optional progress callback. Note: fetch()
		 *                       has no upload-progress API; pass this only
		 *                       when the caller wraps with XMLHttpRequest.
		 *                       Currently unused by this method but kept
		 *                       in the signature so the editor plugin can
		 *                       opt in later (TASK-875).
		 */
		async upload(
			workspaceSlug: string,
			file: File | Blob,
			itemId?: string,
			_onProgress?: (loaded: number, total: number) => void
		): Promise<AttachmentUploadResult> {
			const fd = new FormData();
			// FormData.append needs a filename string for Blob inputs;
			// File already carries its own name.
			if (file instanceof File) {
				fd.append('file', file);
			} else {
				fd.append('file', file, 'upload.bin');
			}
			if (itemId) fd.append('item_id', itemId);

			const headers: Record<string, string> = {};
			const csrf = getCSRFToken();
			if (csrf) headers['X-CSRF-Token'] = csrf;

			const resp = await fetch(`${BASE}/workspaces/${workspaceSlug}/attachments`, {
				method: 'POST',
				headers,
				credentials: 'same-origin',
				body: fd
			});
			if (resp.status === 401) {
				if (typeof window !== 'undefined' && !window.location.pathname.startsWith('/login')) {
					window.location.href = '/login';
				}
				throw new PadApiError({ code: 'unauthorized', message: 'Authentication required' });
			}
			if (!resp.ok) {
				const body = await resp.json().catch(() => null);
				if (body?.error) throw new PadApiError(body.error);
				throw new Error(`upload failed: ${resp.status}`);
			}
			return (await resp.json()) as AttachmentUploadResult;
		},

		/**
		 * Build the GET URL for an attachment. Suitable for <img src> and
		 * <a href> — the browser sends the auth cookie automatically.
		 *
		 * `variant` is optional and currently supports "thumb-sm" or
		 * "thumb-md"; the server falls back to the original if no
		 * derived row exists.
		 */
		downloadUrl(
			workspaceSlug: string,
			attachmentId: string,
			variant?: 'thumb-sm' | 'thumb-md' | 'original'
		): string {
			const base = `${BASE}/workspaces/${workspaceSlug}/attachments/${attachmentId}`;
			return variant ? `${base}?variant=${encodeURIComponent(variant)}` : base;
		},

		/**
		 * Apply a server-side image transform (rotate / crop) to an
		 * attachment, producing a NEW attachment row whose UUID the
		 * editor swaps into the corresponding node. The original is
		 * left in place and reclaimed by orphan GC after the grace
		 * period (TASK-886) once nothing references it.
		 *
		 * Returns the same shape as the upload endpoint so callers
		 * have everything they need (id, dimensions, etc.) to update
		 * the editor node attrs without a follow-up GET.
		 *
		 * Only callable on attachments whose MIME the server's
		 * configured Processor supports (the response is 415 when
		 * not). Editors should gate the UI on
		 * `server.capabilities()` upfront so users don't see a
		 * disabled-then-enabled spinner cycle on each click.
		 */
		async transform(
			workspaceSlug: string,
			attachmentId: string,
			payload: AttachmentTransformRequest
		): Promise<AttachmentTransformResult> {
			const headers: Record<string, string> = { 'Content-Type': 'application/json' };
			const csrf = getCSRFToken();
			if (csrf) headers['X-CSRF-Token'] = csrf;
			const resp = await fetch(
				`${BASE}/workspaces/${workspaceSlug}/attachments/${attachmentId}/transform`,
				{
					method: 'POST',
					headers,
					credentials: 'same-origin',
					body: JSON.stringify(payload)
				}
			);
			if (resp.status === 401) {
				if (typeof window !== 'undefined' && !window.location.pathname.startsWith('/login')) {
					window.location.href = '/login';
				}
				throw new PadApiError({ code: 'unauthorized', message: 'Authentication required' });
			}
			if (!resp.ok) {
				const body = await resp.json().catch(() => null);
				if (body?.error) throw new PadApiError(body.error);
				throw new Error(`transform failed: ${resp.status}`);
			}
			return (await resp.json()) as AttachmentTransformResult;
		},

		/**
		 * Workspace storage usage summary: bytes consumed by live
		 * attachments + the effective limit for the workspace owner's
		 * plan + a flag for whether an admin-set per-user override is
		 * configured.
		 *
		 * Server caches per-workspace for ~30s — uploads invalidate
		 * the cache eagerly so the bar doesn't lag behind a new
		 * upload, but multiple page loads in the cache window collapse
		 * to a single DB read.
		 *
		 * `limit_bytes === -1` means unlimited (pro / self-hosted /
		 * unowned workspaces). Callers should branch on that to render
		 * a counter rather than a capped usage bar.
		 */
		storageUsage(workspaceSlug: string): Promise<WorkspaceStorageInfo> {
			return request<WorkspaceStorageInfo>(
				`/workspaces/${workspaceSlug}/storage/usage`
			);
		},

		/**
		 * Paginated list of attachments in a workspace, used by the
		 * Settings → Storage page. Hides derived blobs (thumbnails) by
		 * default — those are managed automatically and shouldn't show
		 * as user-visible rows.
		 *
		 * `total` in the response is the count of all matching rows
		 * (across all pages); pair it with `limit` + `offset` to render
		 * a classic paginator. Server clamps limit to [1, 200].
		 */
		list(
			workspaceSlug: string,
			filters: AttachmentListFilters = {}
		): Promise<AttachmentListResponse> {
			const params = new URLSearchParams();
			if (filters.category) params.set('category', filters.category);
			if (filters.item) params.set('item', filters.item);
			if (filters.collection) params.set('collection', filters.collection);
			if (filters.sort) params.set('sort', filters.sort);
			if (filters.limit !== undefined) params.set('limit', String(filters.limit));
			if (filters.offset !== undefined) params.set('offset', String(filters.offset));
			const qs = params.toString();
			const suffix = qs ? `?${qs}` : '';
			return request<AttachmentListResponse>(
				`/workspaces/${workspaceSlug}/attachments${suffix}`
			);
		},

		/**
		 * Soft-delete an attachment by ID. The blob on disk stays put
		 * (content-addressed dedupe means the same hash may still be
		 * referenced) — orphan GC reclaims past the grace period.
		 *
		 * Returns 204 No Content. Refuses to delete derived
		 * (thumbnail) rows — caller must delete the original.
		 */
		async delete(workspaceSlug: string, attachmentId: string): Promise<void> {
			await request<void>(
				`/workspaces/${workspaceSlug}/attachments/${attachmentId}`,
				{ method: 'DELETE' }
			);
		}
	},

	// ── Server capabilities ─────────────────────────────────────────────────
	//
	// Reports what the configured image processor can do (formats,
	// transcode flag, max-pixels ceiling). Public endpoint — the
	// editor reads it pre-login on shared-item preview surfaces. The
	// response is static for the lifetime of the binary, so callers
	// can cache freely.

	server: {
		capabilities: () => request<ServerCapabilities>('/server/capabilities')
	},

	// ── Connected apps (TASK-954) ────────────────────────────────────────────
	//
	// Lists every active OAuth grant chain the user has authorized
	// (Claude Desktop, Cursor, …) and lets them revoke one. Cloud-mode-
	// only — the route returns 404 outside cloud mode, the page hides
	// the nav link in self-host.

	connectedApps: {
		list: () => request<{ items: ConnectedApp[] }>('/connected-apps'),
		revoke: (id: string) =>
			request<void>(`/connected-apps/${encodeURIComponent(id)}`, { method: 'DELETE' }),

		// PLAN-1519 / TASK-1524 / IDEA-1517 §3: per-connection mutations.
		// Each method returns the updated ConnectedApp DTO so callers
		// can re-render without a separate list refresh.
		rename: (id: string, name: string) =>
			request<ConnectedApp>(`/connected-apps/${encodeURIComponent(id)}/name`, {
				method: 'PATCH',
				body: JSON.stringify({ name })
			}),
		updateFlags: (
			id: string,
			flags: {
				may_create_workspaces: boolean;
				all_current_workspaces: boolean;
				include_future_workspaces: boolean;
			}
		) =>
			request<ConnectedApp>(`/connected-apps/${encodeURIComponent(id)}/flags`, {
				method: 'PATCH',
				body: JSON.stringify(flags)
			}),
		addWorkspace: (id: string, workspaceSlug: string) =>
			request<ConnectedApp>(`/connected-apps/${encodeURIComponent(id)}/workspaces`, {
				method: 'POST',
				body: JSON.stringify({ workspace: workspaceSlug })
			}),
		removeWorkspace: (id: string, workspaceSlug: string) =>
			request<ConnectedApp>(
				`/connected-apps/${encodeURIComponent(id)}/workspaces/${encodeURIComponent(workspaceSlug)}`,
				{ method: 'DELETE' }
			)
	},

	// ── URL Import ───────────────────────────────────────────────────────────

	// Server-side fetch + convert primitive used by the editor's
	// "Insert from URL" modal. Side-effect-free — the server returns
	// markdown plus metadata; the client decides whether to splice it
	// into an item. See PLAN-1467 / TASK-1472 / internal/urlimport.
	importURL: (url: string) =>
		request<ImportURLResponse>('/import/url', {
			method: 'POST',
			body: JSON.stringify({ url })
		}),

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
			}),
		// Billing stats for the admin Billing dashboard (TASK-828 / PLAN-825).
		// Returns merged Stripe-derived metrics (active subs, MRR, ARR, churn,
		// cancellations) plus local users-table aggregates (customers_by_plan,
		// new_signups_30d). Always 200 — degraded states surface as the
		// stripe_configured + cloud_unreachable booleans on the body.
		// Cloud-mode only (returns 404 in self-host).
		getBillingStats: () =>
			request<AdminBillingStats>('/admin/billing-stats')
	}
};

export { PadApiError, isPlanLimitError, planLimitMessage };
