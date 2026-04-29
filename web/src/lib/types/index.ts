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

// ─── Workspace ────────────────────────────────────────────────────────────────

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
	type: 'text' | 'number' | 'select' | 'multi_select' | 'date' | 'checkbox' | 'url' | 'relation';
	options?: string[];
	terminal_options?: string[];
	default?: any;
	required?: boolean;
	computed?: boolean;
	collection?: string;
	suffix?: string;
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
	collection_slug?: string;
	collection_name?: string;
	collection_icon?: string;
	collection_prefix?: string;
	item_number?: number;
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

export interface ItemCreate {
	title: string;
	content?: string;
	fields?: string;
	tags?: string;
	pinned?: boolean;
	parent_id?: string;
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

// ─── Comments ───────────────────────────────────────────────────────────────

export interface Comment {
	id: string;
	item_id: string;
	workspace_id: string;
	author: string;
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
	has_cli_source: boolean;
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

export interface LibraryPlaybook {
	title: string;
	content: string;
	category: string;
	trigger: string;
	scope: string;
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

// ─── Helper functions ────────────────────────────────────────────────────────

export function parseFields(item: Item): Record<string, any> {
	try {
		return JSON.parse(item.fields);
	} catch {
		return {};
	}
}

export function parseSchema(collection: Collection): CollectionSchema {
	try {
		return JSON.parse(collection.schema);
	} catch {
		return { fields: [] };
	}
}

export function parseSettings(collection: Collection): CollectionSettings {
	try {
		return JSON.parse(collection.settings);
	} catch {
		return { layout: 'balanced', default_view: 'list' };
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
	return statusField?.terminal_options ?? DEFAULT_TERMINAL_STATUSES;
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
