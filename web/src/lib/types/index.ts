// ─── User & Auth ──────────────────────────────────────────────────────────────

export interface User {
	id: string;
	email: string;
	username: string;
	name: string;
	role: string;
	avatar_url?: string;
	created_at: string;
	updated_at: string;
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
	description: string;
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

export interface SearchResponse {
	results: SearchResult[];
	total: number;
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
