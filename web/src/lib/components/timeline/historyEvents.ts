import type { Activity, TimelineEntry } from '$lib/types';
import { parseFieldChanges, type FieldChange } from '$lib/utils/activityChanges';
import { agentNameOf } from '$lib/utils/agentActor';
import { HISTORY_KINDS } from './feed';

/**
 * The History tab's unit is an EVENT, not a row (PLAN-2348 checkpoint 2): one
 * actor through one door, a burst of rows close together, one card. The
 * server stores the pieces of one save separately — an activity row for the
 * field changes, a version row for the body, a note or decision in the fields
 * blob — and rendering them as rows gave one save up to three cards, one of
 * them empty (defect 6).
 *
 * Pure and DOM-free, so each grouping rule is tested on its own.
 */

/** How close two rows must be to belong to one event. */
export const BURST_WINDOW_MS = 2 * 60 * 1000;


export type ActorKind = 'user' | 'agent' | 'system';

/**
 * Who wrote a row. Each field is optional because each row kind records a
 * different subset: an activity names the agent (metadata.agent) and the user
 * it acted for, a version names only the user (U2), and a note or decision
 * names only the actor kind. An unknown field matches anything; two KNOWN
 * values that differ are two writers.
 */
export interface HistoryWho {
	kind: ActorKind;
	agent?: string;
	user?: string;
	source?: string;
	/** A row a workspace import wrote (BUG-3379): its kind and source came
	 *  from the export, unverified. */
	imported?: boolean;
}

export interface HistoryEvent {
	type: 'event';
	/** The newest entry's id: stable while older rows join the event. */
	id: string;
	/** Newest row's time. */
	at: string;
	/** Oldest row's time. */
	firstAt: string;
	who: HistoryWho;
	/** Newest first. */
	entries: TimelineEntry[];
	/** One per field: the oldest `from` and the newest `to` in the event. */
	changes: FieldChange[];
	/** Actions other than a plain update: created, moved, archived, restored. */
	actions: Activity[];
	/** Body versions (not autosaves), newest first. */
	versions: TimelineEntry[];
	/**
	 * A body edit the version throttle wrote no row for (the U2 `body_edited`
	 * marker). True only when the event holds no EDIT version row, because an
	 * edit row already says the body changed and carries the diff. A create
	 * row does not: an edit inside the throttle right after a create leaves
	 * the create row as the item's only version, and the edit only here.
	 */
	bodyEdited: boolean;
	notes: TimelineEntry[];
	decisions: TimelineEntry[];
}

/** A collapsed run of one person's autosaves — or a lone autosave. */
export interface HistoryAutosave {
	type: 'autosave';
	id: string;
	at: string;
	firstAt: string;
	who: HistoryWho;
	entry: TimelineEntry;
	count: number;
}

export type HistoryRow = HistoryEvent | HistoryAutosave;

function parseMeta(meta: string | undefined): Record<string, unknown> {
	if (!meta) return {};
	try {
		const v = JSON.parse(meta);
		return v && typeof v === 'object' ? (v as Record<string, unknown>) : {};
	} catch {
		return {};
	}
}

function nonEmpty(s: string | undefined | null): string | undefined {
	return s ? s : undefined;
}

/** Sources that say nothing about the door a row came through. */
const NO_SOURCE = new Set(['', 'structured']);

export function whoOf(e: TimelineEntry): HistoryWho {
	if (e.kind === 'activity' && e.activity) {
		const a = e.activity;
		return {
			kind: a.actor === 'agent' ? 'agent' : 'user',
			agent: a.actor === 'agent' ? agentNameOf(parseMeta(a.metadata)) : undefined,
			user: nonEmpty(a.actor_name),
			source: NO_SOURCE.has(a.source) ? undefined : a.source
		};
	}
	if (e.kind === 'version' && e.version) {
		const v = e.version;
		// A recovery row is the server's own write of a crashed tab's edits
		// (TASK-2198 U4): never a user, never Web, whatever else it carries.
		if (v.source === 'recovery' || v.created_by === 'system')
			return { kind: 'system', source: 'recovery', imported: v.imported === true ? true : undefined };
		return {
			kind: v.created_by === 'agent' ? 'agent' : 'user',
			user: nonEmpty(v.actor_name),
			source: NO_SOURCE.has(v.source) ? undefined : v.source,
			imported: v.imported === true ? true : undefined
		};
	}
	// Notes and decisions record the actor kind only.
	if (e.actor === 'agent') return { kind: 'agent' };
	if (e.actor === 'user' || !e.actor) return { kind: 'user' };
	// A label rather than a kind (the remote MCP door's long-standing display
	// name): show it as the writer's name.
	return { kind: 'user', user: e.actor };
}

function agrees(a: string | undefined, b: string | undefined): boolean {
	return a === undefined || b === undefined || a === b;
}

export function sameWriter(a: HistoryWho, b: HistoryWho): boolean {
	// An imported row never merges with a native one (BUG-3379): one
	// verified, one not, are different writers whatever they claim.
	return (
		a.kind === b.kind &&
		agrees(a.agent, b.agent) &&
		agrees(a.user, b.user) &&
		agrees(a.source, b.source) &&
		(a.imported ?? false) === (b.imported ?? false)
	);
}

function mergeWho(a: HistoryWho, b: HistoryWho): HistoryWho {
	return {
		kind: a.kind,
		agent: a.agent ?? b.agent,
		user: a.user ?? b.user,
		source: a.source ?? b.source,
		imported: a.imported ?? b.imported
	};
}

function isAutosave(e: TimelineEntry): boolean {
	return e.kind === 'version' && e.version?.source === 'collab-snapshot';
}

function isRecovery(e: TimelineEntry): boolean {
	return e.kind === 'version' && (e.version?.source === 'recovery' || e.version?.created_by === 'system');
}

function newEvent(e: TimelineEntry, who: HistoryWho): HistoryEvent {
	return {
		type: 'event',
		id: e.id,
		at: e.created_at,
		firstAt: e.created_at,
		who,
		entries: [],
		changes: [],
		actions: [],
		versions: [],
		bodyEdited: false,
		notes: [],
		decisions: []
	};
}

/**
 * Fold `e` (older than everything already in `ev`) into the event. Field
 * changes merge per field: the older row supplies `from`, the newer keeps
 * `to`, so "open → in progress" then "in progress → done" reads "open → done"
 * only when both came from ONE writer inside one burst.
 */
function addToEvent(ev: HistoryEvent, e: TimelineEntry) {
	ev.entries.push(e);
	ev.firstAt = e.created_at;
	if (e.kind === 'activity' && e.activity) {
		const meta = parseMeta(e.activity.metadata);
		if (e.activity.action !== 'updated') ev.actions.push(e.activity);
		if (meta.body_edited === 'true') ev.bodyEdited = true;
		const changes = parseFieldChanges(typeof meta.changes === 'string' ? meta.changes : '');
		for (const c of changes) {
			const have = ev.changes.find((x) => x.field === c.field);
			if (have) have.from = c.from;
			else ev.changes.push({ ...c });
		}
	} else if (e.kind === 'version') {
		ev.versions.push(e);
	} else if (e.kind === 'note') {
		ev.notes.push(e);
	} else if (e.kind === 'decision') {
		ev.decisions.push(e);
	}
}

function finish(ev: HistoryEvent): HistoryEvent {
	if (ev.versions.some((v) => !v.version?.is_create)) ev.bodyEdited = false;
	// Notes and decisions read oldest first inside a card, the order written.
	ev.notes.reverse();
	ev.decisions.reverse();
	return ev;
}

/** An event with nothing to say is dropped rather than drawn empty (defect 6). */
function hasContent(ev: HistoryEvent): boolean {
	return (
		ev.changes.length > 0 ||
		ev.actions.length > 0 ||
		ev.versions.length > 0 ||
		ev.bodyEdited ||
		ev.notes.length > 0 ||
		ev.decisions.length > 0
	);
}

/**
 * Group newest-first timeline entries into History rows, newest first.
 * Entries of kinds History does not render are ignored without breaking a
 * burst: a comment between two halves of a save does not split the save.
 */
export function groupHistory(entries: readonly TimelineEntry[]): HistoryRow[] {
	const rows: HistoryRow[] = [];
	let cur: HistoryEvent | null = null;
	const close = () => {
		if (cur) {
			const done = finish(cur);
			if (hasContent(done)) rows.push(done);
		}
		cur = null;
	};
	for (const e of entries) {
		if (!(HISTORY_KINDS as readonly string[]).includes(e.kind)) continue;
		const who = whoOf(e);
		if (isAutosave(e)) {
			close();
			const run = e.autosave_run;
			rows.push({
				type: 'autosave',
				id: e.id,
				at: e.created_at,
				firstAt: run?.first_at ?? e.created_at,
				who,
				entry: e,
				count: run?.count ?? 1
			});
			continue;
		}
		const c: HistoryEvent | null = cur;
		const joins =
			c !== null &&
			// Each recovery stands alone: it is one crashed tab's edits, and two
			// of them are two tabs. (A recovery never joins a person's event —
			// its writer kind is system.)
			!c.versions.some(isRecovery) &&
			sameWriter(c.who, who) &&
			Date.parse(c.firstAt) - Date.parse(e.created_at) <= BURST_WINDOW_MS;
		if (!joins) {
			close();
			cur = newEvent(e, who);
		} else {
			cur!.who = mergeWho(cur!.who, who);
		}
		addToEvent(cur!, e);
	}
	close();
	return rows;
}

// ── Presentation helpers (pure, so the wording is tested) ───────────────────

function joinPhrases(parts: string[]): string {
	if (parts.length <= 1) return parts.join('');
	return parts.slice(0, -1).join(', ') + ' and ' + parts[parts.length - 1];
}


/**
 * The sentence after the writer's name: "changed status", "edited the
 * description and added a note". `noun` is the item's kind ("task").
 */
/** The event is the item's creation. */
export function isCreateEvent(ev: HistoryEvent): boolean {
	return ev.actions.some((a) => a.action === 'created') || ev.versions.some((v) => v.version?.is_create);
}

export function eventVerb(ev: HistoryEvent, noun = 'item', fieldLabel: (key: string) => string = (k) => k): string {
	const parts: string[] = [];
	const created = isCreateEvent(ev);
	if (created) parts.push(`created this ${noun}`);
	for (const a of ev.actions) {
		if (a.action === 'created') continue;
		if (a.action === 'moved') parts.push(`moved this ${noun}`);
		else if (a.action === 'archived') parts.push(`archived this ${noun}`);
		else if (a.action === 'restored') parts.push(`restored this ${noun}`);
		else parts.push(a.action);
	}
	if (!created && ev.changes.length > 0) {
		parts.push(ev.changes.length === 1 ? `changed ${fieldLabel(ev.changes[0].field)}` : `changed ${ev.changes.length} fields`);
	}
	const recovered = ev.versions.some(isRecovery);
	if (recovered) parts.push('recovered unsaved edits');
	const edits = ev.versions.filter((v) => !v.version?.is_create && !isRecovery(v)).length;
	if (edits > 0 || ev.bodyEdited) parts.push('edited the description');
	const n = ev.notes.length;
	if (n > 0) parts.push(n === 1 ? 'added a note' : `added ${n} notes`);
	const d = ev.decisions.length;
	if (d > 0) parts.push(d === 1 ? 'recorded a decision' : `recorded ${d} decisions`);
	return joinPhrases(parts);
}

/** The writer's display name. */
export function whoName(who: HistoryWho): string {
	if (who.kind === 'system') return 'System';
	if (who.kind === 'agent') return who.agent ?? 'Agent';
	return who.user ?? 'Someone';
}

export const SOURCE_LABELS: Record<string, string> = {
	cli: 'CLI',
	web: 'Web',
	skill: 'Skill',
	'collab-snapshot': 'Autosave',
	recovery: 'Recovered'
};

export function sourceLabel(source: string | undefined): string | undefined {
	if (!source) return undefined;
	return SOURCE_LABELS[source] ?? source;
}

// ── Filters ─────────────────────────────────────────────────────────────────

export type HistoryFilter = 'all' | 'fields' | 'content' | 'notes';

export function matchesFilter(row: HistoryRow, filter: HistoryFilter, person: string | null): boolean {
	if (person !== null && whoName(row.who) !== person) return false;
	if (filter === 'all') return true;
	if (row.type === 'autosave') return filter === 'content';
	if (filter === 'fields') return row.changes.length > 0 || row.actions.length > 0;
	if (filter === 'content') return row.versions.length > 0 || row.bodyEdited;
	return row.notes.length > 0 || row.decisions.length > 0;
}

/** Distinct writer names, in order of first appearance (newest first). */
export function peopleOf(rows: readonly HistoryRow[]): string[] {
	const seen: string[] = [];
	for (const r of rows) {
		const n = whoName(r.who);
		if (!seen.includes(n)) seen.push(n);
	}
	return seen;
}

// ── Day separators ──────────────────────────────────────────────────────────

function localDayStart(d: Date): number {
	return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
}

/** "Today", "Yesterday", or a date, for the separator above a row. */
export function dayLabel(at: string, now: Date = new Date()): string {
	const d = new Date(at);
	const days = Math.round((localDayStart(now) - localDayStart(d)) / 86_400_000);
	if (days === 0) return 'Today';
	if (days === 1) return 'Yesterday';
	return d.toLocaleDateString(undefined, {
		weekday: 'short',
		month: 'short',
		day: 'numeric',
		...(d.getFullYear() !== now.getFullYear() ? { year: 'numeric' } : {})
	});
}

/**
 * The item's kind for "created this task", from its collection's name.
 * English plural rules only as far as the default and template collections
 * need them; anything unusual reads as "item" rather than a wrong word.
 */
export function itemNoun(collectionName: string | undefined | null): string {
	const n = (collectionName ?? '').trim().toLowerCase();
	if (!/^[a-z][a-z ]*$/.test(n)) return 'item';
	if (n.endsWith('ies')) return n.slice(0, -3) + 'y';
	if (n.endsWith('ss') || n.endsWith('us') || !n.endsWith('s')) return n;
	return n.slice(0, -1);
}
