// ─── Workspace ────────────────────────────────────────────────────────────────

export interface Workspace {
	id: string;
	name: string;
	slug: string;
	description: string;
	settings: string;
	created_at: string;
	updated_at: string;
}

export interface WorkspaceCreate {
	name: string;
	description?: string;
	template?: string;
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
	default?: any;
	required?: boolean;
	computed?: boolean;
	collection?: string;
	suffix?: string;
}

export interface CollectionSchema {
	fields: FieldDef[];
}

export interface CollectionSettings {
	layout: 'fields-primary' | 'content-primary' | 'balanced';
	default_view: 'list' | 'board' | 'table';
	board_group_by?: string;
	list_sort_by?: string;
	list_group_by?: string;
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
	created_at: string;
	updated_at: string;
	item_count?: number;
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

// ─── Items ───────────────────────────────────────────────────────────────────

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
	last_modified_by?: string;
	source?: string;
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
	created_at: string;
	updated_at: string;
	item_title?: string;
	item_slug?: string;
}

export interface CommentCreate {
	author?: string;
	body: string;
	created_by?: string;
	source?: string;
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
	source: string;
	metadata: string;
	created_at: string;
}

// ─── Dashboard ───────────────────────────────────────────────────────────────

export interface DashboardResponse {
	summary: {
		total_items: number;
		by_collection: Record<string, Record<string, number>>;
	};
	active_phases: {
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
	scope: string;
	priority: string;
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

export function formatItemRef(item: Item): string | null {
	if (!item.item_number) return null;
	const prefix = item.collection_prefix || '';
	return prefix ? `${prefix}-${item.item_number}` : `#${item.item_number}`;
}

/** Build the URL path segment for an item: uses PREFIX-NUMBER ref if available, falls back to slug. */
export function itemUrlId(item: Item): string {
	return formatItemRef(item) ?? item.slug;
}
