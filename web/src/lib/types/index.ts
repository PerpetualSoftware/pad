// ─── User & Auth ──────────────────────────────────────────────────────────────

export interface User {
	id: string;
	email: string;
	username: string;
	name: string;
	role: string;
	avatar_url?: string;
	oauth_providers?: string[];
	totp_enabled?: boolean;
	created_at: string;
	updated_at: string;
}

export interface TOTPSetupResponse {
	secret: string;
	url: string;
}

export interface TOTPVerifyResponse {
	enabled: boolean;
	recovery_codes: string[];
}

export interface TOTPDisableResponse {
	enabled: boolean;
}

export interface UserProfileUpdate {
	name?: string;
	username?: string;
	current_password?: string;
	new_password?: string;
}

export interface APIToken {
	id: string;
	workspace_id: string;
	user_id?: string;
	name: string;
	prefix: string;
	scopes: string;
	expires_at?: string;
	last_used_at?: string;
	created_at: string;
}

export interface APITokenWithSecret extends APIToken {
	token: string;
}

// ─── Share Links ────────────────────────────────────────────────────────────

export interface ShareLink {
	id: string;
	token?: string;
	target_type: string;
	target_id: string;
	workspace_id: string;
	permission: string;
	created_by: string;
	has_password: boolean;
	expires_at?: string;
	max_views?: number;
	require_auth: boolean;
	view_count: number;
	unique_viewers: number;
	last_viewed_at?: string;
	created_at: string;
	url?: string;
	target_title?: string;
}

/** Presentation-only subset of CollectionSettings emitted by the public share
 *  endpoint (GET /api/v1/s/{token}). Authoring affordances (quick_actions,
 *  content_template) are deliberately omitted. */
export type PublicShareSettings = Pick<
	CollectionSettings,
	'layout' | 'default_view' | 'board_group_by' | 'list_sort_by' | 'list_group_by'
>;

/** One saved view in the public share payload (TASK-1681). Projected from
 *  models.View with internal UUIDs (id, workspace_id, collection_id) and
 *  timestamps stripped; `config` is a parsed JSON object. Powers the
 *  read-only view switcher (TASK-1682). */
export interface PublicShareView {
	name: string;
	slug: string;
	view_type: string;
	config: Record<string, unknown>;
	is_default: boolean;
	sort_order: number;
}

/** The `collection` branch of the public share payload (TASK-1678). `settings`
 *  and `schema` are parsed JSON objects, present only when the source collection
 *  defined them. `views` (TASK-1681) is always present — an empty array when the
 *  collection has no saved views (the switcher falls back to settings.default_view). */
export interface PublicShareCollection {
	name: string;
	icon?: string;
	description?: string;
	settings?: PublicShareSettings;
	schema?: CollectionSchema;
	views?: PublicShareView[];
}

/** One item in the public share payload. `fields` is still a JSON string;
 *  `content` is the item's markdown body. */
export interface PublicShareItem {
	title: string;
	ref?: string;
	fields?: string;
	content?: string;
}

/** The shape returned by GET /api/v1/s/{token}. Auth/password gates short-circuit
 *  with `require_auth` / `require_password`; otherwise `type` discriminates the
 *  item vs collection payload. */
export interface SharePayload {
	type?: 'item' | 'collection';
	require_auth?: boolean;
	require_password?: boolean;
	permission?: string;
	share_link?: { target_type: string };
	item?: {
		title: string;
		content?: string;
		fields?: string;
		ref?: string;
		item_ref?: string;
		collection_name?: string;
		collection_icon?: string;
	};
	collection?: PublicShareCollection;
	items?: PublicShareItem[];
}

// ─── Grants ──────────────────────────────────────────────────────────────────

export interface CollectionGrant {
	id: string;
	collection_id: string;
	workspace_id: string;
	user_id: string;
	permission: string;
	granted_by: string;
	created_at: string;
	user_name?: string;
	user_email?: string;
	user_username?: string;
}

export interface ItemGrant {
	id: string;
	item_id: string;
	workspace_id: string;
	user_id: string;
	permission: string;
	granted_by: string;
	created_at: string;
	user_name?: string;
	user_email?: string;
	user_username?: string;
}

// ─── Workspace Membership (current user) ─────────────────────────────────────

/**
 * The current user's effective workspace context, returned by
 * `GET /api/v1/workspaces/{ws}/me`. Used by the workspace store's permission
 * helpers (canEditWorkspace / canEditCollection / canEditItem etc.) to decide
 * which UI affordances to render.
 *
 * The shape mirrors the server's permission cascade:
 * - role: owner / editor / viewer / guest (admin platform users normalize to
 *   "owner"; legacy workspace-scoped tokens normalize to "editor")
 * - collection_access: "all" means no per-collection filter; "specific" means
 *   visibility is gated by visible_collection_ids
 * - visible_collection_ids: nav-visibility list when collection_access is
 *   "specific"; empty when "all". Includes system collections, direct
 *   collection grants, and collections containing item-granted items
 *   (so the collection appears in the sidebar). NOT a sufficient signal
 *   for per-item access — see full_access_collection_ids.
 * - full_access_collection_ids: strict per-item access list when
 *   collection_access is "specific"; empty when "all". Includes only
 *   collections where every item is accessible (member_collection_access
 *   + system collections + direct collection grants). Item-grant
 *   collections are intentionally excluded — having an item grant in
 *   collection X does NOT confer access to siblings in X.
 * - collection_grants / item_grants: direct overrides that beat membership
 *   role per the server's ResolveUserPermission cascade.
 */
export interface WorkspaceMembership {
	role: 'owner' | 'editor' | 'viewer' | 'guest';
	collection_access: 'all' | 'specific';
	visible_collection_ids: string[];
	full_access_collection_ids: string[];
	collection_grants: CollectionGrant[];
	item_grants: ItemGrant[];
}

// ─── Workspace ────────────────────────────────────────────────────────────────

// ClaimCodeResponse is the wire shape returned by
// GET /api/v1/workspaces/{slug}/claim-code (PLAN-1519 / TASK-1525 /
// IDEA-1517 §4). Two mutually-exclusive states:
//
//   - suppressed=false → `code` is set; the modal renders the digits
//     + the locked prompt block and lets the user copy either.
//   - suppressed=true  → `code` is omitted; the modal renders the
//     "your agent can already see this workspace — Connected apps"
//     hint instead. `suppression_grant_name` is the name of the
//     covering connection if available (may be empty).
//
// `expires_at` is the END of the CURRENT 5-minute bucket in RFC3339;
// verification still accepts the previous bucket for ~5 additional
// minutes (sliding 5–10 min lifetime). UI drives a countdown from
// here.
export interface ClaimCodeResponse {
	workspace: string;
	code?: string;
	expires_at: string;
	suppressed: boolean;
	suppression_grant_name?: string;
}

// ImportArtifactResult is the wire shape returned by
// POST /workspaces/{ws}/import-artifact — the new item's ref + slug plus
// any non-fatal warnings (coerced fields, renamed slug, forced-draft) the
// server surfaced while importing the Markdown+frontmatter artifact.
export interface ImportArtifactResult {
	ref: string;
	slug: string;
	warnings: string[];
}

export interface Workspace {
	id: string;
	name: string;
	slug: string;
	owner_id?: string;
	owner_username?: string;
	is_guest?: boolean;
	description: string;
	settings: string;
	sort_order: number;
	context?: WorkspaceContext;
	created_at: string;
	updated_at: string;
}

export interface WorkspaceRepository {
	name?: string;
	role?: string;
	path?: string;
	repo?: string;
}

export interface WorkspacePaths {
	root?: string;
	docs_repo?: string;
	web?: string;
	server?: string;
	skills?: string;
	config?: string;
	install_root?: string;
}

export interface WorkspaceCommands {
	setup?: string;
	build?: string;
	test?: string;
	lint?: string;
	format?: string;
	dev?: string;
	start?: string;
	web?: string;
}

export interface WorkspaceStack {
	languages?: string[];
	frameworks?: string[];
	package_managers?: string[];
}

export interface WorkspaceDeployment {
	mode?: string;
	base_url?: string;
	host?: string;
}

export interface WorkspaceContext {
	repositories?: WorkspaceRepository[];
	paths?: WorkspacePaths;
	commands?: WorkspaceCommands;
	stack?: WorkspaceStack;
	deployment?: WorkspaceDeployment;
	assumptions?: string[];
}

export interface WorkspaceCreate {
	name: string;
	description?: string;
	template?: string;
	context?: WorkspaceContext;
}

export interface WorkspaceUpdate {
	name?: string;
	description?: string;
	settings?: string;
	context?: WorkspaceContext;
}

export interface WorkspaceTemplate {
	name: string;
	category?: string;
	description: string;
	icon?: string;
	collections: string[];
}

// ─── Collections ─────────────────────────────────────────────────────────────

export interface FieldDef {
	key: string;
	label: string;
	type: 'text' | 'number' | 'select' | 'multi_select' | 'date' | 'checkbox' | 'url' | 'relation' | 'json';
	options?: string[];
	terminal_options?: string[];
	default?: any;
	required?: boolean;
	computed?: boolean;
	collection?: string;
	suffix?: string;
	/** Optional ECMAScript-style regex applied to text values. Empty = no pattern check. */
	pattern?: string;
	/** "workspace_collection" enforces uniqueness within a collection (non-empty values only). */
	unique_scope?: string;
}

export interface CollectionSchema {
	fields: FieldDef[];
}

export interface QuickAction {
	label: string;
	prompt: string;
	scope: 'item' | 'collection';
	icon?: string;
}

export interface CollectionSettings {
	layout: 'fields-primary' | 'content-primary' | 'balanced';
	default_view: 'list' | 'board' | 'table';
	board_group_by?: string;
	list_sort_by?: string;
	list_group_by?: string;
	quick_actions?: QuickAction[];
	content_template?: string;
}

export interface Collection {
	id: string;
	workspace_id: string;
	name: string;
	slug: string;
	icon: string;
	description: string;
	schema: string;
	settings: string;
	sort_order: number;
	is_default: boolean;
	is_system: boolean;
	created_at: string;
	updated_at: string;
	item_count?: number;
	active_item_count?: number;
	prefix: string;
}

export interface CollectionCreate {
	name: string;
	slug?: string;
	prefix?: string;
	icon?: string;
	description?: string;
	schema?: string;
	settings?: string;
}

export interface FieldMigration {
	field: string;
	rename_options?: Record<string, string>;
}

export interface CollectionUpdate {
	name?: string;
	prefix?: string;
	icon?: string;
	description?: string;
	schema?: string;
	settings?: string;
	sort_order?: number;
	migrations?: FieldMigration[];
}

// ─── Agent Roles ─────────────────────────────────────────────────────────────

export interface AgentRole {
	id: string;
	workspace_id: string;
	slug: string;
	name: string;
	description: string;
	icon: string;
	tools: string;
	sort_order: number;
	created_at: string;
	updated_at: string;
	item_count?: number;
}

export interface AgentRoleCreate {
	name: string;
	slug?: string;
	description?: string;
	icon?: string;
	tools?: string;
}

export interface AgentRoleUpdate {
	name?: string;
	slug?: string;
	description?: string;
	icon?: string;
	tools?: string;
	sort_order?: number;
}

export interface RoleBoardLane {
	role: AgentRole | null;
	items: Item[];
	assigned_users: string[];
}

// ─── Items ───────────────────────────────────────────────────────────────────

export interface ItemRelationRef {
	id: string;
	slug?: string;
	ref?: string;
	title: string;
	collection_slug?: string;
	status?: string;
}

export interface ItemDerivedClosure {
	is_closed: boolean;
	kind: string;
	summary: string;
	related_items?: ItemRelationRef[];
}

export interface ItemPullRequestMetadata {
	number: number;
	url: string;
	title: string;
	state: string;
	updated_at?: string;
}

export interface ItemCodeContext {
	provider: string;
	repo?: string;
	branch?: string;
	pull_request?: ItemPullRequestMetadata;
}

export interface ItemConventionMetadata {
	category?: string;
	trigger?: string;
	surfaces?: string[];
	enforcement?: string;
	commands?: string[];
}

export interface ItemImplementationNote {
	id?: string;
	summary: string;
	details?: string;
	created_at?: string;
	created_by?: string;
}

export interface ItemDecisionLogEntry {
	id?: string;
	decision: string;
	rationale?: string;
	created_at?: string;
	created_by?: string;
}

/**
 * A distinct tag used within a workspace and the number of items carrying it.
 * Returned by `GET /workspaces/{ws}/tags` (api.tags.list), ordered by count
 * desc then tag asc.
 */
export interface TagCount {
	tag: string;
	count: number;
}

export interface Item {
	id: string;
	workspace_id: string;
	collection_id: string;
	title: string;
	slug: string;
	content: string;
	fields: string;
	tags: string;
	pinned: boolean;
	sort_order: number;
	parent_id?: string;
	assigned_user_id?: string | null;
	agent_role_id?: string | null;
	assigned_user_name?: string;
	assigned_user_email?: string;
	agent_role_name?: string;
	agent_role_slug?: string;
	agent_role_icon?: string;
	role_sort_order?: number;
	created_by: string;
	last_modified_by: string;
	source: string;
	created_at: string;
	updated_at: string;
	// `deleted_at` marks soft-deleted ("archived") items. Server `Item.DeletedAt`
	// uses `omitempty` so this is undefined for live rows and a UTC string for
	// archived ones. The local-first read model (PLAN-1343) keeps archived
	// rows alongside live rows; consumers gate on this field at render time.
	deleted_at?: string | null;
	collection_slug?: string;
	collection_name?: string;
	collection_icon?: string;
	collection_prefix?: string;
	item_number?: number;
	// `seq` is the workspace-scoped monotonic mutation cursor (PLAN-1343 /
	// DOC-1342 decision #1). Stamped server-side on every create / update /
	// soft-delete / restore. Clients track the max `seq` they have seen
	// and request `?since=<seq>` deltas to resume. Optional because old
	// snapshots may predate the column; new responses always populate it.
	seq?: number;
	parent_link_id?: string;
	parent_ref?: string;
	parent_title?: string;
	parent_slug?: string;
	parent_collection_slug?: string;
	has_children?: boolean;
	derived_closure?: ItemDerivedClosure;
	code_context?: ItemCodeContext;
	convention?: ItemConventionMetadata;
	implementation_notes?: ItemImplementationNote[];
	decision_log?: ItemDecisionLogEntry[];
}

// ─── Items index (skinny projection) ─────────────────────────────────────────
// `ItemIndexRow` is the row shape returned by `GET /workspaces/{ws}/items-index`
// (TASK-1344): every column on `Item` EXCEPT the rich-text `content` body, so
// the local-first read model (PLAN-1343) can hydrate a workspace-wide index
// without paying the body cost on bootstrap.
//
// Derived from `Item` via `Omit<…, 'content'>` so adding a new column to
// `Item` automatically flows into the index row without a second edit.
export type ItemIndexRow = Omit<Item, 'content'>;

export interface ItemIndexResponse {
	items: ItemIndexRow[];
	total: number;
	// `cursor` is the workspace-scoped monotonic `seq` cursor (TASK-1353).
	// Holds MAX(seq) across the requested scope as a decimal-encoded
	// string. When the result set is empty but the workspace has items,
	// it falls back to the workspace's current MAX(seq) so the next
	// /items-changes?since=<cursor> poll starts at the right floor.
	// Empty workspaces return `"0"`. Treat the value as opaque — the
	// encoding may change in future, and clients should not parse it
	// as an integer beyond passing it back as `since`.
	cursor: string;
}

// ─── Items changes (delta sync) ──────────────────────────────────────────────
// `ItemChangeRow` is a skinny `ItemIndexRow` with a `deleted` boolean
// flag so a delta consumer can distinguish upserts from tombstones in
// one pass. Soft-deleted rows still carry their full skinny payload
// (deleted_at is populated) — `deleted` is just the derived view of
// that timestamp.
//
// `moved_out` (BUG-1675) marks a row the caller can no longer see
// because the item moved into a collection outside their visibility.
// Unlike `deleted`, these rows carry ONLY `id` + `seq` (no title /
// collection / fields — the destination is hidden from this caller), so
// the consumer evicts the id from its local cache rather than upserting.
export type ItemChangeRow = ItemIndexRow & {
	deleted: boolean;
	moved_out?: boolean;
};

// `ItemChangesResponse` wraps a delta-fetch result from
// `GET /workspaces/{ws}/items-changes?since=<cursor>` (TASK-1354).
//
//   - changes: rows where `seq > since`, ascending by `seq`. Each row
//     includes `deleted` (true when soft-deleted) so the client can
//     apply or remove without a second roundtrip.
//   - cursor: the largest `seq` in the response, decimal-encoded.
//     When the response is empty, the server returns the caller's
//     `since` unchanged so the client doesn't lose position. Treat
//     the value as opaque (re-pass as `?since=<cursor>` on the
//     next poll).
export interface ItemChangesResponse {
	changes: ItemChangeRow[];
	cursor: string;
}

export interface ItemCreate {
	title: string;
	content?: string;
	fields?: string;
	tags?: string;
	pinned?: boolean;
	parent_id?: string;
	assigned_user_id?: string;
	agent_role_id?: string;
	created_by?: string;
	source?: string;
}

export interface ItemUpdate {
	title?: string;
	content?: string;
	fields?: string;
	tags?: string;
	pinned?: boolean;
	sort_order?: number;
	parent_id?: string;
	assigned_user_id?: string;
	agent_role_id?: string;
	clear_assigned_user?: boolean;
	clear_agent_role?: boolean;
	last_modified_by?: string;
	source?: string;
	comment?: string;
	/**
	 * When true, overrides the server-side open-children guard (IDEA-1494)
	 * that otherwise rejects non-terminal → terminal done-field transitions
	 * while the item still has non-terminal children. Mirror of the
	 * CLI's `--force` flag and `models.ItemUpdate.Force` server-side.
	 * BUG-1538 / TASK-1539.
	 */
	force?: boolean;
}

// ─── Bulk mutation (TASK-1668 / TASK-1669) ───────────────────────────────────

// The verbs the bulk endpoint accepts. One mutation applied to many items
// in a single request (POST /workspaces/{ws}/items/bulk), emitting one SSE
// event per affected collection + one webhook instead of per-item fan-out.
export type BulkItemOp =
	| 'archive'
	| 'restore'
	| 'move'
	| 'tag'
	| 'untag'
	| 'set-priority'
	| 'assign';

// `BulkItemsRequest` is a discriminated union on `op` so each verb only
// accepts (and requires) its own params. `ids` are item refs (TASK-5) or
// UUIDs. `force` overrides the open-children guard on status-bearing moves
// (mirrors ItemUpdate.force). `move` requires status and/or collection
// (target slug) — kept both-optional here; the server validates the
// at-least-one rule.
export type BulkItemsRequest =
	| { op: 'archive'; ids: string[]; force?: boolean }
	| { op: 'restore'; ids: string[] }
	| {
			op: 'move';
			ids: string[];
			status?: string;
			collection?: string;
			force?: boolean;
	  }
	| { op: 'set-priority'; ids: string[]; priority: string; force?: boolean }
	| { op: 'tag' | 'untag'; ids: string[]; tags: string[]; force?: boolean }
	| {
			op: 'assign';
			ids: string[];
			// Set an assignee/role by id. To CLEAR, use the clear flags —
			// the server treats JSON null as absent (mirrors ItemUpdate),
			// so `assigned_user_id: null` would be rejected, not a clear.
			assigned_user_id?: string;
			agent_role_id?: string;
			clear_assigned_user?: boolean;
			clear_agent_role?: boolean;
			force?: boolean;
	  };

// One successfully-mutated row.
export interface BulkItemOutcome {
	ref: string;
	id: string;
}

// One row that failed. `code`/`details` carry the structured server error
// when present (e.g. `open_children` with the blocking-child list); `error`
// is always the human-readable message.
export interface BulkItemFailure {
	ref: string;
	error: string;
	code?: string;
	details?: unknown;
}

// Per-row outcome envelope. The HTTP call resolves 200 even on partial
// failure — branch on `failed` to react. `total === updated + failed`.
export interface BulkItemsResponse {
	op: BulkItemOp;
	updated: BulkItemOutcome[];
	failed: BulkItemFailure[];
	total: number;
}

// ─── Versions ────────────────────────────────────────────────────────────────

export interface Version {
	id: string;
	document_id: string; // actually item_id for item versions
	content: string;
	change_summary: string;
	created_by: string;
	source: string;
	is_diff: boolean;
	created_at: string;
}

// ─── Links ───────────────────────────────────────────────────────────────────

export interface ItemLink {
	id: string;
	workspace_id: string;
	source_id: string;
	target_id: string;
	link_type: string;
	created_by: string;
	created_at: string;
	source_title?: string;
	target_title?: string;
	source_slug?: string;
	target_slug?: string;
	source_ref?: string;
	target_ref?: string;
	source_collection_slug?: string;
	target_collection_slug?: string;
	source_status?: string;
	target_status?: string;
}

export interface ItemLinkCreate {
	target_id: string;
	link_type?: string;
	created_by?: string;
}

// ─── Backlinks ──────────────────────────────────────────────────────────────

/**
 * One inbound `[[...]]` reference to the queried item. Wire shape returned by
 * `GET /api/v1/workspaces/{ws}/items/{slug}/backlinks` — populated by the
 * server-side reverse index from PLAN-1593 (Phases 1, 2a, 2b).
 *
 * Mirrors `internal/models/backlink.go`. The Phase 3 UI (TASK-1596) renders
 * these as the "Mentioned in" panel beneath an item's content.
 */
export interface Backlink {
	source_item_id: string;
	source_ref: string;
	source_title: string;
	source_collection_slug: string;
	source_collection_icon: string;
	snippet: string;
	updated_at: string;
	/**
	 * Optional `[[X|display]]` override. `null` (omitted from JSON) when
	 * the link was a bare `[[X]]`. A non-null empty string represents
	 * the editor-distinct `[[X|]]` shape.
	 */
	display_text?: string | null;
	/**
	 * Populated only for cross-workspace backlinks (PLAN-1593 Phase 2b /
	 * TASK-1597). Same-workspace rows omit this field. The renderer uses
	 * it to route links to the foreign workspace and show a small
	 * workspace badge next to the row.
	 */
	source_workspace_slug?: string;
}

// ─── Comments ───────────────────────────────────────────────────────────────

export interface Comment {
	id: string;
	item_id: string;
	workspace_id: string;
	author: string;
	/** Authenticated author's user id; empty for pre-identity / agent comments. */
	user_id?: string;
	body: string;
	created_by: string;
	source: string;
	activity_id?: string;
	parent_id?: string;
	created_at: string;
	updated_at: string;
	item_title?: string;
	item_slug?: string;
	replies?: Comment[];
	reactions?: Reaction[];
}

export interface CommentCreate {
	author?: string;
	body: string;
	created_by?: string;
	source?: string;
	parent_id?: string;
}

export interface Reaction {
	id: string;
	comment_id: string;
	user_id?: string;
	actor: string;
	emoji: string;
	created_at: string;
	actor_name?: string;
}

// ─── Timeline ────────────────────────────────────────────────────────────────

export interface TimelineEntry {
	id: string;
	kind: 'comment' | 'activity' | 'version';
	created_at: string;
	actor: string;
	actor_name?: string;
	source: string;
	comment?: Comment;
	activity?: Activity;
	version?: Version;
}

export interface TimelineResponse {
	entries: TimelineEntry[];
	has_more: boolean;
}

// ─── Views ───────────────────────────────────────────────────────────────────

export interface ViewConfig {
	filters?: { field: string; op: string; value: any }[];
	sort?: { field: string; direction: 'asc' | 'desc' }[];
	group_by?: string;
	visible_fields?: string[];
}

export interface View {
	id: string;
	workspace_id: string;
	collection_id?: string;
	name: string;
	slug: string;
	view_type: 'list' | 'board' | 'table';
	config: string;
	sort_order: number;
	is_default: boolean;
	created_at: string;
	updated_at: string;
}

// ─── Activity ────────────────────────────────────────────────────────────────

export interface Activity {
	id: string;
	workspace_id: string;
	item_id?: string;
	action: string;
	actor: string;
	actor_name?: string;
	source: string;
	metadata: string;
	created_at: string;
	item_title?: string;
	item_slug?: string;
	collection_slug?: string;
}

// ─── Dashboard ───────────────────────────────────────────────────────────────

export interface DashboardActiveItem {
	slug: string;
	title: string;
	collection_slug: string;
	collection_icon: string;
	priority?: string;
	status: string;
	updated_at: string;
	item_ref?: string;
}

export interface DashboardResponse {
	summary: {
		total_items: number;
		by_collection: Record<string, Record<string, number>>;
	};
	active_items: DashboardActiveItem[];
	starred_items?: DashboardActiveItem[];
	active_plans: {
		slug: string;
		title: string;
		progress: number;
		task_count: number;
		done_count: number;
	}[];
	attention: {
		type: string;
		item_slug: string;
		item_title: string;
		collection: string;
		reason: string;
	}[];
	recent_activity: {
		action: string;
		actor: string;
		actor_name?: string;
		source: string;
		created_at: string;
		item_title?: string;
		item_slug?: string;
		collection_slug?: string;
		metadata?: string;
	}[];
	suggested_next: {
		item_slug: string;
		item_title: string;
		collection: string;
		reason: string;
	}[];
	// has_agent_activity is true when any item in the workspace was created
	// by an agent surface (CLI or Remote MCP — both persist source='cli'
	// today; future MCP-distinct attribution lands as source='mcp', which
	// the underlying store query also matches). Drives the connect-agent
	// banner's auto-hide.
	has_agent_activity: boolean;
	// needs_onboarding mirrors AgentBootstrap.NeedsOnboarding (PLAN-1496 /
	// TASK-1504): true when the workspace has zero items with
	// source != 'template'. Drives the post-IDEA-1516 onboarding nudge
	// banner. Flips false the moment any user/agent-sourced item exists.
	needs_onboarding: boolean;
	// onboarding_seed identifies the seeded onboarding entry for the
	// workspace (e.g. IDEA-1 for `startup`, BACK-1 for `scrum`,
	// FEAT-1 for `product`). Present + active drives the
	// OnboardingIdeaBanner. Absent = no IDEA-1-style onboarding seed
	// in this workspace (empty template, hiring/interviewing example
	// items, etc.); banner stays hidden. Computed server-side.
	onboarding_seed?: {
		ref: string;
		title: string;
		slug: string;
		collection_slug: string;
		status: string;
		// active is true while the seed is still in its initial
		// (untouched) status — banner shows only when this is true.
		active: boolean;
	};
}

// ─── Workspace Graph (PLAN-1730 / TASK-1732) ─────────────────────────────────

/** One item in the workspace graph. Keyed by ref (e.g. "TASK-5"). */
export interface GraphNode {
	/** item UUID — correlates SSE item events (item_id) with nodes */
	id: string;
	ref: string;
	title: string;
	/** collection slug */
	collection: string;
	status?: string;
	/** true when the item is in a terminal status for its collection */
	is_terminal: boolean;
	/** number of child items (parent + implements links pointing here) */
	child_count: number;
	updated_at: string;
	/** assigned agent-role slug, when set — feeds the graph view's role filter */
	role?: string;
}

/**
 * One typed relationship between two graph nodes. Source/target are
 * refs. For 'parent', source is the child and target is the parent;
 * for 'blocks', source blocks target.
 */
export interface GraphEdge {
	source: string;
	target: string;
	type:
		| 'parent'
		| 'blocks'
		| 'implements'
		| 'related'
		| 'split-from'
		| 'supersedes'
		| 'wiki-link';
}

/** GET /workspaces/{ws}/graph — the whole workspace as {nodes, edges}. */
export interface GraphResponse {
	nodes: GraphNode[];
	edges: GraphEdge[];
	/**
	 * Focus mode only (PLAN-1780): true when the neighborhood hit the
	 * server's node cap and BFS expansion stopped early — the client
	 * should offer expand-on-click rather than treat the graph as
	 * complete. Absent (falsy) for the whole-workspace view.
	 */
	truncated?: boolean;
}

// ─── Project Report (PLAN-1628 / TASK-1630) ──────────────────────────────────

export type ReportWindow = 'day' | 'week' | '2wk' | 'month';

/** One time-series point: items created vs completed within the bucket. */
export interface ReportBucket {
	/** Sortable UTC label: "YYYY-MM-DD" (day) or "YYYY-MM-DDTHH" (hour). */
	bucket: string;
	created: number;
	completed: number;
}

export interface ReportCollectionCount {
	/** collection slug */
	collection: string;
	count: number;
}

export interface ReportStatusCount {
	collection: string;
	status: string;
	count: number;
}

export interface ReportTotals {
	created: number;
	completed: number;
	/** created - completed */
	net_flow: number;
}

/** Per-collection median duration in hours over a sample. */
export interface ReportDuration {
	collection: string;
	count: number;
	median_hours: number;
}

/** Time from item creation to a positive-terminal transition (completions in window). */
export interface ReportCycleTime {
	sample_size: number;
	median_hours: number;
	p90_hours: number;
	by_collection: ReportDuration[];
}

export interface ReportAgingBucket {
	/** "<1d" | "1-7d" | "7-30d" | ">30d" */
	label: string;
	count: number;
}

/** Point-in-time work-in-progress: open items (non-terminal), their age + distribution. */
export interface ReportWIP {
	open_count: number;
	median_age_hours: number;
	aging_buckets: ReportAgingBucket[];
	/** median_hours = median open-item age, per collection */
	by_collection: ReportDuration[];
}

/**
 * Windowed project report. "completed" counts a status change into a positive
 * terminal value (terminal options minus negative outcomes like
 * rejected/cancelled). buckets is chronological and zero-filled, so it renders
 * directly without client-side gap-filling.
 */
export interface ReportData {
	window: ReportWindow;
	/** periods back from now (0 = current); set via the period-nav controls */
	offset: number;
	granularity: 'hour' | 'day';
	range_start: string; // RFC3339 UTC
	range_end: string; // RFC3339 UTC
	collections: string[]; // slugs included
	buckets: ReportBucket[];
	totals: ReportTotals;
	completed_by_collection: ReportCollectionCount[];
	status_distribution: ReportStatusCount[];
	cycle_time: ReportCycleTime;
	wip: ReportWIP;
	/** "What shipped" — present only when requested via include_items. Deduped by item, newest first. */
	completed_items?: ReportCompletedItem[];
	completed_items_overflow_count?: number;
}

/** One item that reached a positive terminal within the window (the "what shipped" list). */
export interface ReportCompletedItem {
	ref: string; // e.g. TASK-5
	title: string;
	collection: string; // slug
	completed_at: string; // RFC3339
}

/**
 * Per-user Insights personalization for one workspace (PLAN-1628 / TASK-1634).
 * hidden_cards lists toggled-off metric-card IDs:
 * 'throughput' | 'cycle_time' | 'wip' | 'completed_by_collection' | 'status_distribution'.
 * default_window/default_collections restore the view on load (empty = surface defaults / all).
 */
export interface ReportLayout {
	hidden_cards: string[];
	default_window: ReportWindow | '';
	default_collections: string[];
}

// ─── Incremental Sync ────────────────────────────────────────────────────────

export interface ChangesResponse {
	updated: Item[];
	deleted: string[];
	server_time: number;
	collections_changed: boolean;
}

// ─── Search ──────────────────────────────────────────────────────────────────

export interface SearchResult {
	item: Item;
	snippet: string;
	rank: number;
}

export interface SearchFacets {
	collections: Record<string, number>;
	statuses: Record<string, number>;
}

export interface SearchResponse {
	results: SearchResult[];
	total: number;
	limit: number;
	offset: number;
	facets?: SearchFacets;
}

export interface SearchFilters {
	workspace?: string;
	collection?: string;
	status?: string;
	priority?: string;
	fields?: Record<string, string>;
	limit?: number;
	offset?: number;
	sort?: 'relevance' | 'created_at' | 'updated_at' | 'title';
	order?: 'asc' | 'desc';
}

// ─── Convention Library ──────────────────────────────────────────────────────

export interface LibraryConvention {
	title: string;
	content: string;
	category: string;
	trigger: string;
	surfaces: string[];
	enforcement: string;
	commands?: string[];
}

export interface LibraryCategory {
	name: string;
	description: string;
	conventions: LibraryConvention[];
}

export interface ConventionLibraryResponse {
	categories: LibraryCategory[];
}

// ─── Playbook Library ────────────────────────────────────────────────────────

export interface LibraryPlaybookArgument {
	name: string;
	type: string;
	required?: boolean;
	default?: unknown;
	description?: string;
	enum?: string[];
}

export interface LibraryPlaybook {
	title: string;
	content: string;
	category: string;
	trigger: string;
	scope: string;
	/** PLAN-1377 invocation surface — kebab-case slug for `/pad <slug>` routing. */
	invocation_slug?: string;
	/** Argument spec mirroring the body's `## Arguments` section. */
	arguments?: LibraryPlaybookArgument[];
}

export interface PlaybookCategory {
	name: string;
	description: string;
	playbooks: LibraryPlaybook[];
}

export interface PlaybookLibraryResponse {
	categories: PlaybookCategory[];
}

// ─── API Error ───────────────────────────────────────────────────────────────

export interface ApiError {
	code: string;
	message: string;
	/**
	 * Optional structured details supplied by the server for codes that
	 * carry recovery information. Shape varies by code — e.g.
	 * `open_children` (IDEA-1494) returns
	 * `{ open_children: [...], hidden_blocker_count: N, done_field, attempted_value }`.
	 * Consumers branching on `code` cast this to the matching shape.
	 */
	details?: Record<string, unknown>;
}

// ─── Admin Billing Stats (PLAN-825) ──────────────────────────────────────────

// Returned by GET /api/v1/admin/billing-stats. Merges Stripe-derived metrics
// from the pad-cloud sidecar with local users-table aggregates. Two booleans
// drive UI presentation:
//
// - `cloud_unreachable=true`  → sidecar errored; render banner, Stripe fields
//   are zero, local fields are still valid.
// - `stripe_configured=false` → sidecar reachable but no Stripe wired up yet;
//   render "Stripe not configured" placeholder on Stripe-derived cards.
//
// `cloud_unreachable=false` AND `stripe_configured=true` → fully healthy;
// render real numbers on every card. Other combinations imply a degraded
// state — see the two bullets above.
export interface AdminBillingStats {
	stripe_configured: boolean;
	cloud_unreachable: boolean;
	customers_by_plan: Record<string, number>;
	new_signups_30d: number;
	active_subscriptions: number;
	mrr_cents: number;
	arr_cents: number;
	currency: string;
	churn_rate_30d: number;
	cancelled_30d: number;
	computed_at: string;
	cache_age_seconds: number;
}

// ─── Attachments ─────────────────────────────────────────────────────────────
//
// Mirrors the Go internal/models/attachment.go shape for the database row,
// plus the upload-handler response shape (AttachmentUploadResult — not all
// fields overlap with the row because the response also carries derived
// metadata like category and render_mode).

/** A row in the attachments table. */
export interface Attachment {
	id: string;
	workspace_id: string;
	item_id?: string | null; // null = orphan, eligible for GC
	uploaded_by: string;
	storage_key: string;     // "<backend>:<sha256>"
	content_hash: string;
	mime_type: string;
	size_bytes: number;
	filename: string;
	width?: number | null;
	height?: number | null;
	parent_id?: string | null;
	variant?: string | null; // "original" | "thumb-sm" | "thumb-md" | null
	created_at: string;
	deleted_at?: string | null;
}

/** Server response from POST /api/v1/workspaces/{slug}/attachments. */
export interface AttachmentUploadResult {
	id: string;
	url: string;
	mime: string;
	size: number;
	width?: number | null;
	height?: number | null;
	filename: string;
	category: 'image' | 'video' | 'audio' | 'document' | 'text' | 'archive' | 'other';
	render_mode: 'inline' | 'chip' | 'download';
}

/**
 * Request body for POST /api/v1/workspaces/{slug}/attachments/{id}/transform
 * (TASK-879/880). Discriminated by `operation`. Per-op params live in
 * their own fields rather than a generic args bag — keeps the wire
 * format tight and lets the type checker prove the request is well-formed.
 */
export type AttachmentTransformRequest =
	| { operation: 'rotate'; degrees: 90 | 180 | 270 }
	| { operation: 'crop'; rect: { x: number; y: number; w: number; h: number } };

/** Server response shape from /transform. Subset of AttachmentUploadResult. */
export interface AttachmentTransformResult {
	id: string;
	url: string;
	mime: string;
	size: number;
	width?: number | null;
	height?: number | null;
	filename: string;
}

/**
 * Server capability profile from GET /api/v1/server/capabilities.
 * The editor reads this once at mount and gates rotate/crop UI on
 * the image processor's reach.
 */
export interface ServerCapabilities {
	image: {
		image_formats: string[];
		can_transcode: boolean;
		max_pixels: number;
	};
}

/**
 * Consolidated quota summary returned by
 * GET /api/v1/workspaces/{ws}/storage/usage.
 *
 * `limit_bytes === -1` indicates no enforced cap — the workspace
 * is on a pro / self-hosted plan (or has no owner). Render a usage
 * counter rather than a capped bar in that case.
 *
 * `override_active === true` means the workspace owner has a
 * per-user storage_bytes override configured. The flag stays true
 * for pro/self-hosted plans even though the override doesn't change
 * the effective limit there — the admin UI uses the flag to surface
 * "(custom override)" in the user-detail view.
 */
export interface WorkspaceStorageInfo {
	used_bytes: number;
	limit_bytes: number;
	plan: string;
	override_active: boolean;
}

/**
 * Row shape from GET /api/v1/workspaces/{ws}/attachments. Mirrors
 * the store's AttachmentListItem — base attachment columns plus
 * LEFT JOIN'd item title / slug / collection slug for the "in
 * [[Item X]]" link in the settings page. Item fields are absent
 * for orphan attachments.
 */
export interface AttachmentListItem {
	id: string;
	workspace_id: string;
	item_id?: string | null;
	uploaded_by: string;
	storage_key: string;
	content_hash: string;
	mime_type: string;
	size_bytes: number;
	filename: string;
	width?: number | null;
	height?: number | null;
	parent_id?: string | null;
	variant?: string | null;
	created_at: string;
	deleted_at?: string | null;
	item_title?: string | null;
	item_slug?: string | null;
	/**
	 * True when the parent item is soft-deleted. The attachment is
	 * still surfaced (the bytes still count toward quota) but the
	 * UI should render "(deleted)" instead of a clickable link.
	 */
	item_deleted?: boolean;
	collection_slug?: string | null;
}

/**
 * Paginated response from GET /api/v1/workspaces/{ws}/attachments.
 * `total` is the count of all matching rows (across all pages); the
 * UI uses it with `limit` + `offset` to render a classic paginator.
 */
export interface AttachmentListResponse {
	attachments: AttachmentListItem[];
	total: number;
	limit: number;
	offset: number;
}

/** Filters accepted by attachments.list — translated to query params. */
export interface AttachmentListFilters {
	category?: 'image' | 'video' | 'audio' | 'document' | 'text' | 'archive' | 'other';
	item?: 'attached' | 'unattached';
	collection?: string;
	sort?:
		| 'size'
		| 'size_desc'
		| 'filename'
		| 'filename_desc'
		| 'created_at'
		| 'created_at_desc';
	limit?: number;
	offset?: number;
}

// Connected apps (TASK-954) — one OAuth grant chain the user has
// authorized via the consent flow. The `id` is the OAuth request_id
// (chain identifier preserved across refresh-token rotations) — used
// as the URL path param for revoke + audit drilldown.
//
// allowed_workspaces: nil / undefined / ["*"] all mean "any workspace
// the user belongs to"; the page renders the appropriate badge.
// Otherwise the array is the explicit slug list the user picked at
// consent time.
//
// last_used_at and calls_30d come from the MCP audit log enrichment
// (TASK-960). last_used_at is empty when no audit entries exist for
// this connection — the page renders "—" instead of a date.
export interface ConnectedApp {
	id: string;
	client_id: string;
	client_name: string;
	logo_uri?: string;
	redirect_uris?: string[];
	allowed_workspaces?: string[] | null;
	scope_string: string;
	capability_tier: 'read_only' | 'read_write' | 'full_access' | 'unknown';
	connected_at: string;
	last_used_at?: string;
	calls_30d: number;

	// PLAN-1519 / TASK-1524 / IDEA-1517 §3: connection-level state
	// surfaced for the mutation UI. `name` empty until the user names
	// the connection (backfilled rows start blank); the three scope
	// flags default ON for backfilled connections + new-grant defaults.
	name?: string;
	may_create_workspaces?: boolean;
	all_current_workspaces?: boolean;
	include_future_workspaces?: boolean;
}

// ─── Helper functions ────────────────────────────────────────────────────────

export function parseFields(item: Item): Record<string, any> {
	try {
		return JSON.parse(item.fields);
	} catch {
		return {};
	}
}

/**
 * Parse an item's `tags` JSON-array string into a clean string[]: tolerant of
 * empty/garbage (returns []), drops non-strings, and dedupes case-insensitively
 * keeping the first-typed casing. The write path doesn't enforce per-item tag
 * uniqueness, so deduping here keeps rendering keys unique and display tidy.
 */
export function parseTags(item: Pick<Item, 'tags'> | null | undefined): string[] {
	if (!item?.tags) return [];
	try {
		const parsed = JSON.parse(item.tags);
		if (!Array.isArray(parsed)) return [];
		const seen = new Set<string>();
		const out: string[] = [];
		for (const t of parsed) {
			if (typeof t !== 'string') continue;
			const key = t.toLowerCase();
			if (seen.has(key)) continue;
			seen.add(key);
			out.push(t);
		}
		return out;
	} catch {
		return [];
	}
}

const schemaDefaults = (): CollectionSchema => ({ fields: [] });

export function parseSchema(collection: Collection): CollectionSchema {
	try {
		return { ...schemaDefaults(), ...JSON.parse(collection.schema) };
	} catch {
		return schemaDefaults();
	}
}

const settingsDefaults = (): CollectionSettings => ({ layout: 'balanced', default_view: 'list' });

export function parseSettings(collection: Collection): CollectionSettings {
	try {
		return { ...settingsDefaults(), ...JSON.parse(collection.settings) };
	} catch {
		return settingsDefaults();
	}
}

export function getFieldValue(item: Item, key: string): any {
	const fields = parseFields(item);
	return fields[key];
}

export function getStatusOptions(collection: Collection): string[] {
	const schema = parseSchema(collection);
	const statusField = schema.fields.find((f) => f.key === 'status');
	return statusField?.options ?? [];
}

/** Default terminal statuses used as a fallback when a collection's schema
 * doesn't declare terminal_options. */
const DEFAULT_TERMINAL_STATUSES = [
	'done', 'completed', 'resolved', 'cancelled', 'rejected',
	'wontfix', 'fixed', 'implemented', 'archived', 'disabled', 'deprecated'
];

/** Get the terminal status options for a collection. Uses the schema's
 * terminal_options if defined, otherwise falls back to defaults. */
export function getTerminalOptions(collection: Collection): string[] {
	const schema = parseSchema(collection);
	const statusField = schema.fields.find((f) => f.key === 'status');
	return statusField?.terminal_options ?? [...DEFAULT_TERMINAL_STATUSES];
}

/** Check if a status value is terminal (finalized) for a given collection. */
export function isTerminalStatus(status: string, collection: Collection): boolean {
	return getTerminalOptions(collection).includes(status);
}

/** Check if a status value is terminal using the default fallback list.
 * Use when no collection context is available. */
export function isTerminalStatusDefault(status: string): boolean {
	return DEFAULT_TERMINAL_STATUSES.includes(status);
}

export function formatItemRef(item: Item): string | null {
	if (!item.item_number) return null;
	const prefix = item.collection_prefix || '';
	return prefix ? `${prefix}-${item.item_number}` : `#${item.item_number}`;
}

/** Build the URL path segment for an item: uses PREFIX-NUMBER ref if available, falls back to slug. */
export function itemUrlId(item: Item): string {
	return formatItemRef(item) ?? item.slug;
}
