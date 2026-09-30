import { describe, it, expect } from 'vitest';
import type { TimelineEntry, Version } from '$lib/types';
import {
	groupHistory,
	eventVerb,
	matchesFilter,
	peopleOf,
	dayLabel,
	itemNoun,
	whoName,
	type HistoryEvent
} from './historyEvents';

const T0 = Date.parse('2026-09-30T12:00:00Z');
const at = (sec: number) => new Date(T0 + sec * 1000).toISOString();

function activity(
	id: string,
	sec: number,
	opts: { action?: string; actor?: 'user' | 'agent'; name?: string; source?: string; meta?: Record<string, string> } = {}
): TimelineEntry {
	const actor = opts.actor ?? 'user';
	return {
		id,
		kind: 'activity',
		created_at: at(sec),
		actor,
		source: opts.source ?? 'web',
		activity: {
			id,
			workspace_id: 'w',
			action: opts.action ?? 'updated',
			actor,
			actor_name: opts.name ?? 'Dave',
			source: opts.source ?? 'web',
			metadata: JSON.stringify(opts.meta ?? {}),
			created_at: at(sec)
		}
	};
}

function version(id: string, sec: number, v: Partial<Version> = {}): TimelineEntry {
	const full: Version = {
		id,
		document_id: 'i',
		content: '',
		change_summary: '',
		created_by: 'user',
		source: 'web',
		is_diff: true,
		created_at: at(sec),
		actor_name: 'Dave',
		...v
	};
	return { id, kind: 'version', created_at: at(sec), actor: full.created_by, source: full.source, version: full };
}

function note(id: string, sec: number, actor = 'agent'): TimelineEntry {
	return { id, kind: 'note', created_at: at(sec), actor, source: 'structured', note: { summary: 'n ' + id } };
}

const events = (entries: TimelineEntry[]) => groupHistory(entries).filter((r): r is HistoryEvent => r.type === 'event');

describe('groupHistory', () => {
	it('one writer inside the window is one event, its pieces merged', () => {
		const rows = groupHistory([
			note('n1', 30),
			version('v1', 20, { created_by: 'agent', source: 'cli', actor_name: 'Dave' }),
			activity('a1', 10, { actor: 'agent', source: 'cli', meta: { agent: 'claude-code', changes: 'status: open → done' } })
		]);
		expect(rows).toHaveLength(1);
		const ev = rows[0] as HistoryEvent;
		expect(ev.id).toBe('n1');
		expect(ev.who).toEqual({ kind: 'agent', agent: 'claude-code', user: 'Dave', source: 'cli' });
		expect(ev.changes).toEqual([{ field: 'status', from: 'open', to: 'done' }]);
		expect(ev.versions.map((v) => v.id)).toEqual(['v1']);
		expect(ev.notes.map((n) => n.id)).toEqual(['n1']);
	});

	it('a web change and a CLI change by the same person are two events (defect 1)', () => {
		const rows = events([
			activity('cli', 60, { source: 'cli', meta: { changes: 'status: in-progress → done' } }),
			activity('web', 30, { source: 'web', meta: { changes: 'status: open → in-progress' } })
		]);
		expect(rows.map((r) => r.id)).toEqual(['cli', 'web']);
		expect(rows[1].changes[0]).toEqual({ field: 'status', from: 'open', to: 'in-progress' });
	});

	it('two agents acting for one user are two events', () => {
		const rows = events([
			activity('b', 20, { actor: 'agent', meta: { agent: 'rook', changes: 'priority: low → high' } }),
			activity('a', 10, { actor: 'agent', meta: { agent: 'wren', changes: 'priority: medium → low' } })
		]);
		expect(rows.map((r) => r.id)).toEqual(['b', 'a']);
	});

	it('rows further apart than the window are two events', () => {
		const rows = events([
			activity('late', 200, { meta: { changes: 'status: in-progress → done' } }),
			activity('early', 0, { meta: { changes: 'status: open → in-progress' } })
		]);
		expect(rows.map((r) => r.id)).toEqual(['late', 'early']);
	});

	it('the window is measured from the oldest row so far, so a steady run stays one event', () => {
		const rows = events([
			activity('c', 200, { meta: { changes: 'status: b → c' } }),
			activity('b', 100, { meta: { changes: 'status: a → b' } }),
			activity('a', 0, { meta: { changes: 'status: o → a' } })
		]);
		expect(rows).toHaveLength(1);
		expect(rows[0].changes).toEqual([{ field: 'status', from: 'o', to: 'c' }]);
	});

	it('a comment between two halves of a save does not split it', () => {
		const comment: TimelineEntry = { id: 'c', kind: 'comment', created_at: at(15), actor: 'user', source: 'web' };
		const rows = events([
			activity('a2', 20, { meta: { changes: 'priority: low → high' } }),
			comment,
			activity('a1', 10, { meta: { changes: 'status: open → done' } })
		]);
		expect(rows).toHaveLength(1);
		expect(rows[0].entries.map((e) => e.id)).toEqual(['a2', 'a1']);
	});

	it('an autosave is its own row and breaks the event around it', () => {
		const auto = version('auto', 20, { source: 'collab-snapshot' });
		auto.autosave_run = { count: 4, first_at: at(5), oldest_version_id: 'old', lines_added: 18, lines_removed: 6 };
		const rows = groupHistory([
			activity('a2', 30, { meta: { changes: 'priority: low → high' } }),
			auto,
			activity('a1', 10, { meta: { changes: 'status: open → done' } })
		]);
		expect(rows.map((r) => `${r.type}:${r.id}`)).toEqual(['event:a2', 'autosave:auto', 'event:a1']);
		const row = rows[1];
		expect(row.type === 'autosave' && row.count).toBe(4);
		expect(row.firstAt).toBe(at(5));
	});

	it('a recovery version is System, alone, never Web or a user', () => {
		const rows = events([
			activity('a', 20, { meta: { changes: 'status: open → done' } }),
			version('rec', 15, { source: 'recovery', created_by: 'system', actor_name: undefined })
		]);
		expect(rows.map((r) => r.id)).toEqual(['a', 'rec']);
		expect(rows[1].who).toEqual({ kind: 'system', source: 'recovery' });
		expect(whoName(rows[1].who)).toBe('System');
		expect(eventVerb(rows[1])).toBe('recovered unsaved edits');
	});

	it('two recoveries are two events', () => {
		const rows = events([
			version('r2', 20, { source: 'recovery', created_by: 'system' }),
			version('r1', 10, { source: 'recovery', created_by: 'system' })
		]);
		expect(rows.map((r) => r.id)).toEqual(['r2', 'r1']);
	});

	it('body_edited stands in for a body edit only when no version row does', () => {
		const alone = events([activity('a', 10, { actor: 'agent', meta: { agent: 'wren', body_edited: 'true' } })]);
		expect(alone).toHaveLength(1);
		expect(alone[0].bodyEdited).toBe(true);
		expect(eventVerb(alone[0])).toBe('edited the description');

		const withVersion = events([
			activity('a', 20, { actor: 'agent', source: 'cli', meta: { agent: 'wren', body_edited: 'true' } }),
			version('v', 10, { created_by: 'agent', source: 'cli' })
		]);
		expect(withVersion).toHaveLength(1);
		expect(withVersion[0].bodyEdited).toBe(false);
		expect(withVersion[0].versions).toHaveLength(1);
	});

	it('an event with nothing to show is dropped, never an empty card (defect 6)', () => {
		// A note's own update row: its only change is the suppressed notes key.
		const rows = groupHistory([
			activity('a', 10, { actor: 'agent', meta: { agent: 'wren', changes: 'implementation_notes: [] → [x]' } })
		]);
		expect(rows).toEqual([]);
	});

	it('a note joins the update row of the same agent', () => {
		const rows = events([
			note('n', 11),
			activity('a', 10, { actor: 'agent', source: 'cli', meta: { agent: 'wren', changes: 'implementation_notes: a → b' } })
		]);
		expect(rows).toHaveLength(1);
		expect(rows[0].who.agent).toBe('wren');
		expect(eventVerb(rows[0])).toBe('added a note');
	});
});

describe('eventVerb', () => {
	it('names one field, counts several, and joins with "and"', () => {
		const [one] = events([activity('a', 0, { meta: { changes: 'status: open → done' } })]);
		expect(eventVerb(one)).toBe('changed status');
		const [two] = events([activity('a', 0, { meta: { changes: 'status: open → done; priority: low → high' } })]);
		expect(eventVerb(two)).toBe('changed 2 fields');
		const [mixed] = events([
			note('n2', 3, 'user'),
			note('n1', 2, 'user'),
			version('v', 1),
			activity('a', 0, { meta: { changes: 'status: open → done' } })
		]);
		expect(eventVerb(mixed)).toBe('changed status, edited the description and added 2 notes');
	});

	it('a create reads as created, with the item noun', () => {
		const [ev] = events([
			version('v', 1, { is_create: true, source: 'cli' }),
			activity('a', 0, { action: 'created', source: 'cli' })
		]);
		expect(eventVerb(ev, 'task')).toBe('created this task');
	});
});

describe('filters and people', () => {
	const rows = groupHistory([
		note('n', 400),
		activity('f', 300, { meta: { changes: 'status: open → done' } }),
		version('v', 150, { source: 'cli' })
	]);

	it('each filter keeps its kind of event', () => {
		expect(rows.filter((r) => matchesFilter(r, 'fields', null)).map((r) => r.id)).toEqual(['f']);
		expect(rows.filter((r) => matchesFilter(r, 'content', null)).map((r) => r.id)).toEqual(['v']);
		expect(rows.filter((r) => matchesFilter(r, 'notes', null)).map((r) => r.id)).toEqual(['n']);
		expect(rows.filter((r) => matchesFilter(r, 'all', null))).toHaveLength(3);
	});

	it('people are the writers, and the person filter narrows to one', () => {
		expect(peopleOf(rows)).toEqual(['Agent', 'Dave']);
		expect(rows.filter((r) => matchesFilter(r, 'all', 'Dave')).map((r) => r.id)).toEqual(['f', 'v']);
	});
});

describe('dayLabel', () => {
	it('says Today and Yesterday, then a date', () => {
		const now = new Date(2026, 8, 30, 15, 0);
		expect(dayLabel(new Date(2026, 8, 30, 0, 5).toISOString(), now)).toBe('Today');
		expect(dayLabel(new Date(2026, 8, 29, 23, 55).toISOString(), now)).toBe('Yesterday');
		expect(dayLabel(new Date(2026, 8, 20, 12).toISOString(), now)).not.toMatch(/Today|Yesterday/);
	});
});

describe('itemNoun', () => {
	it('singularises collection names and falls back to item', () => {
		expect(itemNoun('Tasks')).toBe('task');
		expect(itemNoun('Companies')).toBe('company');
		expect(itemNoun('Status')).toBe('status');
		expect(itemNoun('Feedback')).toBe('feedback');
		expect(itemNoun('🚀 Launches')).toBe('item');
		expect(itemNoun(undefined)).toBe('item');
	});
});
