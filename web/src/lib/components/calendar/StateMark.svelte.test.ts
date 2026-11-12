import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import StateMark from './StateMark.svelte';
import { OCCURRENCE_STATES, type OccurrenceState } from '$lib/calendar/presentation';

const states = Object.keys(OCCURRENCE_STATES) as OccurrenceState[];
const mark = () => screen.getByRole('img');

describe('StateMark', () => {
	it.each(states)('names the %s mark with the state label from the shared table', state => {
		render(StateMark, { state });
		expect(screen.getByRole('img', { name: OCCURRENCE_STATES[state].label })).toBeInTheDocument();
		expect(mark()).toHaveAttribute('data-mark', OCCURRENCE_STATES[state].mark);
	});
	it.each([
		['outstanding', 'ring', 'amber'], ['scheduled', 'ring-thin', 'gray'], ['unrecorded', 'ring-dashed', 'gray'],
		['done', 'check', 'green'], ['skipped', 'dash', 'gray'], ['moved', 'arrow', 'gray'],
		['lapsed', 'cross', 'red'], ['missed', 'cross', 'red']
	] as [OccurrenceState, string, string][])('draws %s as a %s in %s', (state, shape, tone) => {
		render(StateMark, { state });
		expect(mark()).toHaveAttribute('data-mark', shape);
		expect(mark()).toHaveAttribute('data-tone', tone);
	});
	it('turns the outstanding ring red once its target or deadline has passed, keeping its name', () => {
		render(StateMark, { state: 'outstanding', overdue: true });
		expect(screen.getByRole('img', { name: 'Outstanding' })).toHaveAttribute('data-tone', 'red');
		expect(mark()).toHaveAttribute('data-mark', 'ring');
	});
	it.each(states)('is red for %s only where the design says so', state => {
		for (const overdue of [false, true]) {
			const { unmount } = render(StateMark, { state, overdue });
			const red = state === 'lapsed' || state === 'missed' || (state === 'outstanding' && overdue);
			expect(mark().getAttribute('data-tone') === 'red').toBe(red);
			unmount();
		}
	});
	it('draws a dated field as a square named by its label, red only when overdue', () => {
		const { unmount } = render(StateMark, { label: 'Due' });
		expect(screen.getByRole('img', { name: 'Due' })).toHaveAttribute('data-mark', 'square');
		expect(screen.getByRole('img', { name: 'Due' })).toHaveAttribute('data-tone', 'gray');
		unmount();
		render(StateMark, { overdue: true });
		expect(screen.getByRole('img', { name: 'Date' })).toHaveAttribute('data-tone', 'red');
	});
	it('hides its drawing from assistive technology', () => {
		const { container } = render(StateMark, { state: 'done' });
		for (const svg of container.querySelectorAll('svg')) expect(svg).toHaveAttribute('aria-hidden', 'true');
		expect(container.querySelector('svg')).not.toBeNull();
	});
});
