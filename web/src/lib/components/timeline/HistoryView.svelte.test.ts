/**
 * PLAN-2348 U3: the History tab renders grouped events. These mount the real
 * view over rows from the real grouping, and read what a person would read.
 */
import { describe, it, expect, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { TimelineEntry, Version } from '$lib/types';
import HistoryView from './HistoryView.svelte';
import { groupHistory } from './historyEvents';

vi.mock('$lib/api/client', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/api/client')>();
	return {
		...actual,
		api: {
			...actual.api,
			versions: {
				...actual.api.versions,
				diff: vi.fn(async () => ({ before: 'a\n', after: 'b\n' })),
				restore: vi.fn()
			}
		}
	};
});

const NOW = Date.now();
const at = (secAgo: number) => new Date(NOW - secAgo * 1000).toISOString();

function activity(id: string, secAgo: number, o: { actor?: 'user' | 'agent'; source?: string; meta?: Record<string, string>; action?: string } = {}): TimelineEntry {
	const actor = o.actor ?? 'user';
	return {
		id,
		kind: 'activity',
		created_at: at(secAgo),
		actor,
		source: o.source ?? 'web',
		activity: {
			id,
			workspace_id: 'w',
			action: o.action ?? 'updated',
			actor,
			actor_name: 'Dave',
			source: o.source ?? 'web',
			metadata: JSON.stringify(o.meta ?? {}),
			created_at: at(secAgo)
		}
	};
}

function version(id: string, secAgo: number, v: Partial<Version> = {}): TimelineEntry {
	const full: Version = {
		id,
		document_id: 'i',
		content: '',
		change_summary: '',
		created_by: 'user',
		source: 'web',
		is_diff: true,
		created_at: at(secAgo),
		actor_name: 'Dave',
		lines_added: 2,
		lines_removed: 1,
		...v
	};
	return { id, kind: 'version', created_at: at(secAgo), actor: full.created_by, source: full.source, version: full };
}

let root: HTMLElement | null = null;
let instance: ReturnType<typeof mount> | null = null;

function render(entries: TimelineEntry[]) {
	root = document.body.appendChild(document.createElement('div'));
	instance = mount(HistoryView, {
		target: root,
		props: { rows: groupHistory(entries), itemNoun: 'task', wsSlug: 'ws', itemSlug: 'TASK-1', currentContent: 'b\n' }
	});
	flushSync();
	return root;
}

afterEach(() => {
	if (instance) unmount(instance);
	instance = null;
	root?.remove();
	root = null;
});

const rows = () => Array.from(root!.querySelectorAll('[data-testid="history-row"]'));

describe('HistoryView', () => {
	it('one agent save is one card: who, for whom, the door, and every piece', () => {
		render([
			{ id: 'n', kind: 'note', created_at: at(10), actor: 'agent', source: 'structured', note: { summary: 'Wired the exporter' } },
			version('v', 11, { created_by: 'agent', source: 'cli' }),
			activity('a', 12, { actor: 'agent', source: 'cli', meta: { agent: 'claude-code', changes: 'status: open → done' } })
		]);
		expect(rows()).toHaveLength(1);
		const text = rows()[0].textContent!.replace(/\s+/g, ' ');
		expect(text).toContain('claude-code');
		expect(text).toContain('Agent');
		expect(text).toContain('CLI');
		expect(text).toContain('for Dave');
		expect(text).toContain('changed status, edited the description and added a note');
		expect(text).toContain('+2 −1 lines');
		expect(text).toContain('Wired the exporter');
	});

	it('a recovery row reads System / Recovered, never Web or a person', () => {
		render([version('r', 5, { source: 'recovery', created_by: 'system', actor_name: undefined })]);
		const text = rows()[0].textContent!.replace(/\s+/g, ' ');
		expect(text).toContain('System');
		expect(text).toContain('Recovered');
		expect(text).toContain('recovered unsaved edits');
		expect(text).not.toContain('Web');
	});

	it('an autosave run is one dashed row with its count and lines', () => {
		const auto = version('auto', 5, { source: 'collab-snapshot' });
		auto.autosave_run = { count: 4, first_at: at(700), oldest_version_id: 'old', lines_added: 18, lines_removed: 6 };
		render([auto]);
		expect(rows()[0].getAttribute('data-row-type')).toBe('autosave');
		const text = rows()[0].textContent!.replace(/\s+/g, ' ');
		expect(text).toContain('4 autosaves by Dave while editing');
		expect(text).toContain('+18 −6 lines');
	});

	it('marks imported rows, an autosave and a recovery included (BUG-3379)', () => {
		render([
			version('imp-auto', 5, { source: 'collab-snapshot', imported: true }),
			activity('gap', 30),
			version('imp-rec', 60, { source: 'recovery', created_by: 'system', imported: true }),
			activity('gap2', 90),
			version('native', 120)
		]);
		const marked = rows().filter((r) => r.querySelector('[title^="Imported with the workspace"]'));
		expect(marked.map((r) => r.getAttribute('data-row-type'))).toEqual(['autosave', 'event']);
		// Control: the native version row carries no marker.
		expect(rows()[rows().length - 1].querySelector('[title^="Imported with the workspace"]')).toBeNull();
	});

	it('labels writes through an app, an autosave included, and keeps them apart from direct edits (SPEC-6 U9c)', () => {
		const auto = version('via-auto', 5, { source: 'collab-snapshot', via_app: 'inst-1', via_app_name: 'Support Portal' });
		auto.autosave_run = { count: 2, first_at: at(20), oldest_version_id: 'old', lines_added: 1, lines_removed: 0 };
		render([
			auto,
			version('via', 40, { via_app: 'inst-1', via_app_name: 'Support Portal' }),
			version('direct', 50)
		]);
		const title = '[title="Written through the installed app Support Portal"]';
		const labelled = rows().filter((r) => r.querySelector(title));
		expect(labelled.map((r) => r.getAttribute('data-row-type'))).toEqual(['autosave', 'event']);
		expect(labelled[1].querySelector(title)!.textContent).toBe('via Support Portal');
		// The direct edit by the same person is its own row, unlabelled.
		expect(rows()).toHaveLength(3);
		expect(rows()[2].querySelector(title)).toBeNull();
	});

	it('a throttled body edit renders as an edit, not an empty card', () => {
		render([activity('a', 5, { actor: 'agent', source: 'cli', meta: { agent: 'wren', body_edited: 'true' } })]);
		expect(rows()).toHaveLength(1);
		expect(rows()[0].textContent).toContain('edited the description');
	});

	it('the filters narrow by kind of change', () => {
		render([
			version('v', 400, { source: 'cli' }),
			activity('f', 5, { meta: { changes: 'status: open → done' } })
		]);
		expect(rows()).toHaveLength(2);
		const chip = (label: string) =>
			Array.from(root!.querySelectorAll<HTMLButtonElement>('.filter-chip')).find((b) => b.textContent === label)!;
		chip('Fields').click();
		flushSync();
		expect(rows()).toHaveLength(1);
		expect(rows()[0].textContent).toContain('changed status');
		chip('Content').click();
		flushSync();
		expect(rows()).toHaveLength(1);
		expect(rows()[0].textContent).toContain('edited the description');
		chip('Notes & decisions').click();
		flushSync();
		expect(rows()).toHaveLength(0);
		expect(root!.textContent).toContain('Nothing matches this filter.');
	});

	it('a day separator heads the feed', () => {
		render([activity('f', 5, { meta: { changes: 'status: open → done' } })]);
		expect(root!.querySelector('.day')?.textContent).toBe('Today');
	});
});

describe('HistoryView: an edit the throttle saved no version for', () => {
	it('beside a create, shows the create section and a since-now diff against the current body', async () => {
		render([
			activity('u', 5, { source: 'cli', meta: { body_edited: 'true' } }),
			version('c', 6, { source: 'cli', is_create: true, lines_added: 5, lines_removed: 0 }),
			activity('c', 7, { action: 'created', source: 'cli' })
		]);
		expect(rows()).toHaveLength(1);
		const cards = Array.from(root!.querySelectorAll('.version-card'));
		expect(cards.map((c) => c.getAttribute('data-diff'))).toEqual(['edit', 'current']);
		expect(cards[0].textContent).toContain('+5 −0 lines');
		expect(cards[1].textContent).toContain('every change since the last saved version, up to now');
		(cards[1].querySelector('.show-changes') as HTMLButtonElement).click();
		flushSync();
		await vi.waitFor(() => expect(cards[1].querySelector('.pair-head')).not.toBeNull());
		// No row holds the since-now state, so there is nothing to restore to.
		expect(cards[1].querySelector('.btn-restore')).toBeNull();
	});

	it('older than a saved version, it cannot be isolated and says so', () => {
		render([
			version('v', 5, { source: 'web' }),
			activity('u', 300, { source: 'cli', meta: { body_edited: 'true' } })
		]);
		expect(rows()).toHaveLength(2);
		expect(rows()[1].querySelector('.version-card')).toBeNull();
		expect(rows()[1].textContent).toContain('no version was saved for this edit');
	});
});

// TASK-2198 U4, moved here from TimelineVersionCard.svelte.test.ts by PLAN-2348
// U3: the op-log recovery's version row is the system's, and must not read as a
// user's edit. The card no longer carries attribution; the event header does.
describe('HistoryView recovery attribution (TASK-2198 U4)', () => {
	it('labels a recovery version System / Recovered, and a user version by name / Web', () => {
		const header = (e: TimelineEntry) => {
			render([e]);
			const text = root!.querySelector('.head')?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
			if (instance) unmount(instance);
			instance = null;
			root?.remove();
			root = null;
			return text;
		};
		const recovered = header(
			version('r', 5, {
				created_by: 'system',
				source: 'recovery',
				actor_name: undefined,
				change_summary: 'recovered from an unsaved editor session'
			})
		);
		expect(recovered).toContain('System');
		expect(recovered).toContain('Recovered');
		expect(recovered).not.toContain('Web');
		const user = header(version('u', 5, { source: 'web' }));
		expect(user).toContain('Dave');
		expect(user).toContain('Web');
		expect(user).not.toContain('System');
	});
});
