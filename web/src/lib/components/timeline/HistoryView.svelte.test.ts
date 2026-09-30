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
