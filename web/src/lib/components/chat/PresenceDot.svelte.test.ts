// TASK-4044 2b-i: the presence dot. Shape and label carry the state, not colour.
import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import PresenceDot from './PresenceDot.svelte';
import type { AgentPresence } from '$lib/chat/presence';

afterEach(cleanup);

const STATES: Array<[AgentPresence, string]> = [
	['online', 'Online'],
	['busy_uncertain', 'May be working'],
	['connected_not_accepting', 'Connected, not accepting'],
	['offline', 'Offline'],
	['unknown', 'Status unknown']
];

describe('PresenceDot (TASK-4044 2b-i)', () => {
	it.each(STATES)('%s is labelled %s', (state, label) => {
		const { getByLabelText } = render(PresenceDot, { props: { state } });
		expect(getByLabelText(label)).toBeTruthy();
	});

	it('the five labels are distinct', () => {
		expect(new Set(STATES.map(([, l]) => l)).size).toBe(5);
	});

	it('unknown is a dashed ring and offline is not', () => {
		const shape = (state: AgentPresence) =>
			render(PresenceDot, { props: { state } }).container.querySelector('.presence-dot')!.getAttribute('data-shape');
		expect(shape('unknown')).toBe('dashed');
		expect(shape('offline')).toBe('solid');
		expect(shape('busy_uncertain')).toBe('hollow');
		expect(shape('online')).toBe('solid');
		expect(shape('connected_not_accepting')).toBe('square');
	});

	it('busy says "May be working" with the last-checked time, never Online', () => {
		const at = new Date(2026, 9, 26, 14, 5, 9).getTime();
		const { container, queryByLabelText } = render(PresenceDot, {
			props: { state: 'busy_uncertain', checkedAt: at }
		});
		const el = container.querySelector('.presence-dot')!;
		expect(el.getAttribute('title')).toBe('May be working. Last checked 14:05:09');
		expect(el.getAttribute('aria-label')).toBe('May be working');
		expect(queryByLabelText('Online')).toBeNull();
	});

	it('busy without a checkedAt omits the clause', () => {
		const { container } = render(PresenceDot, { props: { state: 'busy_uncertain' } });
		expect(container.querySelector('.presence-dot')?.getAttribute('title')).toBe('May be working');
	});
});
