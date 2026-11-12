import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import RolePill from './RolePill.svelte';

const zone = 'America/New_York';
const deadline = '2026-11-16T15:00:00Z';
const pill = (props: Record<string, unknown>) => {
	const { container } = render(RolePill, { zone, ...props });
	return container.querySelector('.pill') as HTMLElement;
};
const text = (el: HTMLElement) => el.textContent?.replace(/\s+/g, ' ').trim();

describe('RolePill deadline', () => {
	it('is neutral with a short date when far', () => {
		const el = pill({ kind: 'deadline', value: deadline, behavior: 'lapses', now: new Date('2026-11-12T17:00:00Z') });
		expect(el).toHaveAttribute('data-kind', 'deadline');
		expect(el).toHaveAttribute('data-tone', 'far');
		expect(text(el)).toBe('Deadline Mon 16, 10:00');
	});
	it('is amber within 48 hours and says what happens', () => {
		const el = pill({ kind: 'deadline', value: deadline, behavior: 'lapses', now: new Date('2026-11-16T03:00:00Z') });
		expect(el).toHaveAttribute('data-tone', 'soon');
		expect(text(el)).toBe('Deadline Mon 16, 10:00 · lapses');
	});
	it('is red with the passed wording once passed, without repeating "Deadline"', () => {
		const el = pill({ kind: 'deadline', value: deadline, behavior: 'stays_owed', now: new Date('2026-11-17T00:00:00Z') });
		expect(el).toHaveAttribute('data-tone', 'passed');
		expect(text(el)).toBe('Deadline passed 9 h ago · still owed');
	});
	it('treats a date-only deadline as its whole local day', () => {
		expect(pill({ kind: 'deadline', value: '2026-11-16', now: new Date('2026-11-17T04:59:00Z') })).toHaveAttribute('data-tone', 'soon');
		expect(pill({ kind: 'deadline', value: '2026-11-16', now: new Date('2026-11-17T05:00:00Z') })).toHaveAttribute('data-tone', 'passed');
	});
	it('draws a flag hidden from assistive technology', () => {
		const el = pill({ kind: 'deadline', value: deadline, now: new Date('2026-11-12T17:00:00Z') });
		expect(el.querySelector('svg')).toHaveAttribute('aria-hidden', 'true');
	});
});

describe('RolePill forecast', () => {
	const now = new Date('2026-11-12T17:00:00Z');
	it('is a neutral dashed pill with an approximate date when on time', () => {
		const el = pill({ kind: 'forecast', value: '2026-11-15', deadline, now });
		expect(el).toHaveAttribute('data-kind', 'forecast');
		expect(el).toHaveAttribute('data-tone', 'ontime');
		expect(text(el)).toBe('Forecast Sun 15');
		expect(el.querySelector('svg')).toHaveAttribute('aria-hidden', 'true');
	});
	it('is neutral without a deadline', () => {
		expect(pill({ kind: 'forecast', value: '2030-01-01', now })).toHaveAttribute('data-tone', 'ontime');
	});
	it.each([
		['2026-11-17T21:00:00Z', 'Forecast Tue 17, 16:00 · 1 day late'],
		['2026-11-18', 'Forecast Wed 18 · 2 days late']
	])('is red and says how late when %s lands after the deadline', (value, wording) => {
		const el = pill({ kind: 'forecast', value, deadline, now });
		expect(el).toHaveAttribute('data-tone', 'late');
		expect(text(el)).toBe(wording);
	});
	it('carries provenance in the title', () => {
		expect(pill({ kind: 'forecast', value: '2026-11-15', now, title: 'Set by Alex (agent)' })).toHaveAttribute('title', 'Set by Alex (agent)');
	});
});

describe('RolePill red', () => {
	it('is used only for a passed deadline and a late forecast', () => {
		const now = new Date('2026-11-12T17:00:00Z');
		const tones = [
			pill({ kind: 'deadline', value: deadline, now }),
			pill({ kind: 'deadline', value: deadline, now: new Date('2026-11-16T03:00:00Z') }),
			pill({ kind: 'deadline', value: deadline, now: new Date('2026-11-17T00:00:00Z') }),
			pill({ kind: 'forecast', value: '2026-11-15', deadline, now }),
			pill({ kind: 'forecast', value: '2026-11-18', deadline, now })
		].map(el => el.getAttribute('data-tone'));
		expect(tones).toEqual(['far', 'soon', 'passed', 'ontime', 'late']);
	});
});
