// TASK-4515: how thread rows group and where the day separators fall.
import { describe, expect, it } from 'vitest';
import { dayLabel, layoutRows, listTime } from './grouping';
import type { WindowItem } from './model';
import type { ChatMessage } from '$lib/types';

// Local wall-clock times, so the day maths does not depend on the test machine's zone.
const at = (y: number, mo: number, d: number, h = 12, mi = 0, s = 0) => new Date(y, mo - 1, d, h, mi, s);
const NOW = at(2026, 11, 13, 15, 0);

let seq = 0;
function msg(when: Date, over: Partial<ChatMessage> = {}): WindowItem {
	seq++;
	return {
		kind: 'message',
		message: {
			id: `m${seq}`,
			conversation_id: 'c1',
			seq,
			author_agent_key: 'rook',
			author_is_human: false,
			mine: false,
			source: 'cli',
			body: `body ${seq}`,
			created_at: when.toISOString(),
			deleted: false,
			...over
		}
	};
}
const mine = (when: Date, over: Partial<ChatMessage> = {}) =>
	msg(when, { mine: true, author_is_human: true, author_agent_key: '', ...over });
const pending = (op = 'k1'): WindowItem => ({ kind: 'pending', pending: { op_key: op, body: 'x', status: 'sending' } });
const heads = (rows: { head: boolean }[]) => rows.map((r) => r.head);

describe('layoutRows', () => {
	it('keeps one group for one sender within 5 minutes', () => {
		const rows = layoutRows([msg(at(2026, 11, 13, 9, 0)), msg(at(2026, 11, 13, 9, 4)), msg(at(2026, 11, 13, 9, 9))], { now: NOW });
		expect(heads(rows)).toEqual([true, false, false]);
	});

	it('splits at 5 minutes and 1 second', () => {
		const rows = layoutRows([msg(at(2026, 11, 13, 9, 0, 0)), msg(at(2026, 11, 13, 9, 5, 0)), msg(at(2026, 11, 13, 9, 10, 1))], { now: NOW });
		expect(heads(rows)).toEqual([true, false, true]);
	});

	it('splits on a different sender', () => {
		const t = at(2026, 11, 13, 9, 0);
		const rows = layoutRows([msg(t), mine(t), msg(t, { author_agent_key: 'wren' }), msg(t, { author_agent_key: 'wren' })], { now: NOW });
		expect(heads(rows)).toEqual([true, true, true, false]);
	});

	it('splits when the context workspace changes', () => {
		const t = at(2026, 11, 13, 9, 0);
		const a = { context_workspace: { slug: 'a', name: 'A' } };
		const b = { context_workspace: { slug: 'b', name: 'B' } };
		const rows = layoutRows([msg(t, a), msg(t, a), msg(t, b), msg(t)], { now: NOW });
		expect(heads(rows)).toEqual([true, false, true, true]);
	});

	it('splits on a local day change and puts the day on the first row of each day', () => {
		const rows = layoutRows([msg(at(2026, 11, 12, 23, 58)), msg(at(2026, 11, 13, 0, 1)), msg(at(2026, 11, 13, 0, 2))], { now: NOW });
		expect(heads(rows)).toEqual([true, true, false]);
		expect(rows.map((r) => r.day)).toEqual(['Yesterday', 'Today', null]);
	});

	it('always gives the first row a day and a head', () => {
		const rows = layoutRows([msg(at(2026, 11, 13, 9, 0))], { now: NOW });
		expect(rows).toEqual([{ day: 'Today', head: true }]);
	});

	it('the unread divider forces a head', () => {
		const t = at(2026, 11, 13, 9, 0);
		const rows = layoutRows([msg(t), msg(t), msg(t)], { now: NOW, dividerIndex: 2 });
		expect(heads(rows)).toEqual([true, false, true]);
		expect(rows[2].day).toBeNull();
	});

	it('a tombstone keeps its group', () => {
		const t = at(2026, 11, 13, 9, 0);
		const rows = layoutRows([msg(t), msg(t, { deleted: true, body: '' }), msg(t)], { now: NOW });
		expect(heads(rows)).toEqual([true, false, false]);
		// The server drops a tombstone's context workspace; that is not a source change.
		const a = { context_workspace: { slug: 'a', name: 'A' } };
		const withCtx = layoutRows([msg(t, a), msg(t, { deleted: true, body: '' }), msg(t, a)], { now: NOW });
		expect(heads(withCtx)).toEqual([true, false, false]);
	});

	it('a pending row continues a preceding own group, and otherwise heads its own', () => {
		const recent = new Date(NOW.getTime() - 60_000);
		expect(heads(layoutRows([mine(recent), pending('a'), pending('b')], { now: NOW }))).toEqual([true, false, false]);
		expect(heads(layoutRows([msg(recent), pending('a')], { now: NOW }))).toEqual([true, true]);
		expect(heads(layoutRows([pending('a')], { now: NOW }))).toEqual([true]);
		expect(layoutRows([pending('a')], { now: NOW })[0].day).toBe('Today');
		// An own row from hours ago is a different conversation turn.
		expect(heads(layoutRows([mine(at(2026, 11, 13, 9, 0)), pending('a')], { now: NOW }))).toEqual([true, true]);
	});

	it('out-of-order timestamps do not throw and group on the absolute gap', () => {
		const rows = layoutRows([msg(at(2026, 11, 13, 9, 4)), msg(at(2026, 11, 13, 9, 1)), msg(at(2026, 11, 13, 8, 0))], { now: NOW });
		expect(heads(rows)).toEqual([true, false, true]);
	});

	it('an unreadable timestamp heads its own group', () => {
		const t = at(2026, 11, 13, 9, 0);
		const rows = layoutRows([msg(t), msg(t, { created_at: 'nope' }), msg(t)], { now: NOW });
		expect(heads(rows)).toEqual([true, true, true]);
	});
});

describe('dayLabel', () => {
	it('says Today and Yesterday', () => {
		expect(dayLabel(at(2026, 11, 13, 0, 0).toISOString(), NOW)).toBe('Today');
		expect(dayLabel(at(2026, 11, 12, 23, 59).toISOString(), NOW)).toBe('Yesterday');
	});
	it('names an older day by weekday and date, without the year this year', () => {
		const d = at(2026, 11, 11);
		const label = dayLabel(d.toISOString(), NOW);
		expect(label).toBe(d.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' }));
		expect(label).not.toContain('2026');
	});
	it('adds the year when it is not this year', () => {
		const d = at(2025, 12, 30);
		expect(dayLabel(d.toISOString(), NOW)).toContain('2025');
	});
});

describe('listTime', () => {
	it('today is the time of day', () => {
		const d = at(2026, 11, 13, 9, 40);
		expect(listTime(d.toISOString(), NOW)).toBe(d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }));
	});
	it('yesterday is Yesterday', () => {
		expect(listTime(at(2026, 11, 12, 9, 40).toISOString(), NOW)).toBe('Yesterday');
	});
	it('within a week is the weekday', () => {
		const d = at(2026, 11, 9, 9, 40);
		expect(listTime(d.toISOString(), NOW)).toBe(d.toLocaleDateString(undefined, { weekday: 'short' }));
	});
	it('older is the month and day', () => {
		const d = at(2026, 11, 3, 9, 40);
		expect(listTime(d.toISOString(), NOW)).toBe(d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }));
	});
	it('an empty or unreadable time is empty', () => {
		expect(listTime('', NOW)).toBe('');
		expect(listTime('nope', NOW)).toBe('');
	});
});
